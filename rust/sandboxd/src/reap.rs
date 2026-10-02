//! Reaping the stragglers a run leaves behind.
//!
//! # Why this exists
//!
//! When a run's main process exits, `pty` kills its process group, which
//! catches the ordinary `some-command &` shape. It cannot catch a child that
//! called `setsid` first: that puts it in a new session and process group,
//! beyond the reach of `kill(-pgid)`. npm and `vite preview` both do this, and
//! in practice a completed run left a preview server listening on its port
//! with the runner as its parent, so nothing ever collected it.
//!
//! # Why not a PID namespace
//!
//! A PID namespace is the textbook fix: the kernel destroys every process in
//! the namespace when its init exits, no cooperation needed from the straggler.
//! It was measured on this host and rejected - `unshare(CLONE_NEWPID)` returns
//! `EPERM` for an unprivileged process, because creating a PID namespace
//! requires `CAP_SYS_ADMIN` in the current user namespace. The only way to get
//! one is to create a user namespace first, and that would mean running
//! confined code with `CAP_SYS_ADMIN` inside it. That is a materially different
//! threat model for the component whose entire job is to hold the line, so it
//! is not a change to make quietly.
//!
//! # What this does instead
//!
//! Each run is tagged with a unique marker in its environment. After the run
//! ends, every process whose initial environment still carries that marker is
//! killed. A process that called `setsid` keeps its environment, so it is
//! found; a process belonging to any other run, or one the operator started by
//! hand, does not carry the marker and is never touched.
//!
//! The trade is that a straggler which deliberately scrubs its environment
//! survives. That is a willing evasion of the sweep rather than an accident of
//! process topology, and it costs an attacker nothing they did not already
//! have.

use std::fs;

/// Environment variable carrying the per-run marker.
pub const RUN_ID_VAR: &str = "AIOS_RUN_ID";

/// Kill every live process still carrying `RUN_ID_VAR=<run_id>`.
///
/// Returns how many processes were signalled. Unreadable or already-exited
/// entries are skipped rather than reported: a run is tearing down, and a
/// process that vanished between the scan and the signal is the outcome we
/// wanted anyway.
pub fn sweep_run(run_id: &str) -> usize {
    let needle = format!("{RUN_ID_VAR}={run_id}");
    let needle = needle.as_bytes();
    let me = std::process::id() as i32;

    let mut killed = 0;
    for entry in match fs::read_dir("/proc") {
        Ok(entries) => entries,
        // No procfs means no way to enumerate; the group sweep in `pty` has
        // already run, so this is a best-effort extra layer, not the only one.
        Err(_) => return 0,
    } {
        let Ok(entry) = entry else { continue };
        let name = entry.file_name();
        let Some(name) = name.to_str() else { continue };
        // /proc lists a numeric entry per process; everything else is noise.
        if name.is_empty() || !name.bytes().all(|b| b.is_ascii_digit()) {
            continue;
        }
        let Ok(pid) = name.parse::<i32>() else { continue };
        // Never signal ourselves, however the run was invoked.
        if pid == me {
            continue;
        }

        if !carries(&format!("/proc/{pid}/environ"), needle) {
            continue;
        }

        // SAFETY: the pid came from procfs, so it named a live process moments
        // ago. A pid recycled between the scan and here would have to belong
        // to a process that also inherited this run's marker, which is the
        // process we meant to kill anyway.
        if unsafe { libc::kill(pid, libc::SIGKILL) } == 0 {
            killed += 1;
        }
    }
    killed
}

/// Whether a process's initial environment contains `needle` as one entry.
fn carries(environ: &str, needle: &[u8]) -> bool {
    match fs::read(environ) {
        Ok(raw) => contains_entry(&raw, needle),
        Err(_) => false,
    }
}

/// NUL-separated membership test.
///
/// Matched a whole entry at a time rather than searching for a substring: a
/// run id appearing inside some other variable's value must not count, or
/// sweeping run `abc` would take down run `abc123`'s processes with it.
fn contains_entry(raw: &[u8], needle: &[u8]) -> bool {
    raw.split(|byte| *byte == 0).any(|entry| entry == needle)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn finds_the_marker_among_other_variables() {
        let raw = b"PATH=/usr/bin\0AIOS_RUN_ID=run-42\0HOME=/root\0";
        assert!(contains_entry(raw, b"AIOS_RUN_ID=run-42"));
    }

    #[test]
    fn a_longer_run_id_does_not_match_a_shorter_sweep() {
        // Sweeping run "abc" must not kill run "abc123": without the whole-entry
        // comparison this is a prefix match and takes the wrong processes down.
        let raw = b"PATH=/usr/bin\0AIOS_RUN_ID=abc123\0";
        assert!(!contains_entry(raw, b"AIOS_RUN_ID=abc"));
        assert!(contains_entry(raw, b"AIOS_RUN_ID=abc123"));
    }

    #[test]
    fn a_similar_variable_name_is_not_a_match() {
        let raw = b"MY_AIOS_RUN_ID=run-42\0";
        assert!(!contains_entry(raw, b"AIOS_RUN_ID=run-42"));
    }

    #[test]
    fn a_missing_environ_is_not_a_match() {
        assert!(!carries("/proc/this/path/does/not/exist", b"AIOS_RUN_ID=x"));
    }

    #[test]
    fn sweeping_an_unknown_run_kills_nothing() {
        assert_eq!(sweep_run("definitely-not-a-real-run-id"), 0);
    }
}
