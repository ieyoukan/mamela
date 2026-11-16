package external

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/provider/host"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Runtime is the running providers of one plan or apply.
type Runtime struct {
	claims   map[string][]map[string]string // IDs other plans are to create, by provider
	adopting map[string][]protocol.Adopting // resources to adopt (imported), by provider
	sessions map[string]*host.Session
	programs []store.Program
	// endpoints identify, per provider, the API a plan's changes go to (URL, credential,
	// catalog): an approval is for that API only.
	endpoints map[string]string
	creations Creations
	// guardSettings are each provider's guard settings (mamela.providers.toml).
	guardSettings map[string][]byte
	// stack is the declaration being planned or applied (ADR 0013): the objects created from now
	// on are recorded for it, and only the objects it made are deleted when it no longer has them.
	stack string
	// warnings are for the operator: a provider that runs without a pin (MAM-PRV-0010).
	warnings []string
}

// Programs converts discovered packages into the programs a plan stores.
func Programs(pkgs []Package) []store.Program {
	var out []store.Program
	for _, p := range pkgs {
		out = append(out, store.Program{Provider: p.Provider, Version: p.ProviderVersion, Source: p.Source, Digest: p.Digest, Data: p.Program,
			Catalogs: p.Catalogs})
	}
	return out
}

// Start runs each program (WASM, isolated) with its API from the configuration.
func Start(ctx context.Context, programs []store.Program, cfg Config, wasmtime string) (*Runtime, error) {
	rt := &Runtime{sessions: map[string]*host.Session{}, programs: programs, endpoints: map[string]string{}, guardSettings: map[string][]byte{}}
	for _, p := range programs {
		// Before any of it runs: a program or a catalog the operator did not pin is not started
		// (ADR 0015). A tree chooses the Pomes it imports; the operator chooses what runs with the
		// operator's credentials.
		warnings, err := cfg.checkPins(p)
		if err != nil {
			rt.Close()
			return nil, err
		}
		rt.warnings = append(rt.warnings, warnings...)
		exe, err := host.InspectBytes(p.Source+" ("+p.Provider+")", p.Data)
		if err != nil {
			rt.Close()
			return nil, err
		}
		if exe.Format == host.FormatNative {
			rt.Close()
			return nil, fmt.Errorf("provider %s: a provider from a Pome must be WASM", p.Provider)
		}
		pc := cfg.withShipped(p.Provider, p.Catalogs)
		apis, endpoint, err := pc.apis(p.Provider)
		if err != nil {
			rt.Close()
			return nil, fmt.Errorf("provider %s: %w", p.Provider, err)
		}
		_, settings, err := pc.guard(p.Provider)
		if err != nil {
			rt.Close()
			return nil, err
		}
		rt.guardSettings[p.Provider] = settings
		s, err := host.Start(ctx, exe, host.Options{Wasmtime: wasmtime, APIs: apis})
		if err != nil {
			rt.Close()
			return nil, fmt.Errorf("provider %s: %w", p.Provider, err)
		}
		m := s.Manifest()
		if m.Provider != p.Provider || m.Version != p.Version {
			s.Close()
			rt.Close()
			return nil, fmt.Errorf("the program of %s %s describes itself as %s %s", p.Provider, p.Version, m.Provider, m.Version)
		}
		if err := checkEndpoints(m, apis); err != nil {
			s.Close()
			rt.Close()
			return nil, fmt.Errorf("provider %s: %w", p.Provider, err)
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
		rt.endpoints[p.Provider] = endpoint
	}
	return rt, nil
}

// Warnings are what the operator should read about the providers that started (a provider that is
// not pinned: MAM-PRV-0010). Nothing is stopped by them.
func (rt *Runtime) Warnings() []string {
	if rt == nil {
		return nil
	}
	return slices.Clone(rt.warnings)
}

// Creations is Mamela's record of what it set out to create through providers.
type Creations interface {
	Creations(ctx context.Context, provider, resource string) ([]map[string]string, error)
	CreationStack(ctx context.Context, provider, resource string) (string, error)
	CreationOrder(ctx context.Context, provider string) (map[string]int, error)
	RecordCreation(ctx context.Context, stack, provider, resource string, ids map[string]string, planDigest, operationID string) error
}

// SetClaims sets, by provider, the IDs other plans are to create: observing treats them as taken.
func (rt *Runtime) SetClaims(c map[string][]map[string]string) {
	if rt != nil {
		rt.claims = c
	}
}

// SetStack names the declaration being planned or applied.
func (rt *Runtime) SetStack(stack string) {
	if rt != nil {
		rt.stack = stack
	}
}

// Stack is the declaration being planned or applied.
func (rt *Runtime) Stack() string {
	if rt == nil {
		return ""
	}
	return rt.stack
}

// SetAdoptions sets, by provider, the declared resources to adopt (imported, ADR 0009): observing
// reports the imported object as adoptable for each.
func (rt *Runtime) SetAdoptions(a map[string][]protocol.Adopting) {
	if rt != nil {
		rt.adopting = a
	}
}

// SetCreations sets the record that vouches for providers' claims of ownership (required before
// observing).
func (rt *Runtime) SetCreations(c Creations) {
	if rt != nil {
		rt.creations = c
	}
}

// SetLimits sets the limits on what one change may make or change (the site's budget for one
// machine), checked when planning and again before each request is sent.
func (rt *Runtime) SetLimits(l map[string]int64) {
	if rt == nil {
		return
	}
	for _, s := range rt.sessions {
		s.SetLimits(l)
	}
}

// SetArtifacts sets what makes the artifacts of the providers' changes (ADR 0004).
func (rt *Runtime) SetArtifacts(m host.ArtifactMaker) {
	if rt == nil {
		return
	}
	for _, s := range rt.sessions {
		s.SetArtifacts(m)
	}
}

// GuardReport is one provider's safety check.
type GuardReport struct {
	Provider string
	Result   protocol.GuardResult
	Err      error
}

// Blocked reports whether a check stops the apply: a block finding, or a check that failed.
func (g GuardReport) Blocked() bool {
	return g.Err != nil || slices.ContainsFunc(g.Result.Findings, func(f protocol.Finding) bool { return f.Severity == protocol.SeverityBlock })
}

// Guard runs the safety check of each provider with a guard (ADR 0006), with the changes of it
// an apply carries out, and before's baselines for after.
func (rt *Runtime) Guard(ctx context.Context, stage string, changes map[string][]protocol.Change, baselines map[string][]byte) []GuardReport {
	var out []GuardReport
	for _, name := range rt.names() {
		s := rt.sessions[name]
		if s.Manifest().Guard == nil {
			continue
		}
		res, _, err := s.Guard(ctx, protocol.GuardParams{Stage: stage, Settings: rt.guardSettings[name], Baseline: baselines[name],
			Changes: append([]protocol.Change{}, changes[name]...)})
		out = append(out, GuardReport{Provider: name, Result: res, Err: err})
	}
	return out
}

// Session returns a running provider.
func (rt *Runtime) Session(name string) (*host.Session, bool) { return rt.sessionOf(name) }

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
		out = append(out, spec.External{Manifest: s.Manifest(), Program: s.Executable().Digest, Protocol: protocol.Version,
			Endpoint: rt.endpoints[name]})
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

// CheckGraph verifies that every provider a declaration uses is running, and handles the kind, and
// that a node resource on a provider's resource is on a machine (a node kind).
func (rt *Runtime) CheckGraph(g *graph.Graph) error {
	provided := map[string]graph.Resource{}
	for _, r := range g.Resources {
		if r.Provided != nil {
			provided[r.Name] = r
		}
	}
	for _, r := range g.Resources {
		m, ok := provided[spec.NodeOf(r)]
		if !ok {
			continue
		}
		s, running := rt.sessionOf(m.Provided.Provider)
		if running && !slices.ContainsFunc(s.Manifest().Kinds, func(k protocol.Kind) bool { return k.Kind == m.Kind && k.Node }) {
			return fmt.Errorf("%s is on %s, which is not a machine: provider %s does not declare kind %s a node kind", r.ID(), m.ID(), m.Provided.Provider, m.Kind)
		}
	}
	for _, r := range g.Resources {
		if r.Provided == nil {
			continue
		}
		s, ok := rt.sessionOf(r.Provided.Provider)
		if !ok {
			return fmt.Errorf("%s uses provider %s, which none of the declaration's dependencies provides (add the provider's Pome to Pome.toml)", r.ID(), r.Provided.Provider)
		}
		i := slices.IndexFunc(s.Manifest().Kinds, func(k protocol.Kind) bool { return k.Kind == r.Kind })
		if i < 0 {
			return fmt.Errorf("%s: provider %s does not handle kind %s", r.ID(), r.Provided.Provider, r.Kind)
		}
		if s.Manifest().Kinds[i].Node && !graph.ValidNodeName(r.Name) {
			return fmt.Errorf("%s is a machine, and %q is not a valid hostname: use 1-63 lowercase letters, digits and '-', starting and ending with a letter or digit", r.ID(), r.Name)
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

// Resource is a declared resource of a provider, as the provider receives it.
func Resource(r graph.Resource) protocol.Resource { return resource(r) }

func resource(r graph.Resource) protocol.Resource {
	return protocol.Resource{ID: r.ID(), Kind: r.Kind, Name: r.Name, Spec: r.Provided.Spec, After: r.After}
}

// Observe asks every provider for the state of its declared resources (and those it created
// that are no longer declared) and for its changes, and checks what they say.
func (rt *Runtime) Observe(ctx context.Context, g *graph.Graph, asOf time.Time) (map[string]state.Provided, error) {
	if err := rt.CheckGraph(g); err != nil {
		return nil, err
	}
	out := map[string]state.Provided{}
	declaredOf := map[string][]protocol.Resource{}
	observedOf := map[string][]protocol.Observed{}
	// 1. Every provider observes; its claims of ownership stand only if Mamela recorded that it
	// set out to create the object: the diff, the plan and what is kept see only what Mamela
	// vouches for.
	for _, name := range rt.names() {
		s := rt.sessions[name]
		var declared []protocol.Resource
		for _, r := range g.Sorted() {
			if r.Provided != nil && r.Provided.Provider == name {
				declared = append(declared, resource(r))
			}
		}
		var claimed []map[string]string
		if rt.claims != nil {
			claimed = rt.claims[name]
		}
		observed, _, err := s.ObserveWith(ctx, protocol.ObserveParams{Resources: declared, Claimed: claimed, Adopting: rt.adopting[name]})
		if err != nil {
			return nil, fmt.Errorf("provider %s: observe: %w", name, err)
		}
		if rt.creations == nil {
			return nil, fmt.Errorf("provider %s: no record of what Mamela created to check its claims against", name)
		}
		for i, o := range observed {
			created, err := rt.creations.Creations(ctx, name, o.ID)
			if err != nil {
				return nil, err
			}
			observed[i] = protocol.Vouch(o, created)
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
		// What the provider found that is not declared: never changed. What Mamela made for this
		// declaration is to be deleted (ADR 0013); the rest is kept.
		order, err := rt.creations.CreationOrder(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, o := range observed {
			if isDeclared[o.ID] {
				continue
			}
			id := graph.Resource{Kind: o.Kind, Name: o.Name}.ID()
			if _, taken := out[id]; taken || !o.Exists {
				continue
			}
			p := state.Provided{Provider: name, Kind: o.Kind, Name: o.Name, Observed: o, ObservedAt: asOf}
			if o.Owned {
				if err := rt.planRemoval(ctx, s, &p, order[o.ID]); err != nil {
					return nil, err
				}
			}
			out[id] = p
		}
		declaredOf[name], observedOf[name] = declared, observed
	}

	// 2. A resource whose dependencies do not all exist as Mamela's waits (ADR 0007): no change
	// of it is computed yet.
	ready := func(d protocol.Resource) string { return waitsFor(d, out) }

	// 3. Each provider computes the changes of what does not wait, with what its dependencies are.
	for _, name := range rt.names() {
		s := rt.sessions[name]
		var desired []protocol.Resource
		deps := map[string]protocol.Observed{}
		for _, d := range declaredOf[name] {
			if why := ready(d); why != "" {
				p := out[d.ID]
				p.Waiting = why
				out[d.ID] = p
				continue
			}
			desired = append(desired, d)
			for _, a := range d.After {
				deps[a] = out[a].Observed
			}
		}
		if len(deps) == 0 {
			deps = nil
		}
		changes, err := s.Diff(ctx, desired, observedOf[name], deps)
		if err != nil {
			return nil, fmt.Errorf("provider %s: diff: %w", name, err)
		}
		seen := map[string]bool{}
		for _, c := range changes {
			p, ok := out[c.Resource]
			if !ok || !p.Declared || p.Provider != name || p.Waiting != "" {
				return nil, fmt.Errorf("provider %s: diff changes %s, which is not one of its declared resources ready to change", name, c.Resource)
			}
			if key := c.Resource + "\x00" + string(c.Op); seen[key] {
				return nil, fmt.Errorf("provider %s: diff has %s on %s twice", name, c.Op, c.Resource)
			} else {
				seen[key] = true
			}
			if p.Observed.Exists && !protocol.Targets(p.Observed.IDs, c.IDs) {
				return nil, fmt.Errorf("provider %s: the change of %s names objects %v, not the observed %v", name, c.Resource, c.IDs, p.Observed.IDs)
			}
			if c.Precondition != "" && c.Precondition != p.Observed.Version {
				return nil, fmt.Errorf("provider %s: the change of %s is based on version %s, but it observed %s", name, c.Resource, c.Precondition, p.Observed.Version)
			}
			if err := s.CheckPreconditions(c, p.Observed.Version); err != nil {
				return nil, fmt.Errorf("provider %s: the change of %s: %w", name, c.Resource, err)
			}
			if err := s.CheckLimits(c); err != nil {
				code := "MAM-SITE-0305" // over the site's budget
				if errors.Is(err, protocol.ErrValueNotAllowed) {
					code = "MAM-PRV-0008" // a value the catalog does not allow
				}
				return nil, fmt.Errorf("%s: provider %s: the change of %s: %w", code, name, c.Resource, err)
			}
			pc := state.ProvidedChange{Change: c, Verified: rt.verify(s, c)}
			if o, ok := s.Manifest().Operation(c.Kind, c.Op); ok && o.Support == protocol.Unsupported {
				pc.Unsupported = o.Why
			}
			p.Changes = append(p.Changes, pc)
			out[c.Resource] = p
		}
	}
	return out, nil
}

// Verify classifies the requests a change of provider name sends, with the host's catalog.
func (rt *Runtime) Verify(name string, c protocol.Change) protocol.Verified {
	s, ok := rt.sessionOf(name)
	if !ok {
		return protocol.Verified{}
	}
	return rt.verify(s, c)
}

// verify classifies the requests a change sends, with the host's catalog.
func (rt *Runtime) verify(s *host.Session, c protocol.Change) protocol.Verified {
	o, _ := s.Manifest().Operation(c.Kind, c.Op)
	var calls []protocol.Call
	for _, a := range c.Requests {
		call, _, ok := protocol.FindCall(o.Calls, a.API, a.Method, a.Path, a.Params, false)
		if ok && !slices.ContainsFunc(calls, call.Same) {
			calls = append(calls, call)
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
	// PermitTTL bounds each permit (default 2 hours, the host's limit on one apply: creating a
	// machine from an image waits for the import). A permit is used once, by one apply.
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
	snap.Stack = p.Runtime.Stack()
	snap.AddProvided(provided)
	p.last = snap
	return snap, nil
}

// Apply implements reconcile.Provider.
func (p *Provider) Apply(ctx context.Context, step reconcile.Step) error {
	if step.Approved.Removes {
		return p.remove(ctx, step)
	}
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
		ttl = 2 * time.Hour
	}
	id := step.OperationID + "@" + now.Format(time.RFC3339Nano)
	permit, err := protocol.NewPermit(s.Manifest(), change, id, step.PlanDigest, p.Providers, now.Add(ttl).Format(time.RFC3339))
	if err != nil {
		return err
	}
	if err := p.Store.UsePermit(ctx, id, step.OperationID, step.PlanDigest); err != nil {
		return err
	}
	// Intent first: a change planned when its object did not exist creates it; that is recorded
	// before anything is sent, so that an object it leaves unfinished is still known to be
	// Mamela's (and nothing else is).
	if o, ok := p.last.Provided[r.ID()]; !ok || !o.Observed.Exists {
		if err := p.Store.RecordCreation(ctx, p.Runtime.Stack(), r.Provided.Provider, r.ID(), change.IDs, step.PlanDigest, step.OperationID); err != nil {
			return err
		}
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

// remove carries out the approved deletion of an object that no declaration has any more (ADR
// 0013). Like an explicit operation of a person (mamela act): the intent is recorded first, the
// change goes under a permit for exactly it, and once the object is observed to be gone, Mamela's
// records of it and the machine's registration end. An object that is gone already (an earlier run
// did it, or its answer was lost) only has the records finished.
func (p *Provider) remove(ctx context.Context, step reconcile.Step) error {
	c := step.Approved
	s, ok := p.Runtime.sessionOf(c.Provider)
	if !ok {
		return fmt.Errorf("%s: provider %s is not running", c.ID(), c.Provider)
	}
	i := slices.IndexFunc(c.ProviderChanges, func(x protocol.Change) bool { return x.Op == step.Op })
	if i < 0 {
		return fmt.Errorf("%s: the approved plan has no %s change", c.ID(), step.Op)
	}
	change := c.ProviderChanges[i]
	now := p.Clock().UTC()
	o, there := p.last.Provided[c.ID()]
	if !there || !o.Observed.Exists {
		return p.Store.FinishRemoval(ctx, c.Provider, c.ID(), string(step.Op))
	}
	if !o.Observed.Owned || o.Removal == nil || !plan.SameProviderChanges([]protocol.Change{o.Removal.Change}, []protocol.Change{change}) {
		return fmt.Errorf("%s is no longer as the plan assumed; nothing was deleted", c.ID())
	}
	ttl := p.PermitTTL
	if ttl == 0 {
		ttl = 2 * time.Hour
	}
	id := step.OperationID + "@" + now.Format(time.RFC3339Nano)
	if err := p.Store.StartObservedExplicit(ctx, id, c.Provider, string(step.Op), "plan "+step.PlanDigest, p.Providers, o.Observed, change); err != nil {
		return err
	}
	permit, err := protocol.NewPermit(s.Manifest(), change, id, step.PlanDigest, p.Providers, now.Add(ttl).Format(time.RFC3339))
	if err != nil {
		return err
	}
	if err := p.Store.UsePermit(ctx, id, step.OperationID, step.PlanDigest); err != nil {
		return err
	}
	res := s.Apply(ctx, permit, change, nil, &o.Observed)
	outcome := res.Outcome
	if outcome == host.OutcomeUnknown {
		// Whether it took effect is not known: ask the provider, never send it again blindly.
		if status, _, err := s.Lookup(ctx, change); err == nil && status == protocol.LookupDone {
			outcome = host.OutcomeDone
		}
	}
	detail := ""
	if res.Err != nil {
		detail = res.Err.Error()
	}
	recorded, err := p.Store.Explicit(ctx, id)
	if err != nil {
		return err
	}
	absent := false
	if outcome == host.OutcomeDone {
		if absent, err = Absent(ctx, s, p.Graph, c.Provider, c.ID()); err != nil {
			outcome, detail = host.OutcomeUnknown, "completion could not be verified: "+err.Error()
		}
	}
	if err := p.Store.CompleteExplicit(ctx, recorded, string(outcome), detail, absent); err != nil {
		return err
	}
	switch outcome {
	case host.OutcomeDone:
		if !absent {
			return fmt.Errorf("%s: the deletion was sent, but the object is still there", c.ID())
		}
		return nil
	case host.OutcomePartly:
		return fmt.Errorf("%v (%s): %w", res.Err, changed(res), reconcile.ErrPartlyDone)
	case host.OutcomeUnknown:
		return fmt.Errorf("%s (%s): %w", detail, changed(res), reconcile.ErrUnknownOutcome)
	}
	if res.Err == nil {
		return errors.New("the provider's deletion failed")
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

// checkEndpoints: each API the provider talks to that is configured has a catalog for that API
// (without one, Mamela cannot tell reads from changes and allows neither), and no API is
// configured that the provider does not have.
func checkEndpoints(m protocol.Manifest, apis map[string]host.API) error {
	names := m.APIs
	if len(names) == 0 && m.API != "" {
		names = []string{m.API}
	}
	for key, ep := range apis {
		name := key
		if key == "" {
			if len(m.APIs) > 0 {
				return fmt.Errorf("it talks to several APIs (%v): configure each in [provider.%s.apis.<name>]", m.APIs, m.Provider)
			}
			name = m.API
		}
		switch {
		case ep.Upstream != nil && !slices.Contains(names, name):
			return fmt.Errorf("API %q is configured, and the provider talks to %v", name, names)
		case ep.Upstream != nil && ep.Catalog.API == "":
			return fmt.Errorf("its API %s has no catalog, so Mamela cannot tell its reads from its changes and allows neither; "+
				"set catalog in the provider's section of mamela.providers.toml", name)
		case ep.Catalog.API != "" && ep.Catalog.API != name:
			return fmt.Errorf("the configured catalog is for API %q, the provider talks to %q", ep.Catalog.API, name)
		}
	}
	return nil
}

// waitsFor says what a resource waits for (ADR 0007): a dependency that does not exist yet, is not
// Mamela's, or whose creation did not finish (its IDs may not be all there: a create that was
// interrupted and is finished by applying again). "" when it waits for nothing.
func waitsFor(d protocol.Resource, provided map[string]state.Provided) string {
	for _, a := range d.After {
		dep, ok := provided[a]
		switch {
		case !ok || !dep.Observed.Exists:
			return fmt.Sprintf("waits for %s to be created", a)
		case !dep.Observed.Owned && dep.Observed.Adoptable:
			return fmt.Sprintf("waits for %s, which is to be adopted (mamela act)", a)
		case !dep.Observed.Owned:
			return fmt.Sprintf("waits for %s, which is not Mamela's (Foreign)", a)
		case dep.Observed.Unfinished:
			return fmt.Sprintf("waits for %s, whose creation did not finish (apply again to finish it)", a)
		}
	}
	return ""
}

// planRemoval decides what becomes of an object Mamela made that no declaration has any more (ADR
// 0013): its deletion when it was made for the declaration being planned and its kind has a
// removing operation; otherwise it is kept, with the reason.
func (rt *Runtime) planRemoval(ctx context.Context, s *host.Session, p *state.Provided, order int) error {
	keep := func(code, message, hint string) {
		p.Kept = &state.Kept{Code: code, Message: "not in this declaration; kept: " + message, Hint: hint}
	}
	op, hasOp := s.Manifest().RemoveOp(p.Kind)
	stack, err := rt.creations.CreationStack(ctx, p.Provider, p.Observed.ID)
	if err != nil {
		return err
	}
	switch {
	case !hasOp:
		keep("MAM-PLAN-0304", fmt.Sprintf("provider %s has no operation that deletes a %s", p.Provider, p.Kind),
			"delete it by hand, or with the provider's explicit operation if it has one (mamela act)")
	case stack == "":
		keep("MAM-PLAN-0303", "Mamela made it before it recorded which declaration made it",
			"claim it for the declaration that declares it (mamela claim), or delete it with its provider's explicit delete operation (mamela act)")
	case stack != rt.stack:
		keep("MAM-PLAN-0302", fmt.Sprintf("it was made for another declaration (%s), not for this one (%s)", stack, rt.stack),
			"remove it from the declaration that made it")
	default:
		change, err := s.Prepare(ctx, op.Op, nil, p.Observed)
		if err != nil {
			keep("MAM-PLAN-0306", fmt.Sprintf("its provider could not prepare the deletion (%v)", err), "")
			return nil
		}
		p.Removal = &state.ProvidedChange{Change: change, Verified: rt.verify(s, change)}
		p.RemovalOrder = order
	}
	return nil
}

// Absent says, from the provider's inventory (which includes the objects it made that are no
// longer declared), whether the object of a resource is gone. Not finding the resource at all
// confirms it only when the declaration does not declare it.
func Absent(ctx context.Context, session *host.Session, g *graph.Graph, provider, id string) (bool, error) {
	var resources []protocol.Resource
	declared := false
	for _, r := range g.Sorted() {
		if r.Provided != nil && r.Provided.Provider == provider {
			resources = append(resources, resource(r))
			declared = declared || r.ID() == id
		}
	}
	observed, _, err := session.ObserveWith(ctx, protocol.ObserveParams{Resources: resources})
	if err != nil {
		return false, err
	}
	for _, o := range observed {
		if o.ID == id {
			return !o.Exists, nil
		}
	}
	if declared {
		return false, fmt.Errorf("provider omitted declared resource %s; absence is unconfirmed", id)
	}
	return true, nil
}
