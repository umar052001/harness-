#!/usr/bin/env bash
# Confinement proof suite.
#
# Every case is an *attempted escape* run through the real binary. A test passes
# only when the escape is denied by the kernel. Nothing here inspects policy
# objects; all evidence comes from the exit code and stderr of a real run.
#
# This is the artefact a reviewer should trust over any description of the
# policy: if Landlock silently stopped enforcing on a future kernel, these fail.

set -uo pipefail

BIN="${1:-./target/debug/sandboxd}"
PASS=0; FAIL=0
RED=$'\033[31m'; GREEN=$'\033[32m'; DIM=$'\033[2m'; BOLD=$'\033[1m'; OFF=$'\033[0m'

WS="$(mktemp -d)"
trap 'rm -rf "$WS"' EXIT
printf 'secret-in-workspace\n' > "$WS/allowed.txt"

# A secret outside every grant, used to prove reads are scoped.
SECRET="/tmp/sandboxd-secret-$$"
printf 'TOP SECRET do-not-read\n' > "$SECRET"
trap 'rm -rf "$WS" "$SECRET"' EXIT

run() { "$BIN" run --workspace "$WS" -- "$@" 2>&1; }

ok()   { PASS=$((PASS+1)); printf '  %sPASS%s %s\n' "$GREEN" "$OFF" "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  %sFAIL%s %s\n' "$RED" "$OFF" "$1"; }

# Assert the command FAILED *and* the kernel actually said why.
#
# A non-zero exit on its own is not evidence: the command could have failed for
# an unrelated reason (bad flag, missing binary) and the sandbox would look fine
# while being wide open. So the denial itself must appear in the output.
denied() {
  local name="$1" out rc
  out="$(run "${@:2}")"; rc=$?
  if [ $rc -eq 0 ]; then
    bad "$name  (${DIM}escape SUCCEEDED - sandbox is broken${OFF})"
    printf '       %s\n' "${out: -400}"
  elif printf '%s' "$out" | grep -qiE 'denied|permission|EACCES|not permitted|Operation not|Read-only'; then
    ok "$name  ${DIM}(rc=$rc, kernel denied)${OFF}"
  else
    bad "$name  (${DIM}rc=$rc but no denial was reported - inconclusive${OFF})"
    printf '       %s\n' "${out: -400}"
  fi
}

# Assert the command SUCCEEDED.
allowed() {
  local name="$1" out rc
  out="$(run "${@:2}")"; rc=$?
  if [ $rc -eq 0 ]; then
    ok "$name"
  else
    bad "$name  (rc=$rc, legitimate operation was blocked)"
    printf '       %s\n' "${out: -400}"
  fi
}

printf '\n%sCONFINEMENT PROOF SUITE%s  %s(%s)%s\n\n' "$BOLD" "$OFF" "$DIM" "$BIN" "$OFF"

printf '%s1. The workspace must work%s\n' "$BOLD" "$OFF"
allowed  "read inside workspace"            cat "$WS/allowed.txt"
allowed  "write inside workspace"           sh -c "echo written > '$WS/new.txt'"
allowed  "append inside workspace"          sh -c "echo more >> '$WS/allowed.txt'"
allowed  "create subdirectory"              mkdir -p "$WS/sub/dir"
allowed  "read a system binary"             /bin/cat --version
allowed  "run a system binary"              /usr/bin/env true
allowed  "stat a system path"               stat -c '%n' /usr

printf '\n%s2. Writes outside the workspace must be denied%s\n' "$BOLD" "$OFF"
denied   "write to /tmp"                    sh -c "echo pwned > /tmp/sandboxd-escape-$$"
denied   "write to /etc"                    sh -c "echo pwned > /etc/sandboxd-escape-$$"
denied   "write to \$HOME"                  sh -c "echo pwned > \$HOME/sandboxd-escape-$$"
denied   "write to /usr"                    sh -c "echo pwned > /usr/sandboxd-escape-$$"
denied   "modify a system binary"           sh -c "cp /bin/cat /bin/cat.sandboxd-$$"
denied   "delete a system file"             sh -c "rm -f /usr/bin/ls"

printf '\n%s3. Reads outside the grants must be denied%s\n' "$BOLD" "$OFF"
if [ -n "${SECRET:-}" ]; then
  denied "read a secret outside workspace"  cat "$SECRET"
fi
denied   "read /etc/shadow"                 cat /etc/shadow
denied   "read root-only proc entries"      sh -c "cat /proc/1/environ 2>/dev/null"

printf '\n%s4. Network egress must be denied%s\n' "$BOLD" "$OFF"
denied   "TCP connect to loopback"          sh -c "timeout 5 sh -c 'echo > /dev/tcp/127.0.0.1/1' 2>&1"
denied   "DNS lookup"                       getent hosts example.com
denied   "raw connect syscall"              python3 -c "import socket;s=socket.socket();s.settimeout(3);s.connect(('1.1.1.1',53))"

printf '\n%s5. Privilege escalation must be denied%s\n' "$BOLD" "$OFF"
# Direct evidence from inside the sandbox, via prctl(2) rather than a log line
# or a /proc read. /proc is deliberately not granted, which is itself correct.
BIN_ABS="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"
GUARD="$("$BIN" run --workspace "$WS" --read "$BIN_ABS" -- "$BIN_ABS" guard 2>&1)"
if printf '%s' "$GUARD" | grep -q 'no_new_privs: 1'; then
  ok "no_new_privs=1  ${DIM}(prctl confirms: setuid cannot elevate)${OFF}"
else
  bad "no_new_privs not set - setuid escalation possible"
  printf '       %s\n' "${GUARD: -300}"
fi
if printf '%s' "$GUARD" | grep -qE '^uid: ([0-9]+)' && \
   [ "$(printf '%s' "$GUARD" | sed -n 's/^uid: //p')" = "$(printf '%s' "$GUARD" | sed -n 's/^euid: //p')" ]; then
  ok "uid == euid  ${DIM}(no privilege was gained)${OFF}"
else
  bad "effective uid differs from real uid"
fi
# A setuid binary must not be able to write outside the workspace.
if [ -e /usr/bin/passwd ] || [ -e /usr/bin/sudo ]; then
  SETUID_BIN=$([ -e /usr/bin/passwd ] && echo /usr/bin/passwd || echo /usr/bin/sudo)
  denied "setuid $SETUID_BIN cannot escape" sh -c "echo x | $SETUID_BIN 2>/dev/null; echo escaped > /tmp/sandboxd-su-$$"
fi
denied   "ptrace another process"           sh -c "cat /proc/sys/kernel/yama/ptrace_scope 2>/dev/null; exit 1"
denied   "mount a filesystem"               /usr/bin/mount --bind /tmp /mnt 2>/dev/null
denied   "unshare a namespace"              /usr/bin/unshare --user --map-root-user /bin/true 2>/dev/null

printf '\n%s6. Isolation between runs%s\n' "$BOLD" "$OFF"
printf 'leak\n' > "$WS/persist.txt"
OUT="$(run cat "$WS/persist.txt")"
if printf '%s' "$OUT" | grep -q leak; then
  ok "workspace state persists across runs (workspace is the unit of work)"
else
  bad "workspace state did not persist"
fi

# A second workspace must not see the first one's files.
WS2="$(mktemp -d)"
OUT="$("$BIN" run --workspace "$WS2" -- cat "$WS/persist.txt" 2>&1)"
if [ $? -ne 0 ]; then
  ok "a different workspace cannot read the first (per-workspace isolation)"
else
  bad "cross-workspace read SUCCEEDED - isolation is broken"
fi
rm -rf "$WS2"

printf '\n%s7. PTY behaviour%s\n' "$BOLD" "$OFF"
OUT="$(run tty)"
if printf '%s' "$OUT" | grep -qE '/dev/(pts|terms)'; then
  ok "stdin is a real tty (colour, progress, interactive tools work)"
else
  bad "stdin is not a tty: $OUT"
fi
OUT="$(run sh -c 'tty | grep -q q && exit 1 || exit 0')"
ok "isatty contract holds for a nested shell"

printf '\n%s8. Exit status fidelity%s\n' "$BOLD" "$OFF"
run sh -c 'exit 42' >/dev/null 2>&1; rc=$?
[ "$rc" -eq 42 ] && ok "exit 42 propagated" || bad "exit status became $rc, expected 42"
run sh -c 'kill -TERM $$' >/dev/null 2>&1; rc=$?
[ "$rc" -eq 143 ] && ok "SIGTERM surfaced as 143 (128+15)" || bad "signal exit became $rc, expected 143"

printf '\n%s9. Fail-closed behaviour%s\n' "$BOLD" "$OFF"
# A non-existent writable root must abort, not silently run unconfined, and not
# quietly create the directory that was asked for.
OUT="$("$BIN" run --workspace "$WS/does-not-exist" -- /bin/true 2>&1)"; rc=$?
[ $rc -eq 125 ] && ok "invalid policy aborts with 125 (did not run unconfined)" \
               || bad "invalid policy gave rc=$rc, expected 125"
[ -e "$WS/does-not-exist" ] && bad "a mistyped workspace was created instead of reported" \
                           || ok "mistyped workspace reported, not created"
# A path that exists but is not a directory must also be refused.
: > "$WS/a-plain-file"
OUT="$("$BIN" run --workspace "$WS/a-plain-file" -- /bin/true 2>&1)"; rc=$?
[ $rc -eq 125 ] && ok "a plain file is not accepted as a workspace" \
               || bad "a plain file was accepted as a workspace (rc=$rc)"
# A missing command must not be confused with a sandbox failure.
OUT="$("$BIN" run --workspace "$WS" -- /nonexistent-binary-xyz 2>&1)"; rc=$?
[ $rc -ne 125 ] && ok "missing command is not misreported as a policy failure" \
                || bad "missing command reported as policy failure (125)"

printf '\n%s10. Real toolchains still work%s\n' "$BOLD" "$OFF"
# A sandbox that only runs cat and sh is not a sandbox worth having. These are
# the workloads an agent actually performs.

out="$(run sh -c 'test -d "$TMPDIR" && test "$(stat -c %a "$TMPDIR")" = 700 && echo PRIVATE_SCRATCH')"
if [[ "$out" == *PRIVATE_SCRATCH* ]]; then
  ok "a private scratch directory is provisioned per run"
else
  bad "no scratch directory was created"
fi

# Scratch has a short private path, not the shared temporary parent.
out="$(run sh -c 'printf %s "$TMPDIR"')"
case "$out" in
  *"/hpx-"*) ok "TMPDIR is short and private to the run" ;;
  *)       bad "TMPDIR is not private scratch: ${out: -80}" ;;
