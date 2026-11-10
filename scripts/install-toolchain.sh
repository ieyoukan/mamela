#!/usr/bin/env bash
# Sets up this repository for development: installs the pinned Emela and Wasmtime (toolchain.lock)
# into ./.tools/bin, and fetches the Emela dependencies of this repository's Pomes (Pome.lock)
# into ./.tools/pomes. Both are gitignored; nothing is installed system-wide.
# Every download is verified: the tools against the sha256 of toolchain.lock (archive and
# executable), the Pomes against the sumdb and the hashes in each Pome.lock.
#
# Usage: scripts/install-toolchain.sh   (`mise install` runs it after installing the tools of mise.toml)
# Afterwards, `go run ./cmd/mamela ...` anywhere in the repository finds .tools by itself.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
tools="$root/.tools"
bin="$tools/bin"
cd "$root"

# Prerequisites, all checked before anything is downloaded.
missing=0
need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing: $1 ($2)" >&2
    missing=1
  fi
}
need go "Go $(sed -n 's/^toolchain go//p' go.mod) or any Go >= $(sed -n 's/^go //p' go.mod), which fetches it: https://go.dev/dl/"
need git "https://git-scm.com/"
need tar "to unpack the toolchain"
if [[ "$(uname -s)" == Linux ]]; then
  need xz "tar needs it for the .tar.xz of wasmtime: install xz-utils"
fi
if ! command -v node >/dev/null 2>&1; then
  echo "note: Node.js (version in mise.toml) is not installed; scripts/check.sh needs it for \`emela test\`" >&2
fi
if [[ "$missing" != 0 ]]; then
  exit 1
fi

# The pinned emela and wasmtime (the same installer as `mamela toolchain install`). Copies of the
# pinned ones on PATH (installed by mise) are taken instead of downloading them again.
go run ./cmd/mamela toolchain install --dir "$bin" --downloads "$tools/downloads" | grep -E '^(installed|downloading) |already installed'

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# Fetch the dependencies of every Pome whose lock records some. This is the only step that needs
# the network for them; evaluation and scripts/check.sh then build offline from this cache.
# Always through the proxy and checked against the sumdb, whatever the caller's environment says.
unset EMELA_POMEPROXY EMELA_POMESUMDB EMELA_POMEPRIVATE
export EMELA_POME_CACHE="$tools/pomes"
locked_pomes() {
  # internal/evaluate/testdata holds deliberately broken Pomes (one names a dependency that must
  # stay unfetched); the dependencies they need otherwise are those of pome/mamela.
  git -C "$root" ls-files '*Pome.lock' ':!:internal/evaluate/testdata/**' | while read -r lock; do
    if grep -q '^\[\[pome\]\]' "$root/$lock"; then dirname "$lock"; fi
  done
}
while read -r dir; do
  before="$(sha256 "$root/$dir/Pome.lock")"
  (cd "$root/$dir" && "$bin/emela" pome install >/dev/null && "$bin/emela" pome verify >/dev/null)
  if [[ "$(sha256 "$root/$dir/Pome.lock")" != "$before" ]]; then
    echo "$dir/Pome.lock did not match its Pome.toml and was rewritten; review and commit it" >&2
    exit 1
  fi
  echo "fetched the dependencies of $dir"
done < <(locked_pomes)

go run ./cmd/mamela toolchain status
echo
echo "ready. Next: scripts/check.sh (every check), or go run ./cmd/mamela eval examples/homelab"
echo "to run emela yourself: export PATH=\"$bin:\$PATH\" EMELA_POME_CACHE=\"$tools/pomes\""
