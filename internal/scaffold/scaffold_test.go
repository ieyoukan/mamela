package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// What mamela provider new writes builds, generates its Pome, and passes its own tests (the
// conformance suite against its fake API) as written, against this checkout of Mamela, offline.
func TestNewProviderPassesItsTests(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and tests a whole provider")
	}
	if runtime.Version() != "go1.27.3" {
		t.Skip("build.sh builds with go1.27.3 only")
	}
	root, _ := filepath.Abs("../..")
	if _, err := os.Stat(filepath.Join(root, ".tools/bin/wasmtime")); err != nil {
		t.Skip("no toolchain: run scripts/install-toolchain.sh")
	}
	dir := filepath.Join(t.TempDir(), "mamela-acme-cloud")
	written, err := Write(dir, Options{GoModule: "example.com/me/mamela-acme-cloud", Provider: "acme-cloud",
		MamelaVersion: "v0.0.0-00010101000000-000000000000", Mamela: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"acmecloud/acmecloud.go", "acmecloud/fakeapi/fakeapi.go", "pome/catalogs/acmecloud.json", ".github/workflows/ci.yml", ".gitignore", "build.sh"} {
		if !strings.Contains(strings.Join(written, "\n"), want) {
			t.Errorf("no %s in %v", want, written)
		}
	}
	env := append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off", "GOWORK=off", "GOTOOLCHAIN=local",
		"MAMELA_TOOLCHAIN_DIR="+filepath.Join(root, ".tools/bin"))
	for _, step := range [][]string{
		{"go", "mod", "tidy"},
		{"./build.sh"},
		{"go", "vet", "./..."},
		{"go", "test", "./..."},
	} {
		cmd := exec.Command(step[0], step[1:]...)
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(step, " "), err, out)
		}
	}
	src, err := os.ReadFile(filepath.Join(dir, "pome/src/lib.emel"))
	if err != nil || !strings.Contains(string(src), "pub fn acme_cloud_thing(graph: Mamela.Graph, r: AcmeCloudThing)") {
		t.Fatalf("the generated Pome: %v\n%s", err, src)
	}
}

func TestRefused(t *testing.T) {
	ok := Options{GoModule: "example.com/me/x", Provider: "x", MamelaVersion: "v0.1.0"}
	for name, o := range map[string]func(*Options){
		"provider name": func(o *Options) { o.Provider = "X_y" },
		"go module":     func(o *Options) { o.GoModule = "nothing" },
		"module":        func(o *Options) { o.Module = "lower" },
		"version":       func(o *Options) { o.MamelaVersion = "" },
		"checkout":      func(o *Options) { o.Mamela = t.TempDir() },
	} {
		opts := ok
		o(&opts)
		if _, err := Write(filepath.Join(t.TempDir(), "p"), opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x"), nil, 0o644)
	if _, err := Write(dir, ok); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("a directory that is not empty: %v", err)
	}
}
