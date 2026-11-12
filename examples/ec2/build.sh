#!/usr/bin/env bash
# Builds the example provider into its Pome (examples/ec2/pome/provider.wasm) and writes its
# sha256 into mamela-provider.toml. The build is reproducible: TestPomeProgramIsTheBuild rebuilds
# it and checks that the committed program is exactly this build of the source.
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
out=examples/ec2/pome/provider.wasm
GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -o "$out" ./examples/ec2/cmd/ec2-provider
sum="$(shasum -a 256 "$out" | cut -d' ' -f1)"
cat > examples/ec2/pome/mamela-provider.toml <<TOML
# The provider program of this Pome. Mamela runs it isolated (WASM) and checks this sha256;
# Pome.lock and the sumdb pin the whole Pome, this file and the program included.
provider = "ec2-example"
version = "0.1.0"
program = "provider.wasm"
sha256 = "$sum"

# The catalogs of the APIs the provider calls (ADR 0008): the operator names one in
# mamela.providers.toml (catalog = "pome:ec2") for Mamela to classify the provider's requests with.
[catalogs]
ec2 = "catalogs/ec2.json"
TOML
echo "$out $sum"
