# Threat model

What `sandboxd` actually guarantees, and where the guarantees stop. Written so
that a reviewer can find the edges rather than take the summary on trust.

## The claim

> A command run through `sandboxd` cannot read or write files outside its
> granted paths, cannot open a network connection, and cannot gain privileges it
> did not already have.

## The adversary

A process spawned by the sandbox: an agent's tool invocation, a build script, a
model-authored program. It may be buggy, hostile, or attempting to influence its
own harness. It has the uid of the user who started it and no more.

It is *not* assumed to be cooperative with the reporting mechanism. See
[Limits](#limits-that-are-real).

## Controls

### Filesystem

Landlock path-beneath rules, built deny-by-default. Every rule is an explicit
grant; there is no implicit allow. Rights are split by path type, because
Landlock's directory-only rights (`ReadDir`, `MakeReg`, `Refer` and the rest)
are rejected outright on a regular file rather than ignored, and a
file/directory confusion is a build error instead of a silent over-grant.

Writable roots are enumerated: the workspace, any `--cwd`, the scratch
directory. Read+execute roots are the system directories plus explicit
`--read` grants. Everything else is unreachable.

**Not covered.** Landlock does not mediate `mmap` of already-open descriptors,
does not restrict `ptrace` on a same-uid process outside the ruleset's scope, and
does not protect against a process that was already running when the ruleset was
installed. Mounts created by a privileged parent before the fork are visible.

### Network

ABI v4+ rules with zero network grants, so `bind` and `connect` are refused.
Notably, `socket(2)` is *not* restricted by Landlock — the restriction is on
connecting and binding — so a probe that only tries to create a socket would
falsely report the sandbox as open. The attestation therefore attempts a real
`connect` and requires `EACCES` specifically, which distinguishes "the sandbox
blocked it" from "nothing was listening on that port".

### Privileges

`restrict_self` sets `no_new_privs`, which is what makes setuid binaries
harmless. The child reads the flag back with `prctl(PR_GET_NO_NEW_PRIVS)` and
reports it, so the claim is checked rather than assumed.

`no_new_privs` does not drop capabilities the process already holds, and it does
not restrict `ptrace` of a process the sandbox can see. It specifically prevents
privilege *elevation* through exec.

### Process

A new session and a pty. The pty is not a security boundary; it is there so
tool output looks the way a human expects, with colour and progress intact. The
supervisor drops its copy of the slave so the read loop sees EOF.

A run may be bounded by `--timeout-ms`. On expiry the child's entire process
group is signalled, which reaches processes it spawned: the child is a session
leader after `setsid`, so its process group id equals its pid and a group signal
reaches the whole tree. A bare signal to the direct child would leave background
work running after the control plane considers the run finished. This is a
liveness control, not a resource control — the process group is signalled, not
its CPU or memory budget.

Two descriptor leaks were found and fixed while building the streaming path, and
both are worth recording because neither was visible in any log line:

- The pty slave descriptor returned by `openpty` was `dup2`'d onto 0/1/2 but
  never closed, and it is not close-on-exec. The execed program therefore
  inherited a spare handle to its own terminal and passed it to every
  descendant. It is now closed once stdio is wired.
- The attestation pipe's write end was not close-on-exec, so every descendant
  inherited it. The supervisor reads that pipe to EOF, so one background process
  that outlived the requested command stalled the run for that process's entire
  lifetime. It is now `FD_CLOEXEC`.

Both are regression-tested in §11 of the proof suite, and each case records the
descendant's pid so it fails as inconclusive rather than passing silently when
the descendant dies early.

### Fail-closed

If the ruleset cannot be built, or the policy fails validation, or the kernel
lacks Landlock, the executor exits without exec'ing. There is no path that runs
the command unconfined. Exit code 125 distinguishes a refusal from the command's
own failure, so an operator can tell the two apart.

## Limits that are real

**The attestation is not unforgeable.** The child writes its own evidence to a
pipe, so a deliberately hostile child controls that pipe and could lie about it.
What the attestation establishes is that the sandbox is *configured*; it is not a
proof of good faith. The actual proof of enforcement is the escape suite, which
attempts violations and observes the kernel refusing them. A future version
should attest from outside the sandbox, where a hostile child cannot reach.

**Landlock is not a complete jail.** It is a filesystem and network
least-privilege mechanism. It does not limit CPU, memory, process count, or
wall-clock time. A run that loops forever will occupy a slot until something
outside the sandbox stops it.

**Resource exhaustion is out of scope.** No cgroup, no rlimit. There is now a
wall-clock deadline (`--timeout-ms`) that signals the process group, but nothing
constrains how much CPU or memory that group may consume before it is killed, and
nothing stops a group from forking faster than it is reaped. A caller that needs
real limits must impose them, ideally with a cgroup.

**The scratch directory is not encrypted.** It is `0700` and inside the
workspace, which protects it from other users and from other runs, but the data
in it is the parent's to read.

**Seccomp and copy-on-write are not implemented.** The `Cargo.toml` description
mentions them; the code does not have them. They are the next step, not a
current guarantee, and the description should not be read as a claim.

**ABI 9 is the ceiling.** The `landlock` crate expresses up to ABI v9 while this
kernel reports v10. The executor reports the version it enforced. A kernel
feature only reachable at v10 is simply not used, which is safe but should be
remembered when reading a rule count.

## What would break this

- Landlock disabled or removed from the LSM list at boot. `probe` fails,
  execution is refused.
- A future kernel regression in Landlock. The escape suite fails, loudly.
- A caller granting the real `$HOME` via `--read`. That exposes SSH keys and
  cloud credentials to the sandbox. The executor does not prevent this; the
  operator must not do it.
- A caller running on a kernel where `no_new_privs` does not gate setuid, which
  would be a kernel bug outside this project's control.
