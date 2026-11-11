package pve_test

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

	"github.com/ieyoukan/mamela/examples/pve/fakepve"
	"github.com/ieyoukan/mamela/internal/provider/external"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/providertest"
)

func vm(name, spec string) protocol.Resource {
	return protocol.Resource{ID: "PveVM " + name, Kind: "PveVM", Name: name, Spec: []byte(spec)}
}

// The Proxmox provider passes the conformance suite against a fake Proxmox, with the catalog
// Mamela ships for Proxmox.
func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Suite{
		Provider: providertest.BuildWASI(t, "./cmd/pve-provider"),
		Catalog:  external.ProxmoxCatalog,
		NewAPI:   func() http.Handler { return fakepve.New() },
		Cases: []providertest.Case{
			{Name: "one-vm", Desired: []protocol.Resource{
				vm("blank", `{"node":"pve1","vmid":7050,"pool":"lab","storage":"local-lvm","cores":1,"memoryMiB":512,"diskGiB":1}`)}},
		},
	})
}

// The program in the Pome is exactly a build of this source (build.sh).
func TestPomeProgramIsTheBuild(t *testing.T) {
	out := filepath.Join(t.TempDir(), "provider.wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", out, "./cmd/pve-provider")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	built, _ := os.ReadFile(out)
	committed, err := os.ReadFile("pome/provider.wasm")
	if err != nil || !bytes.Equal(built, committed) {
		t.Fatal("pome/provider.wasm is not a build of this source: run examples/pve/build.sh")
	}
	sum := sha256.Sum256(committed)
	toml, _ := os.ReadFile("pome/mamela-provider.toml")
	if !strings.Contains(string(toml), `sha256 = "`+hex.EncodeToString(sum[:])+`"`) {
		t.Error("pome/mamela-provider.toml does not name the program: run examples/pve/build.sh")
	}
}
