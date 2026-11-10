package host

import (
	"context"
	"errors"
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
			want := OutcomeFailed
			if mode == "ignore-refusal" {
				want = OutcomePartly // it went on and created web; the refusal still fails the apply
				if r.Err == nil || !strings.Contains(r.Err.Error(), "does not believe") {
					t.Errorf("a provider that reports success after a refusal: %v", r.Err)
				}
			}
			if r.Outcome != want || code(r.Err) != CodeRefused {
				t.Fatalf("%s: %v", r.Outcome, r.Err)
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
	r := w.s.Apply(t.Context(), p, c, &web, &obs[0])
	if code(r.Err) != CodeRefused || !strings.Contains(r.Err.Error(), "observed version 1") {
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

func TestEachPhaseAllowsOnlyItsRequests(t *testing.T) {
	catalog := protocol.Catalog{API: "x", Entries: []protocol.Entry{
		{Method: "GET", Path: "/vms/{vm}", Effect: protocol.EffectRead},
		{Method: "POST", Path: "/vms/{vm}/resize", Effect: protocol.EffectChange},
	}}
	read := protocol.Call{Method: "GET", Path: "/vms/{vm}"}
	resize := protocol.Call{Method: "POST", Path: "/vms/{vm}/resize", Params: []string{"cores", "vm"}}
	unknown := protocol.Call{Method: "POST", Path: "/vms/{vm}/poke"}
	permit := &protocol.Permit{Op: "resize", Calls: []protocol.Call{read, resize, unknown}, IDs: map[string]string{"vm": "7"},
		Values:    []protocol.Pin{{Method: "POST", Path: "/vms/{vm}/resize", Param: "cores", Values: []string{"2"}}},
		ExpiresAt: "2026-11-10T01:00:00Z"}
	now := func() time.Time { return time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC) }
	ok := Handler{Handler: kvapi.New()}
	cases := []struct {
		name    string
		g       gate
		r       protocol.HostRequest
		refused string
	}{
		{"describe sends nothing", gate{phase: phaseDescribe}, protocol.HostRequest{Method: "GET", Path: "/vms/7"}, "during describe"},
		{"diff sends nothing", gate{phase: phaseDiff, calls: []protocol.Call{read}}, protocol.HostRequest{Method: "GET", Path: "/vms/7"}, "during diff"},
		{"observe reads", gate{phase: phaseObserve, calls: []protocol.Call{read}}, protocol.HostRequest{Method: "GET", Path: "/vms/7"}, ""},
		{"observe changes nothing", gate{phase: phaseObserve, calls: []protocol.Call{resize}}, protocol.HostRequest{Method: "POST", Path: "/vms/7/resize"}, "only reads"},
		{"an unclassified call is not a read", gate{phase: phaseLookup, calls: []protocol.Call{unknown}}, protocol.HostRequest{Method: "POST", Path: "/vms/7/poke"}, "unclassified"},
		{"undeclared call", gate{phase: phaseObserve, calls: []protocol.Call{read}}, protocol.HostRequest{Method: "GET", Path: "/vms/7/config"}, "not a declared call"},
		{"undeclared query", gate{phase: phaseObserve, calls: []protocol.Call{read}}, protocol.HostRequest{Method: "GET", Path: "/vms/7", Query: map[string][]string{"full": {"1"}}}, "not declared"},
		{"path escape", gate{phase: phaseObserve, calls: []protocol.Call{read}}, protocol.HostRequest{Method: "GET", Path: "/vms/.."}, "path segment"},
		{"the approved change", gate{phase: phaseApply, calls: permit.Calls, permit: permit}, protocol.HostRequest{Method: "POST", Path: "/vms/7/resize", Form: map[string][]string{"cores": {"2"}}}, ""},
		{"another object", gate{phase: phaseApply, calls: permit.Calls, permit: permit}, protocol.HostRequest{Method: "POST", Path: "/vms/8/resize", Form: map[string][]string{"cores": {"2"}}}, "not the approved"},
		{"another object in a parameter", gate{phase: phaseApply, calls: permit.Calls, permit: permit}, protocol.HostRequest{Method: "POST", Path: "/vms/7/resize", Form: map[string][]string{"vm": {"8"}}}, "not the approved"},
		{"an unapproved value", gate{phase: phaseApply, calls: permit.Calls, permit: permit}, protocol.HostRequest{Method: "POST", Path: "/vms/7/resize", Form: map[string][]string{"cores": {"64"}}}, "not an approved value"},
		{"an unclassified call is bound", gate{phase: phaseApply, calls: permit.Calls, permit: permit}, protocol.HostRequest{Method: "POST", Path: "/vms/8/poke"}, "not the approved"},
		{"reads during apply are not bound", gate{phase: phaseApply, calls: permit.Calls, permit: permit}, protocol.HostRequest{Method: "GET", Path: "/vms/8"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := c.g
			g.catalog, g.upstream, g.now = catalog, ok, now
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
		{"refused after a change", []Request{change(Done), change(Refused)}, nil, OutcomePartly},
	}
	for _, c := range cases {
		if got := outcome(c.record, c.err); got.Outcome != c.want || (c.want != OutcomeDone) != (got.Err != nil) {
			t.Errorf("%s: %s %v, want %s", c.name, got.Outcome, got.Err, c.want)
		}
	}
}
