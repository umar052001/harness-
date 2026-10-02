//! Newline-delimited JSON events for streaming runs.
//!
//! The batch `run` subcommand is enough for a one-shot tool call, but an agent
//! harness is interactive and long-lived: the control plane has to render a plan
//! as it is executed, decide whether the run is stuck, and stream a pty to a
//! browser. That needs output *while* the child is alive, not after.
//!
//! One JSON object per line on stdout, human diagnostics on stderr. Keeping the
//! two streams separate matters: the caller must be able to parse every line of
//! stdout without filtering out the sandbox's own chatter, and a human running
//! the binary by hand still gets a readable report.
//!
//! Output chunks are base64 because a pty carries arbitrary bytes. A read can end
//! mid-UTF-8 sequence, and a progress bar emits carriage returns and terminal
//! escapes that are not text in any ordinary sense. Decoding to bytes and
//! letting the consumer decide how to interpret a partial sequence is the only
//! lossless option.

use crate::json;
use crate::policy::Policy;
use crate::pty::{Attestation, RunOutcome};

/// Where events go, once per process.
///
/// Defaults to fd 1. In pipe mode fd 1 belongs to the program being confined, so
/// the event channel is moved to a descriptor the caller nominated. Keeping this
/// explicit is the point: routing events around a protocol on the same
/// descriptor by pattern-matching the child's stderr is how a control plane ends
/// up executing a tool's output as if it were its own.
static EVENTS_FD: std::sync::atomic::AtomicI32 = std::sync::atomic::AtomicI32::new(1);

/// Send the event stream to `fd` instead of stdout.
pub fn set_fd(fd: i32) {
    EVENTS_FD.store(fd, std::sync::atomic::Ordering::Relaxed);
}

/// Emit an event. A trailing newline is always appended, so a consumer can split
/// on `\n` without waiting for more data to disambiguate the last record.
fn emit(line: String) {
    use std::io::Write;
    let fd = EVENTS_FD.load(std::sync::atomic::Ordering::Relaxed);
    // A closed pipe means the consumer hung up. That is not our problem to
    // solve here - the caller in pty.rs notices the sink error and kills the
    // run - so the write error is deliberately dropped to avoid a panic path in
    // the middle of a run.
    let mut f = unsafe { <std::fs::File as std::os::unix::io::FromRawFd>::from_raw_fd(fd) };
    let _ = f.write_all(line.as_bytes());
    let _ = f.write_all(b"\n");
    let _ = f.flush();
    // Hand ownership back: dropping would close a descriptor the caller still
    // owns (stdout, or the pipe it passed us).
    std::mem::forget(f);
}

/// The run is confined and the child has been forked: everything about the
/// policy that the frontend displays is known now, before any output exists.
pub fn started(
    run_id: &str,
    command: &str,
    args: &[String],
    cwd: Option<&std::path::Path>,
    policy: &Policy,
) {
    emit(started_json(run_id, command, args, cwd, policy));
}

/// Formatting is split from printing throughout this module so the wire format
/// can be asserted on without capturing stdout.
fn started_json(
    run_id: &str,
    command: &str,
    args: &[String],
    cwd: Option<&std::path::Path>,
    policy: &Policy,
) -> String {
    let mut cmd = vec![command.to_string()];
    cmd.extend(args.iter().cloned());
    format!(
        "{{\"type\": \"run.started\", \"runId\": {}, \"exec\": {}, \"cwd\": {}, \"effectiveAbi\": {}, \
\"network\": {}, \"workspace\": {}, \"grantedPaths\": {}, \"ruleCount\": {}}}",
        json::string(run_id),
        json::string_array(&cmd),
        // The explicit --cwd wins; falling back to the workspace keeps the field
        // populated for the common case where the two coincide.
        match cwd.or_else(|| policy.workspace_root()) {
            Some(c) => json::string(&c.to_string_lossy()),
            None => "null".to_string(),
        },
        crate::probe::effective_abi() as u32,
        json::string(policy.network.as_str()),
        match policy.workspace_root() {
            Some(w) => json::string(&w.to_string_lossy()),
            None => "null".to_string(),
        },
        json::string_array(&policy.granted_paths()),
        crate::confine::planned_rule_count(policy),
    )
}

/// A chunk of pty output, as base64 bytes.
pub fn output(seq: u64, data: &[u8]) {
    emit(output_json(seq, data));
}

fn output_json(seq: u64, data: &[u8]) -> String {
    format!(
        "{{\"type\": \"output\", \"seq\": {seq}, \"bytes\": {}, \"data\": {}}}",
        data.len(),
        // The base64 has to be wrapped as a JSON string. The alphabet is
        // readable as a bare word but is not a valid JSON value, and this
        // shipped that way once before a consumer's parser caught it.
        json::string(&json::base64(data)),
    )
}

/// The child's own confinement evidence, relayed once the run is over.
pub fn enforcement(a: Option<&Attestation>) {
    emit(enforcement_json(a));
}

pub fn enforcement_json(a: Option<&Attestation>) -> String {
    // A run that produced no attestation never reached the point of being
    // trustworthy, so the evidence reads as absent rather than as success. The
    // `attested` flag lets a consumer tell "not confined" apart from "confined
    // but unverified" without inferring it from zero values.
    match a {
        Some(a) => format!(
            "{{\"type\": \"enforcement\", \"attested\": true, \"noNewPrivs\": {}, \
\"landlockAbi\": {}, \"ruleCount\": {}, \"networkDenied\": {}}}",
            a.no_new_privs, a.landlock_abi, a.rule_count, a.network_denied
        ),
        None => "{\"type\": \"enforcement\", \"attested\": false, \"noNewPrivs\": false, \
\"landlockAbi\": 0, \"ruleCount\": 0, \"networkDenied\": false}"
            .to_string(),
    }
}

