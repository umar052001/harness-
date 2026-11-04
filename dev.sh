#!/usr/bin/env bash
#
# Build everything and start the control plane. One command, from a clean clone.
#
#   ./dev.sh            listen on 127.0.0.1:8091
#   ./dev.sh --addr 127.0.0.1:9000
#
# The three builds are ordered by cost: cargo is incremental, the frontend is a
# second or two, and Go is last so a frontend failure stops before the server
# binary is relinked. Pinned JavaScript tools are installed from the lockfile;
# system tools must already be installed.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$root"

for tool in cargo npm go; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "dev.sh: $tool is not installed or not on PATH" >&2
    exit 1
  fi
done

addr="127.0.0.1:8091"
if [[ "${1:-}" == "--addr" && -n "${2:-}" ]]; then
  addr="$2"
fi

echo "==> building sandboxd (rust)"
cargo build --release --manifest-path rust/sandboxd/Cargo.toml

echo "==> building frontend (vite)"
if [[ ! -d web/node_modules ]]; then
  (cd web && npm ci)
fi
(cd web && npm run build)

echo "==> building server (go)"
mkdir -p .build
(cd go && go build -o ../.build/harness-server ./cmd/server)
(cd go && go build -o ../.build/harness-build ./cmd/harness-build)
npm ci --ignore-scripts --prefix .tools

echo "==> harness++ on http://$addr"
exec ./.build/harness-server \
  -addr "$addr" \
  -web "$root/web/dist" \
  -sandboxd "$root/rust/sandboxd/target/release/sandboxd"
