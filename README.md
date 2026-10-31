# harness++

Sandboxed OpenCode builds with explicit engineering plans, open-weight-first model
selection, Graphify retrieval, Ponytail, and per-stage usage reports. GPT is an
opt-in escalation path, not the default for every task.

## Routed build

Requirements: Linux with working Landlock, Go 1.27+, the built Rust executor,
OpenCode 2.0.32, Graphify CLI/MCP, Node/npm, and an Ollama daemon signed
in for cloud inference. No paid subscription is created by these commands.

```sh
git clone https://github.com/umar052001/harness-.git 'harness++'
cd 'harness++'
npm ci --ignore-scripts --prefix .tools
cargo build --release --manifest-path rust/sandboxd/Cargo.toml
mkdir -p .build
(cd go && go build -o ../.build/harness-build ./cmd/harness-build)
ollama serve                     # separate terminal, if not already running
```

Install Graphify through its [upstream instructions](https://github.com/Graphify-Labs/graphify)
if `graphify` and `graphify-mcp` are not available. The workflow checks for both;
it does not silently skip them. Sign in to Ollama through its CLI if needed.

Run the small fixture:

```sh
./.build/harness-build \
  -prompt examples/counter-prompt.txt \
  -source examples/counter \
  -check '["go","test","./..."]'
```

For your project, put its request in a text file and supply its acceptance command
as a JSON argument array. `-source` copies the project into a fresh build workspace;
the source is never edited. Omit it for a new project. `-allow-gpt` permits use of
the existing OpenCode ChatGPT OAuth account when the plan or fallback justifies it.
It does not imply OpenAI API credits or universal model entitlement.

Builds and reports live in `~/.local/share/harness++/builds/<build-id>/`:

- `plan.json`: Gemma's requirements, code findings, routing reasons, steps and checks.
- `plan-revision.json`: new failure evidence and the bounded fallback, if needed.
- `ledger.json`: model choices, execution outcomes, usage and plugin/tool evidence.
- `summary.json`: per-model token totals, known cost estimates and unknown costs.
- `project/`: the output; private agent configuration is under `.home/`.

Keep this directory private. It can contain prompts, source, generated output and
temporary OpenAI credential state when GPT was explicitly enabled. Never commit it.

## Execution

```text
raw request
  → sandboxed Graphify AST extraction and targeted query
  → sandboxed Gemma cloud planning + model selection (one call normally)
  → bounded OpenCode stages with Graphify MCP and Ponytail
  → sandboxed acceptance command
  → bounded fallback with failure evidence, or a terminal report
```

Model selection happens between stages. OpenCode supports explicit model choice
through ACP session configuration; this orchestrator does not assume a native
automatic cost router exists. A stage starts a fresh bounded OpenCode session and
shares project files plus the concise plan, not the previous session's full chat.
The independent acceptance command, not the agent's prose, decides the build result.

The registry is [`config/models.json`](config/models.json). It records exact IDs,
capabilities, task recommendations, priorities, fallback IDs, authentication methods,
verified limits, dated sources and prices. Unknown limits and prices are `null`.
Open-weight status is separate from hosted inference price; no free hosted tier is
assumed. Disabled entries are informational and cannot be selected by the router.

Ollama's installed `gemma4:cloud` alias maps to cloud `gemma4:31b`. Native Ollama
HTTP calls run **inside the sandbox**, through the signed-in loopback daemon; the
daemon holds the cloud credentials. OpenCode uses the same daemon through `/v1`.
Orchestration, planning requests, routing, retries and reports are implemented in
Go; Rust enforces the sandbox. Graphify's upstream Python runtime is an external
tool dependency, not a Python application layer.

The daemon must be trusted: sandboxed tools granted loopback access can consume
its authenticated account quota. Network access is general, not provider-only.

Ponytail 5.3.2 supplies engineering instructions and commands. It is not a model
router, test runner or independent billing authority. An audit hook observes actual
Ponytail system-prompt injection; completed Graphify MCP calls are checked in ACP
events. Merely listing the tools in a configuration is not considered success.

## Dashboard and application runtime

```sh
./dev.sh
```

The dashboard runs on **http://127.0.0.1:8091**, with a separate workspace store at
`~/.local/share/harness++/workspaces`. This keeps it apart from other checkouts.
Dashboard OpenCode Build uses the same routed workflow: Gemma planning, registered
model choices, bounded fallback, mandatory Graphify and pinned Ponytail injection.
Settings offers Automatic (Gemma-first), enabled model overrides, and an explicit
ChatGPT-subscription permission toggle. Big Pickle and the account-rejected
GPT-5.5 and operator-disallowed Astra are disabled. Automatic builds use Gemma for
planning and initial implementation, then only GPT-6.1 Sol for follow-up stages
and repairs. Sol gets at most two attempts per stage; quota errors are not retried.
Subscription quota and marginal cost remain unknown.
Plans, fallback reasons, session usage and combined per-model accounting survive
reload through the durable event log. Host-owned reports live under
`workspaces/.workflow`, outside writable project directories.

Backend builds require a valid runtime manifest and functional checks. The host
starts temporary confined services and managed PostgreSQL when declared, runs
the checks, and stops verification services. **Build & start app** starts the
app for use afterwards. Static builds currently validate the HTML entry only;
interactive correctness still needs browser testing.

The existing runtime supports persistent workspaces, background jobs, local preview
origins, rootless PostgreSQL, multi-service app builds and functional checks. See
[Environments](docs/ENVIRONMENTS.md) for the launch contract and limits. Generated
code, running services and passed checks are separate states. A local link is not
an internet deployment.

## Validation

```sh
(cd go && go test ./...)
(cd go && go test -race ./internal/runner ./internal/api ./internal/exec)
(cd web && npm test && npm run build)
```

Live model requests consume account quota. See [Workflow](docs/WORKFLOW.md) for
integration evidence, failure tests and reproducible commands. Usage is reported
by providers/agents, not inferred from text length. Native Ollama cache counts and
ACP categories have different semantics. Estimates use dated published rates;
overlapping ACP categories produce a range. ChatGPT subscription costs and missing
usage remain unknown. There is no claim of savings without a comparable baseline.

## Boundaries

- One sequential workflow, at most three planned stages and three attempts per stage (one Gemma plus up to two Sol attempts; a Sol-only stage gets at most two).
- No overall deadline by default; Stop/cancellation still works. Optional deadlines and bounded model retries remain available. Three repeated identical tool failures (or recognized Chromium startup failures) without a successful edit stop an ACP session; this is not a detector for every possible loop.
- Linux Landlock filesystem confinement and `no_new_privs`; no agent CPU/memory cap.
- Host-owned reports are outside the writable project. Tool output and code remain
  untrusted; plugin observations are operational evidence, not tamper-proof proof.
- No automatic deployment to a hosting provider, arbitrary Compose environment, or
  independent guarantee that generated tests cover every requirement.
- Tool binaries must be installed and compatible with OpenCode V2. Failures are
  surfaced rather than quietly replacing Graphify or disabling confinement.

No project license has been selected. Upstream tools retain their own licenses.

## Architecture reference

<img width="1144" height="624" alt="Architecture reference" src="https://github.com/user-attachments/assets/2685e6e0-dec8-4567-90b7-dddd47586db3" />
