// Package providertest is the conformance suite for Mamela providers. A provider that passes it
// keeps Mamela's safety properties as far as a test can show:
//
//   - it describes a valid manifest, and declares every call it sends (the host refuses others);
//   - observe sends only declared reads, diff sends nothing and gives the same changes for the
//     same input;
//   - every change applies within its permit (no request is refused) and the world then matches
//     the declaration (a second diff is empty); a failed, refused or uncertain request is reported
//     as an error, never as success;
//   - resources it created stay visible when they are no longer declared, and are not changed;
//   - lookup says a planned change is not done before it is applied and done once the world
//     matches; when a change request is not sent, or is sent and its response is lost, lookup
//     tells the truth and observing and applying again converges.
//
// Use it from a test of the provider:
//
//	func TestConformance(t *testing.T) {
//		providertest.Run(t, providertest.Suite{
//			Provider: providertest.BuildWASI(t, "./cmd/my-provider"),
//			Catalog:  myapi.Catalog,
//			NewAPI:   func() http.Handler { return myapi.NewFake() },
//			Cases:    []providertest.Case{{Name: "one", Desired: ...}},
//		})
//	}
package providertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/provider/host"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Suite describes the provider under test.
type Suite struct {
	// Provider is the built provider: a WASI program (BuildWASI), or a native one with Native.
	Provider string
	// Native runs a native build. The suite trusts the build under test; Mamela itself runs a
	// native provider only after `mamela provider trust`.
	Native bool
	// Wasmtime is the pinned wasmtime (default: found like the mamela command finds it).
	Wasmtime string
	// Catalog is the host's catalog of the API: the effect of each call. For an API Mamela
	// does not know yet, the one you propose to add to Mamela.
	Catalog protocol.Catalog
	// NewAPI returns a fresh fake of the API, with an empty world.
	NewAPI func() http.Handler
	// Cases are declarations to converge to, each from an empty world.
	Cases []Case
}

// Case is one declaration.
type Case struct {
	Name    string
	Desired []protocol.Resource
}

// BuildWASI builds a Go provider for WASI (GOOS=wasip1 GOARCH=wasm) and returns the program.
func BuildWASI(t testing.TB, pkg string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "provider.wasm")
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s for WASI: %v\n%s", pkg, err, b)
	}
	return out
}

// Run runs the suite.
func Run(t *testing.T, s Suite) {
	t.Helper()
	if s.NewAPI == nil {
		t.Fatal("providertest: Suite.NewAPI is required")
	}
	t.Run("describe", func(t *testing.T) {
		e := s.start(t, -1, "")
		m := e.s.Manifest()
		if m.API != s.Catalog.API {
			t.Errorf("the manifest's api is %q, the catalog's %q", m.API, s.Catalog.API)
		}
		for _, k := range m.Kinds {
			for _, o := range k.Operations {
				if o.Support != protocol.Supported {
					continue
				}
				if len(o.Calls) == 0 {
					t.Errorf("%s %s declares no calls: it cannot do anything", k.Kind, o.Op)
				}
				if v := e.s.Verify(o); len(v.Unclassified) > 0 {
					t.Logf("%s %s: calls the catalog does not classify (always need a person's approval): %v", k.Kind, o.Op, v.Unclassified)
				}
			}
		}
	})
	for _, c := range s.Cases {
		t.Run(c.Name+"/converges", func(t *testing.T) {
			e := s.start(t, -1, "")
			e.converge(c.Desired, 1)
		})
		t.Run(c.Name+"/keeps-undeclared", func(t *testing.T) {
			e := s.start(t, -1, "")
			e.converge(c.Desired, 1)
			observed := e.observe(nil)
			for _, r := range c.Desired {
				if !hasOwned(observed, r) {
					t.Errorf("with nothing declared, observe does not report %s as an existing resource Mamela owns; Mamela must see what it created to keep it", r.ID)
				}
			}
			changes, err := e.s.Diff(e.ctx, nil, observed)
			if err != nil {
				t.Fatalf("diff with nothing declared: %v", err)
			}
			if len(changes) > 0 {
				t.Errorf("with nothing declared, diff changes %s: removing a declaration must not change or delete anything", changes[0].Resource)
			}
		})
		t.Run(c.Name+"/faults", func(t *testing.T) {
			e := s.start(t, -1, "")
			e.converge(c.Desired, 1)
			n := e.up.changes
			if n == 0 {
				t.Skip("the case sends no change")
			}
			for at := range n {
				for _, kind := range []string{faultNotSent, faultLost} {
					t.Run(fmt.Sprintf("%s-at-change-%d", kind, at), func(t *testing.T) {
						s.start(t, at, kind).recover(c.Desired)
					})
				}
			}
		})
	}
}

