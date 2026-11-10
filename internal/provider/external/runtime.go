package external

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/provider/host"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Runtime is the running providers of one plan or apply.
type Runtime struct {
	sessions map[string]*host.Session
	programs []store.Program
}

// Programs converts discovered packages into the programs a plan stores.
func Programs(pkgs []Package) []store.Program {
	var out []store.Program
	for _, p := range pkgs {
		out = append(out, store.Program{Provider: p.Provider, Version: p.ProviderVersion, Source: p.Source, Digest: p.Digest, Data: p.Program})
	}
	return out
}

// Start runs each program (WASM, isolated) with its API from the configuration.
func Start(ctx context.Context, programs []store.Program, cfg Config, wasmtime string) (*Runtime, error) {
	rt := &Runtime{sessions: map[string]*host.Session{}, programs: programs}
	for _, p := range programs {
		exe, err := host.InspectBytes(p.Source+" ("+p.Provider+")", p.Data)
		if err != nil {
			rt.Close()
			return nil, err
		}
		if exe.Format == host.FormatNative {
			rt.Close()
			return nil, fmt.Errorf("provider %s: a provider from a Pome must be WASM", p.Provider)
		}
		up, catalog, err := cfg.api(p.Provider)
		if err != nil {
			rt.Close()
			return nil, err
		}
		s, err := host.Start(ctx, exe, host.Options{Wasmtime: wasmtime, Catalog: catalog, Upstream: up})
		if err != nil {
			rt.Close()
			return nil, fmt.Errorf("provider %s: %w", p.Provider, err)
		}
		m := s.Manifest()
		switch {
		case m.Provider != p.Provider || m.Version != p.Version:
			s.Close()
			rt.Close()
			return nil, fmt.Errorf("the program of %s %s describes itself as %s %s", p.Provider, p.Version, m.Provider, m.Version)
		case up != nil && catalog.API == "":
			s.Close()
			rt.Close()
			return nil, fmt.Errorf("provider %s: its API %s has no catalog, so Mamela cannot tell its reads from its changes and allows neither; "+
				"set catalog in the provider's section of mamela.providers.toml", p.Provider, m.API)
		case catalog.API != "" && catalog.API != m.API:
			s.Close()
			rt.Close()
			return nil, fmt.Errorf("provider %s: the configured catalog is for API %q, the provider talks to %q", p.Provider, catalog.API, m.API)
		}
		for _, k := range m.Kinds {
			if graph.Builtin(k.Kind) {
				s.Close()
				rt.Close()
				return nil, fmt.Errorf("provider %s declares the built-in kind %s", p.Provider, k.Kind)
			}
			for other, os := range rt.sessions {
				if slices.ContainsFunc(os.Manifest().Kinds, func(x protocol.Kind) bool { return x.Kind == k.Kind }) {
					s.Close()
					rt.Close()
					return nil, fmt.Errorf("kind %s is provided by both %s and %s", k.Kind, other, p.Provider)
				}
			}
		}
		rt.sessions[p.Provider] = s
	}
	return rt, nil
}

// Close stops every provider.
func (rt *Runtime) Close() {
	if rt == nil {
		return
	}
	for _, s := range rt.sessions {
		s.Close()
	}
}

// Programs are the programs running.
func (rt *Runtime) Programs() []store.Program {
	if rt == nil {
		return nil
	}
	return rt.programs
}

// Manifests are the providers' manifests.
func (rt *Runtime) Manifests() spec.Providers {
	var out spec.Providers
	for _, name := range rt.names() {
		out = append(out, rt.sessions[name].Manifest())
	}
	return out
}

// Refs identify the providers for the plan's providers digest.
func (rt *Runtime) Refs() []spec.External {
	var out []spec.External
	for _, name := range rt.names() {
		s := rt.sessions[name]
		out = append(out, spec.External{Manifest: s.Manifest(), Program: s.Executable().Digest, Protocol: protocol.Version})
	}
	return out
}