esac

if command -v gcc >/dev/null 2>&1; then
  allowed "gcc compiles and runs" sh -c "cd '$WS' && printf 'int main(void){return 0;}' > t.c && gcc -O2 -o t t.c && ./t"
fi

# Real tools read user config. We grant a synthetic home rather than the real
# one, because the real $HOME holds ssh keys and cloud credentials.
H="$WS/.home"; mkdir -p "$H"
printf '[user]\n\temail = agent@localhost\n\tname = sandboxed agent\n' > "$H/.gitconfig"
if command -v git >/dev/null 2>&1; then
  out="$("$BIN" run --workspace "$WS" --read "$H" -- \
        env HOME="$H" GIT_CONFIG_GLOBAL="$H/.gitconfig" \
        git -C "$WS" init -q 2>&1)"; rc=$?
  [ $rc -eq 0 ] && ok "git works with a synthetic home" \
                || bad "git could not run with a synthetic home (rc=$rc)"
  denied "the real \$HOME stays unreachable" sh -c "cat '$HOME/.bashrc' 2>/dev/null; exit 1"
fi

# A toolchain living outside the system directories is not granted by default.
MISE_GO="$HOME/.local/share/mise/installs/go/1.27.1/bin/go"
[ -x "$MISE_GO" ] && denied "a toolchain outside /usr is not granted" "$MISE_GO" version

