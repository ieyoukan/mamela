# Agent guidance for Mamela

Mamela is an early-stage, safety-first infrastructure controller, not the upstream Emela compiler. See [docs/architecture.md](docs/architecture.md), [docs/development.md](docs/development.md), [docs/lab.md](docs/lab.md), and [docs/adr/0001-sqlite.md](docs/adr/0001-sqlite.md) before implementing. Design goals: deterministic, human-friendly, safe by default.

## Development rules

- Source code and technical identifiers in English; discussion can be in Japanese.
- Emela is used for Desired State generation only. NEVER make user-supplied Emela code directly execute Proxmox, SSH, apt, arbitrary shell, or other infrastructure-changing host functions.
- All real changes run through Go-validated resource schemas, ownership, operation intent, explicit policy decisions and a reconciler/agent adapter.
- Default-deny destructive changes. Deleting a declaration MUST NOT delete a real VM, disk, package or service.
- Treat WASM as a sandbox, NOT as proof that an infrastructure operation is safe.
- Emela v0.27 WASM output can only talk to the host via stdout (user `@export`/`@external` on WASM are compile errors). The Graph ABI is one `mamela-graph/v1` JSON document on stdout (`mamela-graph/v2` when it has resources of external providers), written with `x/json` v0.3.1 (no JSON in std yet); Go decodes it strictly with `encoding/json/v2`. Dependencies are built offline from `.tools/pomes` (filled by `scripts/install-toolchain.sh`), and the evaluator runs `emela pome verify` before every build because `emela build` does not check the cache against Pome.lock. Reject any `main.wasm` whose top-level imports go beyond `wasi:cli/{stdout,stderr,exit,types}@0.3.0` (`internal/component`).
- Evaluation and planning MUST be deterministic: no wall clock, randomness, environment or map iteration order in those paths. Time enters only through observation snapshots / fake clocks. Pin toolchain versions.
- User-facing output (plan, errors) MUST say what changes, why, and how to resolve it, with stable reason codes.
- The MVP is single Control Plane with SQLite. Do not silently introduce PostgreSQL, etcd or distributed SQLite.
- For external API operations, record intent first, re-observe after uncertain outcomes and avoid assuming exactly-once execution.
- Agent must verify node identity, revision digest/signature, validity and local safety constraints before applying.
- Test rejection cases and interrupted/retried operations, not only happy paths.
- Mamela's core knows no particular infrastructure (ADR 0006). Machines are providers' node kinds; safety checks (guards), explicit operations (delete, reboot, adopt) and root setup scripts belong to the provider's contract, enforced by Mamela. Do not add infrastructure-specific code (Proxmox, AWS) to the core.
- No live Proxmox, cloud or package-changing tests by default in CI. Require explicit opt-in (`MAMELA_LAB=1`).
- The lab Proxmox cluster runs unrelated production guests. Live Proxmox tests go only through the mamela-proxmox provider (https://github.com/ieyoukan/mamela-proxmox) and its guard: only touch guests inside the Mamela pool, VMID range and tag; everything else is `Foreign` and read-only. Run `mamela guard` before, and let `mamela apply` / `mamela act` run the guard before and after (docs/lab.md; the Proxmox procedure is in mamela-proxmox's docs/lab.md).
- Deleting, rebooting or adopting is never planned: only a provider's explicit operation through `mamela act --confirm <name>`.
- This repository is public: never commit lab hostnames, addresses, guest inventories or tokens. Site values live in gitignored `mamela.site.toml`, `mamela.providers.toml` (provider endpoints, token IDs and secret file paths) and `lab/local/`.
- Do not claim conceptual Emela DSL examples in docs are valid syntax or shipped APIs.

## Emela skills

The upstream Emela skills are mirrored, unmodified, under `.agents/skills/`:
- `emela-spec`: upstream language specification lookup, useful for language semantics. Its bundled command examples require an **Emela source checkout**, not the Mamela repository root.
- `emela-proposal`: proposing **changes to Emela itself**, not routine Mamela design work.
- `emela-implement`: implementing accepted **Emela compiler/specification changes**; NOT a generic Mamela implementation skill.

For normal Mamela development, use `mamela-dev` in `.agents/skills/mamela-dev/SKILL.md`. Consult upstream docs at https://github.com/emela-lang/emela/tree/main/spec and https://github.com/emela-lang/emela/blob/main/README.md before assuming Emela capabilities. The older `emela-lang/specification` repository describes the 0.1x language and is not the primary reference.

Claude-facing `.claude/skills/` entries are symlinks to canonical `.agents/skills/` entries.
