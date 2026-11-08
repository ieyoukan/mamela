# Mamela

Mamela is an experimental declarative infrastructure controller powered by [Emela](https://github.com/emela-lang/emela).

It aims to reconcile Proxmox VMs and Linux node configuration safely through a Go control plane and pull-based agents.

Early design stage — no production-ready implementation yet. What works today is the evaluation path: an Emela declaration is compiled to WASM, its imports are checked, it runs in a sandbox, and its output is validated into a deterministic Resource Graph.

## Try it

Requires Go 1.27 and Node.js (for `emela test`), on macOS arm64 or Linux x86_64.

```sh
scripts/install-toolchain.sh                      # pinned emela + wasmtime into .tools/ (sha256-verified)
go run ./cmd/mamela eval --twice examples/homelab # evaluate twice; the digests must match
scripts/check.sh                                  # every local check
```

## Documents

- [Architecture](docs/architecture.md) / [Development plan](docs/development.md) / [Lab](docs/lab.md)
- [Emela / WASM evaluation findings](docs/emela-wasm.md) / [`mamela-graph/v1`](docs/graph-schema.md) / [Reason codes](docs/reason-codes.md)
