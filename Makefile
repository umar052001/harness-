# Convenience wrapper around the three builds. `make run` is the one-liner;
# everything else exists so CI and local checks use the same commands.
.PHONY: build frontend server run test proof clean

build: server
	cd web && npm run build

frontend:
	cd web && npm run build

server:
	cargo build --release --manifest-path rust/sandboxd/Cargo.toml
	mkdir -p .build
	cd go && go build -o ../.build/ai-os-server ./cmd/server

run: server
	cd web && npm run build
	./.build/ai-os-server -addr 127.0.0.1:8090 -web "$(CURDIR)/web/dist" \
		-sandboxd "$(CURDIR)/rust/sandboxd/target/release/sandboxd"

test:
	cd go && go test ./...
	cargo test --release --manifest-path rust/sandboxd/Cargo.toml
	cargo clippy --release --all-targets --manifest-path rust/sandboxd/Cargo.toml -- -D warnings

proof:
	./rust/sandboxd/confinement-proof.sh ./rust/sandboxd/target/release/sandboxd

clean:
	rm -rf .build web/dist
	cargo clean --manifest-path rust/sandboxd/Cargo.toml