func (rt *Runtime) names() []string {
	if rt == nil {
		return nil
	}
	var out []string
	for n := range rt.sessions {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// CheckGraph verifies that every provider a declaration uses is running, and handles the kind.
func (rt *Runtime) CheckGraph(g *graph.Graph) error {
	for _, r := range g.Resources {
		if r.Provided == nil {
			continue
		}
		s, ok := rt.sessionOf(r.Provided.Provider)
		if !ok {
			return fmt.Errorf("%s uses provider %s, which none of the declaration's dependencies provides (add the provider's Pome to Pome.toml)", r.ID(), r.Provided.Provider)
		}
		if !slices.ContainsFunc(s.Manifest().Kinds, func(k protocol.Kind) bool { return k.Kind == r.Kind }) {
			return fmt.Errorf("%s: provider %s does not handle kind %s", r.ID(), r.Provided.Provider, r.Kind)
		}
	}
	return nil
}

func (rt *Runtime) sessionOf(name string) (*host.Session, bool) {
	if rt == nil {
		return nil, false
	}
	s, ok := rt.sessions[name]
	return s, ok
}

func resource(r graph.Resource) protocol.Resource {
	return protocol.Resource{ID: r.ID(), Kind: r.Kind, Name: r.Name, Spec: r.Provided.Spec}
}

// Observe asks every provider for the state of its declared resources (and those it created
// that are no longer declared) and for its changes, and checks what they say.
func (rt *Runtime) Observe(ctx context.Context, g *graph.Graph, asOf time.Time) (map[string]state.Provided, error) {
	if err := rt.CheckGraph(g); err != nil {
		return nil, err
	}
	out := map[string]state.Provided{}
	for _, name := range rt.names() {
		s := rt.sessions[name]
		var declared []protocol.Resource
		for _, r := range g.Sorted() {
			if r.Provided != nil && r.Provided.Provider == name {
				declared = append(declared, resource(r))
			}
		}
		observed, _, err := s.Observe(ctx, declared)
		if err != nil {
			return nil, fmt.Errorf("provider %s: observe: %w", name, err)
		}
		changes, err := s.Diff(ctx, declared, observed)
		if err != nil {
			return nil, fmt.Errorf("provider %s: diff: %w", name, err)
		}
		byID := map[string]protocol.Observed{}
		for _, o := range observed {
			if _, dup := byID[o.ID]; dup {
				return nil, fmt.Errorf("provider %s: observe reported %s twice", name, o.ID)
			}
			byID[o.ID] = o
		}
		isDeclared := map[string]bool{}
		for _, d := range declared {
			isDeclared[d.ID] = true
			o, ok := byID[d.ID]
			if !ok {
				return nil, fmt.Errorf("provider %s: observe did not report the declared %s", name, d.ID)
			}
			if o.Kind != d.Kind || o.Name != d.Name {
				return nil, fmt.Errorf("provider %s: observe reported %s as %s %s", name, d.ID, o.Kind, o.Name)
			}
			out[d.ID] = state.Provided{Provider: name, Kind: d.Kind, Name: d.Name, Declared: true, Observed: o,
				SpecDigest: state.SpecDigest(d.Spec), ObservedAt: asOf}
		}
		// What the provider found that is not declared: kept, never changed.
		for _, o := range observed {
			if isDeclared[o.ID] {
				continue
			}
			id := graph.Resource{Kind: o.Kind, Name: o.Name}.ID()
			if _, taken := out[id]; taken || !o.Exists {
				continue
			}
			out[id] = state.Provided{Provider: name, Kind: o.Kind, Name: o.Name, Observed: o, ObservedAt: asOf}
		}
		seen := map[string]bool{}
		for _, c := range changes {
			p, ok := out[c.Resource]
			if !ok || !p.Declared || p.Provider != name {
				return nil, fmt.Errorf("provider %s: diff changes %s, which is not one of its declared resources", name, c.Resource)
			}
			if key := c.Resource + "\x00" + string(c.Op); seen[key] {
				return nil, fmt.Errorf("provider %s: diff has %s on %s twice", name, c.Op, c.Resource)
			} else {
				seen[key] = true
			}
			if c.Precondition != "" && c.Precondition != p.Observed.Version {
				return nil, fmt.Errorf("provider %s: the change of %s is based on version %s, but it observed %s", name, c.Resource, c.Precondition, p.Observed.Version)
			}
			p.Changes = append(p.Changes, state.ProvidedChange{Change: c, Verified: rt.verify(s, c)})
			out[c.Resource] = p
		}
	}
	return out, nil
}

// verify classifies the requests a change sends, with the host's catalog.
func (rt *Runtime) verify(s *host.Session, c protocol.Change) protocol.Verified {
	o, _ := s.Manifest().Operation(c.Kind, c.Op)
	var calls []protocol.Call
	for _, a := range c.Requests {
		for _, call := range o.Calls {
			if call.Method == a.Method && call.Path == a.Path && !slices.ContainsFunc(calls, func(x protocol.Call) bool { return x.Method == call.Method && x.Path == call.Path }) {
				calls = append(calls, call)
			}
		}
	}
	return s.Verify(protocol.Operation{Op: c.Op, Support: protocol.Supported, Calls: calls})
}

// Provider is the reconcile.Provider of a plan with resources of external providers: they go to
// their providers, everything else to Base.
type Provider struct {
	Base      reconcile.Provider // the built-in providers; nil when the plan has no built-in change
	Runtime   *Runtime
	Graph     *graph.Graph // the plan's declaration
	Store     *store.Store // records each permit, once
	Clock     func() time.Time
	Providers string // the providers digest, written into each permit
	// PermitTTL bounds each permit (default 10 minutes).
	PermitTTL time.Duration

	last state.Snapshot
}

// Observe implements reconcile.Provider.
func (p *Provider) Observe(ctx context.Context) (state.Snapshot, error) {
	snap := state.Empty(p.Clock())
	if p.Base != nil {
		var err error
		if snap, err = p.Base.Observe(ctx); err != nil {
			return state.Snapshot{}, err
		}
	}
	provided, err := p.Runtime.Observe(ctx, p.Graph, snap.AsOf)
	if err != nil {
		return state.Snapshot{}, err
	}
	if len(provided) > 0 {
		snap.Provided = provided
	}
	p.last = snap
	return snap, nil
}

// Apply implements reconcile.Provider.
func (p *Provider) Apply(ctx context.Context, step reconcile.Step) error {
	r := step.Resource
	if r.Provided == nil {
		if p.Base == nil {
			return fmt.Errorf("%s: no provider for built-in kinds in this run", r.ID())
		}
		return p.Base.Apply(ctx, step)
	}
	s, ok := p.Runtime.sessionOf(r.Provided.Provider)
	if !ok {
		return fmt.Errorf("%s: provider %s is not running", r.ID(), r.Provided.Provider)
	}
	i := slices.IndexFunc(step.Approved.ProviderChanges, func(c protocol.Change) bool { return c.Op == step.Op })
	if i < 0 {
		return fmt.Errorf("%s: the approved plan has no %s change", r.ID(), step.Op)
	}
	change := step.Approved.ProviderChanges[i]
	now := p.Clock().UTC()
	ttl := p.PermitTTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	id := step.OperationID + "@" + now.Format(time.RFC3339Nano)
	permit, err := protocol.NewPermit(s.Manifest(), change, id, step.PlanDigest, p.Providers, now.Add(ttl).Format(time.RFC3339))
	if err != nil {
		return err
	}
	if err := p.Store.UsePermit(ctx, id, step.OperationID, step.PlanDigest); err != nil {
		return err
	}
	desired := resource(r)
	var observed *protocol.Observed
	if o, ok := p.last.Provided[r.ID()]; ok {
		observed = &o.Observed
	}
	res := s.Apply(ctx, permit, change, &desired, observed)
	switch res.Outcome {
	case host.OutcomeDone:
		return nil
	case host.OutcomePartly:
		return fmt.Errorf("%v (%s): %w", res.Err, changed(res), reconcile.ErrPartlyDone)
	case host.OutcomeUnknown:
		return fmt.Errorf("%v (%s): %w", res.Err, changed(res), reconcile.ErrUnknownOutcome)
	}
	if res.Err == nil {
		return errors.New("the provider's apply failed")
	}
	return res.Err
}

func changed(r host.Result) string {
	var out []string
	for _, q := range r.Changed() {
		out = append(out, q.Method+" "+q.Path+" "+string(q.Outcome))
	}
	if len(out) == 0 {
		return "no change took effect"
	}
	return "sent: " + strings.Join(out, ", ")
}