const (
	faultNotSent = "not-sent" // the request is not sent
	faultLost    = "lost"     // the request takes effect, and its response is lost
)

type env struct {
	t   *testing.T
	ctx context.Context
	s   *host.Session
	up  *faulty
	ops int
	// created is what was applied while its object did not exist, by resource: Mamela's record of
	// what it set out to create, which vouches for the provider's claims of ownership.
	created map[string][]map[string]string
	// lookups, when set, checks each change before it is applied.
	lookups func(protocol.Change)
}

func (s Suite) start(t *testing.T, at int, kind string) *env {
	t.Helper()
	exe, err := host.Inspect(s.Provider)
	if err != nil {
		t.Fatal(err)
	}
	o := host.Options{Catalog: s.Catalog, Wasmtime: s.Wasmtime, CallTimeout: 2 * time.Minute}
	if s.Native {
		if exe.Format != host.FormatNative {
			t.Fatalf("Native is set, and %s is %s", s.Provider, exe.Format)
		}
		o.Source, o.Version = "providertest", "under-test"
		o.Trust.Add(host.Trust{Source: o.Source, Version: o.Version, Digest: exe.Digest})
	} else if o.Wasmtime == "" {
		cwd, _ := os.Getwd()
		if o.Wasmtime, err = host.FindWasmtime(cwd); err != nil {
			t.Fatalf("no wasmtime to run the provider with: %v", err)
		}
	}
	up := &faulty{api: s.NewAPI(), catalog: s.Catalog, at: at, kind: kind}
	o.Upstream, o.Artifacts = up, fakeArtifacts{}
	ctx := t.Context()
	sess, err := host.Start(ctx, exe, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() && sess.Stderr() != "" {
			t.Logf("the provider's standard error:\n%s", sess.Stderr())
		}
		sess.Close()
	})
	return &env{t: t, ctx: ctx, s: sess, up: up, created: map[string][]map[string]string{}}
}

// observe observes and, as Mamela does, keeps the provider's claims of ownership only for what was
// created through it (protocol.Vouch).
func (e *env) observe(desired []protocol.Resource) []protocol.Observed {
	e.t.Helper()
	observed, _, err := e.s.Observe(e.ctx, desired)
	if err != nil {
		e.t.Fatalf("observe: %v", err)
	}
	for i, o := range observed {
		observed[i] = protocol.Vouch(o, e.created[o.ID])
	}
	return observed
}

// round observes, diffs and applies every change once.
func (e *env) round(desired []protocol.Resource) ([]protocol.Change, []host.Result) {
	e.t.Helper()
	observed := e.observe(desired)
	changes, err := e.s.Diff(e.ctx, desired, observed)
	if err != nil {
		e.t.Fatalf("diff: %v", err)
	}
	again, err := e.s.Diff(e.ctx, desired, observed)
	if err != nil {
		e.t.Fatalf("diff (again): %v", err)
	}
	if !sameJSON(changes, again) {
		e.t.Fatalf("diff gave different changes for the same input:\n%s\n%s", jsonText(changes), jsonText(again))
	}
	var results []host.Result
	for _, c := range changes {
		if e.lookups != nil {
			e.lookups(c)
		}
		results = append(results, e.apply(c, desired, observed))
	}
	return changes, results
}

