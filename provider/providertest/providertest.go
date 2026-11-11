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
	"slices"
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
	// Catalogs are the catalogs of a provider with several APIs (Manifest.APIs), by name. Each
	// request reaches NewAPI's handler with its API in the header X-Mamela-Api.
	Catalogs map[string]protocol.Catalog
	// NewAPI returns a fresh fake of the API, with an empty world.
	NewAPI func() http.Handler
	// Cases are declarations to converge to, each from an empty world.
	Cases []Case
	// GuardSettings are the guard's settings (mamela.providers.toml [provider.<name>.guard]), for a
	// provider with a guard: it runs before and after each converge, and must find nothing to block.
	GuardSettings []byte
	// ScriptCredentials are the credentials' IDs and files the provider's scripts are written with
	// (with GuardSettings): each declared script must be written.
	ScriptCredentials protocol.ScriptCredentials
}

// Case is one declaration.
type Case struct {
	Name    string
	Desired []protocol.Resource
	// Explicit are explicit operations (ADR 0006) to carry out, in order, once the declaration has
	// converged: each must be done, and lookup must say so. An operation that adopts acts on an
	// object observed as adoptable; once it is done, Mamela records the object as its own, and the
	// declaration must converge again. A delete removes Mamela's record.
	Explicit []Explicit
	// World, when set, puts objects into the fresh fake API before anything runs: objects that
	// exist before Mamela (one to adopt, someone else's). A case with a world runs only its
	// converge with the guard, and its explicit operations.
	World func(api http.Handler)
}

// Explicit is one explicit operation on one declared resource.
type Explicit struct {
	Op       protocol.Op
	Resource string // the graph ID
}

