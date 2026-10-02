//! PTY-backed process execution with confinement applied pre-exec.
//!
//! The fork/exec sequence is deliberate:
//!
//!   1. open the pty pair
//!   2. fork
//!   3. child: new session, attach the pty as controlling terminal, wire it to
//!      0/1/2, apply Landlock, then exec
//!
//! Confinement is step 3's second half, immediately before `exec`. That is the
//! last point still under our control: after `execve` we no longer choose what
//! runs, and before `fork` we would confine the supervisor as well.
//!
//! Raw libc throughout, deliberately. The child half of this path must be
//! async-signal-safe: no allocation, no unwinding, no panics, no library that
//! might do either between `fork` and `exec`.

use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::process::ExitStatusExt;
use std::path::Path;
use std::process::ExitStatus;

use crate::confine;
use crate::policy::Policy;

/// Evidence collected by the child immediately after confinement, before exec.
///
/// This is deliberately not the parent's opinion. The parent can only report
/// that it *asked* for a ruleset; these values come from the confined process
/// itself, after `restrict_self` succeeded.
///
/// Limitation worth stating plainly: a deliberately hostile child controls this
/// pipe and could lie. The attestation shows the sandbox is configured, not that
/// the child is honest. The real proof of enforcement is the escape suite, which
/// attempts violations and observes the kernel refusing them.
#[derive(Debug, Default, Clone)]
pub struct Attestation {
    pub no_new_privs: bool,
    pub landlock_abi: u32,
    pub rule_count: u32,
    pub network_denied: bool,
}

/// Outcome of a confined run.
pub struct RunOutcome {
    pub status: ExitStatus,
    /// The pty transcript, decoded as UTF-8 lossily.
    ///
    /// Only populated by `run`. `run_streaming` leaves it empty, because a
    /// streaming caller has already received the same bytes chunk by chunk and
    /// re-buffering them would defeat the point of streaming.
    pub output: String,
    /// True when the run ended on a signal.
    pub killed_by_signal: bool,
    pub signal: Option<i32>,
    /// Wall-clock duration of the run.
    pub duration: std::time::Duration,
    /// Evidence gathered by the child, if it got far enough to write any.
    pub attestation: Option<Attestation>,
    /// True when the run hit its deadline and was killed rather than finishing
    /// on its own.
    pub timed_out: bool,
    /// Total pty bytes seen, including any discarded after the cap.
    pub bytes_total: u64,
    /// True when the output cap discarded output. Streaming callers that
    /// forwarded every chunk will have already relayed the real transcript.
    pub truncated: bool,
}

/// Arguments for one confined execution.
pub struct RunSpec<'a> {
    pub command: &'a str,
    pub args: &'a [String],
    pub policy: &'a Policy,
    pub cwd: Option<&'a Path>,
    /// Bytes of pty output to buffer before truncating.
    ///
    /// Only meaningful for `run`; `run_streaming` relays every chunk and uses
    /// this solely to report whether a total was exceeded.
    pub max_output: usize,
    /// Environment overrides applied in the child before exec.
    ///
    /// Used to point the run at its own scratch space, so build tools that
    /// insist on a temporary directory get one they own instead of failing or,
    /// worse, being handed a shared world-writable /tmp.
    pub env: Vec<(String, String)>,
    /// This run's unique marker, used to collect processes that escaped the
    /// process group by calling `setsid`. See `reap` for why a PID namespace
    /// was rejected.
    pub run_id: Option<&'a str>,
    /// Wall-clock limit. On expiry the child's whole process group is killed.
    pub timeout: Option<std::time::Duration>,
}

impl<'a> RunSpec<'a> {
    pub fn new(command: &'a str, args: &'a [String], policy: &'a Policy) -> Self {
        RunSpec {
            command,
            args,
            policy,
            cwd: None,
            max_output: 1 << 20, // 1 MiB
            env: Vec::new(),
            run_id: None,
            timeout: None,
        }
    }

    pub fn cwd(mut self, cwd: &'a Path) -> Self {
        self.cwd = Some(cwd);
        self
    }

    pub fn env(mut self, k: impl Into<String>, v: impl Into<String>) -> Self {
        self.env.push((k.into(), v.into()));
        self
    }

    /// Tag the run so its stragglers can be found after it ends.
    ///
    /// The marker is also injected into the child's environment, because that
    /// is what leaves a trace in `/proc/<pid>/environ`; setting it here alone
    /// would collect nothing.
    pub fn run_id(mut self, id: &'a str) -> Self {
        self.run_id = Some(id);
        self.env.push((crate::reap::RUN_ID_VAR.to_string(), id.to_string()));
        self
    }