func (e *env) apply(c protocol.Change, desired []protocol.Resource, observed []protocol.Observed) host.Result {
	e.t.Helper()
	e.ops++
	p, err := protocol.NewPermit(e.s.Manifest(), c, fmt.Sprintf("op-%d", e.ops), "sha256:plan", "sha256:providers",
		time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339))
	if err != nil {
		e.t.Fatalf("permit for %s %s: %v", c.Op, c.Resource, err)
	}
	var d *protocol.Resource
	for i := range desired {
		if desired[i].ID == c.Resource {
			d = &desired[i]
		}
	}
	var o *protocol.Observed
	for i := range observed {
		if observed[i].ID == c.Resource {
			o = &observed[i]
		}
	}
	if o == nil || !o.Exists {
		e.created[c.Resource] = append(e.created[c.Resource], c.IDs) // intent first, as Mamela records it
	}
	r := e.s.Apply(e.ctx, p, c, d, o)
	if r.Disbelieved {
		e.t.Errorf("%s %s: the provider reported success, and the host found otherwise: %v\n  requests:\n%s", c.Op, c.Resource, r.Err, requests(r.Requests))
	}
	return r
}

// converge applies the declaration in rounds; every apply must be done, and the world must match
// the declaration (an empty diff) after the given number of rounds. Lookup must say that a
// planned change is not done before it is applied, and done once the world matches.
func (e *env) converge(desired []protocol.Resource, rounds int) {
	e.t.Helper()
	var applied []protocol.Change
	e.lookups = func(c protocol.Change) {
		if status := e.lookup(c); status == protocol.LookupDone {
			e.t.Errorf("%s %s: lookup says %s before the change is applied", c.Op, c.Resource, status)
		}
	}
	for range rounds {
		changes, results := e.round(desired)
		for i, r := range results {
			if r.Outcome != host.OutcomeDone {
				e.t.Fatalf("%s %s: %s: %v\n  requests:\n%s", changes[i].Op, changes[i].Resource, r.Outcome, r.Err, requests(r.Requests))
			}
		}
		applied = append(applied, changes...)
	}
	e.lookups = nil
	if changes, _ := e.round(desired); len(changes) > 0 {
		e.t.Fatalf("after applying, the diff still has changes (the world does not match the declaration, or diff is wrong): %s", jsonText(changes))
	}
	for _, c := range applied {
		if status := e.lookup(c); status != protocol.LookupDone {
			e.t.Errorf("%s %s: the world matches the declaration, and lookup says %s", c.Op, c.Resource, status)
		}
	}
}

func (e *env) lookup(c protocol.Change) protocol.LookupStatus {
	e.t.Helper()
	status, _, err := e.s.Lookup(e.ctx, c)
	if err != nil {
		e.t.Fatalf("lookup of %s %s: %v", c.Op, c.Resource, err)
	}
	return status
}

// recover runs a round with the fault, checks what the host and lookup say about the faulted
// change, then requires that observing and applying again converges.
func (e *env) recover(desired []protocol.Resource) {
	e.t.Helper()
	observed := e.observe(desired)
	changes, err := e.s.Diff(e.ctx, desired, observed)
	if err != nil {
		e.t.Fatalf("diff: %v", err)
	}
	for _, c := range changes {
		before := e.up.hit
		r := e.apply(c, desired, observed)
		if before || !e.up.hit {
			continue
		}
		// This apply met the fault.
		if e.up.kind == faultLost && r.Outcome != host.OutcomeUnknown {
			e.t.Errorf("%s %s: the response of a change was lost, and the outcome is %s (want unknown)", c.Op, c.Resource, r.Outcome)
		}
		status := e.lookup(c)
		switch {
		case e.up.kind == faultLost && status == protocol.LookupNotDone:
			e.t.Errorf("%s %s: a change reached the API (its response was lost), and lookup says %s", c.Op, c.Resource, status)
		case r.Outcome == host.OutcomeFailed && status != protocol.LookupNotDone:
			e.t.Errorf("%s %s: no change took effect, and lookup says %s", c.Op, c.Resource, status)
		case r.Outcome == host.OutcomeDone && status != protocol.LookupDone:
			e.t.Errorf("%s %s: the apply is done, and lookup says %s", c.Op, c.Resource, status)
		}
	}
	if !e.up.hit {
		e.t.Fatalf("the fault was not reached (the provider sends fewer changes than in a clean run)")
	}
	e.up.at = -1
	for range 3 {
		changes, results := e.round(desired)
		if len(changes) == 0 {
			return
		}
		for i, r := range results {
			if r.Outcome != host.OutcomeDone {
				e.t.Fatalf("after the fault, %s %s: %s: %v\n  requests:\n%s", changes[i].Op, changes[i].Resource, r.Outcome, r.Err, requests(r.Requests))
			}
		}
	}
	e.t.Fatalf("after the fault, three rounds of observe, diff and apply did not converge")
}

