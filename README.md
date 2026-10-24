# ai-os

A local control plane for coding agents. Describe what you want built; ai-os runs
a real agent inside a kernel-confined sandbox, streams every step to a live log,
and keeps the result as a workspace you can preview, inspect and download.

```
browser ──SSE──► control plane (Go) ──stdio/ACP──► agent
                      │
                      ├── workspace per run, on disk, previewable
                      └── sandboxd (Rust) ──► Landlock, no_new_privs
```
<img width="1144" height="624" alt="image" src="https://github.com/user-attachments/assets/2685e6e0-dec8-4567-90b7-dddd47586db3" />

## Requirements

| | Version |
| --- | --- |
| Go | 1.27+ |
| Rust | 1.85+ (edition 2024) |
| Node | 22.18+ (native TypeScript support for tests; Vite 8 requires 22.12+) |
| An agent | OpenCode, Gemini CLI, goose or Claude Code, installed and authenticated |

Landlock is Linux-only. Scoping the filesystem needs kernel ABI v3; denying
network egress needs v4. The executor refuses to start below those and refuses to
start if the ruleset is only *partially* enforced — it aborts rather than run a
process that is less confined than the policy claims.

## Quickstart

```sh
git clone https://github.com/umar052001/AI-Os.git
cd AI-Os
./dev.sh
```

`dev.sh` builds all three components and starts the server on
<http://127.0.0.1:8090>. Use `./dev.sh --addr 127.0.0.1:9000` to change the port, or
`make run` for the same thing through make.

Open the dashboard:

1. Describe your project in the composer. OpenCode uses **Big Pickle** by default.
2. Enable **Allow tools** to let the agent write files and run commands inside its sandbox.
3. Press **Build**. Activity shows progress and elapsed time; Preview opens on success.
4. Use **Recent projects** to revisit saved output, or **New project** for a fresh workspace.

Settings contains agent selection, an optional model override, time limits, and a
portfolio demo prompt. Tool access starts off; when disabled, requests are denied
automatically rather than waiting for an approval dialog.

When the run ends, switch to **preview** to see the result, or open **files** for
the workspace tree and **↓ zip** for the whole thing.

## What a run does

- **One run at a time.** `MaxConcurrentRuns` defaults to 1. A second request gets
  `409` with the list of what is already running rather than quietly interleaving
  two agents in one machine.
- **One server per workspace store.** A process-held file lock prevents a second
  server from bypassing the concurrency guard. OpenCode subagent delegation is
  disabled in each run's isolated configuration.
- **Stop from any tab.** The stop endpoint cancels the server-side run. A dropped
  run stream also cancels its work; files already written are retained. Reopen
  Recent projects to inspect the result before retrying.
- **Bounded execution.** Startup is bounded to 45 seconds. Prompts use the selected
  run budget, including quiet model generation; silence does not terminate them early.
- **A wall-clock ceiling.** Ten minutes by default, adjustable from 5 to 30 in
  settings. A run that hits it is stopped and says so — no silent hangs.
- **A durable transcript.** Every event is appended to
  `<workspace>/<id>.events.jsonl` as it streams, so previous runs replay their
  full history after a reload instead of showing an empty pane.
- **Durable state.** A host-owned metadata sidecar records the prompt, selected
  model, timestamps, and outcome. Interrupted runs are labelled after a restart.
- **Its own workspace.** Agent output stays in `<id>/`. The control plane stores
  transcripts and metadata alongside it, outside the agent's writable directory.

Run data defaults to `~/.local/share/ai-os/workspaces`; use `-workspaces` to
choose another directory. Generated projects,
prompts, and transcripts are local runtime data and do not belong in this repository.

## Harnesses

