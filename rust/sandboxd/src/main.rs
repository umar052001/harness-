//! sandboxd - fail-closed Linux confinement for AI OS runs.
//!
//! Every run is: probe the kernel, build a deny-by-default Landlock ruleset,
//! fork, attach a pty, apply confinement in the child, exec. If the kernel
//! cannot enforce the policy we abort rather than run unconfined.

mod confine;
mod events;
mod json;
mod policy;
mod probe;
mod pty;
mod reap;

use std::path::PathBuf;
use std::time::Duration;

use policy::{Network, Policy};
use pty::RunSpec;

const USAGE: &str = "\
sandboxd - fail-closed Linux confinement for AI OS runs

USAGE:
    sandboxd probe
        Report the kernel's Landlock capability and exit.

    sandboxd run --workspace <DIR> [--cwd <DIR>] [--read <DIR>] [--response <FILE>]
                 [--run-id <ID>] [--timeout-ms <MS>] [--env KEY=VALUE] [--allow-net]
                 -- <CMD> [ARGS...]
        Run CMD confined and print the transcript once the run finishes. Write
        access is limited to <DIR>; everything else is read-only or unreachable.
        --read adds a read+execute grant beyond the system set. Network egress
        is denied unless --allow-net, which this build refuses without an
        explicit decision.

    sandboxd exec --workspace <DIR> [--cwd <DIR>] [--read <DIR>] [--response <FILE>]
                  [--run-id <ID>] [--timeout-ms <MS>] [--env KEY=VALUE]
                  [--stdio pty|pipe] [--events-fd N] [--allow-net --net-why TEXT]
                  -- <CMD> [ARGS...]
        Same confinement, but stream the run. Emits newline-delimited JSON
        events on stdout as the run happens; human diagnostics go to stderr.
        Events: run.started, output (base64 pty chunks), enforcement, exit.
        This is what the control plane uses for a live log.

        --stdio pty   (default) child gets a pty. Human-shaped output: colour,
                     progress bars, carriage returns.
        --stdio pipe  child inherits the supervisor's stdin/stdout/stderr.
                     Required for anything that speaks a protocol over stdio: a
                     pty echoes input, rewrites \n as \r\n and line-buffers, so
                     a JSON-RPC peer would receive its own frames.
        --events-fd N write the NDJSON event stream to descriptor N instead of
                     stdout. Mandatory in pipe mode, where stdout belongs to the
                     child: the event channel has to be a different descriptor
                     rather than a line pattern to recognise in the child's own
                     stderr.
        --allow-net   lifts the network deny. Requires --net-why, and the reason
                     is recorded in the policy report, the attestation and the
                     response file, so the grant is auditable after the fact.

EXIT CODES:
    0   command ran and exited 0
    N   command's own exit status (128+N if killed by signal N)
    125 sandbox refused to run (policy invalid, or kernel cannot confine)
    126 child failed to set up its sandbox
    127 command not found

SUBCOMMANDS:
    probe   Report the kernel's Landlock capability.
    exec    Stream a confined run as JSON events.
    guard   Report this process's own confinement state, read via prctl(2).
            Intended to be run *inside* a sandbox to produce direct evidence
            that the privilege gate is set, without trusting a log line.
";

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match run(&args) {
        Ok(code) => std::process::exit(code),
        Err(e) => {
            eprintln!("sandboxd: {e}");
            std::process::exit(125);
        }
    }
}

fn run(args: &[String]) -> Result<i32, String> {
    match args.first().map(String::as_str) {
        None => {
            print!("{USAGE}");
            Ok(0)
        }
        Some("probe") => match probe::report() {
            Ok(r) => {
                print!("{}", r.render());
                Ok(0)
            }
            Err(e) => {
                eprintln!("sandboxd: {e}");
                Ok(125)
            }
        },
        Some("run") => cmd_run(&args[1..], false),
        Some("exec") => cmd_run(&args[1..], true),
        Some("guard") => cmd_guard(),
        Some("-h") | Some("--help") | Some("help") => {
            print!("{USAGE}");
            Ok(0)
        }
        Some(other) => Err(format!("unknown subcommand {other:?}\n\n{USAGE}")),
    }
}

