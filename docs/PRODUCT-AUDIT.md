# Product audit — 4 November 2026

**What this repo does:** harness++ turns requests into saved application projects,
then runs them locally and records their checks. Go manages the work; Rust limits
filesystem access; the browser shows progress and output.

**Load assumed:** one trusted operator on a Linux workstation, one build at a
time, one user-facing app. This review does not assume public hosting or untrusted
tenants. “Any app” is a product direction, not an accurate compatibility claim.

## Fixed in this pass

| Finding | Change | Evidence |
| --- | --- | --- |
| A response taking over ten seconds failed in both app proxies. | One shared proxy policy allows two minutes for headers and preserves cancellation. | `TestAppProxySlowResponseAndCancellation` sends an eleven-second response and cancels another request. |
| Healthy apps stopped after twenty-four hours. | Services now follow their lifetime context, Stop, or a required peer's exit. | Live multi-service startup, stop, and peer-exit tests. No twenty-four-hour soak was run. |
| Subscription GPT was enabled by default despite the opt-in wording. | New dashboard sessions start with it disabled. | Browser check of the unchecked permission; existing backend permission tests. |
| The unauthenticated executable could bind publicly and accepted foreign Host headers. | Loopback-only bind validation and a local-host HTTP guard. | Foreign hosts, deceptive suffixes, IPv4, IPv6, and app subdomains tested. This does not authenticate local callers. |
| Development-origin CORS preflight did not allow DELETE, blocking service stops. | DELETE and HEAD are included in the preflight response. | `TestCORSAllowsServiceStop`. |
| A published-host request panicked when workspace initialization failed. | Return 503 when the publication store is unavailable. | `TestUnavailableStorePublishedHost`. |
| A symlink named like an ordinary file could expose `.home` agent state. | Check resolved path segments as well as the requested path. | `TestWorkspaceAliasCannotExposeAgentState`. This is not protection against an agent copying its own credentials into another file. |
| Failed final metadata writes could still produce a successful terminal response. | Treat persistence failure as a failed outcome. | `TestFailedOutcomeWriteCannotReportSuccess`. |
| Prompt summaries could split a Unicode character. | Truncate by characters. | `TestPromptSummaryKeepsUnicode`. |
| Preview width could shrink inside flex layout; fullscreen preview did not trap keyboard focus. | Prevent preview shrinkage and use a native modal dialog, closed before removal. | Browser width, Escape, focus-return, and mobile checks. |
| Stop in the active-run banner targeted the selected historical project. | Pass the active run ID explicitly. | Browser test with mocked active-run and stop endpoints while a different history entry is selected. |
| No explicit contract described text AI in the finished application. | Optional `ai:ollama` supplies non-secret backend settings; planner instructions distinguish text from images. | Manifest validation and real sandbox runtime environment checks. Live inference was not exercised. |
| App execution could not find toolchains available during coding. | Moved PATH resolution and toolchain grants to the shared executor package; both callers use it. | Resolver regression and sandbox runtime tests. |
| README and settings led with integrations and historical demo details. | Product-first README, shorter settings text, removed both pitch decks and stale documentation links. | Link/reference review and frontend build. |

`make test` now also checks Go vet, the frontend production build, and the audit
plugin. A GitHub Actions workflow runs those checks and Go's race detector.
The workflow itself has not run on GitHub yet.

## Must fix before expanding the trust boundary

1. **Network-enabled apps can call the control plane**
   (`go/internal/environment/app.go`, `go/internal/api/local.go`, `rust/sandboxd/src/policy.rs`).
   - **What this is:** App and agent processes can reach loopback services, including
     harness++ and the signed-in Ollama daemon.
   - **Problem:** A hostile app can read the local API or consume model quota. Origin
     and Host checks protect browser requests, not direct requests from a process.
   - **Fix:** Separate the app network from the host and provide a narrow broker for
     allowed services. Authenticate control-plane requests independently of apps.
   - **If we skip it:** This remains a trusted local tool, not a service for strangers.