    pub fn timeout(mut self, d: std::time::Duration) -> Self {
        self.timeout = Some(d);
        self
    }
}

/// How the child is wired to the supervisor's stdio.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Stdio {
    /// A pty. Gives tools colour, progress bars and interactive behaviour.
    ///
    /// This is the default because a plain pipe makes most build tools disable
    /// progress output, so a build step streams nothing until it finishes.
    Pty,
    /// The supervisor's own stdin/stdout/stderr, inherited unchanged.
    ///
    /// Required for anything that speaks a protocol over stdio. A pty cannot
    /// carry one: the line discipline echoes input back, translates \n to \r\n,
    /// and buffers until a newline, so a JSON-RPC peer receives its own frames
    /// as output and never sees a clean write. Agents that implement ACP are
    /// exactly this shape, so driving them through a pty is not a tuning problem
    /// - the protocol simply cannot work.
    Pipe,
}

/// The child of the current run, as a raw pid for a signal handler.
///
/// set during the parent side of a fork, cleared when the deliverer decides the
/// run is over. The handler in main.rs uses it to reach the child's *process
/// group*, because the child called setsid and a bare kill(pid) would leave the
/// grandchildren it spawned running.
static CHILD_PID: std::sync::atomic::AtomicI32 = std::sync::atomic::AtomicI32::new(0);

/// Remember the pid that leads the group a supervisor signal should kill.
pub fn remember_child(pid: libc::pid_t) {
    CHILD_PID.store(pid, std::sync::atomic::Ordering::Relaxed);
}

/// Forget the current child. A late signal arriving after this would otherwise
/// kill the next run's group.
pub fn forget_child() {
    CHILD_PID.store(0, std::sync::atomic::Ordering::Relaxed);
}

/// Kill the current run's process group; the signal handler's whole job.
///
/// Kept to libc::kill so it stays async-signal-safe for the handler.
pub fn kill_child_group(sig: libc::c_int) {
    let pid = CHILD_PID.load(std::sync::atomic::Ordering::Relaxed);
    if pid > 0 {
        kill_group(pid, sig);
    }
}

/// Run `command` confined, handing it the supervisor's own stdio.
///
/// There is no output to relay: the child writes straight to the supervisor's
/// stdout. Enforcement evidence is still collected, because it travels over the
/// attestation pipe rather than stdio.
///
/// The deadline still applies and still signals the whole process group, so a
/// piped agent that spawns background work does not outlive its run.
pub fn run_piped(spec: &RunSpec<'_>) -> Result<RunOutcome, String> {
    let mut fds = [0 as libc::c_int; 2];
    // SAFETY: pipe writes two fds into the array on success; both are adopted
    // into OwnedFd immediately after.
    if unsafe { libc::pipe(fds.as_mut_ptr()) } != 0 {
        return Err(format!("pipe: {}", io::Error::last_os_error()));
    }
    let (att_read, att_write) =
        unsafe { (OwnedFd::from_raw_fd(fds[0]), OwnedFd::from_raw_fd(fds[1])) };

    // Formatted before the fork: allocating on the child side is not
    // async-signal-safe.
    let attest_prefix = format!(
        "landlock_abi={} rule_count={} ",
        crate::probe::effective_abi() as u32,
        crate::confine::planned_rule_count(spec.policy)
    );

    let pid = match unsafe { libc::fork() } {
        -1 => {
            let e = io::Error::last_os_error();
            return Err(format!("fork: {e}"));
        }
        0 => {
            // Child. Never returns.
            drop(att_read);
            child_exec_piped(&att_write, &attest_prefix, spec);
        }
        pid => pid,
    };
    drop(att_write);
    // A supervisor TERM/INT must reach the child's whole group; record its pid
    // until this run is collected.
    remember_child(pid);

    let started = std::time::Instant::now();
    // Wait for the child, but poll so the deadline can interrupt: in pipe mode
    // there is no output stream to drive an event loop, so a blocking waitpid
    // would make the timeout unenforceable.
    //
    // This call reaps the child and hands back the raw status. It cannot be
    // followed by wait_child: waitpid reports ECHILD the second time, so the
    // status has to come from the same call that collected it.
    let waited = wait_until(pid, spec.timeout);
    let timed_out = waited.timed_out;
    if timed_out {
        kill_group(pid, libc::SIGKILL);
    }
    let status = match waited.status {
        Some(s) => s,
        // The deadline fired and the kill is in flight, so block for the reap.
        None => wait_child(pid)?,
    };
    collect_run(spec, pid);
    let duration = started.elapsed();

    // The attestation pipe buffer is never a bottleneck: the child writes one
    // short line before exec, so reading after waitpid cannot deadlock.
    let attestation = read_attestation(&att_read);

    let (killed, signal) = if libc::WIFSIGNALED(status) {
        (true, Some(libc::WTERMSIG(status)))
    } else {
        (false, None)
    };
    forget_child();

    Ok(RunOutcome {
        status: status_from_raw(status),
        output: String::new(),
        killed_by_signal: killed,
        signal,
        duration,
        attestation,
        timed_out,
        bytes_total: 0,
        truncated: false,
    })
}

