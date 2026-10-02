# sandboxd

A small executor that runs a command inside a kernel-enforced sandbox, then
returns proof of what it enforced.

The point is not that the command is untrusted in theory. It is that the
isolation is a property of the kernel rather than a property of the program, and
that a caller can check it afterwards instead of taking a log line on trust.

## What it enforces

| Control | Mechanism | Evidence |
| --- | --- | --- |
| Filesystem scope | Landlock path rules, deny by default | `confinement-proof.sh` §2–3 |
| Network egress | Landlock `bind`/`connect` denied | §4, plus the child's own `connect` probe |
| Privilege escalation | `no_new_privs` | §5, read back via `prctl(2)` |
| Process isolation | new session, own pty | §7 |
| Exit fidelity | signals mapped to 128+N | §8 |
| Fail-closed | refuses to exec if the ruleset cannot be built | §9 |
| Run lifetime | deadline kills the whole process group | §11 |
| Stream integrity | every emitted event parses as JSON | §11 |

Everything not explicitly granted is unreachable. There is no allow-by-default
path.

## Running it

```sh
cargo build

# Run something confined.
./target/debug/sandboxd run --workspace /path/to/ws -- /bin/echo hello

# Same confinement, but stream the run as it happens.
./target/debug/sandboxd exec --workspace /path/to/ws -- /bin/sh -c 'for i in 1 2 3; do echo tick $i; sleep 1; done'

# Ask what the kernel can do.
./target/debug/sandboxd probe

# Report this process's own confinement state, via prctl.
./target/debug/sandboxd run --workspace /path/to/ws --read "$PWD/target/debug/sandboxd" \
  -- "$PWD/target/debug/sandboxd" guard
```

## `run` and `exec`

Both apply identical confinement; they differ only in how pty bytes reach the
caller. `run` buffers the transcript and prints it at the end. `exec` emits
newline-delimited JSON on stdout as output arrives, which is what the control
plane uses for a live log:

```
{"type": "run.started", "runId": "...", "exec": ["sh","-c","..."], "effectiveAbi": 9, ...}
{"type": "output", "seq": 0, "bytes": 8, "data": "dGljayAxDQo="}
{"type": "output", "seq": 1, "bytes": 8, "data": "dGljayAyDQo="}
{"type": "enforcement", "attested": true, "noNewPrivs": true, "networkDenied": true, ...}
{"type": "exit", "exitCode": 0, "durationNs": "2996000000", "timedOut": false, ...}
```

Output is base64 because a pty carries arbitrary bytes: a read can end mid-UTF-8
sequence, and progress bars emit carriage returns and escapes that are not text
in any ordinary sense. Human diagnostics go to stderr so every line of stdout is
parseable.

Flags worth knowing:

- `--read DIR` grants read+execute beyond the system set, for toolchains that do
  not live in `/usr`. Denied by default, opt-in per grant.
- `--env KEY=VALUE` sets an environment variable in the child. The child does not
  inherit the supervisor's environment.
- `--response FILE` writes the `sandbox.v1.ExecResponse` contract as protojson.
- `--timeout-ms MS` bounds the run. On expiry the child's **entire process
  group** is killed, so a background process it spawned does not survive and keep
  running after the control plane believes the run has stopped.
- `--allow-net` exists and is **refused** by the current policy validation. Loosening
  network access should be a deliberate change, not a flag someone flips.

## What a run looks like

```
$ sandboxd run --workspace ./ws -- /bin/cat /etc/shadow
Landlock      : ENFORCED at ABI v9
  policy:
  network    : DENIED (tcp bind + connect)
  no_new_privs: true
  writable   :
    + ./ws  (rw)
  read-only  :
    + /usr  (ro,exec)
    + /etc  (ro,exec)
  everything else: unreachable
confinement: ruleset built at kernel ABI v9
             rules      : 12 path rule(s), network mediated, zero grants (deny all)
             enforcement: asserted at restrict_self()
exec        : /bin/cat /etc/shadow
---
cat: /etc/shadow: Permission denied
---
exit        : 1
```

## Proving it

```sh
./confinement-proof.sh ./target/debug/sandboxd
```

50 cases, all attempted escapes, all denied by the kernel. The suite is the
artefact to trust over any description of the policy: if Landlock ever stops
enforcing, these fail.

Three things the suite is careful about:

- **A denial must be evidenced.** A non-zero exit alone does not count. The
  output has to contain the kernel's refusal, otherwise the command might simply
  have failed for an unrelated reason and the sandbox would look fine while being
  open.
- **A case must prove it ran.** Anything that needs a background process records
  its pid and fails as *inconclusive* if it never appeared. Two cases in this
  suite did exactly that while being written — the descendant was killed before it
  could start, so the assertions passed without testing anything.
- **Real toolchains are exercised.** A sandbox that only runs `cat` and `sh` is
  not worth having, so §10 compiles with `gcc`, commits with `git` against a
  synthetic home, and confirms a toolchain outside `/usr` is *not* granted.

## Design notes worth knowing

**A real `HOME` is never granted.** The user's home directory holds SSH keys and
cloud credentials. Granting it would hand every sandboxed process all of them, so
tools that want configuration get a synthetic home instead, seeded deliberately.

**Each run gets a private scratch directory.** Build tools insist on a temporary
directory. A shared `/tmp` would be both a cross-run information leak and an
escape surface, so `TMPDIR` points inside the workspace and inherits the same
policy as everything else.

**Landlock ABI 9 versus the kernel's 10.** The `landlock` crate currently
expresses up to ABI v9. Confinement at a lower ABI than the kernel offers is
safe; the executor reports the version it actually enforced rather than the one
the kernel advertises.

**A mistyped workspace is an error.** The scratch directory lives inside the
workspace, so creating it before checking would quietly materialise a path the
caller got wrong and start a run against an empty directory. The check happens
first.
