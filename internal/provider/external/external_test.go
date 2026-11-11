package external

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
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
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
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
	// An absolute [replace] path is used as is.
	write(t, filepath.Join(root, "abs/Pome.toml"), "[pome]\nname = \"example.com/abs\"\n[replace]\n\"example.com/local\" = { path = \""+filepath.ToSlash(filepath.Join(root, "local"))+"\" }\n")
	if abs, err := Discover(filepath.Join(root, "abs"), cache); err != nil || len(abs) != 1 || abs[0].Provider != "local" {
		t.Errorf("an absolute replace path: %+v %v", abs, err)
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
			rt.SetCreations(noCreations{})
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

// The catalogs Mamela ships are consistent.
func TestBuiltinCatalogs(t *testing.T) {
	for name, c := range Catalogs {
		if err := c.Check(); err != nil || c.API != name {
			t.Errorf("%s: %v (api %q)", name, err, c.API)
		}
	}
}

// noCreations is a record of nothing created.
type noCreations struct{}

func (noCreations) Creations(context.Context, string, string) ([]map[string]string, error) {
	return nil, nil
}
func (noCreations) RecordCreation(context.Context, string, string, map[string]string, string, string) error {
	return nil
}

// recorded is a record of creations.
type recorded map[string][]map[string]string

func (r recorded) Creations(_ context.Context, provider, resource string) ([]map[string]string, error) {
	return r[provider+"/"+resource], nil
}
func (r recorded) RecordCreation(_ context.Context, provider, resource string, ids map[string]string, _, _ string) error {
	r[provider+"/"+resource] = append(r[provider+"/"+resource], ids)
	return nil
}

// An object that carries the provider's marker is Mamela's only if Mamela recorded that it set out
// to create it: a copied marker (here, an item with Mamela's owner that Mamela never made) is
// someone else's object, and nothing changes it.
func TestOwnershipNeedsMamelasRecord(t *testing.T) {
	api := kvapi.New()
	api.Put(kvapi.Item{Name: "web", Value: "old", Owner: "mamela", Version: 1})
	srv := httptest.NewServer(api)
	defer srv.Close()
	cfg := Config{Provider: map[string]APIConfig{"kv-example": {APIURL: srv.URL, Catalog: "../../../examples/kv-demo/kv-catalog.json"}}}
	rt, err := Start(t.Context(), []store.Program{program(t, wasm(t, "../../../examples/kv/cmd/kv-provider"))}, cfg, wasmtime(t))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rec := recorded{}
	rt.SetCreations(rec)
	if _, err := rt.Observe(t.Context(), declaration(t), time.Unix(0, 0)); err == nil || !strings.Contains(err.Error(), "did not create it") {
		t.Fatalf("an item with a copied marker is taken as Mamela's: %v", err)
	}
	rec.RecordCreation(t.Context(), "kv-example", "KVItem web", map[string]string{"item": "web"}, "", "")
	got, err := rt.Observe(t.Context(), declaration(t), time.Unix(0, 0))
	if err != nil || !got["KVItem web"].Observed.Owned || len(got["KVItem web"].Changes) != 1 {
		t.Fatalf("an item Mamela recorded creating: %+v %v", got["KVItem web"], err)
	}
}

// An AWS API's requests are signed with SigV4 over their body, for the configured region and
// service; the signature changes with the body.
func TestSigV4(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }
	sign := SigV4("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "ap-northeast-1", "ec2", now)
	req := func(body string) *http.Request {
		r, _ := http.NewRequest("POST", "https://ec2.ap-northeast-1.amazonaws.com/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}
	a, b := req("Action=StartInstances&InstanceId.1=i-1"), req("Action=StartInstances&InstanceId.1=i-2")
	if err := sign(a); err != nil {
		t.Fatal(err)
	}
	sign(b)
	auth := a.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261111/ap-northeast-1/ec2/aws4_request") ||
		a.Header.Get("X-Amz-Date") != "20261111T120000Z" || strings.Contains(auth, "wJalr") {
		t.Fatalf("authorization %q date %q", auth, a.Header.Get("X-Amz-Date"))
	}
	if auth == b.Header.Get("Authorization") {
		t.Fatal("the signature does not cover the body")
	}
	if body, _ := io.ReadAll(a.Body); string(body) != "Action=StartInstances&InstanceId.1=i-1" {
		t.Fatalf("the body was consumed by signing: %q", body)
	}
	for _, bad := range []string{
		"[provider.ec2]\napi_url = \"https://ec2.example\"\nauth = \"aws-sigv4\"\ntoken_id = \"AKID\"\n",
		"[provider.ec2]\napi_url = \"https://ec2.example\"\nregion = \"us-east-1\"\n",
	} {
		p := filepath.Join(t.TempDir(), "p.toml")
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}

// A resource that needs another first waits until that one exists and is Mamela's: no change of
// it is computed until then, and then its provider is told what its dependency is (ADR 0007).
func TestAfterWaitsForTheDependency(t *testing.T) {
	api := kvapi.New()
	srv := httptest.NewServer(api)
	defer srv.Close()
	cfg := Config{Provider: map[string]APIConfig{"kv-example": {APIURL: srv.URL, Catalog: "../../../examples/kv-demo/kv-catalog.json"}}}
	rt, err := Start(t.Context(), []store.Program{program(t, wasm(t, "../../../examples/kv/cmd/kv-provider"))}, cfg, wasmtime(t))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rec := recorded{}
	rt.SetCreations(rec)
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
		`{"kind":"KVItem","name":"web","provider":"kv-example","spec":{"value":"v1"}},` +
		`{"kind":"KVItem","name":"db","provider":"kv-example","after":["KVItem web"],"spec":{"value":"v1"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := rt.Observe(t.Context(), g, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if db := got["KVItem db"]; db.Waiting == "" || len(db.Changes) != 0 || !strings.Contains(db.Waiting, "KVItem web") {
		t.Fatalf("db before web exists: %+v", db)
	}
	if web := got["KVItem web"]; web.Waiting != "" || len(web.Changes) != 1 {
		t.Fatalf("web: %+v", web)
	}
	// web is created through Mamela (recorded first, then made).
	api.Put(kvapi.Item{Name: "web", Value: "v1", Owner: "mamela", Version: 1})
	rec.RecordCreation(t.Context(), "kv-example", "KVItem web", map[string]string{"item": "web"}, "", "")
	got, err = rt.Observe(t.Context(), g, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if db := got["KVItem db"]; db.Waiting != "" || len(db.Changes) != 1 || db.Changes[0].Change.Op != "create-item" {
		t.Fatalf("db once web is Mamela's: %+v", db)
	}
}

// A profile's temporary credentials are read for each request, with their session token, so that
// renewed credentials are used; the approval's scope names the profile, not the key (ADR 0007).
func TestSigV4Profile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "credentials")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", file)
	write := func(key, token string) {
		os.WriteFile(file, []byte("[other]\naws_access_key_id = AKIAOTHER\naws_secret_access_key = x\n\n[role]\naws_access_key_id = "+key+
			"\naws_secret_access_key = secret\naws_session_token = "+token+"\n"), 0o600)
	}
	now := func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }
	sign := SigV4Profile("role", "ap-northeast-1", "ecs", now)
	req := func() *http.Request {
		r, _ := http.NewRequest("POST", "https://ecs.ap-northeast-1.amazonaws.com/", strings.NewReader(`{}`))
		return r
	}
	write("ASIAFIRST", "token-1")
	a := req()
	if err := sign(a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.Header.Get("Authorization"), "Credential=ASIAFIRST/") || a.Header.Get("X-Amz-Security-Token") != "token-1" {
		t.Fatalf("signed %q with token %q", a.Header.Get("Authorization"), a.Header.Get("X-Amz-Security-Token"))
	}
	write("ASIARENEWED", "token-2")
	b := req()
	sign(b)
	if !strings.Contains(b.Header.Get("Authorization"), "Credential=ASIARENEWED/") || b.Header.Get("X-Amz-Security-Token") != "token-2" {
		t.Fatal("renewed credentials are not used")
	}
	if err := SigV4Profile("missing", "ap-northeast-1", "ecs", now)(req()); err == nil || !strings.Contains(err.Error(), "renew") {
		t.Fatalf("a profile without credentials: %v", err)
	}
	cfg := func(profile string) string {
		p := filepath.Join(t.TempDir(), "p.toml")
		os.WriteFile(p, []byte("[provider.aws]\napi_url = \"https://ecs.ap-northeast-1.amazonaws.com\"\nauth = \"aws-sigv4\"\nprofile = \""+profile+
			"\"\nregion = \"ap-northeast-1\"\nservice = \"ecs\"\n"), 0o600)
		return p
	}
	c, err := LoadConfig(cfg("role"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, id1, err := c.api("aws")
	if err != nil {
		t.Fatal(err)
	}
	write("ASIATHIRD", "token-3")
	_, _, id2, _ := c.api("aws")
	c2, _ := LoadConfig(cfg("other"))
	_, _, id3, _ := c2.api("aws")
	if id1 != id2 || id1 == id3 {
		t.Fatalf("scope: renewed %v, other profile %v", id1 == id2, id1 == id3)
	}
	for _, bad := range []string{
		"[provider.aws]\napi_url = \"https://x\"\nauth = \"aws-sigv4\"\nprofile = \"role\"\ntoken_id = \"A\"\ntoken_file = \"t\"\nregion = \"r\"\nservice = \"s\"\n",
		"[provider.kv]\napi_url = \"https://x\"\nprofile = \"role\"\n",
	} {
		p := filepath.Join(t.TempDir(), "p.toml")
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}

// A provider with several APIs is configured in one section per API, each taking the provider's
// auth, profile and region; with apis, the provider's own section has no api_url.
func TestSeveralAPIsConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "credentials")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", file)
	os.WriteFile(file, []byte("[role]\naws_access_key_id = ASIA\naws_secret_access_key = s\naws_session_token = t\n"), 0o600)
	load := func(text string) (Config, error) {
		p := filepath.Join(t.TempDir(), "p.toml")
		os.WriteFile(p, []byte(text), 0o600)
		return LoadConfig(p)
	}
	good := "[provider.aws]\nauth = \"aws-sigv4\"\nprofile = \"role\"\nregion = \"ap-northeast-1\"\n\n" +
		"[provider.aws.apis.ec2]\napi_url = \"https://ec2.ap-northeast-1.amazonaws.com\"\nservice = \"ec2\"\ncatalog = \"builtin:ec2\"\n"
	c, err := load(good)
	if err != nil {
		t.Fatal(err)
	}
	apis, id, err := c.apis("aws")
	if err != nil || apis["ec2"].Upstream == nil || apis["ec2"].Catalog.API != "ec2" || id == "" {
		t.Fatalf("%+v %q %v", apis, id, err)
	}
	if err := checkEndpoints(protocol.Manifest{Provider: "aws", APIs: []string{"ec2", "ecs"}}, apis); err != nil {
		t.Errorf("an API left unconfigured is not an error (its requests are not sent): %v", err)
	}
	if err := checkEndpoints(protocol.Manifest{Provider: "aws", APIs: []string{"ecs"}}, apis); err == nil {
		t.Error("an API the provider does not have is configured")
	}
	for name, bad := range map[string]string{
		"api_url beside apis": strings.Replace(good, "region = \"ap-northeast-1\"\n", "region = \"ap-northeast-1\"\napi_url = \"https://x\"\n", 1),
		"no service":          strings.Replace(good, "service = \"ec2\"\n", "", 1),
		"guard in an api":     good + "[provider.aws.apis.ec2.guard]\nx = 1\n",
	} {
		if _, err := load(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A resource waits for a dependency that does not exist, is not Mamela's, or whose creation did not
// finish; it waits for nothing once each of them is Mamela's and finished.
func TestWaitsFor(t *testing.T) {
	d := protocol.Resource{ID: "KVItem db", After: []string{"KVItem web"}}
	for _, c := range []struct {
		web  *protocol.Observed
		want string
	}{
		{nil, "to be created"},
		{&protocol.Observed{}, "to be created"},
		{&protocol.Observed{Exists: true}, "not Mamela's"},
		{&protocol.Observed{Exists: true, Owned: true, Unfinished: true}, "did not finish"},
		{&protocol.Observed{Exists: true, Owned: true}, ""},
	} {
		provided := map[string]state.Provided{}
		if c.web != nil {
			provided["KVItem web"] = state.Provided{Observed: *c.web}
		}
		got := waitsFor(d, provided)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("web %+v: %q, want %q", c.web, got, c.want)
		}
	}
}