/// Result of polling for a child's exit.
struct Waited {
    /// The raw status word, present once the child has been reaped.
    status: Option<libc::c_int>,
    /// True when the deadline expired while the child was still running.
    timed_out: bool,
}

/// Poll for `pid`'s exit until it is reaped or `timeout` expires.
///
/// Returns the status word when the child was collected here, and `None` when
/// the deadline fired first - the caller then has to signal the group and reap.
fn wait_until(pid: libc::pid_t, timeout: Option<std::time::Duration>) -> Waited {
    let start = std::time::Instant::now();
    loop {
        let mut status: libc::c_int = 0;
        // SAFETY: waitpid on a child of this process with valid out-params.
        let r = unsafe { libc::waitpid(pid, &mut status, libc::WNOHANG) };
        if r == pid {
            return Waited {
                status: Some(status),
                timed_out: false,
            };
        }
        if r == -1 {
            // ECHILD or an interrupt we cannot recover from; the caller falls
            // back to a blocking wait, which will surface the real error.
            return Waited {
                status: None,
                timed_out: false,
            };
        }
        if let Some(limit) = timeout {
            if start.elapsed() >= limit {
                return Waited {
                    status: None,
                    timed_out: true,
                };
            }
        }
        std::thread::sleep(std::time::Duration::from_millis(10));
    }
}

/// Child side of a piped run: inherit stdio untouched, then confine and exec.
fn child_exec_piped(attest: &OwnedFd, prefix: &str, spec: &RunSpec<'_>) -> ! {
    // New session, so the child leads its own process group and the deadline's
    // group signal reaches its helpers.
    if unsafe { libc::setsid() } == -1 {
        unsafe { libc::_exit(126) };
    }
    child_tail(attest, prefix, spec)
}

/// Run `command` confined by `policy`, buffering the pty transcript.
///
/// The pty is what gives agent runs their character: a plain pipe makes most
/// tools disable colour, progress bars and interactive behaviour, so a `build`
/// step streams nothing until it finishes.
pub fn run(spec: &RunSpec<'_>) -> Result<RunOutcome, String> {
    let mut out: Vec<u8> = Vec::new();
    let mut truncated = false;
    let outcome = run_streaming(spec, |chunk| {
        if out.len() + chunk.len() > spec.max_output {
            let room = spec.max_output.saturating_sub(out.len());
            out.extend_from_slice(&chunk[..room]);
            truncated = true;
        } else {
            out.extend_from_slice(chunk);
        }
        Ok(())
    })?;
    let mut outcome = outcome;
    if truncated {
        out.extend_from_slice(b"\n[output truncated]\n");
    }
    outcome.truncated = truncated;
    outcome.output = String::from_utf8_lossy(&out).into_owned();
    Ok(outcome)
}