The agent conversation speaks [Agent Client Protocol](https://agentclientprotocol.com)
over stdio. Harness compatibility depends on the agent's ACP implementation. A CLI harness
(Claude Code) is wrapped in the same interface.

| Agent | Transport | Notes |
| --- | --- | --- |
| OpenCode | ACP | Big Pickle; negotiated through session config options |
| Gemini CLI | ACP | |
| goose | ACP | |
| KiloCode | ACP | |
| Claude Code | CLI | wrapped, not native ACP |

The catalogue is a manifest list, so adding an agent is a data change:

```go
{ID: "myagent", Name: "My Agent", Kind: harness.KindACP, Command: "myagent", Args: []string{"acp"}}
```

Pass `-harness-overlay <dir>` to load additional manifests without rebuilding.

## Architecture

| Component | Language | Responsibility |
| --- | --- | --- |
| `go/cmd/server` | Go | HTTP control plane: SSE run stream, workspaces, previews, publish, archives |
| `go/internal/api` | Go | Routes, workspace store, publish index, run registry |
| `go/internal/runner` | Go | Run lifecycle, timeouts, event emission, reaping |
| `go/internal/acp` | Go | ACP client: sessions, streaming updates, permission policy |
| `go/internal/harness` | Go | Agent catalogue and discovery |
| `rust/sandboxd` | Rust | Executes a command under Landlock and returns an attested result |
| `web` | Solid + Vite + Tailwind | The dashboard |
| `go/internal/router` | Go | Chooses the cheapest tool good enough for a task |
| `go/internal/toolschema` | Go | Compiles an unknown tool's JSON Schema into a typed message at runtime |

The Rust and Go sides share `proto/sandbox/v1/sandbox.proto`. The executor writes
its result as protojson and Go decodes it into the generated type, so a
mismatched field is a decode error rather than a quiet inconsistency. The
generated file is checked in; regenerate it after editing the proto:

```sh
protoc -I proto --go_out=go/gen --go_opt=paths=source_relative proto/sandbox/v1/sandbox.proto
```

Do not hand-edit `go/gen/sandbox/v1/sandbox.pb.go`. It carries a length-prefixed
copy of the schema, so a textual edit that changes a string's length corrupts it
in a way that panics at init rather than failing to compile.

### HTTP API

| Route | Purpose |
| --- | --- |
| `GET /api/health` | Sandbox, planner and workspace status, plus live run counts |
| `GET /api/harnesses` | Agent catalogue and what is installed |
| `POST /api/run` | Start a run; responds with the event stream as SSE |
| `GET /api/runs` | Previous runs on disk |
| `GET /api/runs/active` | What is running right now |
| `POST /api/runs/{id}/stop` | Cancel an active run; returns 202 while cleanup completes |
| `GET /api/workspace/{id}` | File listing |
| `GET /api/workspace/{id}/events` | A run's persisted transcript |
| `GET /api/workspace/{id}/raw/{path}` | One workspace file |
| `GET /api/workspace/{id}/archive` | The workspace as a zip |
| `GET`/`POST /api/workspace/{id}/publish` | Give a built site its own origin |

A local site link is served at `<run-id>.localhost:<port>`; this is not an internet
deployment. The site uses a separate origin, with `frame-ancestors` limited to
the dashboard.

## Verification

```sh
make test     # Go, frontend regression tests, Rust, clippy
make proof    # the sandbox's own escape attempts
```

Individually:

```sh
(cd go && go test ./...)
(cd go && go test -race ./internal/api ./internal/acp ./internal/runner ./internal/exec)
(cd web && npm test && npm run build)
cargo test --release --manifest-path rust/sandboxd/Cargo.toml   # 15 tests
cargo clippy --release --all-targets --manifest-path rust/sandboxd/Cargo.toml
./rust/sandboxd/confinement-proof.sh ./rust/sandboxd/target/release/sandboxd
```

`confinement-proof.sh` checks allowed workspace operations, denied filesystem and
network access, exit codes, deadlines, and descendant cleanup against the real binary.

## Security model

- **Enforced by the kernel.** Filesystem and network scope via Landlock;
  privilege escalation via `no_new_privs`; a synthetic `HOME` rather than your
  real one, because your real one holds SSH keys and cloud credentials.
- **Denied by default.** A tool that asks permission is refused unless
  **Allow tools** is enabled.
- **Network grants.** Model-backed agents receive network access to reach their
  providers. This is general network access, not a provider-domain allowlist;
  tools inherit it. Filesystem confinement still applies.
- **Not enforced.** The child's self-attestation is not unforgeable — a hostile
  child controls the pipe it writes to. Treat the evidence as a report from a
  process you have chosen to run, not a hardware attestation.
- **Not implemented.** Seccomp and copy-on-write are described in
  `Cargo.toml` but not built. There are no CPU, memory or I/O limits; the
  wall-clock ceiling is the control plane's, not the kernel's.
- **Local single-user service.** The server binds to loopback by default and
  rejects cross-origin mutations. It has no authentication or tenant separation;
  do not expose the API as a public service.
- **Known gap.** PID namespaces are unavailable to an unprivileged process on
  some hosts, so leftover children are reaped by matching an exact `AIOS_RUN_ID`
  environment marker rather than by tearing down a namespace.

`docs/THREAT_MODEL.md` describes what an attacker who has
compromised the agent process can still do.

## Project layout

```
go/                 control plane, ACP client, router, workspace store
rust/sandboxd/      the executor: Landlock, PTY, run reaping, proof script
web/                dashboard (Solid, Vite, Tailwind)
proto/sandbox/v1/   the contract between the two languages
docs/               threat model
dev.sh              build everything and run
```

## Limitations

- The router has no persistent store; tool outcomes live for the life of the
  process.
- Concurrency is one run per server by default. There is no queue — a rejected
  run is rejected, not deferred.
- Raw previews use an opaque sandboxed origin. Published local previews use
  their own localhost origin and allow same-origin access within that site.
- Landlock ABI v3 or newer is required, and v4 for network enforcement. Below
  that ai-os refuses to start rather than degrade.

## Licence

Not yet chosen. Add a `LICENSE` file before treating this as redistributable.
