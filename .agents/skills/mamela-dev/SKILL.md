---
name: mamela-dev
description: Implement or design Mamela's safe IaC controller, Resource Graph, SQLite store, Proxmox provider, Emela/WASM integration or pull-based Go agent. Use for Mamela architecture, implementation, testing and reviews.
license: Apache-2.0
compatibility: Requires Go and optionally Emela CLI, Node.js and Wasmtime for Emela/WASM experiments. GitHub repo: ieyoukan/mamela.
metadata:
  repository: github.com/ieyoukan/mamela
  version: "1"
---

# Mamela development skill

Mamela is a declarative, continuously reconciled IaC control plane, with Go Control Plane, SQLite, Proxmox, and a pull-based Go Agent. The Emela side compiles the **desired state**; it is not allowed to perform changes directly.

## Before working

1. Read `docs/architecture.md`, `docs/development.md`, `docs/lab.md`, and `docs/adr/0001-sqlite.md`.
2. Identify whether the task is DSL evaluation, graph validation, state persistence, reconciliation, provider, agent, or CLI; keep these layers separate.
3. If Emela syntax / effects / WASM output matter, inspect [upstream README](https://github.com/emela-lang/emela) and `spec/` of the current Emela repository. The mirrored `emela-spec` skill refers to upstream checkout paths and tools.
4. Check real compiler behavior with `emela check` / `emela run` where possible; do not invent Emela standard-library APIs or output ABIs.

## Implementation workflow

1. Define behavior with a resource example, expected observation, safety decision, failure/retry scenario and a test.
2. Add typed resource/graph validation before connecting to the provider or agent.
3. Make the planner produce explicit, reviewable operations with resource identity, generation, preconditions and plan digest.
4. Use a deterministic policy that returns **Auto / ApprovalRequired / Denied**. Unknown actions are Denied.
5. Persist operation intent to SQLite before side effects. Implement re-observation and idempotent retry for uncertain outcomes.
6. Implement provider/agent behavior behind small Go interfaces so tests use in-memory fake adapters.
7. Test rejection, simultaneous updates, interrupted operations and stale observations before allowing write operations in a live environment.

## Safety invariants

- Emela/WASM runs without arbitrary network/host filesystem/Proxmox credentials; `main.wasm` imports are checked against an allowlist (stdout, stderr, exit) before running.
- Evaluation and planning are deterministic: same inputs produce the same graph digest and plan digest.
- Guests outside the Mamela pool / VMID range / tag are `Foreign`: observe only, never change.
- Source removal is **not deletion authorization**.
- A package install is not automatically safe; maintainer scripts can be privileged.
- mTLS identity alone is insufficient: Agent also validates node binding, revision signature, expiry and local policy.
- A `Ready` status means observed readiness, not merely matching `observedGeneration`.
- Observed State is time-stamped and can be stale.
- Never record secrets, long-lived bootstrap tokens or credentials in resource graph, logs or test fixtures.
- No automatic destructive actions or arbitrary shell execution.
- Production changes require separate authorization and explicit opt-in.
- When safety cannot be established, report a Blocked/ApprovalRequired outcome instead of guessing.

## Validation

- Go: `go test ./...` (when Go code exists).
- Emela: `emela fmt --check`, `emela check`, `emela test` in the Pome root, and `emela build --target wasm` only after checking runtime/tool prerequisites.
- The Graph ABI is one `mamela-graph/v1` JSON document on stdout (Emela v0.27 has no user WASM exports; JSON comes from the experimental, pinned Pome `github.com/emela-lang/x/json`). Decode strictly: reject unknown fields, duplicate keys, trailing data and floats. Verify behavior with the pinned emela / Wasmtime 49+ before relying on it.
- Live Proxmox tests only with `MAMELA_LAB=1` and passing preflight/postflight (docs/lab.md).
- Document unsupported behavior as a limitation rather than asserting it is implemented.
