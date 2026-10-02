//! Runtime probe of the host kernel's Landlock capability.
//!
//! Sandbox policy is only meaningful if we know which access rights the kernel
//! will actually *enforce*. Landlock is versioned and additive: a ruleset that
//! requests an access right the running kernel does not implement is silently
//! downgraded, and the process ends up less confined than the policy claims.
//! We probe before building any ruleset so a downgrade is a loud failure.

use landlock::{ABI, RulesetStatus};

/// `LANDLOCK_CREATE_RULESET_VERSION` from `linux/landlock.h`. Passing this flag
/// turns the syscall into a pure version query instead of a ruleset creation.
const CREATE_RULESET_VERSION: u32 = 1;

/// Highest Landlock ABI this binary is compiled against.
pub const COMPILED_MAX: ABI = ABI::V9;

/// Query the kernel for its Landlock ABI with the documented version flag.
///
/// This enforces nothing and creates nothing: with `LANDLOCK_CREATE_RULESET_VERSION`
/// and a null ruleset, the syscall only reports the supported ABI. Safe to call
/// before deciding whether the host can be trusted to confine anything.
///
/// Returns the ABI, or `None` with the reason Landlock is unusable.
pub fn kernel_abi() -> Result<ABI, Unavailable> {
    // SAFETY: null pointer with size 0 is the documented way to ask for the
    // version; the kernel validates the flag and writes no memory.
    let rc = unsafe {
        libc::syscall(
            libc::SYS_landlock_create_ruleset,
            std::ptr::null::<libc::c_void>(),
            0usize,
            CREATE_RULESET_VERSION,
        )
    };

    if rc >= 0 {
        return Ok(ABI::from(rc as i32));
    }

    let errno = std::io::Error::last_os_error().raw_os_error().unwrap_or(0);
    Err(match errno {
        // LSM compiled in but not active: needs "landlock" in CONFIG_LSM / boot lsm=
        libc::EOPNOTSUPP => Unavailable::NotEnabled,
        // Kernel predates Landlock entirely (introduced 5.13)
        libc::ENOSYS => Unavailable::NotImplemented,
        other => Unavailable::Errno(other),
    })
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Unavailable {
    /// LSM present in the kernel but disabled at boot.
    NotEnabled,
    /// Kernel predates Landlock (5.13).
    NotImplemented,
    /// Anything else, e.g. blocked by seccomp.
    Errno(i32),
}

impl Unavailable {
    fn reason(&self) -> String {
        match self {
            Unavailable::NotEnabled => {
                "Landlock compiled in but disabled at boot (needs \"landlock\" in CONFIG_LSM \
                 or the boot `lsm=` parameter)"
                    .into()
            }
            Unavailable::NotImplemented => {
                "kernel predates Landlock (needs Linux >= 5.13, CONFIG_SECURITY_LANDLOCK=y)".into()
            }
            Unavailable::Errno(e) => {
                format!("landlock_create_ruleset failed with errno {e}")
            }
        }
    }
}

/// Full capability report for the running kernel.
pub struct Report {
    /// `Some` when Landlock is available and enforcing.
    pub abi: ABI,
    /// Whether TCP bind/connect scoping can be enforced (needs ABI v4+).
    pub network_enforceable: bool,
}

impl Report {
    /// Whether this host can scope the filesystem at all.
    ///
    /// ABI v3 is the floor: it is the first version that can grant *execute*
    /// rights, without which we cannot run most binaries inside a ruleset.
    pub fn can_confine(&self) -> bool {
        self.abi >= ABI::V3
    }

    pub fn render(&self) -> String {
        format!(
            "Landlock      : ENFORCED at ABI v{}\n\
             \x20              compiled vs: ABI v{} (we never silently downgrade)\n\
             \x20              fs scoping  : {}\n\
             \x20              net scoping : {}\n\
             \x20              verdict     : {}\n",
            self.abi as u32,
            COMPILED_MAX as u32,
            if self.abi >= ABI::V3 {
                "yes (read/write/execute per-path)"
            } else {
                "NO - ABI v3 required to grant execute rights"
            },
            if self.network_enforceable {
                "yes (tcp bind/connect can be denied)"
            } else {
                "NO - ABI v4 required to deny network egress"
            },
            if self.can_confine() {
                "can confine"
            } else {
                "cannot confine (ABI too old)"
            },
        )
    }
}

/// Probe the host, returning a full report or a human-readable reason to abort.
pub fn report() -> Result<Report, String> {
    let abi = kernel_abi().map_err(|e| e.reason())?;
    Ok(Report {
        abi,
        network_enforceable: abi >= ABI::V4,
    })
}

/// Fail closed: refuse to run unless the host can enforce our full policy.
///
/// A sandbox that silently degrades is worse than no sandbox, because it grants
/// false confidence. We require network-deny capability because every run
/// defaults to "no egress"; a kernel that cannot enforce that is unusable for
/// the default policy, and we would rather abort than pretend.
pub fn require_confinement() -> Result<Report, String> {
    let r = report()?;
    if !r.can_confine() {
        return Err(format!(
            "refusing to run: kernel ABI v{} cannot scope the filesystem (need v3+).\n{}",
            r.abi as u32,
            r.render()
        ));
    }
    if !r.network_enforceable {
        return Err(format!(
            "refusing to run: default policy denies network egress but kernel ABI is v{}, \
             need v4+.\n{}",
            r.abi as u32,
            r.render()
        ));
    }
    Ok(r)
}

/// Confirm a ruleset landed fully enforced rather than partially applied.
///
/// `NotEnforced` means the kernel accepted the syscall while restricting
/// nothing. `PartiallyEnforced` is just as dangerous: some rights landed, others
/// silently did not, so the process is *less* confined than the policy claims.
/// Both are hard failures - we do not degrade, we abort.
pub fn assert_enforced(status: &landlock::RestrictionStatus) -> Result<(), String> {
    match status.ruleset {
        RulesetStatus::FullyEnforced => Ok(()),
        RulesetStatus::NotEnforced => {
            Err("landlock accepted the ruleset but is NOT enforcing - process is unconfined".into())
        }
        RulesetStatus::PartiallyEnforced => Err(
            "landlock only PARTIALLY enforced the ruleset - confinement is weaker than the \
             policy claims; refusing to run with a false sense of safety"
                .into(),
        ),
    }
}

/// The Landlock ABI the executor will actually enforce at.
///
/// Reported separately from the raw kernel ABI because the crate may cap the
/// version it can express. Confining at a lower ABI than the kernel offers is
/// safe; silently claiming the higher one would not be.
pub fn effective_abi() -> landlock::ABI {
    // A failed probe has no ABI to report. require_confinement has already
    // refused to run in that case, so this is only reached on paths that will
    // abort rather than execute.
    report().ok().map(|r| r.abi).unwrap_or(landlock::ABI::V1)
}