/// The run finished. Always the last event on the stream.
pub fn exit(outcome: &RunOutcome) {
    emit(exit_json(outcome));
}

fn exit_json(outcome: &RunOutcome) -> String {
    let code = if outcome.killed_by_signal {
        128 + outcome.signal.unwrap_or(0)
    } else {
        outcome.status.code().unwrap_or(125)
    };
    format!(
        "{{\"type\": \"exit\", \"exitCode\": {}, \"signal\": {}, \"durationNs\": {}, \
\"timedOut\": {}, \"bytesTotal\": {}, \"truncated\": {}}}",
        code,
        outcome.signal.unwrap_or(0),
        // protojson's int64-as-string rule, so a nanosecond duration survives a
        // decoder that would otherwise coerce it to a float64 and lose precision.
        json::string(&outcome.duration.as_nanos().to_string()),
        outcome.timed_out,
        outcome.bytes_total,
        outcome.truncated,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pty::RunOutcome;
    use std::os::unix::process::ExitStatusExt;
    use std::process::ExitStatus;

    /// A single line of well-formed JSON.
    ///
    /// Checked with a small scanner rather than a substring match, because the
    /// failure this guards against - a value emitted as a bare word instead of a
    /// quoted string - looks entirely correct when read by eye.
    fn assert_single_line_json(line: &str) {
        assert!(
            !line.trim_end().contains('\n'),
            "event must be one line: {line}"
        );
        let mut depth = 0i32;
        let (mut in_str, mut escaped) = (false, false);
        for c in line.chars() {
            if in_str {
                if escaped {
                    escaped = false;
                } else if c == '\\' {
                    escaped = true;
                } else if c == '"' {
                    in_str = false;
                }
                continue;
            }
            match c {
                '"' => in_str = true,
                '{' | '[' => depth += 1,
                '}' | ']' => depth -= 1,
                _ => {}
            }
            assert!(depth >= 0, "unbalanced JSON: {line}");
        }
        assert!(!in_str, "unterminated string: {line}");
        assert_eq!(depth, 0, "unbalanced JSON: {line}");
    }

    #[test]
    fn output_event_quotes_its_base64() {
        let line = output_json(7, b"tick 1\n");
        assert_single_line_json(&line);
        assert!(line.contains("\"data\": \"dGljayAxCg==\""), "got {line}");
        assert!(line.contains("\"seq\": 7"), "got {line}");
        assert!(line.contains("\"bytes\": 7"), "got {line}");
    }

    #[test]
    fn output_event_survives_a_utf8_sequence_split_across_chunks() {
        // Two of the three bytes of a character: routine pty behaviour, and the
        // reason output is base64 rather than an embedded JSON string.
        let a = output_json(0, &[0xE2, 0x9C]);
        let b = output_json(1, &[0x93]);
        assert_single_line_json(&a);
        assert_single_line_json(&b);
    }

    #[test]
    fn output_event_survives_pty_control_bytes() {
        // Carriage returns and ANSI escapes from progress bars must not be able
        // to corrupt event framing.
        let line = output_json(0, b"\r\n\x1b[2K\x1b[1G50%\r");
        assert_single_line_json(&line);
    }

    fn outcome(code: i32, signal: Option<i32>, timed_out: bool) -> RunOutcome {
        RunOutcome {
            status: ExitStatus::from_raw(code << 8),
            output: String::new(),
            killed_by_signal: signal.is_some(),
            signal,
            duration: std::time::Duration::new(1, 500_000_000),
            attestation: None,
            timed_out,
            bytes_total: 42,
            truncated: false,
        }
    }

    #[test]
    fn exit_event_reports_normal_exit() {
        let line = exit_json(&outcome(0, None, false));
        assert_single_line_json(&line);
        assert!(line.contains("\"exitCode\": 0"), "got {line}");
        assert!(line.contains("\"timedOut\": false"), "got {line}");
        // int64 as a string, per protojson.
        assert!(
            line.contains("\"durationNs\": \"1500000000\""),
            "got {line}"
        );
    }

    #[test]
    fn exit_event_maps_a_signal_to_the_shell_convention() {
        // 128+N is what a shell reports and what the Go side compares against.
        let line = exit_json(&outcome(0, Some(9), true));
        assert_single_line_json(&line);
        assert!(line.contains("\"exitCode\": 137"), "got {line}");
        assert!(line.contains("\"signal\": 9"), "got {line}");
        assert!(line.contains("\"timedOut\": true"), "got {line}");
    }

    #[test]
    fn enforcement_event_distinguishes_absent_evidence_from_failure() {
        let missing = enforcement_json(None);
        assert_single_line_json(&missing);
        assert!(missing.contains("\"attested\": false"), "got {missing}");

        let present = enforcement_json(Some(&Attestation {
            no_new_privs: true,
            landlock_abi: 9,
            rule_count: 12,
            network_denied: true,
        }));
        assert_single_line_json(&present);
        assert!(present.contains("\"attested\": true"), "got {present}");
        assert!(present.contains("\"landlockAbi\": 9"), "got {present}");
    }
}
