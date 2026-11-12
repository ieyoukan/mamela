#!/usr/bin/env bash
# Runs every local check: Go formatting, vet and tests (with the pinned toolchain required),
# Emela formatting, type checks and tests, and the determinism check of the example.
# Nothing here touches the network or real infrastructure.
#
# Usage: scripts/check.sh   (run scripts/install-toolchain.sh first; needs Node.js, see mise.toml)
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
export PATH="$root/.tools/bin:$PATH"
export MAMELA_REQUIRE_TOOLCHAIN=1
export EMELA_POMEPROXY=off
export EMELA_POME_CACHE="$root/.tools/pomes"

step() { printf '\n== %s\n' "$*"; }

if [[ ! -x .tools/bin/emela || ! -x .tools/bin/wasmtime ]]; then
  echo "the toolchain is not installed: run scripts/install-toolchain.sh first" >&2; exit 1
fi
if ! command -v node >/dev/null 2>&1; then
  echo "Node.js is needed for \`emela test\` (version in mise.toml)" >&2; exit 1
fi

step "go: gofmt"
unformatted="$(gofmt -l . | grep -v '^\.tools/' || true)"
if [[ -n "$unformatted" ]]; then
  echo "not gofmt-ed:"; echo "$unformatted"; exit 1
fi

step "go: vet"
go vet ./...

step "emela: the dependency cache matches every Pome.lock"
git ls-files '*Pome.lock' ':!:internal/evaluate/testdata/**' | while read -r lock; do
  if grep -q '^\[\[pome\]\]' "$lock"; then
    (cd "$(dirname "$lock")" && emela pome verify >/dev/null) || {
      echo "$lock: the cache differs (run scripts/install-toolchain.sh)"; exit 1; }
  fi
done

step "go: test (race detector on)"
go test -race -count=1 ./...

step "emela: Mamela's Pomes (fmt, check, test on JS and WASM)"
for dir in pome/mamela pome/k3s; do
  (cd "$dir" && emela pome verify >/dev/null && emela fmt --check && emela check && emela test && emela test --target wasm)
done

step "emela: example and test Pomes (fmt)"
# Only the directories that are Pomes: examples/kv is a provider written in Go, with its Pome in
# examples/kv/pome.
for dir in examples/*/ examples/*/pome/ internal/evaluate/testdata/*/; do
  [[ -f "$dir/Pome.toml" ]] || continue
  emela fmt --check "$dir" >/dev/null || { echo "not formatted: $dir (run: emela fmt $dir)"; exit 1; }
done

step "emela: examples (check)"
for dir in examples/*/ examples/*/pome/; do
  [[ -f "$dir/Pome.toml" ]] || continue
  (cd "$dir" && emela check)
done

step "mamela: evaluate the example twice (determinism)"
go run ./cmd/mamela eval --twice examples/homelab

printf '\nall checks passed\n'
