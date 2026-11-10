#!/usr/bin/env bash
# Builds mamelet (the Mamela agent) for the lab VMs: linux/amd64, static, reproducible flags.
# The result goes into each VM's cloud-init seed (controlplane.mamelet_binary in the site file).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="${1:-$root/.mamela/bin/mamelet-linux-amd64}"
version="$(git -C "$root" describe --always --dirty 2>/dev/null || echo dev)"
mkdir -p "$(dirname "$out")"
cd "$root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$out" ./cmd/mamelet
echo "built $out ($version)"
shasum -a 256 "$out"
