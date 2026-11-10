#!/usr/bin/env bash
# Builds the example provider into its Pome (examples/kv/pome/provider.wasm) and writes its
# sha256 into mamela-provider.toml. The build is reproducible: TestPomeProgramIsTheBuild rebuilds
# it and checks that the committed program is exactly this build of the source.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"
out=examples/kv/pome/provider.wasm
GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -o "$out" ./examples/kv/cmd/kv-provider
sum="$(shasum -a 256 "$out" | cut -d' ' -f1)"
cat > examples/kv/pome/mamela-provider.toml <<TOML
# The provider program of this Pome. Mamela runs it isolated (WASM) and checks this sha256;
# Pome.lock and the sumdb pin the whole Pome, this file and the program included.
provider = "kv-example"
version = "0.1.0"
program = "provider.wasm"
sha256 = "$sum"
TOML
echo "$out $sum"
