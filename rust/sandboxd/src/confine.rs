//! Landlock ruleset construction and enforcement.
//!
//! Enforced in the child process after `fork` and before `exec`, which is the
//! earliest point where the kernel can decide the policy soundly. Confining the
//! parent would confine us too.

use landlock::{
    ABI, Access, AccessFs, AccessNet, BitFlags, CompatLevel, Compatible, PathBeneath, PathFd,
    Ruleset, RulesetAttr, RulesetCreatedAttr,
};

use crate::policy::{Mode, Network, Policy, device_grants, system_read_paths};
use crate::probe;

/// Landlock access rights that are only meaningful on a directory.
///
/// Granting any of these on a regular file is an error, not a no-op, and the
/// crate rejects it at `add_rules` time. Files and directories therefore need
/// genuinely different right sets rather than one shared superset.
fn dir_only() -> BitFlags<AccessFs, u64> {
    BitFlags::from(AccessFs::ReadDir)
        | BitFlags::from(AccessFs::RemoveDir)
        | BitFlags::from(AccessFs::RemoveFile)
        | BitFlags::from(AccessFs::MakeDir)
        | BitFlags::from(AccessFs::MakeReg)
        | BitFlags::from(AccessFs::MakeSock)
        | BitFlags::from(AccessFs::MakeFifo)
        | BitFlags::from(AccessFs::MakeSym)
        | BitFlags::from(AccessFs::Refer)
}

/// Rights for a path that may only be read.
///
/// `from_read` minus the directory-only rights, which cannot legally appear on
/// a file. `Execute` is OR'd in separately because it only exists from ABI v3.
fn read_access(abi: ABI) -> BitFlags<AccessFs, u64> {
    let base: BitFlags<AccessFs, u64> = AccessFs::from_read(abi);
    let base = base & !dir_only();
    if abi >= ABI::V3 {
        base | AccessFs::Execute
    } else {
        base
    }
}

/// Rights for a directory that may be read and listed.
fn read_dir_access(abi: ABI) -> BitFlags<AccessFs, u64> {
    let base: BitFlags<AccessFs, u64> = AccessFs::from_read(abi);
    if abi >= ABI::V3 {
        base | AccessFs::Execute
    } else {
        base
    }
}

/// Rights for a file that may be modified.
fn write_file_access(abi: ABI) -> BitFlags<AccessFs, u64> {
    let all: BitFlags<AccessFs, u64> = AccessFs::from_all(abi);
    all & !dir_only() & !AccessFs::MakeChar & !AccessFs::MakeBlock
}

/// Rights for a writable directory.
///
/// Full access except device-node creation, which needs privileges we do not
/// have and never want to grant. enumflags2 has no `Sub` for bitflags, so the
/// removal is expressed as a masked intersection.
fn write_dir_access(abi: ABI) -> BitFlags<AccessFs, u64> {
    let all: BitFlags<AccessFs, u64> = AccessFs::from_all(abi);
    all & !AccessFs::MakeChar & !AccessFs::MakeBlock
}

/// Pick the correct right set for a path and mode.
///
/// Directories get the directory-shaped sets; everything else gets the
/// file-shaped sets. Getting this wrong is a hard error from the kernel rather
/// than a silent over-grant, which is the behaviour we want.
fn access_for(path: &std::path::Path, mode: Mode, abi: ABI) -> BitFlags<AccessFs, u64> {
    match (mode, path.is_dir()) {
        (Mode::ReadOnly, true) => read_dir_access(abi),
        (Mode::ReadOnly, false) => read_access(abi),
        (Mode::ReadWrite, true) => write_dir_access(abi),
        (Mode::ReadWrite, false) => write_file_access(abi),
    }
}

/// Build the ruleset for a policy at the given kernel ABI.
///
/// Kept separate from enforcement so an invalid policy is rejected in the
/// parent, before we fork, and so `describe` can report what *would* be applied.
fn build(policy: &Policy, abi: ABI) -> Result<Vec<PathBeneath<PathFd>>, String> {
    let mut rules: Vec<PathBeneath<PathFd>> = Vec::new();

    // Read + execute grants: the system set plus caller additions. These let the
    // process start; they cannot be narrowed further without breaking exec.
    let read_paths: Vec<_> = system_read_paths()
        .into_iter()
        .chain(policy.extra_read.iter().cloned())
        .filter(|p| p.exists())
        .collect();

    for p in read_paths {
        let fd = PathFd::new(&p).map_err(|e| format!("open read grant {}: {e}", p.display()))?;
        rules.push(PathBeneath::new(fd, access_for(&p, Mode::ReadOnly, abi)));
    }

    // Pseudo-devices, each with the narrowest rights it needs to be useful.
    for (p, mode) in device_grants() {
        if !p.exists() {
            continue; // container without /dev, or a kernel without pty
        }
        let fd = PathFd::new(&p).map_err(|e| format!("open device grant {}: {e}", p.display()))?;
        rules.push(PathBeneath::new(fd, access_for(&p, mode, abi)));
    }

    // Writable roots, added with the broader right set. Landlock is additive, so
    // a path matching both a read and a write rule correctly ends up with the
    // union of the two.
    for p in &policy.writable {
        let fd = PathFd::new(p).map_err(|e| format!("open writable root {}: {e}", p.display()))?;
        rules.push(PathBeneath::new(fd, access_for(p, Mode::ReadWrite, abi)));
    }

    Ok(rules)
}