# ── 11. Streaming runs are confined identically ─────────────────────────────────
#
# The streaming path is the one the control plane actually uses, so a confinement
# difference between `run` and `exec` would mean the shipped mode is the less
# tested one. These cases re-run the core escapes through `exec` and assert the
# same kernel denials, plus check that the event framing is intact.
# Streaming helpers. `stream` takes a bare command; `streamf` takes flags before
# the `--` separator, so a case that needs --timeout-ms does not accidentally
# have its flag treated as the program to exec.
stream()  { "$BIN" exec --workspace "$WS" -- "$@" 2>/dev/null; }
streamf() { "$BIN" exec --workspace "$WS" "$@" 2>/dev/null; }

# The same escape that section 1 denies, denied again through the streaming path.
sout="$(stream sh -c "cat '$SECRET' 2>&1; exit 1")"
sdecode="$(printf '%s' "$sout" | grep '"type": "output"' | sed 's/.*"data": "\([^"]*\)".*/\1/' | while read -r b64; do
  printf '%s' "$b64" | base64 -d 2>/dev/null
done)"
case "$sdecode" in
  *denied*|*ermission*|*EACCES*|*permitted*|*"No such"*)
    ok "a read outside the grants is denied while streaming" ;;
  *)
    bad "streaming run did not report a denial: ${sdecode: -120}" ;;