/// Report this process's own confinement state straight from the kernel.
///
/// Uses `prctl(2)` rather than reading `/proc`, because `/proc` is deliberately
/// not granted inside the sandbox - and a self-report is only worth anything if
/// it comes from the syscall that enforces the flag, not from a file the policy
/// could have influenced.
fn cmd_guard() -> Result<i32, String> {
    // SAFETY: PR_GET_NO_NEW_PRIVS takes no arguments and only writes through
    // the return value.
    let nnp = unsafe { libc::prctl(libc::PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0) };
    if nnp == -1 {
        return Err(format!(
            "prctl(PR_GET_NO_NEW_PRIVS): {}",
            std::io::Error::last_os_error()
        ));
    }
    // A second, independent witness: the effective uid/gid cannot have been
    // raised if the gate is set.
    let uid = unsafe { libc::getuid() };
    let euid = unsafe { libc::geteuid() };

    println!("no_new_privs: {nnp}");
    println!("uid: {uid}");
    println!("euid: {euid}");
    println!("is_tty: {}", unsafe { libc::isatty(0) } == 1);

    // Non-zero exit when the gate is somehow clear, so a test can assert on it.
    Ok(if nnp == 1 { 0 } else { 1 })
}

/// Shared implementation for `run` and `exec`.
///
/// The two differ only in how pty bytes reach the caller, so they share every
/// step that could affect security. Confinement must not be able to behave
/// differently in streaming mode - that would make the stream, which is the mode
/// the control plane actually uses, the less tested of the two.
fn cmd_run(args: &[String], stream: bool) -> Result<i32, String> {
    install_signal_handlers();
    let mut workspace: Option<PathBuf> = None;
    let mut cwd: Option<PathBuf> = None;
    let mut extra_read: Vec<PathBuf> = Vec::new();
    let mut allow_net = false;
    let mut net_why = String::new();
    let mut stdio = pty::Stdio::Pty;
    let mut events_fd: Option<i32> = None;
    let mut response: Option<PathBuf> = None;
    let mut env: Vec<(String, String)> = Vec::new();
    let mut run_id: Option<String> = None;
    let mut timeout: Option<Duration> = None;
    let mut command: Vec<String> = Vec::new();

    let mut it = args.iter();
    while let Some(a) = it.next() {
        match a.as_str() {
            "--workspace" => {
                workspace = Some(PathBuf::from(it.next().ok_or("--workspace needs a value")?))
            }
            "--cwd" => cwd = Some(PathBuf::from(it.next().ok_or("--cwd needs a value")?)),
            "--read" => extra_read.push(PathBuf::from(it.next().ok_or("--read needs a value")?)),
            "--response" => {
                response = Some(PathBuf::from(it.next().ok_or("--response needs a value")?))
            }
            "--run-id" => run_id = Some(it.next().ok_or("--run-id needs a value")?.to_string()),
            "--timeout-ms" => {
                let raw = it.next().ok_or("--timeout-ms needs a value")?;
                let ms: u64 = raw.parse().map_err(|_| {
                    format!("--timeout-ms wants a number of milliseconds, got {raw:?}")
                })?;
                if ms == 0 {
                    return Err("--timeout-ms of 0 would kill the run before it starts".into());
                }
                timeout = Some(Duration::from_millis(ms));
            }
            "--env" => {
                let kv = it.next().ok_or("--env needs a value")?.to_string();
                let (k, v) = kv
                    .split_once('=')
                    .ok_or_else(|| format!("--env wants KEY=VALUE, got {kv:?}"))?;
                env.push((k.to_string(), v.to_string()));
            }
            "--allow-net" => allow_net = true,
            "--net-why" => net_why = it.next().ok_or("--net-why needs a value")?.to_string(),
            "--events-fd" => {
                let v = it.next().ok_or("--events-fd needs a value")?;
                events_fd = Some(
                    v.parse::<i32>()
                        .map_err(|_| format!("--events-fd: want a descriptor, got {v}"))?,
                );
            }
            "--stdio" => {
                stdio = match it.next().ok_or("--stdio needs a value")?.as_str() {
                    "pty" => pty::Stdio::Pty,
                    "pipe" => pty::Stdio::Pipe,
                    other => return Err(format!("--stdio: want pty or pipe, got {other}")),
                }
            }
            "--" => {
                command.extend(it.cloned());
                break;
            }
            other => return Err(format!("unexpected flag {other:?} before --")),
        }
    }

    if command.is_empty() {
        return Err("no command given; put it after --".into());
    }
    let workspace = workspace.ok_or("--workspace is required")?;
    let run_id = run_id.unwrap_or_else(|| format!("run-{}-{}", std::process::id(), started_ns()));

    // 1. The workspace must already exist.
    //
    // Checked before anything is created, because the scratch directory below
    // lives inside the workspace: creating it first would happily materialise a
    // mistyped path and start a run against an empty directory instead of
    // reporting the typo. Fail loudly rather than silently starting fresh.
    if !workspace.is_dir() {
        return Err(format!(
            "workspace {} does not exist or is not a directory",
            workspace.display()
        ));
    }

    // 2. Prove the host can confine before we do anything else.
    let report = probe::require_confinement()?;

    // 3. Build and validate the policy, deny-by-default.
    let scratch = prepare_scratch(&workspace)?;

    let mut pol = Policy::workspace(&workspace);
    for r in &extra_read {
        pol = pol.with_read(r);
    }
    if let Some(c) = &cwd {
        pol = pol.with_writable(c);
    }
    if allow_net {
        pol = pol.with_network_why(Network::Allow, net_why);
    }
    // In pipe mode the child's stdout is the supervisor's stdout. If events went
    // there too, the executor would be writing into the middle of a protocol
    // stream. Refused here rather than silently dropped, because a caller that
    // asked for a stream and got none would have no way to tell.
    if stdio == pty::Stdio::Pipe && events_fd.is_none() {
        return Err(
            "--stdio pipe needs --events-fd: stdout belongs to the confined program, so the \
             event stream has to have a descriptor of its own"
                .into(),
        );
    }
    if let Some(fd) = events_fd {
        if !(3..=1024).contains(&fd) {
            return Err(format!(
                "--events-fd {fd} is not a descriptor this process can hand on (want 3..=1024)"
            ));
        }
        events::set_fd(fd);
    }

    pol.validate()?;

    // 4. Show what is in effect, so the transcript is self-documenting. In
    //    streaming mode this goes to stderr, because stdout is the event
    //    channel and every line of it has to parse.
    //
    //    In pipe mode the child's stdout is the supervisor's stdout, so the
    //    event channel is off entirely: the report would corrupt the protocol.
    // Events are emitted whenever there is somewhere to emit them to. In pty mode
    // that is stdout; in pipe mode the caller has already been forced to supply
    // --events-fd, so the whole event set goes there and stdout stays the child's.
    let events_on = stream || stdio == pty::Stdio::Pipe;
    // In pipe mode the child's stdout *is* this process's stdout, so anything
    // the supervisor prints there lands in the middle of the protocol stream.
    // The report goes to stderr in that mode whether or not streaming is on.
    let human_to_stderr = stream || stdio == pty::Stdio::Pipe;
    if human_to_stderr {
        eprint!("{}", report.render());
        eprint!("{}", pol.render());
        eprint!("{}", confine::describe(&pol));
        eprintln!("exec        : {} {}", command[0], command[1..].join(" "));
        eprintln!("---");
        if events_on {
            events::started(&run_id, &command[0], &command[1..], cwd.as_deref(), &pol);
        }
    } else {
        print!("{}", report.render());
        print!("{}", pol.render());
        print!("{}", confine::describe(&pol));
        println!("exec        : {} {}", command[0], command[1..].join(" "));
        println!("---");
    }

    // 5. Run confined, with the scratch directory as its temporary space.
    let exe = command[0].clone();
    let rest: Vec<String> = command[1..].to_vec();
    let scratch_s = scratch.to_string_lossy().into_owned();
    let mut spec = RunSpec::new(&exe, &rest, &pol)
        .env("TMPDIR", &scratch_s)
        .env("TMP", &scratch_s)
        .env("TEMP", &scratch_s);
    // Caller overrides come last so they win over the defaults above.
    for (k, v) in &env {
        spec = spec.env(k, v);
    }
    if let Some(t) = timeout {
        spec = spec.timeout(t);
    }
    // Tagged after the caller's --env overrides, so a caller cannot
    // accidentally shadow the marker the teardown sweep looks for.
    let spec = spec.run_id(&run_id);
    let spec = match &cwd {
        Some(c) => spec.cwd(c),
        None => spec,
    };

    let out = match (stream, stdio) {
        (_, pty::Stdio::Pipe) => pty::run_piped(&spec)?,
        (true, pty::Stdio::Pty) => {
            let mut seq: u64 = 0;
            pty::run_streaming(&spec, |chunk| {
                events::output(seq, chunk);
                seq += 1;
                Ok(())
            })?
        }
        (false, pty::Stdio::Pty) => pty::run(&spec)?,
    };

    // Emit the machine-readable contract before anything else can fail, so a
    // caller that asked for a response always gets one.
    if let Some(path) = &response {
        write_response(path, &run_id, &out, &pol)?;
    }

    if events_on {
        events::enforcement(out.attestation.as_ref());
        events::exit(&out);
    } else {
        print!("{}", out.output);
        if !out.output.ends_with('\n') {
            println!();
        }
        println!("---");
    }

    // The exit status is reported on stderr in pipe mode for the same reason:
    // the control plane is reading the child's stdout, and a line reading
    // "exit : 0" there would be indistinguishable from agent output.
    let exit_to_stderr = stdio == pty::Stdio::Pipe;

    if out.timed_out {
        eprintln!("sandboxd: run exceeded its deadline and its process group was killed");
    }
    if out.killed_by_signal {
        if !stream {
            if exit_to_stderr {
                eprintln!("exit        : killed by signal {}", out.signal.unwrap_or(0));
            } else {
                println!("exit        : killed by signal {}", out.signal.unwrap_or(0));
            }
        }
        return Ok(128 + out.signal.unwrap_or(0));
    }
    match out.status.code() {
        Some(0) => {
            if !stream {
                if exit_to_stderr {
                    eprintln!("exit        : 0");
                } else {
                    println!("exit        : 0");
                }
            }
            Ok(0)
        }
        Some(c) => {
            if !stream {
                if exit_to_stderr {
                    eprintln!("exit        : {c}");
                } else {
                    println!("exit        : {c}");
                }
            }
            Ok(c)
        }
        None => {
            if !stream {
                if exit_to_stderr {
                    eprintln!("exit        : unknown");
                } else {
                    println!("exit        : unknown");
                }
            }
            Ok(125)
        }
    }
}

