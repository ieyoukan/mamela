package kv_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The program in the Pome is exactly a build of this source (build.sh), and
// mamela-provider.toml names it: what a user imports is what was reviewed.
func TestPomeProgramIsTheBuild(t *testing.T) {
	out := filepath.Join(t.TempDir(), "provider.wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", out, "./cmd/kv-provider")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	built, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("pome/provider.wasm")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(built, committed) {
		t.Fatal("pome/provider.wasm is not a build of this source: run examples/kv/build.sh")
	}
	sum := sha256.Sum256(committed)
	toml, err := os.ReadFile("pome/mamela-provider.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(toml), `sha256 = "`+hex.EncodeToString(sum[:])+`"`) {
		t.Error("pome/mamela-provider.toml does not name the program's sha256: run examples/kv/build.sh")
	}
}