// Catalog reads a catalog file the provider's Pome ships (ADR 0008), as Mamela reads it: test
// with the catalog you ship, never with one of your own making.
func Catalog(t testing.TB, path string) protocol.Catalog {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := protocol.DecodeCatalog(data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c
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
		switch {
		case len(m.APIs) > 0:
			for _, api := range m.APIs {
				if s.Catalogs[api].API != api {
					t.Errorf("the manifest's api %q has no catalog in Suite.Catalogs", api)
				}
			}
		case m.API != s.Catalog.API:
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
		for _, sc := range m.Scripts {
			if _, err := e.s.Script(e.ctx, protocol.ScriptParams{Name: sc.Name, Settings: s.GuardSettings, Credentials: s.ScriptCredentials}); err != nil {
				t.Errorf("script %s: %v", sc.Name, err)
			}
		}
	})
	for _, c := range s.Cases {
		s := s
		if c.World != nil {
			newAPI := s.NewAPI
			s.NewAPI = func() http.Handler {
				api := newAPI()
				c.World(api)
				return api
			}
		}
		t.Run(c.Name+"/converges", func(t *testing.T) {
			e := s.start(t, -1, "")
			before := e.guard(protocol.GuardBefore, nil, s.GuardSettings)
			e.converge(c.Desired, 1)
			e.guard(protocol.GuardAfter, before, s.GuardSettings)
		})
		if len(c.Explicit) > 0 {
			// Each explicit operation as it should go, then with its first change not sent, then
			// with the answer to it lost: lookup must never take for done what was not done, nor
			// for not done what took effect.
			for _, fault := range []string{"", faultNotSent, faultLost} {
				name := c.Name + "/explicit"
				if fault != "" {
					name += "-" + fault
				}
				t.Run(name, func(t *testing.T) {
					e := s.start(t, -1, "")
					e.converge(c.Desired, 1)
					for _, x := range c.Explicit {
						if !e.explicit(c, x, fault) {
							return
						}
					}
				})
			}
		}
		if c.World != nil {
			continue
		}
		t.Run(c.Name+"/keeps-undeclared", func(t *testing.T) {
			e := s.start(t, -1, "")
			e.converge(c.Desired, 1)
			observed := e.observe(nil)
			for _, r := range c.Desired {
				if !hasOwned(observed, r) {
					t.Errorf("with nothing declared, observe does not report %s as an existing resource Mamela owns; Mamela must see what it created to keep it", r.ID)
				}
			}
			changes, err := e.s.Diff(e.ctx, nil, observed, nil)
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
	o := host.Options{Wasmtime: s.Wasmtime, CallTimeout: 2 * time.Minute}
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
	catalogs := s.Catalogs
	if len(catalogs) == 0 {
		catalogs = map[string]protocol.Catalog{"": s.Catalog}
	}
	up := &faulty{api: s.NewAPI(), catalogs: catalogs, at: at, kind: kind}
	o.APIs = map[string]host.API{}
	for name, c := range catalogs {
		o.APIs[name] = host.API{Catalog: c, Upstream: up}
	}
	o.Artifacts = fakeArtifacts{}
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

// explicit carries out one explicit operation, with the fault on its first change, and checks
// what lookup says. It reports whether the sequence can go on (the operation is known done).
func (e *env) explicit(c Case, x Explicit, fault string) bool {
	t := e.t
	t.Helper()
	observed := e.observe(c.Desired)
	i := slices.IndexFunc(observed, func(o protocol.Observed) bool { return o.ID == x.Resource })
	if i < 0 {
		t.Fatalf("%s %s: not observed", x.Op, x.Resource)
	}
	op, _ := e.s.Manifest().Operation(observed[i].Kind, x.Op)
	switch {
	case op.Adopts && (!observed[i].Adoptable || observed[i].Owned):
		t.Fatalf("%s %s: adopts, and the resource is not observed as adoptable (and not owned): %s", x.Op, x.Resource, jsonText(observed[i]))
	case !op.Adopts && !observed[i].Owned:
		t.Fatalf("%s %s: not an observed resource Mamela owns", x.Op, x.Resource)
	}
	var d *protocol.Resource
	for j := range c.Desired {
		if c.Desired[j].ID == x.Resource {
			d = &c.Desired[j]
		}
	}
	change, err := e.s.Prepare(e.ctx, x.Op, d, observed[i])
	if err != nil {
		t.Fatalf("prepare %s %s: %v", x.Op, x.Resource, err)
	}
	if status := e.lookup(change); status == protocol.LookupDone {
		t.Errorf("%s %s: lookup says done before anything was sent", x.Op, x.Resource)
	}
	if fault != "" {
		e.up.at, e.up.kind, e.up.hit = e.up.changes, fault, false
	}
	r := e.apply(change, c.Desired, observed)
	e.up.at = -1
	status := e.lookup(change)
	switch {
	case fault == "" && r.Outcome != host.OutcomeDone:
		t.Fatalf("%s %s: %s: %v\n  requests:\n%s", x.Op, x.Resource, r.Outcome, r.Err, requests(r.Requests))
	case fault == "" && status != protocol.LookupDone && status != protocol.LookupUnknown:
		t.Errorf("%s %s: done, and lookup says %s", x.Op, x.Resource, status)
	case fault == faultNotSent && status == protocol.LookupDone:
		t.Errorf("%s %s: nothing was sent, and lookup says done", x.Op, x.Resource)
	case fault == faultLost && e.up.hit && status == protocol.LookupNotDone:
		t.Errorf("%s %s: the change took effect (its answer was lost), and lookup says not done", x.Op, x.Resource)
	}
	if fault != "" && r.Outcome == host.OutcomeDone && e.up.hit {
		t.Errorf("%s %s: a change failed, and the apply is done", x.Op, x.Resource)
	}
	if r.Outcome != host.OutcomeDone && status != protocol.LookupDone {
		return false // not known done: nothing more can be built on it
	}
	switch {
	case op.Adopts:
		e.created[x.Resource] = append(e.created[x.Resource], change.IDs)
		e.converge(c.Desired, 2)
	case slices.ContainsFunc(change.Requests, func(a protocol.Approved) bool {
		en, _ := e.up.catalogFor(a.API).LookupApproved(a)
		return en.Effect == protocol.EffectLoseData
	}):
		delete(e.created, x.Resource)
	}
	return true
}

// guard runs the provider's safety check (if it has one): it must not fail, and on a fake API
// with nothing outside the provider's reach, find nothing to block.
func (e *env) guard(stage string, baseline []byte, settings []byte) []byte {
	e.t.Helper()
	res, _, err := e.s.Guard(e.ctx, protocol.GuardParams{Stage: stage, Settings: settings, Baseline: baseline})
	if err != nil {
		e.t.Fatalf("guard %s: %v", stage, err)
	}
	for _, f := range res.Findings {
		if f.Severity == protocol.SeverityBlock {
			e.t.Errorf("guard %s blocks: %s %s", stage, f.Code, f.Message)
		}
	}
	return res.Baseline
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

// diff computes the changes as Mamela does (ADR 0007): a resource whose dependencies (After) do
// not all exist as Mamela's waits, and the others are given what their dependencies are. It
// returns how many resources wait.
func (e *env) diff(desired []protocol.Resource, observed []protocol.Observed) ([]protocol.Change, int, error) {
	byID := map[string]protocol.Observed{}
	for _, o := range observed {
		byID[o.ID] = o
	}
	var ready []protocol.Resource
	deps := map[string]protocol.Observed{}
	waiting := 0
	for _, d := range desired {
		ok := true
		for _, a := range d.After {
			if o := byID[a]; !o.Exists || !o.Owned || o.Unfinished {
				ok = false
			}
		}
		if !ok {
			waiting++
			continue
		}
		ready = append(ready, d)
		for _, a := range d.After {
			deps[a] = byID[a]
		}
	}
	if len(deps) == 0 {
		deps = nil
	}
	changes, err := e.s.Diff(e.ctx, ready, observed, deps)
	return changes, waiting, err
}

// round observes, diffs and applies every change once.
func (e *env) round(desired []protocol.Resource) ([]protocol.Change, []host.Result) {
	changes, results, _ := e.roundWaiting(desired)
	return changes, results
}

func (e *env) roundWaiting(desired []protocol.Resource) ([]protocol.Change, []host.Result, int) {
	e.t.Helper()
	observed := e.observe(desired)
	changes, waiting, err := e.diff(desired, observed)
	if err != nil {
		e.t.Fatalf("diff: %v", err)
	}
	again, _, err := e.diff(desired, observed)
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
	return changes, results, waiting
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
// the declaration (an empty diff, nothing waiting) after the given number of rounds, and as many
// more as resources wait for others (ADR 0007). Lookup must say that a
// planned change is not done before it is applied, and done once the world matches.
func (e *env) converge(desired []protocol.Resource, rounds int) {
	e.t.Helper()
	var applied []protocol.Change
	e.lookups = func(c protocol.Change) {
		if status := e.lookup(c); status == protocol.LookupDone {
			e.t.Errorf("%s %s: lookup says %s before the change is applied", c.Op, c.Resource, status)
		}
	}
	for i := 0; i < rounds+len(desired); i++ {
		changes, results, waiting := e.roundWaiting(desired)
		for i, r := range results {
			if r.Outcome != host.OutcomeDone {
				e.t.Fatalf("%s %s: %s: %v\n  requests:\n%s", changes[i].Op, changes[i].Resource, r.Outcome, r.Err, requests(r.Requests))
			}
		}
		applied = append(applied, changes...)
		if i+1 >= rounds && waiting == 0 {
			break
		}
	}
	e.lookups = nil
	if changes, _, waiting := e.roundWaiting(desired); len(changes) > 0 || waiting > 0 {
		e.t.Fatalf("after applying, the diff still has changes or resources wait (the world does not match the declaration, or diff is wrong): %d waiting, %s", waiting, jsonText(changes))
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
	// Round after round (a resource that waits for another is changed in a later round), until
	// the round that meets the fault.
	for round := 0; round <= len(desired) && !e.up.hit; round++ {
		e.recoverRound(desired)
	}
	if !e.up.hit {
		e.t.Fatalf("the fault was not reached (the provider sends fewer changes than in a clean run)")
	}
	e.up.at = -1
	for range 3 + len(desired) {
		changes, results, waiting := e.roundWaiting(desired)
		if len(changes) == 0 && waiting == 0 {
			return
		}
		for i, r := range results {
			if r.Outcome != host.OutcomeDone {
				e.t.Fatalf("after the fault, %s %s: %s: %v\n  requests:\n%s", changes[i].Op, changes[i].Resource, r.Outcome, r.Err, requests(r.Requests))
			}
		}
	}
	e.t.Fatalf("after the fault, rounds of observe, diff and apply did not converge")
}

func (e *env) recoverRound(desired []protocol.Resource) {
	e.t.Helper()
	observed := e.observe(desired)
	changes, _, err := e.diff(desired, observed)
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
}

// faulty sends requests to the fake API, and injects one fault at the at-th change request.
type faulty struct {
	api      http.Handler
	catalogs map[string]protocol.Catalog // by API ("" for a provider with one)
	at       int
	kind     string
	changes  int
	hit      bool
}

func (f *faulty) Do(ctx context.Context, r protocol.HostRequest) (int, []byte, error) {
	return f.send(ctx, r, nil)
}

// Upload implements host.Uploader.
func (f *faulty) Upload(ctx context.Context, r protocol.HostRequest, file host.File) (int, []byte, error) {
	return f.send(ctx, r, &file)
}

func (f *faulty) catalogFor(api string) protocol.Catalog {
	if c, ok := f.catalogs[api]; ok {
		return c
	}
	return f.catalogs[""]
}

func (f *faulty) send(ctx context.Context, r protocol.HostRequest, file *host.File) (int, []byte, error) {
	h := host.Handler{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Set("X-Mamela-Api", r.API) // which of the provider's APIs (ADR 0007)
		f.api.ServeHTTP(w, req)
	})}
	do := func() (int, []byte, error) {
		if file != nil {
			return h.Upload(ctx, r, *file)
		}
		return h.Do(ctx, r)
	}
	if isChange(f.catalogFor(r.API), r) {
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
// isChange says whether the catalog classifies a request as anything but a read: its method,
// path and fixed parameters (a query API's Action, an AWS JSON API's X-Amz-Target) find its entry.
func isChange(c protocol.Catalog, r protocol.HostRequest) bool {
	params, err := protocol.JSONParams(r.JSON)
	if err != nil {
		return true
	}
	for _, part := range []map[string][]string{r.Query, r.Form, r.Header} {
		for k, v := range part {
			params[k] = append(params[k], v...)
		}
	}
	var calls []protocol.Call
	for _, e := range c.Entries {
		calls = append(calls, protocol.Call{Method: e.Method, Path: e.Path, Fixed: e.Fixed})
	}
	call, _, ok := protocol.FindCall(calls, "", r.Method, r.Path, params, true)
	if !ok {
		return true
	}
	e, _ := c.LookupCall(call)
	return e.Effect != protocol.EffectRead
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