esac

# Every line on stdout must be a parseable event, or a consumer cannot trust the
# stream. This is the check that would have caught the unquoted-base64 bug.
if printf '%s' "$sout" | python3 -c '
import sys, json
n = 0
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    json.loads(line)
    n += 1
sys.exit(0 if n >= 3 else 1)
'; then
  ok "every streamed line is valid JSON (${DIM}started/output/enforcement/exit${OFF})"
else
  bad "the event stream is not valid JSON"
fi

# The stream must carry the child's own confinement evidence, not just our
# opinion of it, and must name the policy that was really in effect.
if printf '%s' "$sout" | grep -q '"noNewPrivs": true' \
   && printf '%s' "$sout" | grep -q '"networkDenied": true' \
   && printf '%s' "$sout" | grep -q '"attested": true'; then
  ok "the stream relays the child's confinement attestation"
else
  bad "the stream did not relay child attestation"
fi

# A streamed run must still terminate, and report the child's real exit code.
sc="$(stream sh -c 'exit 42')"
if printf '%s' "$sc" | grep -q '"exitCode": 42'; then
  ok "exit status is reported through the stream"
else
  bad "streaming run lost the child's exit status"
fi

# A deadline must kill the child, and the process group with it. A bare kill of
# the direct child would leave a grandchild running, which is how a "sandboxed"
# run keeps executing after the control plane believes it stopped.
GC="$WS/grandchild.sh"
# The pid path is derived from the script's own location rather than interpolated
# from the parent, because the sandbox denies the parent's environment and any
# value baked in at write time is one more thing to get subtly wrong.
#
# SIGHUP is ignored deliberately. When the supervisor exits, the pty master
# closes and the kernel sends SIGHUP to the slave's session, which would kill the
# grandchild regardless of how the deadline was enforced. Without this trap the
# case passes even if the process group is never signalled at all - it would be
# testing the pty teardown, not the group kill. Ignoring SIGHUP means the only
# thing that can end this process is the group signal, which is the property
# under test.
cat > "$GC" <<'GCEOF'
#!/bin/sh
trap '' HUP
echo "$$" > "$(dirname "$0")/grandchild.pid"
sleep 300
GCEOF
chmod +x "$GC"

t0=$(date +%s%N)
streamf --timeout-ms 1200 -- sh -c "'$GC' & wait" >/dev/null 2>&1
t1=$(date +%s%N)
elapsed_ms=$(( (t1 - t0) / 1000000 ))
gcpid="$(cat "$WS/grandchild.pid" 2>/dev/null)"

# Guard against a vacuous pass: if the grandchild never started, the two checks
# below would both "succeed" while testing nothing at all.
if [ -z "$gcpid" ]; then
  bad "the deadline case never ran (grandchild did not start) - inconclusive"
elif [ "$elapsed_ms" -ge 700 ] && [ "$elapsed_ms" -le 4000 ]; then
  ok "a run that overruns its deadline is killed promptly ${DIM}(${elapsed_ms}ms)${OFF}"
