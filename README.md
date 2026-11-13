# Mamela

Mamela is an experimental declarative infrastructure controller powered by [Emela](https://github.com/emela-lang/emela).

It aims to reconcile machines and Linux node configuration safely through a Go control plane and pull-based agents. Mamela itself knows no particular infrastructure: machines come from providers, untrusted WASM programs distributed as Pomes — Proxmox VE through [mamela-proxmox](https://github.com/ieyoukan/mamela-proxmox), AWS-style APIs as in [`examples/ec2`](examples/ec2). Each provider writes its own safety checks (guards), explicit operations (delete, reboot, adopt) and setup scripts, and Mamela enforces them ([ADR 0006](docs/adr/0006-proxmox-out-of-core.md)).

Early design stage — no production-ready implementation yet. What works today is the evaluation path: an Emela declaration is compiled to WASM, its imports are checked, it runs in a sandbox, and its output is validated into a deterministic Resource Graph.

## Use it in your own project

Requires Go 1.27 on macOS arm64 or Linux x86_64 (the platforms Emela publishes binaries for).

```sh
go install github.com/ieyoukan/mamela/cmd/mamela@latest
mamela toolchain install         # the pinned emela and wasmtime (sha256-checked), into your user cache
export PATH="<the directory it printed>:$PATH"   # to run emela yourself

emela new infra && cd infra      # a new Pome
emela pome add github.com/ieyoukan/mamela/pome/mamela@v0.3.0   # not published yet: until then, v0.2.0 (with Vm)
# add a provider's Pome, write src/main.emel (see examples/homelab/src/main.emel), then:
mamela eval .                    # evaluate it into a Resource Graph (read-only)
```

If you use mise, `mise use go@1.27.3 github:emela-lang/emela@0.27.0 wasmtime@50.0.1` gives you the
same emela and wasmtime, and Mamela uses them from PATH. `mamela toolchain` shows which toolchain
and Pome cache Mamela uses. Mamela runs only the exact
emela and wasmtime of [`toolchain.lock`](toolchain.lock): another build could turn the same
declaration into a different graph.

## Develop

With [mise](https://mise.jdx.dev/):

```sh
mise trust && mise install      # Go, Node.js, emela, wasmtime (mise.lock), then .tools/ and the Pome dependencies
mise run check                  # every check that CI runs
go run ./cmd/mamela eval examples/homelab   # finds the toolchain by itself, from anywhere in the repository
```

Without mise: install Go (any version that can fetch the one in `go.mod`), Node.js (the version in
`mise.toml`, for `emela test`), git and tar (and xz on Linux), then run `scripts/install-toolchain.sh`
and `scripts/check.sh`.

## Documents

- [Architecture](docs/architecture.md) / [Development plan](docs/development.md) / [Lab](docs/lab.md)
- [Emela / WASM evaluation findings](docs/emela-wasm.md) / [`mamela-graph/v1`](docs/graph-schema.md) / [Reason codes](docs/reason-codes.md)
- [Writing a provider](docs/provider-authoring.md) (`mamela provider new` writes one that passes its tests) / [ADR 0002: the provider contract](docs/adr/0002-provider-contract.md) / [ADR 0006: Proxmox out of the core](docs/adr/0006-proxmox-out-of-core.md) / [ADR 0009: provider kit, generated Pomes, moving from Terraform](docs/adr/0009-provider-kit-and-imports.md)
