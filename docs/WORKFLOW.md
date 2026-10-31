# OpenCode workflow

## Responsibilities

- `go/cmd/harness-build`: CLI and internal sandbox workers.
- `go/internal/workflow`: registry validation, Graphify extraction/query, Gemma
  planning, sequential dispatch, acceptance checks, fallback and cost reports.
- `go/internal/runner`: the existing OpenCode ACP lifecycle, model configuration,
  permission handling and cleanup.
- `rust/sandboxd`: kernel confinement and process supervision.
- `integrations/ponytail`: directory entrypoint for the pinned upstream plugin.
- `integrations/audit`: observes Ponytail injection and Graphify tool calls; blocks
  edits/shell before Graphify retrieval.

No Python orchestration is shipped. Graphify 0.9.67 is an installed external tool
whose Python runtime and libraries receive narrow read/execute grants. AST-only
extraction does not make LLM calls. MCP retrieval and agent tool calls remain
inside the sandbox.

## Inspection findings — 2026-10-31

OpenCode is 2.0.32. The operator's global configuration includes Ponytail 5.3.2
and a Graphify MCP pointing at another repository. The new workflow does **not**
inherit that repository or the operator's personal skill directories: it writes
a private configuration and points Graphify at the current build workspace.

The old optional narrator/planner uses `OLLAMA_API_KEY`; none was set in the
inspected environment. The installed Ollama daemon already has cloud sign-in.
Its `gemma4:cloud` alias maps to `gemma4:31b` on `https://ollama.com`. Metadata
reports completion/tools/thinking/vision and 262,144 context tokens. Both native
`/api/chat` and OpenAI-compatible `/v1/chat/completions` answered real requests.
Planning uses the native endpoint from a sandboxed Go worker. OpenCode uses `/v1`.

Kimi is installed and catalog-listed, but a real request returned `provider.quota`:
the model is not included in this account's free usage. It is disabled in the
registry. The same error explicitly identified Gemma as free-plan eligible; its
numeric quota and the account's actual billing plan are unknown. Public published
rates are recorded as rate equivalents, not proof of a charge to this account.

OpenAI authentication is ChatGPT OAuth. The authenticated catalog lists
`gpt-6.1-terra`, `gpt-6.1-sol`, `gpt-6-luna`, `gpt-6-astra`, `gpt-5.5`, and
`gpt-5.3-codex-spark`. Catalog visibility alone does not verify every entitlement.
Only the configured escalation target is used by this workflow; API pricing is
not applied to ChatGPT subscription usage.

## Model selection and boundaries

OpenCode supports explicit model selection through ACP session config options and
its public HTTP session model endpoint. No native automatic cost router was
verified. The Go dispatcher chooses between stages and starts fresh sessions.
Project files plus the compact plan carry state; full conversation histories are
not duplicated between models.

Normally one Gemma call supplies both the plan and stage routes. One corrective
call is permitted for invalid plan JSON/schema. Transport failures do not trigger
repeated planning calls. A host registry allowlist validates every selection and
enforces the GPT permission and escalation justification. A failed stage or
acceptance check permits one configured fallback, never an unbounded retry loop.
The revised plan stores the failure and next model. Maximum stages/attempts and
the total deadline are checked by the host, not delegated to a model.

Ponytail injects instructions; it does not independently enforce quality, select
models or meter billing. Configuration alone is not accepted as integration:
the workflow checks system-prompt injection and completed Graphify MCP retrieval
in the actual agent turn. These observations are not tamper-proof attestations.

## Credentials

Ollama sign-in stays in the trusted host daemon. No Ollama key is copied into a
project. Sandboxed code can nevertheless consume the daemon account's quota via
loopback, because this is a trusted local single-user workflow, not tenant isolation.

For GPT, the host captures the supported OpenCode `/api/credential` response in
memory, selects only the active OpenAI OAuth credential, and passes it through a
mode-0600 file to a sandboxed Go worker. The worker starts an authenticated private
OpenCode server, uses the credential-create API, and joins the server. Tokens are
never put in command arguments, reports, configuration or Git. A short-lived random
server password goes only into that child's environment and HTTP Basic auth.
Credential cleanup uses the private OpenCode API after the stage. The operator's
original credential database is never opened or modified by this code.

Do not share private build directories. Interrupted cleanup can leave private
OpenCode state containing credential material. Refreshing a copied OAuth session
may require reconnecting the original account if the provider rotates credentials.

## Usage

`ledger.json` records each planning/routing or implementation turn with its model,
reason, escalation, execution result and reported usage. Commands and ACP events
are saved separately outside the writable project. `summary.json` groups tasks
and reported input/output/cache counts by model and gives overall reported totals.
Missing counts are `null`; incomplete/failed calls increase unknown counters.

Native Ollama prompt accounting reports cached input separately within the prompt
count. ACP category semantics can overlap, so its dollar estimate is a range,
not an exact sum or invoice. All rates are dated in the registry. Subscription
costs and unavailable provider counters remain unknown. Deterministic Graphify
and acceptance commands do not consume LLM tokens. There is no baseline-backed
cost-savings claim yet.

## Running

```sh
make workflow
./.build/harness-build -prompt examples/counter-prompt.txt \
  -source examples/counter -check '["go","test","./..."]' -allow-gpt
```

The normal smoke build completed using Gemma for planning/routing and implementation,
with real Graphify MCP use, Ponytail injection and passing Go acceptance tests.
The latest report contained 11,925 input, 1,270 output and 87,392 cache-read tokens
across two model turns. The published-rate equivalent was approximately USD 0.0022–0.0144;
actual free/subscription consumption was not reconciled. Cache categories must not
be added to those totals as though they were disjoint.

Offline checks:

```sh
cd go
go test -race ./internal/workflow ./internal/runner ./internal/api ./internal/exec
go vet ./...
```

Opt-in fallback checks consume real account quota:

```sh
cd go
HARNESS_WORKFLOW_LIVE=1 go test -tags workflow_live -run TestLiveFallback -v ./internal/workflow
```

They inject an HTTP-rejected model request and an inadequate implementation, then
require the real fallback to pass the normal acceptance command. Each test retains
its usage report under `~/.local/share/harness++/validations/`.

Live fallback verification passed on 2026-10-31: an HTTP-rejected Gemma request
escalated to GPT-6.1 Sol and passed the fixture tests. The rejected turn returned
no usage counters (unknown, not zero); the GPT turn reported 10,716 input and
527 output tokens plus 59,904 cache-read tokens, with unknown subscription cost.
Quality-failure verification also passed with Gemma followed by GPT; the live test
deliberately broke the generated implementation and the unchanged acceptance tests
rejected it before fallback repaired it. No assertion was weakened to get a pass.

## Sources

- [OpenCode V2 configuration](https://opencode.ai/v2/docs/config)
- [Provider accounts and Ollama discovery](https://opencode.ai/v2/docs/providers)
- [Public API/OpenAPI](https://opencode.ai/v2/openapi.json)
- [V2 plugin API](https://opencode.ai/v2/docs/build/plugins)
- [OpenCode 2.0.32 server authentication](https://github.com/anomalyco/opencode/blob/v2.0.32/packages/cli/src/server-process.ts)
- [Ollama Cloud and alias naming](https://docs.ollama.com/cloud)
- [Ollama published prices](https://ollama.com/pricing)
- [Gemma metadata](https://ollama.com/library/gemma4)
- [Ponytail 5.3.2 installation](https://github.com/DietrichGebert/ponytail/blob/v5.3.2/INSTALL.md)
- [Graphify upstream](https://github.com/Graphify-Labs/graphify), installed CLI help