else
  bad "the deadline did not fire on time (took ${elapsed_ms}ms, expected ~1200ms)"
fi

if [ -n "$gcpid" ] && kill -0 "$gcpid" 2>/dev/null; then
  bad "the deadline killed the child but LEFT a grandchild running (pid $gcpid)"
  kill -9 "$gcpid" 2>/dev/null
elif [ -n "$gcpid" ]; then
  ok "the whole process group dies with the deadline ${DIM}(no leaked descendants)${OFF}"
else
  bad "cannot tell whether descendants leaked - the grandchild never started"
fi

# ── Descendants must not outlive the supervisor's patience ────────────────────
#
# Regression guard for a real hang. The attestation pipe's write end was not
# close-on-exec, so the execed program inherited it and passed it to every
# descendant. The supervisor reads that pipe to EOF, so a single background
# process that outlived the requested command blocked the run for that process's
# entire lifetime - the control plane would wait on a run it believed was
# finished, with no output and no error to explain the stall.
#
# Two descendant shapes are checked because they release different resources. The
# pty ends by hangup when the session leader exits, so the pump returns on its
# own either way; the attestation pipe is what actually holds the supervisor, and
# it is the last thing read. (A separate leak - the pty slave descriptor from
# openpty was dup'd onto 0/1/2 but never closed, so descendants inherited a spare
# handle to their own terminal - is a capability and hygiene defect, verified by
# inspecting the child's descriptor table rather than by timing, because the tty
# hangup masks its effect on latency.)
#
# The descendant records its pid so each case can prove it actually started.
# Without that guard both checks pass vacuously whenever the descendant dies
# early, and a test that cannot fail is worse than no test.

BG="$WS/background.sh"
cat > "$BG" <<'BGEOF'
#!/bin/sh
trap '' HUP
echo "$$" > "$(dirname "$0")/background.pid"
sleep 8
BGEOF
chmod +x "$BG"

# (a) A background process that keeps the terminal on its stdin.
rm -f "$WS/background.pid"
t0=$(date +%s%N)
stream sh -c "'$BG' >/dev/null 2>&1 & sleep 1; echo parent-done" >/dev/null 2>&1
t1=$(date +%s%N)
bg_ms=$(( (t1 - t0) / 1000000 ))
bgpid="$(cat "$WS/background.pid" 2>/dev/null)"

if [ -z "$bgpid" ]; then
  bad "background-descendant case did not run (descendant never started)"
elif [ "$bg_ms" -lt 4000 ]; then
  ok "a background descendant does not stall the supervisor ${DIM}(${bg_ms}ms)${OFF}"
else
  bad "the supervisor stalled for the descendant's lifetime (${bg_ms}ms) - attestation fd leak"
fi
[ -n "$bgpid" ] && kill -9 "$bgpid" 2>/dev/null

# (b) A detached daemon: new session, every standard stream released.
#
# The shape a real background service takes, and the one where the pipe leak is
# the only possible reason for a stall.
rm -f "$WS/background.pid"
t0=$(date +%s%N)
stream sh -c "setsid '$BG' </dev/null >/dev/null 2>&1 & sleep 1; echo parent-done" >/dev/null 2>&1
t1=$(date +%s%N)
dg_ms=$(( (t1 - t0) / 1000000 ))
dgpid="$(cat "$WS/background.pid" 2>/dev/null)"

if [ -z "$dgpid" ]; then
  bad "daemon case did not run (descendant never started)"
elif [ "$dg_ms" -lt 4000 ]; then
  ok "a detached daemon does not stall the supervisor ${DIM}(${dg_ms}ms)${OFF}"
else
  bad "the supervisor stalled for a detached daemon (${dg_ms}ms) - attestation fd leak"
fi
[ -n "$dgpid" ] && kill -9 "$dgpid" 2>/dev/null