/// Handle the access families this policy mediates, at the kernel's ABI.
///
/// Network rights are *handled* with no rules attached whenever egress is
/// denied, which is exactly how Landlock denies: a mediated right with no
/// matching rule has no grant.
fn handle(policy: &Policy, abi: ABI) -> Result<Ruleset, String> {
    // Hard requirement must be set *before* create(): it is what makes the
    // kernel reject any access right it cannot actually enforce, instead of
    // silently downgrading to partial enforcement. Setting it later validates
    // nothing.
    let mut rs = Ruleset::default()
        .set_compatibility(CompatLevel::HardRequirement)
        .handle_access(AccessFs::from_all(abi))
        .map_err(|e| format!("handle fs access: {e}"))?;

    if policy.network == Network::Deny && abi >= ABI::V4 {
        rs = rs
            .handle_access(AccessNet::from_all(abi))
            .map_err(|e| format!("handle net access: {e}"))?;
    } else if policy.network == Network::Deny {
        return Err(format!(
            "policy denies network egress but kernel ABI is v{}, need v4+",
            abi as u32
        ));
    }

    Ok(rs)
}

/// Apply the policy to the calling process.
///
/// Must be called in the child, post-fork, pre-exec. Enforces
/// `PR_SET_NO_NEW_PRIVS` (without which Landlock refuses to apply) and verifies
/// the kernel actually enforced the ruleset instead of accepting and ignoring
/// it.
pub fn confine(policy: &Policy) -> Result<(), String> {
    // Use the kernel's own ABI rather than our compile-time max: we ask for
    // everything the kernel knows, then assert full enforcement. This way a
    // future kernel with extra rights tightens us automatically, and a kernel
    // that enforces less than we need shows up as PartiallyEnforced.
    let report = probe::report()?;
    let abi = report.abi;

    let rules = build(policy, abi)?;
    let created = handle(policy, abi)?
        .create()
        .map_err(|e| format!("create ruleset: {e}"))?
        .add_rules(rules.into_iter().map(Ok::<_, landlock::RulesetError>))
        .map_err(|e| format!("add rules: {e}"))?
        .set_compatibility(CompatLevel::HardRequirement);

    let status = created
        .restrict_self()
        .map_err(|e| format!("restrict_self: {e}"))?;

    probe::assert_enforced(&status)?;

    if policy.no_new_privs && !status.no_new_privs {
        return Err(
            "PR_SET_NO_NEW_PRIVS was not confirmed - setuid privilege escalation remains possible"
                .into(),
        );
    }

    Ok(())
}

/// Report what was requested and what the kernel offered, for the transcript.
///
/// A reviewer should be able to read the run log and see the requested policy,
/// the ABI, and the confirmation that enforcement actually happened.
pub fn describe(policy: &Policy) -> String {
    let mut s = String::new();
    match probe::report() {
        Ok(r) => {
            let built = build(policy, r.abi).and_then(|rules| {
                handle(policy, r.abi)?
                    .create()
                    .map_err(|e| format!("create: {e}"))?
                    .add_rules(rules.into_iter().map(Ok::<_, landlock::RulesetError>))
                    .map_err(|e| format!("add_rules: {e}"))?;
                Ok::<(), String>(())
            });
            match built {
                Ok(()) => {
                    s.push_str(&format!(
                        "confinement: ruleset built at kernel ABI v{}\n",
                        r.abi as u32
                    ));
                    s.push_str("             rules      : ");
                    s.push_str(&format!(
                        "{} path rule(s), network {}\n",
                        system_read_paths().len() + policy.extra_read.len() + policy.writable.len(),
                        match policy.network {
                            Network::Deny => "mediated, zero grants (deny all)",
                            Network::Allow => "not mediated",
                        }
                    ));
                    s.push_str("             enforcement: asserted at restrict_self()\n");
                }
                Err(e) => s.push_str(&format!("confinement: FAILED TO BUILD - {e}\n")),
            }
        }
        Err(e) => s.push_str(&format!("confinement: KERNEL CANNOT CONFINE - {e}\n")),
    }
    s
}

/// How many rules `build` would install, without opening anything.
///
/// Counted separately from `build` so the number can be reported in the
/// attestation, which is written from the child on a path where opening files is
/// no longer safe. Both functions read the same policy, so the counts agree.
pub fn planned_rule_count(policy: &Policy) -> u32 {
    let read = system_read_paths()
        .into_iter()
        .chain(policy.extra_read.iter().cloned())
        .filter(|p| p.exists())
        .count();
    let devices = device_grants().iter().filter(|(p, _)| p.exists()).count();
    let writable = policy.writable.iter().filter(|p| p.exists()).count();
    (read + devices + writable) as u32
}