2. **A new attempt can change the previous attempt's database**
   (`go/internal/api/server.go`, `go/internal/api/workflow.go`, `go/internal/environment/postgres.go`).
   - **What this is:** Attempts share a project ID and its PostgreSQL connection.
     Verification runs generated build commands and checks against that database.
   - **Problem:** A destructive migration or poorly written test can delete existing
     data. The old source directory does not restore it.
   - **Fix:** Add a tested backup/restore path and verify migrations and checks on a
     separate database before applying them to the user's data.
   - **If we skip it:** Operators must back up data themselves and review migrations.

## Should fix for dependable everyday use

3. **Verification can pass without testing the requested behavior**
   (`go/internal/api/workflow.go`, `go/internal/environment/manifest.go`).
   - **What this is:** A static build passes when an HTML entry exists. Backend
     checks are commands supplied by the same agent that wrote the app.
   - **Problem:** An empty page or an always-successful check does not establish
     working navigation, saved data, or the requested feature.
   - **Fix:** Add host-owned browser scenarios for representative app types and
     check requirements independently of agent-authored tests.
   - **If we skip it:** “Completed” means the current checks passed, not that every
     feature works. The UI must keep that distinction.

4. **Long histories are loaded in full**
   (`go/internal/api/workspace.go:ReadEvents`, `go/internal/api/server.go:handleWorkspaceEvents`,
   `go/internal/workflow/workflow.go:agent`, `web/src/App.tsx`).
   - **What this is:** The host stores whole event arrays, and the browser reloads
     a whole transcript while observing a disconnected running job.
   - **Problem:** Unlimited-duration, verbose jobs can grow memory and repeatedly
     transfer large histories. Tool-output limits do not cap the event count.
   - **Fix:** Stream agent logs to disk and add sequence-based history paging; keep
     only the visible activity window in memory without deleting the saved record.
   - **If we skip it:** Large projects and long sessions can make the dashboard slow
     or exhaust memory. No large-history load benchmark was run in this pass.

5. **External AI capabilities are not interchangeable**
   (`go/internal/environment/contract.go`, `docs/AI-APPS.md`).
   - **What this is:** The new app contract supplies the existing Ollama text model.
     It does not manage image providers, app secrets, or per-app inference budgets.
   - **Problem:** Requesting a story and generated illustrations cannot produce real
     image-model output through the current text integration alone.
   - **Fix:** Select and verify an image provider, add scoped secret delivery and
     storage, then test a full illustrated-story flow and failure handling.
   - **If we skip it:** Text generation and uploaded illustrations are possible;
     image generation must be reported as unconfigured.

## Checks and coverage

Read the build entry points, workflow routing and credential handoff, API job and
storage paths, preview/proxy routing, manifest and PostgreSQL lifecycle, ACP
transport, Go sandbox streaming, Rust confinement policy, and main dashboard
components. Reviewed the associated tests, dependencies, build script, and runtime
and threat-model documentation. This was risk-based, not a line-by-line inspection
of every file.

Executed locally:

- Go tests and vet; all-package race tests.
- Sandboxed multi-service Node/Python execution, compiled Go startup, failed
  builds and readiness, peer teardown, and app proxy lifecycle tests.
- Eight frontend tests, TypeScript/Vite production build, and plugin test.
- Sixteen Rust tests, clippy with warnings denied, and 71 confinement checks.
- Desktop/mobile browser checks against an isolated workspace store.

Frontend review assumes desktop/localhost, with a 390px mobile fallback and no SEO
requirement. Targets: LCP ≤2,500ms, INP ≤200ms, CLS ≤0.1 at p75; initial JavaScript
≤200KB gzip; Lighthouse accessibility ≥95 and performance ≥90. These are targets,
not measured field results. Accessibility is best-effort toward WCAG AA, owned by
the project maintainer. The current built JavaScript is about 26KB gzip.

**Verdict:** A stronger local alpha. Public hosting, destructive-data safety, and
independent feature verification still need work before a broad production claim.

**Not checked:** Live cloud inference or account entitlement, image generation,
new PostgreSQL backup/restore behavior, multi-user load, long-duration soak,
screen-reader behavior, Lighthouse scores, dependency vulnerability databases,
and the new workflow on GitHub. Historical live-model evidence is not a new test.