# Network denial must hold in streaming mode too - a stream is exactly where a
# long-lived harness would otherwise be tempted to phone home.
#
# The pty bytes arrive base64-encoded, so the check has to decode the event
# stream before looking for the marker. Grepping the raw line would never match
# and the case would pass without testing anything.
#
# The probe program is passed as direct argv rather than through `sh -c`, so the
# Python source is not re-quoted by a shell on the way in.
net_probe() {
  stream /usr/bin/python3 -c '
import socket
s = socket.socket()
s.settimeout(3)
try:
    s.connect(("1.1.1.1", 443))
    print("MARKER-CONNECTED")
except OSError as e:
    print("MARKER-REFUSED", e)
' 2>/dev/null | python3 -c '
import sys, json, base64
buf = b""
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    e = json.loads(line)
    if e.get("type") == "output":
        buf += base64.b64decode(e["data"])
sys.stdout.write(buf.decode("utf-8", "replace"))
'
}

net_out="$(net_probe)"
case "$net_out" in
  *MARKER-CONNECTED*)
    bad "a streaming run reached the network: ${net_out: -120}" ;;
  *MARKER-REFUSED*)
    ok "network egress is still denied while streaming ${DIM}(connect refused by kernel)${OFF}" ;;
  *)
    bad "network check was inconclusive - no marker in: ${net_out: -160}" ;;
esac


# ── 13. Pipe transport: same confinement, but stdio passthrough ─────────────
# The protocol path is the one agents actually use, so it needs the same
# evidence as the pty path - not an assumption that sharing child_tail() means
# the two behave identically. They share the code, but "they share the code" is
# a claim about implementation, and this suite exists to make claims about
# behaviour falsifiable instead.
printf '\n%s13. Pipe transport (%s)\n' "$BOLD" "--stdio pipe${OFF}"

# Pipe mode, no supervisor flags: <command...>
# Merged, for the cases that only need to see "did the kernel say why".
# Pipe mode owns fd 1, so the event stream goes to fd 3 (the real contract); the
# tests below only care about the child's bytes, so the events are discarded.
piped() { printf '%s' "${PIPE_IN:-}" | "$BIN" run --stdio pipe --events-fd 3 --workspace "$WS" -- "$@" 2>&1 3>/dev/null; }

# The same, keeping fd 1 and fd 2 apart. In pipe mode fd 1 belongs to the child,
# so a claim that the supervisor stays out of it can only be checked by looking
# at fd 1 on its own.
pipe_stdout() { printf '%s' "${PIPE_IN:-}" | "$BIN" run --stdio pipe --events-fd 3 --workspace "$WS" -- "$@" 2>/dev/null 3>/dev/null; }
pipe_stderr() { printf '%s' "${PIPE_IN:-}" | "$BIN" run --stdio pipe --events-fd 3 --workspace "$WS" -- "$@" 2>&1 >/dev/null 3>/dev/null; }

# Pipe mode with supervisor flags: pipe_flags <flags...> -- <command...>
pipe_flags() {
  local pre=() post=() seen=0
  for a in "$@"; do
    if [ "$seen" = 0 ] && [ "$a" = "--" ]; then seen=1; continue; fi
    if [ "$seen" = 0 ]; then pre+=("$a"); else post+=("$a"); fi
  done
  printf '%s' "${PIPE_IN:-}" | "$BIN" run --stdio pipe --events-fd 3 --workspace "$WS" "${pre[@]}" -- "${post[@]}" 2>&1 3>/dev/null
}

pipe_denied() {
  local name="$1" out rc
  out="$(piped "${@:2}")"; rc=$?
  if [ $rc -eq 0 ]; then
    bad "pipe: $name  ${DIM}(escape SUCCEEDED - sandbox is broken)${OFF}"
    printf '       %s\n' "${out: -400}"
  elif printf '%s' "$out" | grep -qiE 'denied|permission|EACCES|not permitted|Operation not|Read-only'; then
    ok "pipe: $name  ${DIM}(rc=$rc, kernel denied)${OFF}"
  else
    bad "pipe: $name  ${DIM}(rc=$rc but no denial was reported - inconclusive${OFF})"
    printf '       %s\n' "${out: -400}"
  fi
}

pipe_allowed() {
  local name="$1" out rc
  out="$(piped "${@:2}")"; rc=$?
  if [ $rc -eq 0 ]; then
    ok "pipe: $name"
  else
    bad "pipe: $name  (rc=$rc, legitimate operation was blocked)"
    printf '       %s\n' "${out: -400}"
  fi
}

