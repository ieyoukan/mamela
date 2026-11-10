#!/usr/bin/env bash
# Runs every local check: Go formatting, vet and tests (with the pinned toolchain required),
# Emela formatting, type checks and tests, and the determinism check of the example.
# Nothing here touches the network or real infrastructure.
#
# Usage: scripts/check.sh   (run scripts/install-toolchain.sh first)
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
export PATH="$root/.tools/bin:$PATH"
export MAMELA_REQUIRE_TOOLCHAIN=1
export EMELA_POMEPROXY=off
export EMELA_POME_CACHE="$root/.tools/pomes"

step() { printf '\n== %s\n' "$*"; }

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

step "emela: Mamela Pome (fmt, check, test on JS and WASM)"
(cd pome/mamela && emela fmt --check && emela check && emela test && emela test --target wasm)

step "emela: example and test Pomes (fmt)"
for dir in examples/*/ internal/evaluate/testdata/*/; do
  emela fmt --check "$dir" >/dev/null || { echo "not formatted: $dir (run: emela fmt $dir)"; exit 1; }
done

step "emela: examples (check)"
for dir in examples/*/; do
  (cd "$dir" && emela check)
done

step "mamela: evaluate the example twice (determinism)"
go run ./cmd/mamela eval --twice examples/homelab

printf '\nall checks passed\n'