/// Forward supervisor signals to the child's process group.
///
/// The alternative - letting a TERM or INT take the default action and kill this
/// supervisor mid-run - strands the confined child: sandboxd dies but the agent
/// keeps running with nobody reading its protocol, and the event pipe write end
/// stays open so the caller's reader never sees EOF. Killing the child's group
/// lets the run end normally, so the enforcement and exit events still reach the
/// caller and the stream closes.
fn install_signal_handlers() {
    unsafe {
        libc::signal(libc::SIGTERM, handle_term as libc::sighandler_t);
        libc::signal(libc::SIGINT, handle_term as libc::sighandler_t);
        libc::signal(libc::SIGHUP, handle_term as libc::sighandler_t);
    }
}

extern "C" fn handle_term(_: libc::c_int) {
    // The child is a setsid leader, so a group kill also reaches its
    // grandchildren; async-signal-safe by containing only libc::kill.
    pty::kill_child_group(libc::SIGKILL);
}

/// Create the workspace's private scratch directory and return it.
///
/// Build tools insist on a temporary directory, and the alternative - a shared
/// `/tmp` - is both a cross-run information leak and an escape surface. Keeping
/// it inside the workspace also means it inherits the same policy as everything
/// else.
///
/// The directory is stable per workspace rather than unique per run, which is a
/// deliberate trade: run-scoped uniqueness would need a per-run `mkdir` whose
/// name is unpredictable, and `mktemp`-style schemes have a history of racing.
/// The mode is 0700 so concurrent runs by the same user still cannot read each
/// other's artifacts. Runs that must not share scratch should be given separate
/// workspaces.
fn prepare_scratch(workspace: &std::path::Path) -> Result<PathBuf, String> {
    let scratch = workspace.join(".sandbox-tmp");
    std::fs::create_dir_all(&scratch)
        .map_err(|e| format!("create scratch {}: {e}", scratch.display()))?;
    set_mode_0700(&scratch)?;
    Ok(scratch)
}