// faulty sends requests to the fake API, and injects one fault at the at-th change request.
type faulty struct {
	api     http.Handler
	catalog protocol.Catalog
	at      int
	kind    string
	changes int
	hit     bool
}

func (f *faulty) Do(ctx context.Context, r protocol.HostRequest) (int, []byte, error) {
	return f.send(ctx, r, nil)
}

// Upload implements host.Uploader.
func (f *faulty) Upload(ctx context.Context, r protocol.HostRequest, file host.File) (int, []byte, error) {
	return f.send(ctx, r, &file)
}

func (f *faulty) send(ctx context.Context, r protocol.HostRequest, file *host.File) (int, []byte, error) {
	h := host.Handler{Handler: f.api}
	do := func() (int, []byte, error) {
		if file != nil {
			return h.Upload(ctx, r, *file)
		}
		return h.Do(ctx, r)
	}
	if isChange(f.catalog, r) {
		i := f.changes
		f.changes++
		if i == f.at {
			f.hit = true
			if f.kind == faultNotSent {
				return 0, nil, fmt.Errorf("%w: injected: connection refused", host.ErrNotSent)
			}
			do()
			return 0, nil, errors.New("injected: the connection was lost after the request was sent")
		}
	}
	return do()
}

// fakeArtifacts makes artifacts of fake content: a download claims its declared sha256 (the fake
// API does not check it), a seed gets a name and a sha256 of its own.
type fakeArtifacts struct{}

func (fakeArtifacts) Make(_ context.Context, a protocol.Artifact) (host.Prepared, error) {
	content := "fake " + string(a.Kind) + " " + a.ID
	info := protocol.ArtifactInfo{Name: a.Name, SHA256: a.SHA256, Size: int64(len(content))}
	if a.Kind == protocol.ArtifactSeed {
		sum := sha256.Sum256([]byte(content + a.Node))
		info.NodeID = a.NodeID
		if info.NodeID == 0 { // as Mamela does: a number of its own, the same for the same node
			info.NodeID = 1_000_000_000 + int64(binary.BigEndian.Uint16(sum[:2]))
		}
		info.Name, info.SHA256 = fmt.Sprintf("mamela-seed-%s-%d.iso", a.Node, info.NodeID), hex.EncodeToString(sum[:])
	}
	return host.Prepared{Info: info, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil }}, nil
}

// isChange: anything the catalog does not classify as a read.
func isChange(c protocol.Catalog, r protocol.HostRequest) bool {
	for _, e := range c.Entries {
		if e.Method != r.Method {
			continue
		}
		if p, err := protocol.ParsePath(e.Path); err == nil {
			if _, ok := p.Match(r.Path); ok {
				return e.Effect != protocol.EffectRead
			}
		}
	}
	return true
}

func hasOwned(observed []protocol.Observed, r protocol.Resource) bool {
	for _, o := range observed {
		if o.Kind == r.Kind && o.Name == r.Name && o.Exists && o.Owned {
			return true
		}
	}
	return false
}

func requests(rs []host.Request) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString("    " + r.String() + "\n")
	}
	return b.String()
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a, json.Deterministic(true))
	y, err2 := json.Marshal(b, json.Deterministic(true))
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func jsonText(v any) string {
	b, _ := json.Marshal(v, json.Deterministic(true))
	return string(b)
}
