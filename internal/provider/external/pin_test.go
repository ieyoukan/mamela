package external

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
)

// pinnedFixture is a legitimate kv provider and a rogue one, each in a tree a branch author could
// write, and the operator's file as it is pinned to the legitimate one.
type pinnedFixture struct {
	api      *kvapi.Server
	requests *atomic.Int32
	url      string
	good     []byte // the legitimate program
	rogue    []byte // a program that deletes during observe
	catalog  string // the legitimate catalog, as shipped
}

func newPinnedFixture(t *testing.T) pinnedFixture {
	t.Helper()
	f := pinnedFixture{api: kvapi.New(), requests: &atomic.Int32{}}
	f.api.Put(kvapi.Item{Name: "victim", Value: "precious", Owner: "someone", Version: 1})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.api.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	f.good = wasm(t, "../../../examples/kv/cmd/kv-provider")
	f.rogue = wasm(t, "../host/testdata/rogue", "-ldflags=-X main.mode=observe-delete")
	b, err := os.ReadFile("../../../examples/kv-demo/kv-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	f.catalog = string(b)
	return f
}

// cfg is the operator's file with the provider pinned to the legitimate program and catalog (with
// pins false, as operators have it today: unpinned).
func (f pinnedFixture) cfg(pins bool) Config {
	a := APIConfig{APIURL: f.url, Catalog: "pome:kv"}
	if pins {
		a.ProgramSHA256, a.CatalogSHA256 = sum(f.good), sum([]byte(f.catalog))
	}
	return Config{Provider: map[string]APIConfig{"kv-example": a}}
}

// start discovers the tree's providers and starts them with cfg, and observes once.
func (f pinnedFixture) start(t *testing.T, tree string, cfg Config) (*Runtime, error) {
	t.Helper()
	pkgs, err := Discover(tree, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Start(t.Context(), Programs(pkgs), cfg, wasmtime(t))
}

// A3: the operator's pins decide which program and which catalog run with the provider's API. What a
// declaration's tree ships instead does not start, and nothing reaches the API.
func TestPinsDecideWhatRuns(t *testing.T) {
	f := newPinnedFixture(t)
	t.Run("the pinned program and catalog run", func(t *testing.T) {
		rt, err := f.start(t, declarationWith(t, f.good, f.catalog), f.cfg(true))
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Close()
		if w := rt.Warnings(); len(w) != 0 {
			t.Errorf("warnings for a pinned provider: %v", w)
		}
		rt.SetCreations(noCreations{})
		if _, err := rt.Observe(t.Context(), declaration(t), time.Unix(0, 0)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("another program does not start", func(t *testing.T) {
		before := f.requests.Load()
		// The rogue program ships a catalog that is valid (the delete is a lose-data): only the pin
		// stands between it and the operator's credentials.
		harmless := strings.Replace(deleteAsRead, `"effect":"read"}]}`, `"effect":"lose-data"}]}`, 1)
		_, err := f.start(t, declarationWith(t, f.rogue, harmless), f.cfg(true))
		if err == nil || !strings.Contains(err.Error(), "MAM-PRV-0009") || !strings.Contains(err.Error(), "program_sha256") ||
			!strings.Contains(err.Error(), "mamela provider pin") {
			t.Fatalf("a program the operator did not pin: %v", err)
		}
		if f.requests.Load() != before || len(f.api.Items()) != 1 {
			t.Errorf("the refused provider reached the API: %d requests", f.requests.Load()-before)
		}
	})
	t.Run("another catalog does not start", func(t *testing.T) {
		wider := strings.Replace(f.catalog, `"entries": [`, `"entries": [{"method": "GET", "path": "/extra", "effect": "read"},`, 1)
		if wider == f.catalog {
			t.Fatal("the fixture's catalog changed shape")
		}
		_, err := f.start(t, declarationWith(t, f.good, wider), f.cfg(true))
		if err == nil || !strings.Contains(err.Error(), "MAM-PRV-0009") || !strings.Contains(err.Error(), "catalog_sha256") {
			t.Fatalf("a catalog the operator did not pin: %v", err)
		}
	})
	t.Run("an unpinned provider runs, with a warning", func(t *testing.T) {
		rt, err := f.start(t, declarationWith(t, f.good, f.catalog), f.cfg(false))
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Close()
		w := strings.Join(rt.Warnings(), "\n")
		for _, want := range []string{"MAM-PRV-0010", "kv-example", "program_sha256", "catalog_sha256", "mamela provider pin", "strict_pins"} {
			if !strings.Contains(w, want) {
				t.Errorf("the warning lacks %q:\n%s", want, w)
			}
		}
	})
	t.Run("strict_pins refuses an unpinned provider", func(t *testing.T) {
		cfg := f.cfg(false)
		cfg.StrictPins = true
		_, err := f.start(t, declarationWith(t, f.good, f.catalog), cfg)
		if err == nil || !strings.Contains(err.Error(), "MAM-PRV-0010") {
			t.Fatalf("strict_pins with no pin: %v", err)
		}
	})
}

// The file's pins are checked when it is read: a digest that is not one, a pin in the wrong place,
// and with strict_pins a provider without pins (before anything is planned).
func TestPinSettings(t *testing.T) {
	load := func(text string) (Config, error) {
		p := filepath.Join(t.TempDir(), "p.toml")
		write(t, p, text)
		return LoadConfig(p)
	}
	hex64 := strings.Repeat("ab", 32)
	one := "[provider.kv]\napi_url = \"https://kv.example\"\ncatalog = \"pome:kv\"\n"
	multi := "[provider.aws]\nauth = \"aws-sigv4\"\nprofile = \"p\"\nregion = \"r\"\n"
	ec2 := "[provider.aws.apis.ec2]\napi_url = \"https://ec2.example\"\nservice = \"ec2\"\ncatalog = \"pome:ec2\"\n"
	for name, text := range map[string]string{
		"a pinned provider":                   one + "program_sha256 = \"" + hex64 + "\"\ncatalog_sha256 = \"sha256:" + strings.ToUpper(hex64) + "\"\n",
		"a file catalog needs no pin":         "[provider.kv]\napi_url = \"https://kv.example\"\ncatalog = \"kv.json\"\n",
		"several APIs, each pinned":           multi + "program_sha256 = \"" + hex64 + "\"\n" + ec2 + "catalog_sha256 = \"" + hex64 + "\"\n",
		"strict, pinned":                      "strict_pins = true\n" + one + "program_sha256 = \"" + hex64 + "\"\ncatalog_sha256 = \"" + hex64 + "\"\n",
		"strict, a file catalog, one program": "strict_pins = true\n[provider.kv]\napi_url = \"https://kv.example\"\ncatalog = \"kv.json\"\nprogram_sha256 = \"" + hex64 + "\"\n",
	} {
		if _, err := load(text); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]struct{ text, want string }{
		"not a sha256":               {one + "program_sha256 = \"abc\"\n", "not a sha256"},
		"a catalog pin on a file":    {"[provider.kv]\napi_url = \"https://kv.example\"\ncatalog = \"kv.json\"\ncatalog_sha256 = \"" + hex64 + "\"\n", "file of your own"},
		"a program pin on an api":    {multi + ec2 + "program_sha256 = \"" + hex64 + "\"\n", "belongs to the provider's section"},
		"a catalog pin with apis":    {multi + "catalog_sha256 = \"" + hex64 + "\"\n" + ec2, "belong to each api's section"},
		"strict without pins":        {"strict_pins = true\n" + one, "MAM-PRV-0010"},
		"strict without the catalog": {"strict_pins = true\n" + one + "program_sha256 = \"" + hex64 + "\"\n", "catalog_sha256 in [provider.kv]"},
	} {
		if _, err := load(c.text); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		}
	}
}

// What the pins say of a package, for `mamela provider pin`; and a file with pins survives being
// written out again (mamela controlplane bundle rewrites the providers file).
func TestPinLinesAndRoundTrip(t *testing.T) {
	program := []byte("\x00asm\x01\x00\x00\x00")
	catalog := []byte(`{"schema":"mamela-catalog/v1","api":"kv-example/v1","entries":[]}`)
	pkg := Package{Source: "example.com/kv", Version: "v1.0.0", Provider: "kv-example", Digest: "sha256:" + sum(program), Catalogs: map[string][]byte{"kv": catalog}}

	if lines := (Config{}).PinLines(pkg); len(lines) != 2 || lines[0].State() != "not pinned" || lines[0].Section != "[provider.kv-example]" ||
		lines[0].Want != sum(program) || lines[1].Key != "catalog_sha256" || lines[1].Want != sum(catalog) {
		t.Errorf("a provider the file does not have: %+v", lines)
	}
	cfg := Config{Provider: map[string]APIConfig{"kv-example": {APIURL: "https://kv.example", Catalog: "pome:kv", ProgramSHA256: sum(program), CatalogSHA256: strings.Repeat("0", 64)}}}
	lines := cfg.PinLines(pkg)
	if lines[0].State() != "pinned" || lines[1].State() != "differs" || lines[1].Section != "[provider.kv-example]" {
		t.Errorf("a configured provider: %+v", lines)
	}

	var out strings.Builder
	if err := toml.NewEncoder(&out).Encode(Config{StrictPins: true, Provider: cfg.Provider}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "p.toml")
	write(t, p, out.String())
	back, err := LoadConfig(p)
	if err != nil || !back.StrictPins || back.Provider["kv-example"].ProgramSHA256 != sum(program) {
		t.Errorf("the pins after a round trip: %+v %v\n%s", back, err, out.String())
	}
}
