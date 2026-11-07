# Agent guidance for Mamela

Mamela is an early-stage, safety-first infrastructure controller, not the upstream Emela compiler. See [docs/architecture.md](docs/architecture.md), [docs/development.md](docs/development.md), and [docs/adr/0001-sqlite.md](docs/adr/0001-sqlite.md) before implementing.

## Development rules

- Source code and technical identifiers in English; discussion can be in Japanese.
- Emela is used for Desired State generation only. NEVER make user-supplied Emela code directly execute Proxmox, SSH, apt, arbitrary shell, or other infrastructure-changing host functions.
- All real changes run through Go-validated resource schemas, ownership, operation intent, explicit policy decisions and a reconciler/agent adapter.
- Default-deny destructive changes. Deleting a declaration MUST NOT delete a real VM, disk, package or service.
- Treat WASM as a sandbox, NOT as proof that an infrastructure operation is safe.
- The MVP is single Control Plane with SQLite. Do not silently introduce PostgreSQL, etcd or distributed SQLite.
- For external API operations, record intent first, re-observe after uncertain outcomes and avoid assuming exactly-once execution.
- Agent must verify node identity, revision digest/signature, validity and local safety constraints before applying.
- Test rejection cases and interrupted/retried operations, not only happy paths.
- No live Proxmox or package-changing tests by default in CI. Require explicit opt-in.
- Do not claim conceptual Emela DSL examples in docs are valid syntax or shipped APIs.

## Emela skills

The upstream Emela skills are mirrored, unmodified, under `.agents/skills/`:
- `emela-spec`: upstream language specification lookup, useful for language semantics. Its bundled command examples require an **Emela source checkout**, not the Mamela repository root.
- `emela-proposal`: proposing **changes to Emela itself**, not routine Mamela design work.
- `emela-implement`: implementing accepted **Emela compiler/specification changes**; NOT a generic Mamela implementation skill.

For normal Mamela development, use `mamela-dev` in `.agents/skills/mamela-dev/SKILL.md`. Consult upstream docs at https://github.com/emela-lang/emela/tree/main/spec and https://github.com/emela-lang/emela/blob/main/README.md before assuming Emela capabilities. The older `emela-lang/specification` repository describes the 0.1x language and is not the primary reference.

Claude-facing `.claude/skills/` entries are symlinks to canonical `.agents/skills/` entries.
