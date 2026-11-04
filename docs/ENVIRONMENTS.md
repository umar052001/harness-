# Project databases

harness++ manages PostgreSQL through rootless Podman. The host chooses the image,
resource limits, loopback port, and volume; agents receive a database connection,
not a container-engine socket or arbitrary container-management access.

## Setup

Install Podman through your distribution's package manager. The user needs working
unprivileged user namespaces and subordinate UID/GID ranges. Verify:

```sh
podman info --format '{{.Host.Security.Rootless}}'
```

This must print `true`. harness++ refuses to start a database through a rootful runtime.
Podman is used locally with remote mode disabled. Existing Docker installations are
independent of this runtime.

## Use

1. Select a saved project in the dashboard.
2. Expand **Project database** and select **Start PostgreSQL**.
3. Choose **Continue project** and describe the backend or migration work.

The agent receives `DATABASE_URL` in its environment. Credentials are kept in a
mode-0600 file under the host-owned `.environments` directory beside workspaces;
the status API never returns them. An agent can read its own credentials and must
not write them into project files or logs.

Agents need a database client or application driver. When mise is present, the
runner resolves its selected tool paths once and removes configuration-dependent
shims from the sandbox PATH. System clients such as `/usr/bin/psql` remain usable
without granting access to the operator's mise configuration.

Database services outlive individual coding jobs and harness++ server restarts.
**Stop database** stops the container but retains its named volume. Service changes
are blocked while jobs are active, preventing a database stop during a migration.
Stopping a database does not restore an earlier data version: file checkpoints
and database backups are different operations. Branches of a project currently
share one database; use additive migrations and explicit data backups.

## Runtime policy

- Image: `docker.io/library/postgres:17-alpine`; existing containers retain their
  resolved image. Automatic upgrades are not performed.
- One named container and volume per project, checked against ownership labels.
- Loopback-only published port, randomly assigned by Podman.
- 512 MiB memory, one CPU, and 128 processes per database.
- PostgreSQL runs as its image's unprivileged user with capabilities dropped and
  `no-new-privileges` enabled.
- Startup waits for `pg_isready`; errors are shown instead of assuming readiness.

This is a local development database, not a production deployment service. Other
host processes can reach its port, but need the project credentials. Agent network
access is still governed by the runner's network grant, not by a per-domain firewall.
There is no database backup scheduler or data-volume deletion button.
Stop unused databases to release their compute resources.

## Local application servers

Static HTML previews do not execute backends. A backend application declares its
build, services, database, and functional checks in `ai-os.app.json` at the workspace
root. The same contract is used across frameworks:

```json
{
  "version": 1,
  "database": "postgres",
  "build": [{"cwd": "api", "command": ["go", "build", "-o", "server", "."]}],
  "services": [
    {"name": "api", "cwd": "api", "command": ["./server"], "route": "/api", "healthPath": "/api/health"},
    {"name": "web", "cwd": "web", "command": ["node", "server.js"], "route": "/", "healthPath": "/"}
  ],
  "checks": [{"command": ["python3", "tests/acceptance.py"]}]
}
```

**Build & start app** executes this sequence:

1. Provision or reattach the declared PostgreSQL database, if requested.
2. Run the build steps in order (including dependency installation or migrations
   explicitly declared by the project).
3. Launch every service through sandboxd and wait for HTTP 2xx at each health path.
4. Run the project's functional check commands. Nonzero exits fail startup and
   stop all services. Missing checks remain **not supplied**, never **passed**.
5. Publish the application through its separate local origin.

The dashboard reports **Code generated**, runtime state, and check outcomes
separately. Checks are supplied by the project; passing them does not prove they
cover every requirement. A process exit marks the runtime failed and stops peers.
Failure diagnostics retain a bounded output tail and redact the managed database
connection/password. Runtime status resets after control-plane restart.

Commands are argument arrays, executed without an implicit shell, using installed
system or managed toolchains, using the same resolved PATH and toolchain read grants
as coding jobs, or project-local executables. `cwd` must be an
existing workspace-relative directory. Services receive `HOST=127.0.0.1`, distinct
`PORT` values, and `NAME_URL` for direct service access. `APP_URL` points to a local
gateway with the same route table as the published origin, so acceptance tests
exercise frontend/API routing together. Prefixes are preserved: `/api/items`
reaches the API as `/api/items`. Longest matching route wins at path boundaries.

Service names are unique lowercase identifiers, starting with a letter and using
letters, digits or underscores (up to 32 characters). `database` is reserved.
The root service may be named `app`; its `APP_URL` still points to the gateway.

One project with up to four HTTP services runs at a time. There are at most eight
build steps and eight check steps, each bounded to two minutes; the startup request
is bounded to five minutes. Each readiness wait has a twenty-second limit. Service
lifetime ends on **Stop app**, a required service exit, or control-plane shutdown.
Stop joins cleanup; after restarting harness++ choose **Build & start app** again. Saved data
survives, so migrations and tests must be idempotent and preserve user records.

Legacy `{"entry":"relative/server.js"}` manifests remain supported as one Node
service with no functional checks. No user app is silently given passing checks.

Application processes have the same Landlock filesystem confinement and general
network grant as coding jobs. They are local previews, not production deployments
or resource-limited containers. The server should bind `HOST=127.0.0.1` as directed.
Coding jobs and application commands share resolver grants, including the target
of `/etc/resolv.conf` on systemd hosts, so sandboxed dependency installs can use DNS.
The application origin cannot route to control-plane API handlers. Stopped apps
return an explicit 503 instead of serving HTML as an API response.

PostgreSQL credentials are passed through `DATABASE_URL`. Declaring
`"database":"postgres"` provisions it before building; omitting that declaration
does not invent a connection. SQLite-backed applications keep their
database files in their workspace. Stop an application before continuing its project
so file checkpoints cannot copy a live database mid-write. PostgreSQL data remains
shared across attempts, while SQLite files are copied with the attempt.

Declare `"ai":"ollama"` for backend text generation. The host supplies
`OLLAMA_BASE_URL` and `OLLAMA_MODEL`; see [AI applications](AI-APPS.md).
Both the local preview proxy and verification gateway allow up to two minutes
for response headers, so text generation is not cut off after ten seconds.
Client cancellation still cancels upstream requests. Stream longer jobs or return
a job ID and let the browser poll; no request should wait indefinitely.

## Verification

```sh
cd go
go test -tags environment_live -run TestLiveDatabaseRetainsDataAcrossRestart -v ./internal/environment
go test -race -tags environment_live -run 'TestLiveMultiServiceVerification|TestLiveGoBuildAndReadiness|TestLiveStartupFailuresAreBounded|TestLiveServiceExitStopsPeers|TestLiveRuntimeDNS|TestAppProxyLifecycle|TestAppDeclaredPostgres' -v ./internal/environment ./internal/api
```

This opt-in test pulls the image if needed, checks startup and connection metadata,
writes data, stops and restarts the database, then verifies the data survived. It
removes only the container and volume it created for the test.

Runtime integration tests cover Node/Python services, a compiled Go server, build
failures/timeouts, unsuccessful readiness, failed functional checks, route isolation,
peer cleanup, persistent SQLite data, and managed PostgreSQL across control-plane
restart. Redis, non-HTTP worker supervision, arbitrary Compose deployments, hosted
deployment, and automatic model-driven repair are not implemented by this contract.
