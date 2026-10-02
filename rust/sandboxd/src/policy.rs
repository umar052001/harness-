//! The sandbox policy: what a confined run is allowed to touch.
//!
//! The design rule is deny-by-default. A policy starts from "nothing is
//! permitted" and the caller explicitly *grants* the minimum, because a policy
//! expressed as a list of restrictions silently grows holes over time while one
//! expressed as a list of grants cannot.

use std::path::{Path, PathBuf};

/// Access modes a path may be granted.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    /// Read only. Use for system paths the sandbox needs but must not mutate.
    ReadOnly,
    /// Read/write, including create, delete and rename. Use only for the
    /// workspace and any explicitly mounted scratch.
    ReadWrite,
}

impl Mode {
    /// Human-readable name, used in the policy report.
    #[allow(dead_code)]
    pub fn as_str(&self) -> &'static str {
        match self {
            Mode::ReadOnly => "ro",
            Mode::ReadWrite => "rw",
        }
    }
}

/// Whether the sandboxed process may reach the network.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Network {
    /// Deny all TCP bind and connect. The default for every run.
    Deny,
    /// Explicitly allow. Requires the operator to opt in; never inferred.
    Allow,
}

impl Network {
    /// Machine-readable name, for the event stream and the response contract.
    pub fn as_str(&self) -> &'static str {
        match self {
            Network::Deny => "deny",
            Network::Allow => "allow",
        }
    }
}

/// System paths granted read+execute so a process can start at all.
///
/// Without these a confined process cannot exec anything. This is the single
/// largest grant in the policy, which is why it is read-only and why each entry
/// is enumerated rather than globbed.
pub fn system_read_paths() -> Vec<PathBuf> {
    [
        "/usr",     // Arch: bin, lib, include, share are all symlinked here
        "/etc",     // resolv.conf, passwd - read-only
        "/var/lib", // certificates, tzdata
        "/etc/alternatives",
    ]
    .iter()
    .map(PathBuf::from)
    .collect()
}

/// Pseudo-devices a normal process needs, with the minimum rights for each.
///
/// Discovered the hard way: without these, `/dev/null` is unreachable and every
/// tool that writes to it fails, which makes the sandbox useless in practice
/// while still looking correctly configured.
///
/// Rights are deliberately narrow. `/dev/null` and `/dev/zero` are writable
/// because redirection targets them; the entropy sources are read-only; nothing
/// here can open a real device.
pub fn device_grants() -> Vec<(PathBuf, Mode)> {
    use Mode::*;
    vec![
        (PathBuf::from("/dev/null"), ReadWrite),
        (PathBuf::from("/dev/zero"), ReadWrite),
        (PathBuf::from("/dev/full"), ReadWrite),
        (PathBuf::from("/dev/random"), ReadOnly),
        (PathBuf::from("/dev/urandom"), ReadOnly),
        (PathBuf::from("/dev/tty"), ReadWrite),
        // A process that opens its own pty needs these. Granting /dev/ptmx and
        // the /dev/pts directory does let it allocate pseudo-terminals, which is
        // a real capability: it is accepted because nested harnesses genuinely
        // do this, and pty allocation grants no access outside the policy.
        (PathBuf::from("/dev/ptmx"), ReadWrite),
        (PathBuf::from("/dev/pts"), ReadWrite),
    ]
}

/// A complete confinement policy.
#[derive(Debug, Clone)]
pub struct Policy {
    /// Directories the sandboxed process may write to. Everything else is
    /// read-only or unreachable.
    pub writable: Vec<PathBuf>,
    /// Additional read-only grants beyond the system set.
    pub extra_read: Vec<PathBuf>,
    pub network: Network,
    /// Why network egress was granted, when it was.
    ///
    /// Empty means the run is network-denied. Non-empty is a promise that this
    /// string appears in the policy description, the attestation and the
    /// response, so the grant is visible after the fact rather than only at the
    /// moment it was made.
    pub net_rationale: String,
    /// Apply `PR_SET_NO_NEW_PRIVS`. Always true; kept explicit because it is
    /// the other half of why Landlock is enforceable by an unprivileged user.
    pub no_new_privs: bool,
}

impl Policy {
    /// Strict default: write only the workspace, no network, no privilege gain.
    pub fn workspace(root: impl AsRef<Path>) -> Self {
        Policy {
            writable: vec![root.as_ref().to_path_buf()],
            extra_read: Vec::new(),
            network: Network::Deny,
            net_rationale: String::new(),
            no_new_privs: true,
        }
    }