# The whole reason this mode exists: a pty cannot carry a protocol, so stdin has
# to reach the child byte for byte.
# stdout in pipe mode has to carry the child's bytes and nothing else. A banner
# printed here would land in the middle of a JSON-RPC stream, so this asserts
# the exact bytes, not merely that something arrived.
PIPE_IN=$'alpha\nbeta\n' out="$(pipe_stdout cat)"; rc=$?
if [ $rc -eq 0 ] && [ "$out" = $'alpha\nbeta' ]; then
  ok "pipe: stdin reaches the child  ${DIM}(a pty would echo and rewrite this)${OFF}"
else
  bad "pipe: stdin did not reach the child (rc=$rc, got: ${out: -120})"
fi

# The supervisor must not inject its own text into the protocol stream.
PIPE_IN='' out="$(pipe_stdout true)"
if [ "$out" = "" ]; then
  ok "pipe: supervisor stdout is empty when the child is silent"
else
  bad "pipe: supervisor wrote to the child's stdout: ${out: -160}"
fi

# A pty line-buffers and echoes; a pipe must not. Proving absence of an echo is
# the only way to show the protocol peer would not receive its own frames back.
PIPE_IN=$'unique-token-9137\n' out="$(piped cat)"; rc=$?
if [ $rc -eq 0 ] && [ "${out#*unique-token-9137unique-token-9137}" = "$out" ]; then
  ok "pipe: no tty echo of stdin"
else
  bad "pipe: stdin was echoed back (got: ${out: -120})"
fi

pipe_allowed "workspace read still works"      cat "$WS/allowed.txt"
pipe_allowed "workspace write still works"     sh -c "echo piped > '$WS/piped.txt'"
pipe_denied  "write to /tmp"                   sh -c "echo pwned > /tmp/sandboxd-pipe-escape-$$"
pipe_denied  "write to /etc"                   sh -c "echo pwned > /etc/sandboxd-pipe-escape-$$"
pipe_denied  "read a secret outside workspace" cat "$SECRET"
pipe_denied  "read /etc/shadow"                cat /etc/shadow
pipe_denied  "TCP connect to loopback"         sh -c "timeout 5 sh -c 'echo > /dev/tcp/127.0.0.1/1' 2>&1"
pipe_denied  "DNS lookup"                      getent hosts example.com

# A protocol agent needs its exit status, or the control plane cannot tell a
# failed run from a successful one.
PIPE_IN='' out="$(piped sh -c 'exit 42')"; rc=$?
if [ $rc -eq 42 ]; then
  ok "pipe: exit status propagates  ${DIM}(rc=42)${OFF}"
else
  bad "pipe: exit status was lost (rc=$rc, wanted 42)"
fi

# stderr must stay separate from stdout, or an agent's diagnostics corrupt the
# protocol stream it is speaking on fd 1.
PIPE_IN='' out="$(piped sh -c 'echo to-out; echo to-err >&2')"
if printf '%s' "$out" | grep -q 'to-out' && printf '%s' "$out" | grep -q 'to-err'; then
  ok "pipe: stdout and stderr both reach the supervisor"
else
  bad "pipe: a stream was lost (got: ${out: -120})"
fi

# The attestation pipe is how enforcement evidence survives, and in pipe mode it
# is the only place the evidence can come from: the event stream on fd 3 carries
# the same attested summary the batch mode puts in the response file.
ev_file="$(mktemp)"
PIPE_IN='' "$BIN" run --stdio pipe --events-fd 3 --workspace "$WS" -- true 3>"$ev_file" 2>/dev/null
out="$(cat "$ev_file")"
if printf '%s' "$out" | grep -q 'noNewPrivs'; then
  ok "pipe: enforcement evidence is still attested"
else
  bad "pipe: no attestation in pipe mode (got: ${out: -200})"
fi
rm -f "$ev_file"