/// Run `command` confined, handing each pty chunk to `on_output` as it arrives.
///
/// This is the primitive the live-log frontend is built on: a run can last
/// minutes, and a caller that only sees output after exit has no way to show
/// progress, show a plan being executed, or cancel a runaway job.
///
/// The callback is invoked on the supervisor thread, so an error from it aborts
/// the capture - but the child is still reaped, never left as a zombie.
pub fn run_streaming<F>(spec: &RunSpec<'_>, mut on_output: F) -> Result<RunOutcome, String>
where
    F: FnMut(&[u8]) -> io::Result<()>,
{
    let mut master: RawFd = -1;
    let mut slave: RawFd = -1;
    // SAFETY: openpty writes two valid fds into the out-params on success. We
    // take ownership immediately after and adopt them into OwnedFd.
    let rc = unsafe {
        libc::openpty(
            &mut master,
            &mut slave,
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            std::ptr::null_mut(),
        )
    };
    if rc != 0 {
        return Err(format!("openpty: {}", io::Error::last_os_error()));
    }
    let master = unsafe { OwnedFd::from_raw_fd(master) };
    let slave = unsafe { OwnedFd::from_raw_fd(slave) };

    // Attestation pipe. The child writes its evidence here after confinement and
    // before exec; the parent reads it back. Created before the fork so no
    // allocation happens on the child side.
    let mut fds = [0 as libc::c_int; 2];
    // SAFETY: pipe writes two fds into the array on success; both are adopted
    // into OwnedFd immediately after.
    if unsafe { libc::pipe(fds.as_mut_ptr()) } != 0 {
        return Err(format!("pipe: {}", io::Error::last_os_error()));
    }
    let (att_read, att_write) =
        unsafe { (OwnedFd::from_raw_fd(fds[0]), OwnedFd::from_raw_fd(fds[1])) };

    // The static half of the attestation is formatted here, before the fork,
    // because allocating on the child side is not async-signal-safe.
    let attest_prefix = format!(
        "landlock_abi={} rule_count={} ",
        crate::probe::effective_abi() as u32,
        crate::confine::planned_rule_count(spec.policy)
    );

    let pid = match unsafe { libc::fork() } {
        -1 => {
            let e = io::Error::last_os_error();
            return Err(format!("fork: {e}"));
        }
        0 => {
            // Child. Never returns. The write end is closed first so the parent
            // sees EOF rather than waiting on a descriptor we no longer need.
            drop(att_read);
            child_exec(&slave, &att_write, &attest_prefix, spec);
        }
        pid => pid,
    };

    // Parent: drop our copy of the slave. If we held it open, the pty would
    // never report EOF after the child exits and the read loop below would hang.
    drop(slave);
    // The child holds the write end only; the parent must close its copy or the
    // read below would block forever once the child exits.
    drop(att_write);
    // A supervisor TERM/INT must reach the child's whole group.
    remember_child(pid);

    let started = std::time::Instant::now();
    let pump = pump(&master, spec.timeout, pid, &mut on_output)?;

    // A sink that failed (the Go side hung up, the log file was removed) is not
    // a sandbox failure, but the run still has to be reaped and the deadline
    // still honoured, so this only affects whether the child gets signalled.
    if pump.sink_failed {
        kill_group(pid, libc::SIGKILL);
    }

    let status = wait_child(pid)?;
    collect_run(spec, pid);
    let duration = started.elapsed();

    // The child writes a short fixed-format line before exec, so the pipe buffer
    // is never a bottleneck and reading after waitpid cannot deadlock.
    let attestation = read_attestation(&att_read);

    // WIFSIGNALED/WTERMSIG are functions in libc's Rust bindings, not patterns.
    let (killed, signal) = if libc::WIFSIGNALED(status) {
        (true, Some(libc::WTERMSIG(status)))
    } else {
        (false, None)
    };
    forget_child();

    Ok(RunOutcome {
        status: status_from_raw(status),
        output: String::new(),
        killed_by_signal: killed,
        signal,
        duration,
        attestation,
        timed_out: pump.timed_out,
        bytes_total: pump.bytes_total,
        truncated: false,
    })
}

/// What the read loop observed while draining the pty.
struct Pump {
    timed_out: bool,
    bytes_total: u64,
    sink_failed: bool,
}

/// Drain the master side until EOF, relaying each chunk to the sink.
///
/// `poll` rather than a blocking `read`, because a deadline has to be able to
/// interrupt a wait, and the only portable way to do that here is to stop
/// waiting. On expiry the child's entire process group is signalled: the child
/// called `setsid`, so it leads its own group and a group kill reaches the
/// grandchildren it spawned, which a bare `kill(pid)` would leave running.
fn pump<F>(
    master: &OwnedFd,
    timeout: Option<std::time::Duration>,
    pid: libc::pid_t,
    on_output: &mut F,
) -> Result<Pump, String>
where
    F: FnMut(&[u8]) -> io::Result<()>,
{
    let mut state = Pump {
        timed_out: false,
        bytes_total: 0,
        sink_failed: false,
    };
    let mut buf = [0u8; 8192];
    let start = std::time::Instant::now();
    // After a deadline kill we still want the tail of the output, so we allow a
    // short grace window rather than abandoning the transcript immediately.
    let mut grace: Option<std::time::Instant> = None;

    loop {
        let now = std::time::Instant::now();
        let wait_ms: libc::c_int = match (timeout, grace) {
            (Some(t), g) => {
                let deadline = g.unwrap_or(start + t);
                // Saturating: a deadline in the past yields a zero timeout,
                // which poll reports as an immediate expiry.
                let remaining = deadline.saturating_duration_since(now);
                remaining.as_millis().min(i32::MAX as u128) as libc::c_int
            }
            (None, _) => -1,
        };

        let mut pfd = libc::pollfd {
            fd: master.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        };
        // SAFETY: pfd is a single valid pollfd, and nfds matches. The fd is
        // owned by master, which outlives this call.
        let rc = unsafe { libc::poll(&mut pfd, 1, wait_ms) };

        if rc == 0 {
            // Timed out. Signal the group and drain whatever lands next.
            if grace.is_none() {
                kill_group(pid, libc::SIGKILL);
                state.timed_out = true;
                grace = Some(std::time::Instant::now() + std::time::Duration::from_secs(5));
                continue;
            }
            // The grace window closed too, so the child is unkillable (uninterruptible
            // sleep, or a wedged namespace). Give up on the transcript; waitpid below
            // still reports whatever status is available.
            break;
        }

        if rc < 0 {
            let e = io::Error::last_os_error();
            if e.kind() == io::ErrorKind::Interrupted {
                continue;
            }
            break;
        }

        // SAFETY: buf is a valid writable region of buf.len() bytes, and the fd
        // is owned by master which outlives this call.
        let n = unsafe {
            libc::read(
                master.as_raw_fd(),
                buf.as_mut_ptr().cast::<libc::c_void>(),
                buf.len(),
            )
        };

        if n > 0 {
            let n = n as usize;
            state.bytes_total += n as u64;
            if on_output(&buf[..n]).is_err() {
                state.sink_failed = true;
            }
            continue;
        }

        if n == 0 {
            break; // clean EOF
        }

        let e = io::Error::last_os_error();
        if e.kind() == io::ErrorKind::Interrupted {
            continue;
        }
        // EIO is the normal Linux pty teardown signal once the last slave closes.
        // Any other error ends capture; the child's exit status is still valid.
        break;
    }

    Ok(state)
}

/// Signal a process group, falling back to the bare pid.
///
/// The child is a session leader after `setsid`, so its pgid equals its pid and
/// the negative form reaches the whole tree. The fallback covers the case where
/// setup failed before `setsid` and no group was ever established.
///
/// SAFETY: `pid` is our own child, either immediately after fork or after a
/// reaped waitpid, so the pid cannot have been recycled.
fn kill_group(pid: libc::pid_t, sig: libc::c_int) {
    // SAFETY: kill with a negative pid targets the process group; ESRCH simply
    // means it already exited, which is the outcome we wanted anyway.
    // SAFETY: kill with a negative pid targets the process group; ESRCH simply
    // means it already exited, which is the outcome we wanted anyway.
    unsafe { libc::kill(-pid, sig) };
    // SAFETY: same contract; only reached if the group kill found nothing.
    unsafe { libc::kill(pid, sig) };
}

/// Leave nothing running once the run's own process is gone.
///
/// Two layers, because neither covers the whole field on its own:
///
/// 1. The process group, for the ordinary `command &` shape. Signalled through
///    `sweep_group` below.
/// 2. The run's environment marker, for a child that called `setsid` and so
///    left the group entirely. That is what npm and `vite preview` do, and it
///    is how a preview server came to outlive a finished run and keep holding
///    its port.
///
/// Called after the child has been reaped, so the exit status is already ours
/// and neither layer can delay or distort it.
fn collect_run(spec: &RunSpec<'_>, pid: libc::pid_t) {
    sweep_group(pid);
    if let Some(run_id) = spec.run_id {
        crate::reap::sweep_run(run_id);
    }
}

/// Sweep the process group after the child has been reaped.
///
/// Only the group is signalled, never the bare pid. The pid has already been
/// reaped here, so a bare `kill(pid)` could hit a recycled pid belonging to an
/// unrelated process; a process group id stays reserved for as long as the group
/// has any member, so `kill(-pid)` is both the safer and the more correct form
/// here.
///
/// SIGTERM first so a well-behaved child can shut down, then SIGKILL because the
/// point is that nothing survives - a background server that ignores TERM must
/// not outlive the run. This is a deliberate trade: a run can no longer leave a
/// daemon behind for later, which is the intended behaviour for a platform
/// where every action is attested and confined to the run.
fn sweep_group(pid: libc::pid_t) {
    // SAFETY: negative pid targets the process group led by our own child.
    // ESRCH means the group is already empty, which is the desired end state.
    unsafe { libc::kill(-pid, libc::SIGTERM) };
    // A grace window rather than an instant KILL, so a child that is mid-write
    // gets to finish. Bounded, because we cannot wait forever on a wedged one.
    std::thread::sleep(std::time::Duration::from_millis(150));
    // SAFETY: as above; anything that ignored or outlived SIGTERM goes now.
    unsafe { libc::kill(-pid, libc::SIGKILL) };
}

unsafe fn set_cloexec(fd: &OwnedFd) {
    let raw = fd.as_raw_fd();
    let flags = unsafe { libc::fcntl(raw, libc::F_GETFD) };
    if flags != -1 {
        unsafe { libc::fcntl(raw, libc::F_SETFD, flags | libc::FD_CLOEXEC) };
    }
}

/// Send the child's confinement evidence to the parent.
///
/// Deliberately allocation-free: it runs after fork, where the only safe
/// operations are async-signal-safe ones. A fixed-format line keeps it to a
/// stack buffer, and it is comfortably under PIPE_BUF so the write is atomic.
fn write_attestation(fd: &OwnedFd, prefix: &str) {
    // Ask the kernel what it actually did, rather than assuming the request
    // took effect.
    //
    // SAFETY: PR_GET_NO_NEW_PRIVS writes only through the return value.
    let nnp = unsafe { libc::prctl(libc::PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0) } == 1;

    // There is no prctl for reading back network restrictions, so the only
    // honest way to know is to try the syscall Landlock actually governs.
    //
    // Creating a socket is deliberately *not* the test: Landlock does not
    // restrict socket(2), only bind(2) and connect(2). Probing creation would
    // wrongly report the sandbox as open. So this attempts a real connect and
    // requires the specific errno the kernel raises for a Landlock denial, which
    // distinguishes "the sandbox blocked it" from "nothing was listening".
    //
    // The connect is non-blocking and bounded: verification must be a fast
    // yes/no, not something that can stall a run. A denied connect fails
    // immediately with EACCES; an allowed one either completes (source port
    // bound, egress open) or stays pending against a peer that never answers -
    // which is `not denied`, because an unrestricted sandbox would be in the
    // same TCP retransmit wait, and this flag exists to report the sandbox, not
    // the network.
    let net_denied = {
        let fd = unsafe { libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0) };
        if fd < 0 {
            // No socket at all is a stronger restriction, so still denied.
            true
        } else {
            // SAFETY: fcntl on a descriptor we own; O_NONBLOCK makes connect
            // return EINPROGRESS instead of pinning the child against a peer.
            let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
            if flags >= 0 {
                unsafe { libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) };
            }
            let addr = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: 1u16.to_be(), // loopback port 1: closed, so a
                // successful sandbox shows a distinct
                // ECONNREFUSED rather than success.
                sin_addr: libc::in_addr {
                    s_addr: u32::from_be_bytes([127, 0, 0, 1]),
                },
                sin_zero: [0; 8],
            };
            let eager = unsafe {
                libc::connect(
                    fd,
                    &addr as *const libc::sockaddr_in as *const libc::sockaddr,
                    std::mem::size_of::<libc::sockaddr_in>() as libc::socklen_t,
                )
            };
            let denied =
                if eager == 0 {
                    // Completed immediately: egress is open.
                    false
                } else {
                    match io::Error::last_os_error().raw_os_error() {
                        Some(libc::EACCES) => true,
                        Some(libc::EINPROGRESS) | Some(libc::EINTR) => {
                            // Wait briefly for the handshake to complete.
                            let mut pfd = libc::pollfd {
                                fd,
                                events: libc::POLLOUT,
                                revents: 0,
                            };
                            // SAFETY: single valid pollfd; blocks at most the
                            // stated milliseconds, cannot stall the run.
                            let ready = unsafe { libc::poll(&mut pfd, 1, 250) };
                            if ready > 0 {
                                let mut err: libc::c_int = 0;
                                let mut len = std::mem::size_of::<libc::c_int>()
                                    as libc::socklen_t;
                                // SAFETY: err is a valid int, len matches.
                                unsafe {
                                    libc::getsockopt(
                                        fd,
                                        libc::SOL_SOCKET,
                                        libc::SO_ERROR,
                                        &mut err as *mut libc::c_int
                                            as *mut libc::c_void,
                                        &mut len,
                                    )
                                };
                                err == libc::EACCES
                            } else {
                                // Pending or unknown result: egress not shown
                                // to be blocked.
                                false
                            }
                        }
                        // ECONNREFUSED, any reachability error: egress open.
                        _ => false,
                    }
                };
            unsafe { libc::close(fd) };
            denied
        }
    };

    // Copy the pre-rendered prefix into a stack buffer, then append the two
    // booleans as single bytes. No allocation, no formatting, no panic path.
    let mut buf = [0u8; 160];
    let p = prefix.as_bytes();
    // The two flags are written as ASCII digits, not as their numeric byte
    // values: 0x01 is a control character and would not survive the parse back.
    if p.len() >= buf.len() - 32 {
        return;
    }
    buf[..p.len()].copy_from_slice(p);
    let mut n = p.len();
    for (key, val) in [("no_new_privs=", nnp), ("network_denied=", net_denied)] {
        let k = key.as_bytes();
        buf[n..n + k.len()].copy_from_slice(k);
        n += k.len();
        buf[n] = if val { b'1' } else { b'0' };
        n += 1;
        buf[n] = b' ';
        n += 1;
    }
    n -= 1; // trim the trailing space, replaced by the newline below
    if n < buf.len() {
        buf[n] = b'\n';
        n += 1;
    }
    // SAFETY: writing `n` bytes from a valid initialised stack buffer to the
    // attestation pipe. write is async-signal-safe and the length is well under
    // PIPE_BUF, so the write cannot block or interleave.
    unsafe {
        libc::write(fd.as_raw_fd(), buf.as_ptr() as *const libc::c_void, n);
    }
}

