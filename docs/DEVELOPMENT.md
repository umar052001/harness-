# Project execution roadmap

## Architecture

- The HTTP layer owns requests, persistent records, and event delivery.
- The runner owns one bounded agent invocation and its cleanup.
- ACP translates agent messages; it does not choose project policy.
- The Rust executor owns OS-level confinement and process supervision.
- The browser displays state; it is not the authority on job completion.

Keep these boundaries when adding project workflows. Use explicit state transitions,
typed records, and narrow interfaces where implementations actually vary.

## 1. Usage and evaluation

Capture agent-reported token counts, context snapshots, and estimated cost. Preserve
unknown values. Store accounting with run metadata and display it in live and saved
runs. The benchmark command runs versioned tasks with host-owned acceptance checks.

The current OpenCode adapter reads the optional token object on the ACP v1 prompt
response. This is an OpenCode-supported extension, not a stable cross-agent token
contract. Context and cumulative session cost use standard `usage_update` messages.
Each run currently creates one fresh session, so its session cost belongs to that run.
Changing this assumption requires baseline subtraction when sessions are reused.

Cost is agent-reported, not independently reconciled billing. Interrupted runs may
lack final totals. Do not use incomplete reports to claim a hard spending cap or
treat missing usage as free.

## 2. Persistent projects and jobs

The first increment uses the existing host-owned JSON metadata and transcript
store, with one concurrent job by default. Jobs survive browser disconnects and
are cancelled and joined at server shutdown. Saved attempts can be continued by
copying project files into a new workspace; `projectId` groups the lineage and
`parentRunId` records the checkpoint. Prior versions are not overwritten.

This is file-based continuation, not restoration of an agent process, conversation,
or running database. A queue, restart recovery beyond interrupted-state reporting,
and managed service lifecycles remain subsequent work. Introduce SQLite when the
queue needs transactional claims; a second store is not needed for the current
single-worker, atomic-sidecar design.

Separate project workspaces from attempts. Introduce a durable job store and one
worker before adding parallel workers. A browser connection only observes jobs;
explicit cancellation and server shutdown control execution.

Persist job state before dispatch. On restart, mark in-flight jobs interrupted and
reconcile owned processes. Resume from saved files and a concise handoff; do not
silently repeat migrations or other side effects. Preserve prior attempts and
accounting. Tests must cover disconnect, cancellation, restart, and duplicate start.

## 3. One managed full-stack environment

The database increment is implemented in `internal/environment`: rootless Podman
owns one PostgreSQL container and retained volume per project. The dashboard can
start and stop it, and continuation jobs receive its scoped connection through
`DATABASE_URL`. The versioned app contract now supports build steps, up to four HTTP
services with isolated-origin routing, declared PostgreSQL provisioning, readiness,
and project-supplied functional checks through sandboxd. Live tests exercise Node,
Python, Go, routing and database persistence. See [Environments](ENVIRONMENTS.md).
Reusable full-stack templates, independent acceptance checks, and bounded automatic
repair remain pending; generated checks are not independent proof of completeness.

Start with Go, PostgreSQL, and Solid. A host-owned manager provisions scoped service
credentials, ports, volumes, and health checks. Use rootless containers where
supported; never give agent code the host Docker socket. Keep project data, preview
services, and coding jobs on separate lifecycles.

Acceptance: build a database-backed app, run migrations and integration tests,
restart ai-os, and add a feature without losing data or rebuilding the project.

## 4. Capability packs and verification

Ship versioned templates, short task-specific instructions, and executable checks.
Activate only the tools and instructions a task needs. Product packs belong in the
repository; personal agent configuration and credentials do not.

Start with static sites, then the managed full-stack template, then one AI feature
adapter. AI integrations need deterministic fixtures and a separate opt-in live
check. Track generated-application spend separately from coding spend.

Agent completion means verification can begin. Builds, migration checks, API tests,
and browser smoke tests determine the project outcome. Bound repair attempts and
retain diagnostics instead of looping indefinitely.

## 5. Model policy

After collecting benchmark evidence, add Economy, Balanced, and Quality policies
with manual overrides. Route per job, not per token. Use deterministic tooling for
scaffolding and verification. Escalate models for reasoning failures; diagnose
environment failures separately. Show the recommendation and reason.

Budgets must account for delayed reporting and in-flight requests. A precise hard
cap requires a metered execution boundary, not just a UI estimate.

## 6. Selective parallelism

Only parallelize independent work after checkpoints, accounting, and verification
are reliable. Bound workers and prevent concurrent writes to the same project.
Compare wall-clock time and cost per verified result against the single-agent
baseline before enabling parallel execution by default.