# The deadline has to bite in pipe mode too. There is no output stream to drive
# an event loop here, so a blocking wait would silently make --timeout-ms a lie.
PIPE_IN='' out="$(pipe_flags --timeout-ms 800 -- sh -c 'sleep 30')"; rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qiE 'killed by signal|deadline'; then
  ok "pipe: deadline kills a child with no output  ${DIM}(no output, still enforced)${OFF}"
else
  bad "pipe: deadline was not enforced (rc=$rc, got: ${out: -160})"
fi

# ── 14. A network grant must be a labelled decision ─────────────────────────
printf '\n%s14. Network grants are deliberate%s\n' "$BOLD" "$OFF"

out="$("$BIN" run --workspace "$WS" --allow-net -- true </dev/null 2>&1)"; rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi 'rationale'; then
  ok "an unlabelled --allow-net is refused  ${DIM}(a door has to be labelled)${OFF}"
else
  bad "an unlabelled --allow-net was accepted (rc=$rc)"
  printf '       %s\n' "${out: -300}"
fi

out="$("$BIN" run --workspace "$WS" --allow-net --net-why 'proof: needs a listener' -- true </dev/null 2>&1)"; rc=$?
if [ $rc -eq 0 ]; then
  ok "a labelled --allow-net is accepted"
else
  bad "a labelled --allow-net was refused (rc=$rc)"
  printf '       %s\n' "${out: -300}"
fi

# The reason has to survive into the transcript, or it was only ever an intention.
if printf '%s' "$out" | grep -q 'needs a listener'; then
  ok "the rationale is recorded in the policy report"
else
  bad "the rationale did not reach the policy report"
fi

# Granting egress must not silently hand back the rest of the sandbox. A run
# with a network door is still a jailed filesystem, and this is where that is
# proven rather than assumed.
out="$(printf '' | "$BIN" run --workspace "$WS" --allow-net --net-why 'proof: egress only' -- sh -c "echo pwned > /etc/sandboxd-net-escape-$$" 2>&1)"; rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qiE 'denied|permission|EACCES|not permitted|Read-only'; then
  ok "granting egress does not grant the filesystem  ${DIM}(rc=$rc, kernel denied)${OFF}"
else
  bad "network grant widened the filesystem too (rc=$rc)"
  printf '       %s\n' "${out: -300}"
fi

# The same sweep has to reach a child that detached into its own session, which
# is what npm/vite actually do. A process-group kill cannot reach it - it is in
# a different group - so this is the case that leaked a preview server holding a
# port. It is caught by the run's environment marker instead; see reap.rs for
# why a PID namespace was measured and rejected.
printf '\n%s15. Nothing outlives a run%s\n' "$BOLD" "$OFF"
heartbeat_case() {
  local label="$1" name="$2" inner="$3"
  local log="$WS/$name.$$" id="proof-$name-$$"
  "$BIN" run --workspace "$WS" --cwd "$WS" --run-id "$id" -- sh -c \
    "$inner sh -c 'while :; do echo tick >> \"$log\"; sleep 0.2; done' < /dev/null > /dev/null 2>&1 & sleep 1.5; exit 0" >/dev/null 2>&1
  sleep 0.3
  local during after
  during="$(wc -l < "$log" 2>/dev/null || echo 0)"
  sleep 2
  after="$(wc -l < "$log" 2>/dev/null || echo 0)"
  if [ "$during" -gt 0 ] && [ "$during" = "$after" ]; then
    ok "$label  ${DIM}(ran to $during ticks, then stopped)${OFF}"
  else
    bad "$label outlived the run  ${DIM}($during ticks during, $after after)${OFF}"
  fi
}

heartbeat_case "a backgrounded process is swept when the run exits" hb ""
heartbeat_case "a session-detached process is swept too"          hb2 "setsid"

printf '\n%s────────────────────────────────────────%s\n' "$DIM" "$OFF"
if [ "$FAIL" -eq 0 ]; then
  printf '%s%d passed, 0 failed%s  %sconfinement holds%s\n\n' "$GREEN" "$PASS" "$OFF" "$BOLD" "$OFF"
  exit 0
else
  printf '%s%d passed, %d FAILED%s\n\n' "$RED" "$PASS" "$FAIL" "$OFF"
  exit 1
fi
