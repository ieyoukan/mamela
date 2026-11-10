package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
	"github.com/ieyoukan/mamela/examples/kv/kvprovider"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// build builds a Go program (for WASI unless native) once per test binary.
var builds sync.Map

func build(t *testing.T, pkg string, native bool, flags ...string) string {
	t.Helper()
	key := strings.Join(append([]string{pkg, map[bool]string{true: "native", false: "wasi"}[native]}, flags...), " ")
	if p, ok := builds.Load(key); ok {
		return p.(string)
	}
	dir, err := os.MkdirTemp("", "mamela-host-test-")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "provider")
	args := append([]string{"build", "-o", out}, flags...)
	cmd := exec.Command("go", append(args, pkg)...)
	cmd.Env = os.Environ()
	if !native {
		cmd.Env = append(cmd.Env, "GOOS=wasip1", "GOARCH=wasm")
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
	builds.Store(key, out)
	return out
}

func rogue(t *testing.T, mode string) string {
	return build(t, "./testdata/rogue", false, "-ldflags=-X main.mode="+mode)
}

func wasmtime(t *testing.T) string {
	t.Helper()
	cwd, _ := os.Getwd()
	w, err := FindWasmtime(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

type world struct {
	api *kvapi.Server
	s   *Session
	now time.Time
}

func start(t *testing.T, program string, o Options) (*world, error) {
	t.Helper()
	exe, err := Inspect(program)
	if err != nil {
		return nil, err
	}
	w := &world{api: kvapi.New(), now: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	if o.Wasmtime == "" {
		o.Wasmtime = wasmtime(t)
	}
	o.Catalog, o.Upstream = kvapi.Catalog, Handler{Handler: w.api}
	o.Now = func() time.Time { return w.now }
	w.s, err = Start(t.Context(), exe, o)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { w.s.Close() })
	return w, nil
}

func must(t *testing.T, program string) *world {
	t.Helper()
	w, err := start(t, program, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func code(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

var web = protocol.Resource{ID: "KVItem/web", Kind: kvprovider.Kind, Name: "web", Spec: []byte(`{"value":"v1"}`)}

// plan observes and diffs one item and returns its change and permit.
func (w *world) plan(t *testing.T, opID string) (protocol.Change, protocol.Permit, []protocol.Observed) {
	t.Helper()
	obs, _, err := w.s.Observe(t.Context(), []protocol.Resource{web})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := w.s.Diff(t.Context(), []protocol.Resource{web}, obs)
	if err != nil || len(changes) != 1 {
		t.Fatalf("diff: %v %v", changes, err)
	}
	p, err := protocol.NewPermit(w.s.Manifest(), changes[0], opID, "sha256:plan", "sha256:providers", w.now.Add(time.Minute).Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	return changes[0], p, obs
}

func TestInspect(t *testing.T) {
	exe, err := Inspect(build(t, "../../../examples/kv/cmd/kv-provider", false))
	if err != nil || exe.Format != FormatWASIp1 || !strings.HasPrefix(exe.Digest, "sha256:") {
		t.Fatalf("the example provider: %+v %v", exe, err)
	}
	// A provider that reads files imports path_open: refused before it runs.
	_, err = Inspect(build(t, "./testdata/rogue", false, "-tags=readfile"))
	if code(err) != CodeExecutable || !strings.Contains(err.Error(), "path_open") {
		t.Errorf("a provider that reads files: %v", err)
	}
	native, err := Inspect(build(t, "../../../examples/kv/cmd/kv-provider", true))
	if err != nil || native.Format != FormatNative {
		t.Errorf("a native build: %+v %v", native, err)
	}
}

// A WASM provider gets no environment.
func TestWASMProviderIsIsolated(t *testing.T) {
	program := rogue(t, "env")
	t.Setenv("HOME", "/home/someone")
	w := must(t, program)
	if v := w.s.Manifest().Version; v != `env-0-""` {
		t.Errorf("the provider saw an environment: %s", v)
	}
}

func TestNativeRunsOnlyWhenTrusted(t *testing.T) {
	program := build(t, "./testdata/rogue", true, "-ldflags=-X main.mode=env")
	exe, err := Inspect(program)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := start(t, program, Options{Source: "example.com/kv", Version: "0.1.0"}); code(err) != CodeNotTrusted {
		t.Fatalf("an untrusted native provider: %v", err)
	}
	var other TrustFile
	other.Add(Trust{Source: "example.com/kv", Version: "0.1.0", Digest: "sha256:" + strings.Repeat("0", 64)})
	if _, err := start(t, program, Options{Source: "example.com/kv", Version: "0.1.0", Trust: other}); code(err) != CodeNotTrusted {
		t.Fatalf("trusted for another build: %v", err)
	}
	var trust TrustFile
	trust.Add(Trust{Source: "example.com/kv", Version: "0.1.0", Digest: exe.Digest})
	if _, err := start(t, program, Options{Source: "example.com/kv", Version: "0.2.0", Trust: trust}); code(err) != CodeNotTrusted {
		t.Fatalf("trusted as another version: %v", err)
	}
	t.Setenv("HOME", "/home/someone")
	w, err := start(t, program, Options{Source: "example.com/kv", Version: "0.1.0", Trust: trust})
	if err != nil {
		t.Fatalf("a trusted native provider: %v", err)
	}
	if v := w.s.Manifest().Version; v != `env-0-""` {
		t.Errorf("the native provider got the user's environment: %s", v)
	}
}

// What runs is what was inspected and trusted, even if the file is replaced afterwards.
func TestReplacedExecutableDoesNotRun(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "provider")
	data, err := os.ReadFile(build(t, "./testdata/rogue", true, "-ldflags=-X main.mode=env"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, data, 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := Inspect(program)
	if err != nil {
		t.Fatal(err)
	}
	var trust TrustFile
	trust.Add(Trust{Source: "example.com/kv", Version: "0.1.0", Digest: exe.Digest})
	marker := filepath.Join(dir, "replacement-ran")
	if err := os.WriteFile(program, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Start(t.Context(), exe, Options{Source: "example.com/kv", Version: "0.1.0", Trust: trust})
	if err != nil {
		t.Fatalf("the inspected provider: %v", err)
	}
	defer s.Close()
	if v := s.Manifest().Version; v != `env-0-""` {
		t.Errorf("manifest %s: not the inspected build", v)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the replacement ran")
	}
}

// A provider that stops reading its input cannot hold the host past the deadline.
func TestProviderThatDoesNotReadIsStopped(t *testing.T) {
	program := build(t, "./testdata/rogue", true, "-ldflags=-X main.mode=deaf")
	exe, err := Inspect(program)
	if err != nil {
		t.Fatal(err)
	}
	var trust TrustFile
	trust.Add(Trust{Source: "x", Version: "1", Digest: exe.Digest})
	s, err := Start(t.Context(), exe, Options{Source: "x", Version: "1", Trust: trust, CallTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	big := make([]protocol.Resource, 20000) // far more than a pipe holds
	for i := range big {
		big[i] = protocol.Resource{ID: fmt.Sprintf("KVItem/r%d", i), Kind: kvprovider.Kind, Name: fmt.Sprintf("r%d", i), Spec: []byte(`{"value":"0123456789abcdef"}`)}
	}
	start := time.Now()
	_, _, err = s.Observe(t.Context(), big)
	if code(err) != CodeProvider || !strings.Contains(err.Error(), "did not read") || time.Since(start) > 10*time.Second {
		t.Errorf("after %s: %v", time.Since(start), err)
	}
}

func TestTrustFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mamela.trust.toml")
	f, err := LoadTrust(path)
	if err != nil || len(f.Native) != 0 {
		t.Fatalf("a missing file: %+v %v", f, err)
	}
	tr := Trust{Source: "example.com/kv", Version: "0.1.0", Digest: "sha256:" + strings.Repeat("a", 64)}
	f.Add(tr)
	f.Add(tr)
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	g, err := LoadTrust(path)
	if err != nil || len(g.Native) != 1 || !g.Trusts(tr.Source, tr.Version, tr.Digest) {
		t.Fatalf("%+v %v", g, err)
	}
	os.WriteFile(path, []byte("[[native]]\nsource = \"x\"\nversion = \"1\"\ndigest = \"md5:x\"\n"), 0o644)
	if _, err := LoadTrust(path); err == nil {
		t.Error("a trust without a sha256 digest was accepted")
	}
}

func TestObserveSendsOnlyDeclaredReads(t *testing.T) {
	for _, mode := range []string{"observe-change", "observe-undeclared"} {
		t.Run(mode, func(t *testing.T) {
			w := must(t, rogue(t, mode))
			_, record, err := w.s.Observe(t.Context(), []protocol.Resource{web})
			if code(err) != CodeRefused {
				t.Fatalf("observe: %v", err)
			}
			if len(record) == 0 || record[0].Outcome != Refused {
				t.Errorf("record: %v", record)
			}
			if items := w.api.Items(); len(items) != 0 {
				t.Errorf("the API changed: %v", items)
			}
		})
	}
}

func TestApplyStaysWithinThePermit(t *testing.T) {
	for _, mode := range []string{"other-object", "extra-param", "unapproved-value", "ignore-refusal"} {
		t.Run(mode, func(t *testing.T) {
			w := must(t, rogue(t, mode))
			c, p, obs := w.plan(t, "op-1")
			r := w.s.Apply(t.Context(), p, c, &web, &obs[0])
			if r.Outcome != OutcomeFailed || code(r.Err) != CodeRefused {
				t.Fatalf("%s: %v", r.Outcome, r.Err)
			}
			if mode == "ignore-refusal" && !strings.Contains(r.Err.Error(), "does not believe") {
				t.Errorf("a provider that reports success after a refusal: %v", r.Err)
			}
			for name, it := range w.api.Items() {
				if name != "web" || it.Value != "v1" {
					t.Errorf("the API has %s=%q", name, it.Value)
				}
			}
		})
	}
}

func TestPermitIsTheApprovedChangeOnce(t *testing.T) {
	w := must(t, build(t, "../../../examples/kv/cmd/kv-provider", false))
	c, p, obs := w.plan(t, "op-1")

	other := p
	other.IDs = map[string]string{"item": "other"}
	if r := w.s.Apply(t.Context(), other, c, &web, &obs[0]); code(r.Err) != CodePermit {
		t.Errorf("a permit for other objects: %v", r.Err)
	}
	wider := p
	wider.Calls = append(wider.Calls, protocol.Call{Method: "DELETE", Path: "/items/{item}"})
	if r := w.s.Apply(t.Context(), wider, c, &web, &obs[0]); code(r.Err) != CodePermit {
		t.Errorf("a permit with more calls: %v", r.Err)
	}
	if r := w.s.Apply(t.Context(), p, c, &web, &obs[0]); r.Outcome != OutcomeDone {
		t.Fatalf("the approved change: %s %v", r.Outcome, r.Err)
	}
	if r := w.s.Apply(t.Context(), p, c, &web, &obs[0]); code(r.Err) != CodePermit {
		t.Errorf("the same permit again: %v", r.Err)
	}
}

func TestPermitExpires(t *testing.T) {
	w := must(t, build(t, "../../../examples/kv/cmd/kv-provider", false))
	c, p, obs := w.plan(t, "op-1")
	w.now = w.now.Add(time.Hour)
	if r := w.s.Apply(t.Context(), p, c, &web, &obs[0]); code(r.Err) != CodePermit || len(w.api.Items()) != 0 {
		t.Errorf("an expired permit: %v", r.Err)
	}
}

// A change based on an observation does not apply to a newer version of the object.
func TestPreconditionIsTheObservedVersion(t *testing.T) {
	w := must(t, build(t, "../../../examples/kv/cmd/kv-provider", false))
	w.api.Put(kvapi.Item{Name: "web", Value: "v0", Owner: "mamela", Version: 1})
	c, p, obs := w.plan(t, "op-1")
	if c.Op != kvprovider.OpUpdate || c.Precondition != "1" {
		t.Fatalf("change: %+v", c)
	}
	w.api.Put(kvapi.Item{Name: "web", Value: "changed by someone", Owner: "mamela", Version: 2})
	// The approved request carries version 1; the API refuses it (409). The host refuses one
	// that carries another version (TestEachPhaseAllowsOnlyItsRequests).
	r := w.s.Apply(t.Context(), p, c, &web, &obs[0])
	if r.Outcome != OutcomeFailed {
		t.Fatalf("%s: %v", r.Outcome, r.Err)
	}
	if it := w.api.Items()["web"]; it.Value != "changed by someone" {
		t.Errorf("the newer version was overwritten: %+v", it)
	}
}

func TestBrokenProviderIsStopped(t *testing.T) {
	w := must(t, rogue(t, "garbage"))
	if _, _, err := w.s.Observe(t.Context(), nil); code(err) != CodeProtocol {
		t.Errorf("garbage on stdout: %v", err)
	}
	if _, err := w.s.Diff(t.Context(), nil, nil); code(err) != CodeProtocol {
		t.Errorf("a call after the provider was stopped: %v", err)
	}

	w, err := start(t, rogue(t, "hang"), Options{CallTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.s.Observe(t.Context(), nil); code(err) != CodeProvider || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("a provider that does not answer: %v", err)
	}
}

// A catalog and a permit for one resize of VM 7: cores=2, based on version d1.
var (
	testCatalog = protocol.Catalog{API: "x", Entries: []protocol.Entry{
		{Method: "GET", Path: "/vms/{vm}", Effect: protocol.EffectRead, Query: []string{"full"}},
		{Method: "POST", Path: "/vms/{vm}/resize", Effect: protocol.EffectChange, Form: []string{"cores", "digest", "vm"}, Precondition: "digest"},
		{Method: "POST", Path: "/vms/{vm}/tags", Effect: protocol.EffectChange, Form: []string{"tags"}},
		{Method: "POST", Path: "/vms/{vm}/sneaky", Effect: protocol.EffectChange, Form: []string{"digest"}},
	}}
	readVM    = protocol.Call{Method: "GET", Path: "/vms/{vm}", Query: []string{"full"}}
	resizeVM  = protocol.Call{Method: "POST", Path: "/vms/{vm}/resize", Form: []string{"cores", "digest", "vm"}}
	tagVM     = protocol.Call{Method: "POST", Path: "/vms/{vm}/tags", Form: []string{"tags"}}
	pokeVM    = protocol.Call{Method: "POST", Path: "/vms/{vm}/poke"}
	sneakyVM  = protocol.Call{Method: "POST", Path: "/vms/{vm}/sneaky", Query: []string{"digest"}}
	testNow   = func() time.Time { return time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC) }
	resizeTo2 = protocol.Approved{Method: "POST", Path: "/vms/{vm}/resize", Params: map[string][]string{"cores": {"2"}, "digest": {"d1"}}}
)

func testPermit() *protocol.Permit {
	return &protocol.Permit{Op: "resize", Calls: []protocol.Call{readVM, resizeVM, tagVM, pokeVM, sneakyVM}, IDs: map[string]string{"vm": "7"},
		Requests:     []protocol.Approved{resizeTo2, {Method: "POST", Path: "/vms/{vm}/poke"}},
		Precondition: "d1", ExpiresAt: "2026-11-10T01:00:00Z"}
}

func form(kv ...string) map[string][]string {
	out := map[string][]string{}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = append(out[kv[i]], kv[i+1])
	}
	return out
}

func TestEachPhaseAllowsOnlyItsRequests(t *testing.T) {
	permit := testPermit()
	apply := gate{phase: phaseApply, calls: permit.Calls, permit: permit}
	resize := func(f map[string][]string) protocol.HostRequest {
		return protocol.HostRequest{Method: "POST", Path: "/vms/7/resize", Form: f}
	}
	cases := []struct {
		name    string
		g       gate
		r       protocol.HostRequest
		refused string
	}{
		{"describe sends nothing", gate{phase: phaseDescribe}, protocol.HostRequest{Method: "GET", Path: "/vms/7"}, "during describe"},
		{"diff sends nothing", gate{phase: phaseDiff, calls: []protocol.Call{readVM}}, protocol.HostRequest{Method: "GET", Path: "/vms/7"}, "during diff"},
		{"observe reads", gate{phase: phaseObserve, calls: []protocol.Call{readVM}}, protocol.HostRequest{Method: "GET", Path: "/vms/7", Query: form("full", "1")}, ""},
		{"observe changes nothing", gate{phase: phaseObserve, calls: []protocol.Call{resizeVM}}, protocol.HostRequest{Method: "POST", Path: "/vms/7/resize"}, "only reads"},
		{"an unclassified call is not a read", gate{phase: phaseLookup, calls: []protocol.Call{pokeVM}}, protocol.HostRequest{Method: "POST", Path: "/vms/7/poke"}, "unclassified"},
		{"undeclared call", gate{phase: phaseObserve, calls: []protocol.Call{readVM}}, protocol.HostRequest{Method: "GET", Path: "/vms/7/config"}, "not a declared call"},
		{"undeclared query", gate{phase: phaseObserve, calls: []protocol.Call{readVM}}, protocol.HostRequest{Method: "GET", Path: "/vms/7", Query: form("all", "1")}, "not declared"},
		{"a query parameter in the form", gate{phase: phaseObserve, calls: []protocol.Call{readVM}}, protocol.HostRequest{Method: "GET", Path: "/vms/7", Form: form("full", "1")}, "not declared in the form"},
		{"path escape", gate{phase: phaseObserve, calls: []protocol.Call{readVM}}, protocol.HostRequest{Method: "GET", Path: "/vms/.."}, "path segment"},
		{"the approved change", apply, resize(form("cores", "2", "digest", "d1")), ""},
		{"an approved request without parameters", apply, protocol.HostRequest{Method: "POST", Path: "/vms/7/poke"}, ""},
		{"another object", apply, protocol.HostRequest{Method: "POST", Path: "/vms/8/resize", Form: form("cores", "2", "digest", "d1")}, "not the approved"},
		{"another object in a parameter", apply, resize(form("cores", "2", "digest", "d1", "vm", "8")), "not the approved"},
		{"an unapproved value", apply, resize(form("cores", "64", "digest", "d1")), "not the approved request"},
		{"an approved value left out", apply, resize(form("digest", "d1")), "not the approved request"},
		{"a parameter added", apply, resize(form("cores", "2", "digest", "d1", "vm", "7")), "not the approved request"},
		{"a declared call with no approved request", apply, protocol.HostRequest{Method: "POST", Path: "/vms/7/tags", Form: form("tags", "x")}, "not an approved request"},
		{"the precondition left out", apply, resize(form("cores", "2")), "observed version d1"},
		{"the precondition moved to the query", apply, protocol.HostRequest{Method: "POST", Path: "/vms/7/resize", Form: form("cores", "2"), Query: form("digest", "d1")}, "not declared in the query"},
		{"a parameter where the API does not read it", gate{phase: phaseApply, calls: permit.Calls, permit: permit}, protocol.HostRequest{Method: "POST", Path: "/vms/7/sneaky", Query: form("digest", "d1")}, "the API does not read it from the query"},
		{"in both places", gate{phase: phaseObserve, calls: []protocol.Call{readVM}}, protocol.HostRequest{Method: "GET", Path: "/vms/7", Query: form("full", "1"), Form: form("full", "1")}, "both"},
		{"an unclassified call is bound", apply, protocol.HostRequest{Method: "POST", Path: "/vms/8/poke"}, "not the approved"},
		{"reads during apply are not bound", apply, protocol.HostRequest{Method: "GET", Path: "/vms/8"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := c.g
			g.catalog, g.upstream, g.now = testCatalog, Handler{Handler: kvapi.New()}, testNow
			if g.permit != nil {
				g.used = make([]bool, len(g.permit.Requests))
			}
			_, perr := g.send(context.Background(), c.r)
			switch {
			case c.refused == "" && perr != nil:
				t.Errorf("refused: %v", perr)
			case c.refused != "" && (perr == nil || perr.Code != protocol.CodeRefused || !strings.Contains(perr.Message, c.refused)):
				t.Errorf("got %v, want a refusal with %q", perr, c.refused)
			case c.refused != "" && g.record[0].Outcome != Refused:
				t.Errorf("record: %v", g.record)
			}
		})
	}
}

// stub answers every request with a fixed outcome.
type stub struct {
	err  error
	sent int
}

func (s *stub) Do(context.Context, protocol.HostRequest) (int, []byte, error) {
	s.sent++
	if s.err != nil {
		return 0, nil, s.err
	}
	return 200, []byte("{}"), nil
}

func TestApprovedRequestIsSentOnce(t *testing.T) {
	permit := testPermit()
	up := &stub{}
	g := gate{phase: phaseApply, calls: permit.Calls, permit: permit, catalog: testCatalog, upstream: up, now: testNow, used: make([]bool, 2)}
	req := protocol.HostRequest{Method: "POST", Path: "/vms/7/resize", Form: form("cores", "2", "digest", "d1")}
	if _, err := g.send(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := g.send(t.Context(), req); err == nil || !strings.Contains(err.Message, "already sent once") {
		t.Errorf("the same approved request again: %v", err)
	}
	if up.sent != 1 {
		t.Errorf("%d sent", up.sent)
	}
}

// After a change whose outcome is unknown, nothing more changes in the same apply.
func TestNoChangeAfterAnUnknownOutcome(t *testing.T) {
	permit := testPermit()
	up := &stub{err: errors.New("connection lost")}
	g := gate{phase: phaseApply, calls: permit.Calls, permit: permit, catalog: testCatalog, upstream: up, now: testNow, used: make([]bool, 2)}
	if _, err := g.send(t.Context(), protocol.HostRequest{Method: "POST", Path: "/vms/7/resize", Form: form("cores", "2", "digest", "d1")}); err == nil || err.Code != protocol.CodeUncertain {
		t.Fatalf("first: %v", err)
	}
	up.err = nil
	if _, err := g.send(t.Context(), protocol.HostRequest{Method: "POST", Path: "/vms/7/poke"}); err == nil || !strings.Contains(err.Message, "unknown") {
		t.Errorf("a change after an unknown outcome: %v", err)
	}
	if _, err := g.send(t.Context(), protocol.HostRequest{Method: "GET", Path: "/vms/7"}); err != nil {
		t.Errorf("a read after an unknown outcome: %v", err)
	}
	if up.sent != 2 {
		t.Errorf("%d sent, want 2 (the change, then the read)", up.sent)
	}
	if r := outcome(g.record, nil); r.Outcome != OutcomeUnknown {
		t.Errorf("outcome %s", r.Outcome)
	}
}

// The HTTP upstream does not follow redirects: the host checked this request, not another.
func TestHTTPDoesNotFollowRedirects(t *testing.T) {
	var other int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/items/web" {
			http.Redirect(w, r, "/api/items/other", http.StatusTemporaryRedirect)
			return
		}
		other++
	}))
	defer srv.Close()
	status, _, err := HTTP{Base: srv.URL + "/api", Client: srv.Client()}.Do(t.Context(), protocol.HostRequest{Method: "PUT", Path: "/items/web", Form: form("value", "v1")})
	if err != nil || status != http.StatusTemporaryRedirect || other != 0 {
		t.Errorf("status %d, err %v, %d requests to the redirect target", status, err, other)
	}
}

func TestOutcomeComesFromTheRecord(t *testing.T) {
	change := func(o RequestOutcome) Request { return Request{Method: "PUT", Path: "/x", Change: true, Outcome: o} }
	read := Request{Method: "GET", Path: "/x", Outcome: Done}
	failed := errors.New("provider failed")
	cases := []struct {
		name   string
		record []Request
		err    error
		want   Outcome
	}{
		{"done", []Request{read, change(Done)}, nil, OutcomeDone},
		{"nothing changed", []Request{read, change(NotSent)}, failed, OutcomeFailed},
		{"refused by the API", []Request{change(Failed)}, failed, OutcomeFailed},
		{"partly", []Request{change(Done), change(NotSent)}, failed, OutcomePartly},
		{"maybe", []Request{change(Done), change(Maybe)}, failed, OutcomeUnknown},
		{"maybe, and the provider says it is fine", []Request{change(Maybe), change(Done)}, nil, OutcomeUnknown},
		{"refused, and the provider says it is fine", []Request{change(Refused)}, nil, OutcomeFailed},
		{"the API refused the change, and the provider says it is fine", []Request{read, change(Failed)}, nil, OutcomeFailed},
		{"not sent, and the provider says it is fine", []Request{change(NotSent)}, nil, OutcomeFailed},
		{"one change failed after another, and the provider says it is fine", []Request{change(Done), change(Failed)}, nil, OutcomePartly},
		{"refused after a change", []Request{change(Done), change(Refused)}, nil, OutcomePartly},
	}
	for _, c := range cases {
		if got := outcome(c.record, c.err); got.Outcome != c.want || (c.want != OutcomeDone) != (got.Err != nil) {
			t.Errorf("%s: %s %v, want %s", c.name, got.Outcome, got.Err, c.want)
		}
	}
}