/// Parse the child's attestation line.
///
/// Absence is not an error: a child killed during setup never writes one, and
/// the caller already treats a failed confinement as a hard failure.
fn read_attestation(fd: &OwnedFd) -> Option<Attestation> {
    use std::io::Read;
    let mut s = String::new();
    let mut f = match fd.try_clone() {
        Ok(clone) => std::fs::File::from(clone),
        Err(_) => return None,
    };
    if f.read_to_string(&mut s).is_err() {
        return None;
    }
    let mut a = Attestation::default();
    let mut any = false;
    for kv in s.split_whitespace() {
        let Some((k, v)) = kv.split_once('=') else {
            continue;
        };
        let n: u32 = match v.parse() {
            Ok(n) => n,
            Err(_) => continue,
        };
        match k {
            "landlock_abi" => a.landlock_abi = n,
            "rule_count" => a.rule_count = n,
            "no_new_privs" => a.no_new_privs = n == 1,
            "network_denied" => a.network_denied = n == 1,
            _ => continue,
        }
        any = true;
    }
    // Opt-in dump of the raw line, for when a flag comes back unexpected. Parsing
    // failures are otherwise silent by design - a child killed during setup
    // writes nothing - which is right in production and unhelpful when debugging.
    if std::env::var_os("SANDBOXD_DEBUG_ATTEST").is_some() {
        eprintln!("[sandboxd] attestation: {s:?}");
    }
    if any { Some(a) } else { None }
}

