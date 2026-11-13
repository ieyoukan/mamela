# Mamela

Mamela is an experimental declarative infrastructure controller powered by [Emela](https://github.com/emela-lang/emela).

It aims to reconcile machines and Linux node configuration safely through a Go control plane and pull-based agents. Mamela itself knows no particular infrastructure: machines come from providers, untrusted WASM programs distributed as Pomes — Proxmox VE through [mamela-proxmox](https://github.com/ieyoukan/mamela-proxmox), AWS-style APIs as in [`examples/ec2`](examples/ec2). Each provider writes its own safety checks (guards), explicit operations (delete, reboot, adopt) and setup scripts, and Mamela enforces them ([ADR 0006](docs/adr/0006-proxmox-out-of-core.md)).

Early development — no production-ready implementation yet. Evaluation, planning and approvals, the operation journal, isolated providers, and the pull-based mamelet are implemented. The development log records limited live infrastructure checks; these do not establish production readiness. See the [operator experience review](docs/reviews/2026-11-12-operator-experience.md) for current limitations.

## Try the current implementation

Requires Go 1.27 on macOS arm64 or Linux x86_64 (the platforms Emela publishes binaries for). The current examples require Mamela Pome v0.3.0, which is not published yet. Use the checkout workflow below; v0.2.0 has the older `Vm` API and is not a compatible substitute for these examples.

```sh
# From a Mamela checkout:
scripts/install-toolchain.sh
go run ./cmd/mamela eval --twice examples/homelab
```

For a separate project, use `emela new`, declare the Mamela and provider dependencies in `Pome.toml`, and use `[replace]` entries pointing at their local Pomes until compatible versions are published. The examples' `Pome.toml` files show the required dependencies. Evaluation alone does not create infrastructure.

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
- [Running from a repository of declarations (pull requests, approval by comment)](docs/gitops.md) / [ADR 0010](docs/adr/0010-gitops-from-the-control-plane.md)
