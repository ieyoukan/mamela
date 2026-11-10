# Mamela

Mamela is an experimental declarative infrastructure controller powered by [Emela](https://github.com/emela-lang/emela).

It aims to reconcile Proxmox VMs and Linux node configuration safely through a Go control plane and pull-based agents.

Early design stage — no production-ready implementation yet. What works today is the evaluation path: an Emela declaration is compiled to WASM, its imports are checked, it runs in a sandbox, and its output is validated into a deterministic Resource Graph.

## Use it in your own project

Requires Go 1.27 on macOS arm64 or Linux x86_64 (the platforms Emela publishes binaries for).

```sh
go install github.com/ieyoukan/mamela/cmd/mamela@latest
mamela toolchain install         # the pinned emela and wasmtime (sha256-checked), into your user cache
export PATH="<the directory it printed>:$PATH"   # to run emela yourself

emela new infra && cd infra      # a new Pome
emela pome add github.com/ieyoukan/mamela/pome/mamela@v0.1.0
# write src/main.emel (see examples/lab-l2/src/main.emel), then:
mamela eval .                    # evaluate it into a Resource Graph (read-only)
```

`mamela toolchain` shows which toolchain and Pome cache Mamela uses. Mamela runs only the exact
emela and wasmtime of [`toolchain.lock`](toolchain.lock): another build could turn the same
declaration into a different graph.

## Develop

Requires Go (any version that can fetch the one in `go.mod`), Node.js (`.node-version`, for
`emela test`), git and tar (and xz on Linux).

```sh
scripts/install-toolchain.sh          # toolchain.lock and every Pome.lock into .tools/ (gitignored)
scripts/check.sh                      # every check that CI runs
go run ./cmd/mamela eval examples/homelab   # finds .tools by itself, from anywhere in the repository
```

## Documents

- [Architecture](docs/architecture.md) / [Development plan](docs/development.md) / [Lab](docs/lab.md)
- [Emela / WASM evaluation findings](docs/emela-wasm.md) / [`mamela-graph/v1`](docs/graph-schema.md) / [Reason codes](docs/reason-codes.md)
