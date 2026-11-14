#!/usr/bin/env bash
# Builds the example provider into its Pome (examples/kv/pome/provider.wasm), writes its sha256
# into mamela-provider.toml, and generates the Pome's Emela (src/lib.emel) from its manifest. The
# build is reproducible: TestPomeProgramIsTheBuild rebuilds it and checks that the committed program
# is exactly this build of the source, and that the Emela is what it generates.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"
# Build with exactly this Go release: another one gives another program.
want_go=go1.27.3
got_go="$(go env GOVERSION)"
if [[ "$got_go" != "$want_go" ]]; then
  echo "build with $want_go (this is $got_go): GOTOOLCHAIN=$want_go $0" >&2
  exit 1
fi
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
# The Pome's Emela is generated from the program's manifest (the kind's spec).
go run ./cmd/mamela provider pome examples/kv/pome >/dev/null
echo "$out $sum"