/// Restrict a path to owner-only permissions.
///
/// Scratch space holds compiler output and build artifacts, which can include
/// material derived from source the run was asked to handle. 0700 keeps it to the
/// owning user even when the workspace sits under a shared parent.
fn set_mode_0700(path: &std::path::Path) -> Result<(), String> {
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))
        .map_err(|e| format!("chmod 0700 {}: {e}", path.display()))
}

// ─── ExecResponse emission ────────────────────────────────────────────────────
//
// The Go control plane consumes this. Rather than inventing a private format, it
// is emitted as protojson for sandbox.v1.ExecResponse, so the Go side decodes it
// with the generated type and a malformed field is a decode error rather than a
// silent mismatch. The wire rules protojson imposes - int64 as a string, bytes
// as base64, lowerCamelCase names - are all followed here.

// Write the ExecResponse for one run to `path`.
//
// Written to a file rather than stdout so the child's own output stays
// uncorrupted: the executor's own diagnostics and the sandboxed program's
// output are different things and mixing them would make both harder to read.
// It is also the fallback for a streamed run, where stdout carries the event
// stream and the full transcript is already delivered chunk by chunk.
fn write_response(
    path: &std::path::Path,
    run_id: &str,
    out: &pty::RunOutcome,
    policy: &policy::Policy,
) -> Result<(), String> {
    let exit_code = if out.killed_by_signal {
        128 + out.signal.unwrap_or(0)
    } else {
        out.status.code().unwrap_or(125)
    };

    // The run used a pty, which merges the child's stdout and stderr into one
    // stream. The transcript therefore lands in stdout and stderr is left empty
    // rather than pretending the streams were separable.
    let (att, abi, rules, net) = match &out.attestation {
        Some(a) => (
            a.no_new_privs,
            a.landlock_abi,
            a.rule_count,
            a.network_denied,
        ),
        // A child that produced no attestation never got far enough to be
        // trusted, so the evidence reads as absent rather than as success.
        None => (false, 0, 0, false),
    };

    let json = format!(
        "{{\n  \"runId\": {},\n  \"exitCode\": {},\n  \"signal\": {},\n  \"stdout\": {},\n  \"stderr\": \"\",\n  \"durationNs\": \"{}\",\n  \"enforcement\": {{\n    \"noNewPrivs\": {},\n    \"landlockAbi\": {},\n    \"networkDenied\": {},\n    \"grantedPaths\": {},\n    \"ruleCount\": {}\n  }}\n}}\n",
        json::string(run_id),
        exit_code,
        out.signal.unwrap_or(0),
        json::string(&json::base64(out.output.as_bytes())),
        out.duration.as_nanos(),
        att,
        abi,
        net,
        json::string_array(&policy.granted_paths()),
        rules,
    );

    std::fs::write(path, json).map_err(|e| format!("write response {}: {e}", path.display()))
}

/// A monotonic-ish run identifier for callers that did not supply one.
///
/// Only needs to be unique enough to correlate a response with a transcript, so
/// wall-clock nanoseconds plus the pid is sufficient. Not a security boundary.
fn started_ns() -> u128 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0)
}