/// Reconstruct an `ExitStatus` from a raw `waitpid` status word.
fn status_from_raw(status: libc::c_int) -> ExitStatus {
    ExitStatus::from_raw(status)
}

/// Reap the child, retrying on EINTR.
fn wait_child(pid: libc::pid_t) -> Result<libc::c_int, String> {
    loop {
        let mut status: libc::c_int = 0;
        // SAFETY: `status` is a valid out-param; we do not use WNOWAIT, so the
        // child is reaped here.
        let rc = unsafe { libc::waitpid(pid, &mut status, 0) };
        if rc == pid {
            return Ok(status);
        }
        if rc == -1 {
            let e = io::Error::last_os_error();
            if e.kind() == io::ErrorKind::Interrupted {
                continue;
            }
            return Err(format!("waitpid: {e}"));
        }
    }
}

/// Child path: session, pty wiring, confinement, exec.
///
/// Reached only via `fork`. Every failure calls `_exit` directly rather than
/// unwinding - panicking across a fork boundary is undefined behaviour.
fn child_exec(slave: &OwnedFd, attest: &OwnedFd, prefix: &str, spec: &RunSpec<'_>) -> ! {
    let slave_fd = slave.as_raw_fd();

    // New session. Detaches from the supervisor's terminal and process group so
    // a signal aimed at us does not reach the sandboxed process, and vice versa.
    if unsafe { libc::setsid() } == -1 {
        unsafe { libc::_exit(126) };
    }

    // Make the pty slave our controlling terminal, so isatty() and job control
    // behave as they would for a human at a terminal.
    if unsafe { libc::ioctl(slave_fd, libc::TIOCSCTTY, 0) } == -1 {
        unsafe { libc::_exit(126) };
    }

    // Wire the pty onto 0/1/2. dup2 clears FD_CLOEXEC on the new descriptors, so
    // they survive the exec below.
    for fd in 0..=2 {
        if unsafe { libc::dup2(slave_fd, fd) } == -1 {
            unsafe { libc::_exit(126) };
        }
    }

    // Drop the original slave descriptor, once 0/1/2 hold the pty. dup2 does not
    // consume its source, and the descriptor is not close-on-exec, so it would
    // otherwise be inherited by the execed program and every descendant.
    // Consequence: the master never sees the slave close, so a run that leaves a
    // background process behind blocks the supervisor for that process's whole
    // lifetime.
    if slave_fd > 2 && unsafe { libc::close(slave_fd) } == -1 {
        unsafe { libc::_exit(126) };
    }

    child_tail(attest, prefix, spec)
}

