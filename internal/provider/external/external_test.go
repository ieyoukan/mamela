package external

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/provider/host"
	"github.com/ieyoukan/mamela/internal/store"
)

func wasm(t *testing.T, pkg string, flags ...string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "p.wasm")
	cmd := exec.Command("go", append(append([]string{"build", "-o", out}, flags...), pkg)...)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func providerToml(name, program string, data []byte) string {
	sum := sha256.Sum256(data)
	return "provider = \"" + name + "\"\nversion = \"0.1.0\"\nprogram = \"" + program + "\"\nsha256 = \"" + hex.EncodeToString(sum[:]) + "\"\n"
}

// Providers come from the [replace] directories and from the Pome cache for Pome.lock.
func TestDiscover(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	program := []byte("\x00asm\x01\x00\x00\x00")
	write(t, filepath.Join(root, "app/Pome.toml"), `[pome]
name = "example.com/app"
[dependencies]
"example.com/Acme/kv" = "v1.2.0"
"example.com/local" = "v0.1.0"
[replace]
"example.com/local" = { path = "../local" }
`)
	write(t, filepath.Join(root, "app/Pome.lock"), "version = 1\n[[pome]]\npath = \"example.com/Acme/kv\"\nversion = \"v1.2.0\"\n")
	write(t, filepath.Join(cache, "example.com/!acme/kv@v1.2.0/provider.wasm"), string(program))
	write(t, filepath.Join(cache, "example.com/!acme/kv@v1.2.0", ProviderFile), providerToml("kv", "provider.wasm", program))
	write(t, filepath.Join(root, "local/bin/p.wasm"), string(program))
	write(t, filepath.Join(root, "local", ProviderFile), providerToml("local", "bin/p.wasm", program))

	pkgs, err := Discover(filepath.Join(root, "app"), cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 || pkgs[0].Provider != "local" || pkgs[0].Version != "local" || pkgs[1].Provider != "kv" || pkgs[1].Source != "example.com/Acme/kv" {
		t.Fatalf("%+v", pkgs)
	}

	bad := map[string]string{
		"another sha256":      providerToml("local", "bin/p.wasm", []byte("other")),
		"outside the Pome":    providerToml("local", "../app/Pome.toml", program),
		"absolute":            providerToml("local", "/etc/hosts", program),
		"an unknown key":      providerToml("local", "bin/p.wasm", program) + "native = true\n",
		"the same name twice": providerToml("kv", "bin/p.wasm", program),
	}
	for name, content := range bad {
		write(t, filepath.Join(root, "local", ProviderFile), content)
		if _, err := Discover(filepath.Join(root, "app"), cache); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	write(t, filepath.Join(root, "local/bin/p.wasm"), "#!/bin/sh\n")
	write(t, filepath.Join(root, "local", ProviderFile), providerToml("local", "bin/p.wasm", []byte("#!/bin/sh\n")))
	if _, err := Discover(filepath.Join(root, "app"), cache); err == nil || !strings.Contains(err.Error(), "WASM") {
		t.Errorf("a native program in a Pome: %v", err)
	}
}

func program(t *testing.T, data []byte) store.Program {
	sum := sha256.Sum256(data)
	return store.Program{Provider: "kv-example", Version: "0.1.0", Source: "test", Digest: "sha256:" + hex.EncodeToString(sum[:]), Data: data}
}

func declaration(t *testing.T) *graph.Graph {
	t.Helper()
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[{"kind":"KVItem","name":"web","provider":"kv-example","spec":{"value":"v1"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func wasmtime(t *testing.T) string {
	cwd, _ := os.Getwd()
	w, err := host.FindWasmtime(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// What a provider's diff says is checked before it becomes part of a plan.
func TestObserveChecksTheDiff(t *testing.T) {
	srv := httptest.NewServer(kvapi.New())
	defer srv.Close()
	cfg := Config{Provider: map[string]APIConfig{"kv-example": {APIURL: srv.URL, Catalog: "../../../examples/kv-demo/kv-catalog.json"}}}
	if _, err := Start(t.Context(), []store.Program{program(t, wasm(t, "../host/testdata/rogue"))},
		Config{Provider: map[string]APIConfig{"kv-example": {APIURL: srv.URL}}}, wasmtime(t)); err == nil || !strings.Contains(err.Error(), "no catalog") {
		t.Errorf("an API without a catalog: %v", err)
	}
	for _, c := range []struct{ mode, want string }{
		{"", ""},
		{"diff-undeclared", "not one of its declared resources"},
		{"diff-precondition", "based on version 999"},
		{"diff-no-precondition", "needs a precondition (version)"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			var flags []string
			if c.mode != "" {
				flags = append(flags, "-ldflags=-X main.mode="+c.mode)
			}
			data := wasm(t, "../host/testdata/rogue", flags...)
			sum := sha256.Sum256(data)
			rt, err := Start(t.Context(), []store.Program{{Provider: "kv-example", Version: "0.1.0", Source: "test",
				Digest: "sha256:" + hex.EncodeToString(sum[:]), Data: data}}, cfg, wasmtime(t))
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			got, err := rt.Observe(t.Context(), declaration(t), time.Unix(0, 0))
			if c.want == "" {
				if err != nil || len(got["KVItem web"].Changes) != 1 {
					t.Fatalf("%+v %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
	// A program that is not the version its Pome names does not start.
	data := wasm(t, "../host/testdata/rogue")
	sum := sha256.Sum256(data)
	if _, err := Start(t.Context(), []store.Program{{Provider: "kv-example", Version: "9.9.9", Source: "test",
		Digest: "sha256:" + hex.EncodeToString(sum[:]), Data: data}}, cfg, wasmtime(t)); err == nil {
		t.Error("a program of another version started")
	}
}
