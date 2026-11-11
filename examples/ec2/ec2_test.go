package ec2_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/examples/ec2/fakeec2"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/providertest"
)

func instance(name, spec string) protocol.Resource {
	return protocol.Resource{ID: "Ec2Instance " + name, Kind: "Ec2Instance", Name: name, Spec: []byte(spec)}
}

const web = `{"imageId":"ami-0123456789abcdef0","instanceType":"t3.small","subnetId":"subnet-01","securityGroupId":"sg-01","rootDevice":"/dev/sda1","diskGiB":20,"network":{"mode":"dhcp"}}`

// The EC2 provider passes the conformance suite against a fake EC2 with Mamela's EC2 catalog:
// within its permits (one instance per RunInstances, the seed's content filled by the host),
// converging, keeping undeclared instances, and recovering from a lost or unsent request.
func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Suite{
		Provider: providertest.BuildWASI(t, "./cmd/ec2-provider"),
		Catalog:  providertest.Catalog(t, "pome/catalogs/ec2.json"), // as the Pome ships it
		NewAPI:   func() http.Handler { return fakeec2.New() },
		Cases: []providertest.Case{
			{Name: "one-instance", Desired: []protocol.Resource{instance("web", web)}},
			{Name: "two-instances", Desired: []protocol.Resource{instance("web", web),
				instance("db", strings.Replace(web, "t3.small", "t3.medium", 1))}},
		},
	})
}

// The program in the Pome is exactly a build of this source (build.sh).
func TestPomeProgramIsTheBuild(t *testing.T) {
	out := filepath.Join(t.TempDir(), "provider.wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", out, "./cmd/ec2-provider")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	built, _ := os.ReadFile(out)
	committed, err := os.ReadFile("pome/provider.wasm")
	if err != nil || !bytes.Equal(built, committed) {
		t.Fatal("pome/provider.wasm is not a build of this source: run examples/ec2/build.sh")
	}
	sum := sha256.Sum256(committed)
	toml, _ := os.ReadFile("pome/mamela-provider.toml")
	if !strings.Contains(string(toml), `sha256 = "`+hex.EncodeToString(sum[:])+`"`) {
		t.Error("pome/mamela-provider.toml does not name the program: run examples/ec2/build.sh")
	}
}