/// Everything a child does after its stdio is wired: chdir, environment,
/// confinement, attestation, and exec.
///
/// Shared by the pty and pipe paths so the two cannot drift. The security
/// relevant half - confine, then attest, then exec, never anything in between -
/// lives here once, which is the only way to be sure a new transport inherits
/// it rather than reimplementing it.
///
/// Reached only via `fork`. Every failure calls `_exit` directly rather than
/// unwinding: panicking across a fork boundary is undefined behaviour.
fn child_tail(attest: &OwnedFd, prefix: &str, spec: &RunSpec<'_>) -> ! {
    if let Some(dir) = spec.cwd {
        let c = match std::ffi::CString::new(dir.as_os_str().as_encoded_bytes()) {
            Ok(c) => c,
            Err(_) => unsafe { libc::_exit(126) },
        };
        if unsafe { libc::chdir(c.as_ptr()) } == -1 {
            unsafe { libc::_exit(126) };
        }
    }

    // Environment overrides, applied before confinement so that anything the
    // policy inspects already sees the sandboxed view of the world.
    for (k, v) in &spec.env {
        let (Ok(k), Ok(v)) = (
            std::ffi::CString::new(k.as_str()),
            std::ffi::CString::new(v.as_str()),
        ) else {
            unsafe { libc::_exit(126) };
        };
        if unsafe { libc::setenv(k.as_ptr(), v.as_ptr(), 1) } == -1 {
            unsafe { libc::_exit(126) };
        }
    }

    // Confinement, last thing before exec. On failure we exit rather than run
    // unconfined. Fail closed, always - no exceptions for "but the command is
    // probably fine".
    if let Err(e) = confine::confine(spec.policy) {
        let msg = format!("[sandboxd] refusing to exec unconfined: {e}\n");
        unsafe {
            libc::write(2, msg.as_ptr() as *const libc::c_void, msg.len());
        }
        unsafe { libc::_exit(127) };
    }

    // Report what confinement actually achieved, from inside the sandbox.
    // Written before exec so the parent always has it, even for a program that
    // produces no output at all.
    write_attestation(attest, prefix);

    // Close the attestation pipe across exec. Otherwise the write end is
    // inherited by every descendant, and the supervisor - which reads this pipe
    // to EOF - blocks until the last one exits.
    // SAFETY: `attest` is a valid descriptor owned by this child, and the call
    // is a single fcntl, which is async-signal-safe.
    unsafe { set_cloexec(attest) };

    // Build argv on the child's stack via CString before exec.
    let mut cargs: Vec<std::ffi::CString> = Vec::with_capacity(spec.args.len() + 1);
    let prog = match std::ffi::CString::new(spec.command) {
        Ok(c) => c,
        Err(_) => unsafe { libc::_exit(127) },
    };
    cargs.push(prog.clone());
    for a in spec.args {
        match std::ffi::CString::new(a.as_str()) {
            Ok(c) => cargs.push(c),
            Err(_) => unsafe { libc::_exit(127) },
        }
    }
    let mut argv: Vec<*const libc::c_char> = cargs.iter().map(|c| c.as_ptr()).collect();
    argv.push(std::ptr::null());

    // SAFETY: argv is a null-terminated array of valid NUL-terminated strings,
    // and prog outlives the call (execvp only returns on failure).
    unsafe { libc::execvp(prog.as_ptr(), argv.as_ptr()) };
    unsafe { libc::_exit(127) }
}
