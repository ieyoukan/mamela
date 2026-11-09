#!/usr/bin/env bash
# Installs the pinned Emela and Wasmtime toolchain into ./.tools (gitignored), and fetches the
# Emela dependencies of this repository's Pomes into ./.tools/pomes.
# Every download is verified: the tools against a pinned sha256, the Pomes against the sumdb and
# the hashes in each Pome.lock. Nothing is installed system-wide.
#
# Usage: scripts/install-toolchain.sh
# Afterwards: export PATH="$PWD/.tools/bin:$PATH"
set -euo pipefail

EMELA_VERSION="v0.27.0"
WASMTIME_VERSION="v49.0.5"

root="$(cd "$(dirname "$0")/.." && pwd)"
tools="$root/.tools"
bin="$tools/bin"
cache="$tools/downloads"
mkdir -p "$bin" "$cache"

os="$(uname -s)"
arch="$(uname -m)"
case "$os/$arch" in
  Darwin/arm64)
    emela_asset="emela-${EMELA_VERSION}-aarch64-apple-darwin.tar.gz"
    emela_sha="f2075416bc5fc1569ce96e656758cc39c5a29fd121f8ce87a25fe480ce3a3103"
    wasmtime_asset="wasmtime-${WASMTIME_VERSION}-aarch64-macos.tar.xz"
    wasmtime_sha="38c8a1b43a04a67f2d953498e470117aaf46ba155e1a7e3158aa7f87259bd750"
    ;;
  Linux/x86_64)
    emela_asset="emela-${EMELA_VERSION}-x86_64-unknown-linux-gnu.tar.gz"
    emela_sha="db25af80d5fcd1ce32f37a1b21fb93cc3dd6650816f660cc526729b27057ea8c"
    wasmtime_asset="wasmtime-${WASMTIME_VERSION}-x86_64-linux.tar.xz"
    wasmtime_sha="101b181779afec556ac2cc6639a7ee3661de20c17f7948e0b290f63e9da9bdce"
    ;;
  *)
    echo "unsupported platform: $os/$arch (Emela publishes macOS arm64 and Linux x86_64)" >&2
    exit 1
    ;;
esac

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

fetch() {
  local url="$1" file="$2" want="$3"
  if [[ ! -f "$cache/$file" ]] || [[ "$(sha256 "$cache/$file")" != "$want" ]]; then
    echo "downloading $file"
    curl -fsSL --proto '=https' --tlsv1.2 -o "$cache/$file.part" "$url"
    mv "$cache/$file.part" "$cache/$file"
  fi
  local got
  got="$(sha256 "$cache/$file")"
  if [[ "$got" != "$want" ]]; then
    echo "sha256 mismatch for $file: got $got, want $want" >&2
    rm -f "$cache/$file"
    exit 1
  fi
}

fetch "https://github.com/emela-lang/emela/releases/download/${EMELA_VERSION}/${emela_asset}" "$emela_asset" "$emela_sha"
fetch "https://github.com/bytecodealliance/wasmtime/releases/download/${WASMTIME_VERSION}/${wasmtime_asset}" "$wasmtime_asset" "$wasmtime_sha"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/emela"
tar -xzf "$cache/$emela_asset" -C "$work/emela"
emela_bin="$(find "$work/emela" -type f -name emela -perm -u+x | head -n1)"
[[ -n "$emela_bin" ]] || { echo "emela binary not found in $emela_asset" >&2; exit 1; }
install -m 0755 "$emela_bin" "$bin/emela"

mkdir -p "$work/wasmtime"
tar -xJf "$cache/$wasmtime_asset" -C "$work/wasmtime"
wasmtime_bin="$(find "$work/wasmtime" -type f -name wasmtime -perm -u+x | head -n1)"
[[ -n "$wasmtime_bin" ]] || { echo "wasmtime binary not found in $wasmtime_asset" >&2; exit 1; }
install -m 0755 "$wasmtime_bin" "$bin/wasmtime"

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

echo "installed:"
"$bin/emela" --version
"$bin/wasmtime" --version
echo "add to PATH: export PATH=\"$bin:\$PATH\""
echo "and to use the fetched dependencies: export EMELA_POME_CACHE=\"$tools/pomes\""