    pub fn with_writable(mut self, path: impl AsRef<Path>) -> Self {
        self.writable.push(path.as_ref().to_path_buf());
        self
    }

    pub fn with_read(mut self, path: impl AsRef<Path>) -> Self {
        self.extra_read.push(path.as_ref().to_path_buf());
        self
    }

    /// Grant network egress, recording why.
    ///
    /// The rationale is mandatory. A cloud-hosted agent has to reach its model
    /// provider, so the grant is legitimate and unavoidable - but "a sandbox
    /// that can phone home is not a sandbox" is only true if nobody can tell
    /// afterwards whether it did.
    pub fn with_network_why(mut self, n: Network, why: impl Into<String>) -> Self {
        self.network = n;
        self.net_rationale = why.into();
        self
    }

    /// Fail-closed validation before we build a ruleset.
    ///
    /// Every writable root must exist and be a real directory. A typo'd path in
    /// a policy is otherwise the most expensive kind of bug: the sandbox starts
    /// fine and then cannot do its job, or - worse - we widen a rule to "fix" it.
    pub fn validate(&self) -> Result<(), String> {
        if self.writable.is_empty() {
            return Err(
                "policy grants no writable root - the process could not do anything".into(),
            );
        }
        for p in &self.writable {
            if !p.exists() {
                return Err(format!("writable root does not exist: {}", p.display()));
            }
            if !p.is_dir() {
                return Err(format!("writable root is not a directory: {}", p.display()));
            }
        }
        for p in &self.extra_read {
            if !p.exists() {
                return Err(format!("read grant does not exist: {}", p.display()));
            }
        }
        // Network egress is not refused outright, but it is never silent. It has
        // to be requested by naming why, and that reason is carried into the
        // ruleset description, the attestation and the response file, so a later
        // reader can see what was granted and on what basis.
        if self.network == Network::Allow && self.net_rationale.is_empty() {
            return Err(
                "network egress requested with no rationale - pass --net-why to record why this \
                 run may phone home. A sandbox that can reach the network is not a sandbox; it \
                 is a jail with a door, and the door has to be labelled."
                    .into(),
            );
        }
        Ok(())
    }

    /// Every path the ruleset grants, in one list.
    ///
    /// This is the complete answer to "what can this process touch", including
    /// the system set and the pseudo-devices. It is deliberately not derived from
    /// the caller's arguments alone: the largest grants in any policy are the
    /// ones nobody passes on the command line, and an enforcement report that
    /// omitted them would be a report about a sandbox other than the one running.
    ///
    /// Consumers of this list are documentation and display, so the `/dev`
    /// entries are included even though they are not paths in any user sense -
    /// a UI that hides them cannot explain why a tool can still write to /dev/null.
    pub fn granted_paths(&self) -> Vec<String> {
        let mut v: Vec<String> = Vec::new();
        let mut push = |p: &Path| v.push(p.to_string_lossy().into_owned());
        for p in &self.writable {
            push(p);
        }
        for p in system_read_paths() {
            push(&p);
        }
        for p in &self.extra_read {
            push(p);
        }
        for (p, _mode) in device_grants() {
            push(&p);
        }
        v
    }

    /// The primary writable root, which is the workspace for every policy the
    /// control plane builds.
    pub fn workspace_root(&self) -> Option<&Path> {
        self.writable.first().map(|p| p.as_path())
    }

    /// Human-readable summary, printed before every run so the confinement in
    /// effect is never a mystery.
    pub fn render(&self) -> String {
        let mut s = String::from("policy:\n");
        s.push_str(&format!(
            "  network    : {}",
            match self.network {
                Network::Deny => "DENIED (tcp bind + connect)",
                Network::Allow => "ALLOWED",
            }
        ));
        // The reason for a network grant is printed next to the grant, so a
        // transcript reader sees what was opened and on whose judgement.
        if self.network == Network::Allow && !self.net_rationale.is_empty() {
            s.push_str(&format!("  ({})", self.net_rationale));
        }
        s.push_str(&format!(
            "\n  no_new_privs: {}\n  writable   :\n",
            self.no_new_privs
        ));
        for p in &self.writable {
            s.push_str(&format!("    + {}  (rw)\n", p.display()));
        }
        s.push_str("  read-only  :\n");
        for p in system_read_paths() {
            s.push_str(&format!("    + {}  (ro,exec)\n", p.display()));
        }
        for p in &self.extra_read {
            s.push_str(&format!("    + {}  (ro,exec)\n", p.display()));
        }
        s.push_str("  everything else: unreachable\n");
        s
    }
}
