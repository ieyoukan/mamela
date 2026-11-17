#!/usr/bin/env bash
# Builds mamelet (the Mamela agent) for the lab VMs: linux/amd64, static, reproducible flags.
# The result goes into each VM's cloud-init seed (controlplane.mamelet_binary in the site file).
# Its version is mamelet's release (internal/mamelet/release.go) and the commit it is built from:
# "0.10.0+849289e" ("-dirty" when the tree has changes).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="${1:-$root/.mamela/bin/mamelet-linux-amd64}"
release="$(sed -n 's/^const Release = "\(.*\)"$/\1/p' "$root/internal/mamelet/release.go")"
if [[ -z "$release" ]]; then
  echo "no release in internal/mamelet/release.go" >&2; exit 1
fi
commit="$(git -C "$root" describe --always --dirty --exclude '*' 2>/dev/null || echo dev)"
version="$release+$commit"
mkdir -p "$(dirname "$out")"
cd "$root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$out" ./cmd/mamelet
echo "built $out ($version)"
shasum -a 256 "$out"
