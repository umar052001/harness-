# harness++

Build and improve applications from plain-language requests. harness++ plans the
work, runs a coding agent in a restricted workspace, checks the result, and keeps
the source and build history on your machine.

It is a **local development tool for Linux**. The current version supports web apps
and backend services, with persistent project files and optional PostgreSQL. It is
not a hosting service or a guarantee that generated software is correct.

## What you can do

- Create a project, watch its progress, and inspect the plan and model usage.
- Preview static sites or build and run applications with multiple HTTP services.
- Continue a saved project in a new attempt without replacing its previous files.
- Run project checks, inspect failures, and let the build workflow attempt repairs.
- Download the source and build output.
- Build text-AI features using the existing Ollama cloud connection.

Common uses include dashboards, booking systems, internal tools, and writing apps.
Image generation needs a separate image provider. Native mobile builds, GPU jobs,
arbitrary containers, and production deployment are not managed runtimes today.

## Start locally

Install these prerequisites:

- Linux with working Landlock support, Go 1.27.1+, Rust/Cargo, and Node/npm.
- OpenCode 2.0.32 on `PATH`.
- Ollama with access to `gemma4:cloud`; sign in through Ollama for cloud inference.
- [Graphify](https://github.com/Graphify-Labs/graphify), including `graphify` and
  `graphify-mcp` on `PATH`. It supplies the code lookup used by the build workflow.
- Rootless Podman if your applications need managed PostgreSQL.

```sh
git clone https://github.com/umar052001/harness-.git 'harness++'
cd 'harness++'
ollama serve # in a separate terminal, if it is not already running
./dev.sh
```

Open **http://127.0.0.1:8091**. Enter a request and start a build. For a backend
project, choose **Build & start app** after generation to run it. Local app links
work on this machine; they are not internet deployments.

The server accepts loopback bind addresses only. It has no user authentication;
do not expose it through a public tunnel or reverse proxy.

### Model settings

Automatic builds use Gemma for planning and the initial implementation. Enable
the ChatGPT subscription option in Settings to allow GPT-6.1 Sol for follow-up work
and repairs. No other GPT model is enabled. Provider limits still apply.

Models are recorded in [`config/models.json`](config/models.json). A catalog entry
does not prove account access. Unknown usage, prices, and remaining quota stay
unknown in reports. Runtime AI calls made by your app are separate from build usage.

## How it works

```text
Dashboard or CLI
      │
      ▼
Go coordinator ─── plan, model choice, retries, saved history
      │
      ▼
Rust sandbox ───── code lookup → coding tools → build and checks
      │                         │
      │                         └── cloud model connection
      ▼
Saved project ──── local preview / app services / source download
                                     │
                                     └── optional project PostgreSQL
```

The dashboard uses Solid and Vite. Go owns jobs, model routing, storage, and
application lifecycles. Rust enforces filesystem access with Landlock and
`no_new_privs`. Graphify supplies code relationships; Ponytail supplies the coding
instructions. These tools are implementation details, not the product's output.

Backend projects describe commands, services, and checks in `ai-os.app.json`.
The older filename remains supported for saved projects. The host runs those
checks in temporary sandboxed services before marking generation complete.
Static projects currently receive only an HTML-entry check. Passing generated
checks does not prove that every requested feature works.

## Your files and data

Dashboard projects live in `~/.local/share/harness++/workspaces/`. Plans and usage
reports are kept separately under `.workflow/`; transcripts and metadata sit
beside each project. CLI builds live in `~/.local/share/harness++/builds/`.

Keep these directories private: they contain your prompts, code, and tool output.
Cloud models may receive project code. The sandbox restricts filesystem writes;
it is not a private cloud connection, a network firewall, or a CPU/memory limit.

**Continuing a project copies files, not its PostgreSQL database.** Attempts share
the project database. Back it up before schema changes; reverting source files
does not undo migrations. See [application runtimes](docs/ENVIRONMENTS.md).

## CLI

```sh
make workflow
./.build/harness-build \
  -prompt examples/counter-prompt.txt \
  -source examples/counter \
  -check '["go","test","./..."]'
```

Use a text file for your request. `-source` copies an existing project into a new
workspace; omit it for a new project. `-check` supplies the acceptance command as
a JSON argument array. `-allow-gpt` permits the existing subscription account.

## Development

```sh
npm ci --prefix web
make test
(cd go && go test -race ./...)
make server
make proof
```

`make test` runs Go tests and vet, frontend tests and build, integration-plugin
tests, and Rust tests and clippy. Live runtime and model tests are opt-in; see
[runtime tests](docs/ENVIRONMENTS.md#verification) and [workflow tests](docs/WORKFLOW.md).
Model tests consume provider quota.

## Current limits

- One build and one user-facing application at a time.
- Up to three planned stages, with bounded model attempts and cancellation.
- No overall build deadline by default. Individual tools and startup checks have
  deadlines; quiet model generation is not treated as a failed loop.
- Application services stop with the control plane; PostgreSQL data persists.
- No automatic database backups, app secret manager, image provider, deployment,
  or isolation suitable for untrusted tenants.

See the [product audit](docs/PRODUCT-AUDIT.md) for concrete release gaps and the
[documentation index](docs/INDEX.md) for technical details.

No project license has been selected. Upstream tools retain their own licenses.
