// Package kit writes a provider from its kinds (ADR 0009). A provider's author writes, for each
// kind of resource, how to list its objects, what changes a declaration needs, and how to tell
// whether a request is in effect; kit does the rest of the protocol: the manifest, observing
// (declared, missing and undeclared objects), the diff of every declared resource with its
// dependencies, applying a change's requests in order and at most once, looking up what is in
// effect, explicit operations and the guard.
//
//	func main() { sdk.Serve(&kit.Provider{Name: "kv", Version: "0.1.0", API: "kv/v1", Kinds: []kit.Kind{itemKind{}}}) }
//
// What kit does not decide stays the author's, and Mamela still checks all of it: every request
// against the catalog and the change's permit, ownership against Mamela's records.
package kit

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// Object is one object of a kind, as its List found it.
type Object struct {
	// Name is the name it was declared with (from its marker or tag); Scope, when the kind has
	// one (Scoped), what else a declaration names it by (an AWS boundary).
	Name, Scope string
	// IDs identify it at the API (and are what Mamela records and vouches for); Version is its
	// version, for preconditions; State is what Diff compares (encoded as JSON).
	IDs     map[string]string
	Version string
	State   any
	// Unfinished: a create that was interrupted (marked, not finished). Mamela keeps the claim only
	// for an object it recorded creating; what waits for it waits.
	Unfinished bool
	// Adoptable: made by something else and recognized as the declared one (an import, ADR 0009):
	// an explicit operation that adopts may take it over.
	Adoptable bool
	// Foreign: someone else's object with a declared name (no mark of Mamela's). A declaration of
	// it is refused: Mamela neither changes nor takes it over. List may leave it out, but then a
	// declaration of it reads as one to create.
	Foreign bool
}

// Kind is one kind of resource.
type Kind interface {
	// Kind is the resource kind ("KVItem").
	Kind() protocol.ResourceKind
	// Reads are the calls that list its objects. Observing uses them, and so do applying and looking
	// up (kit adds them to every operation).
	Reads() []protocol.Call
	// Operations are its changes and explicit operations, with the calls each sends (not the reads).
	Operations() []protocol.Operation
	// List returns every object of the kind this provider made (marked, tagged).
	List(c *Client) ([]Object, error)
	// Diff returns the changes that make the world what a declaration says. It reads nothing:
	// what it needs is in Desired. The same Desired gives the same changes.
	Diff(d Desired) ([]protocol.Change, error)
	// Done says whether an approved request is in effect already: apply skips it, and lookup
	// counts it. It must never say done for what was not done.
	Done(c *Client, ch protocol.Change, a protocol.Approved) (bool, error)
}

// Missing is a kind that observes something for a declared resource that does not exist yet
// (what creating it needs: free IDs, an image's ID).
type Missing interface {
	Missing(c *Client, r protocol.Resource) (any, error)
}

// Absent is a kind whose declared resources that do not exist yet still have a version (what a
// create is based on, its precondition) or IDs.
type Absent interface {
	Absent(r protocol.Resource) (version string, ids map[string]string)
}

// Scoped is a kind whose declarations are told apart by a scope as well as by name.
type Scoped interface {
	Scope(r protocol.Resource) (string, error)
}

// Explicit is a kind with explicit operations (delete, reboot, adopt): never planned, prepared
// when a person asks (mamela act).
type Explicit interface {
	Prepare(op protocol.Op, o protocol.Observed, desired *protocol.Resource) (protocol.Change, error)
}

// Documented is a kind that says what a resource of it is (for its generated Pome).
type Documented interface {
	Doc() string
}

// Specified is a kind whose spec is a Go struct: its fields become the manifest's spec (from json
// and mamela tags, see SpecOf), from which the kind's Pome is generated.
type Specified interface {
	Spec() any
}

// Node is a kind whose resources are machines mamelet runs on (ADR 0004).
type Node interface {
	IsNode() bool
}

// Desired is what Diff computes a resource's changes from.
type Desired struct {
	Resource protocol.Resource
	Observed protocol.Observed
	// Dependencies are the observations of the resources it needs first (after), by graph ID.
	Dependencies map[string]protocol.Observed
}

// Spec decodes the declared spec (rejecting unknown members).
func (d Desired) Spec(v any) error {
	if err := json.Unmarshal(d.Resource.Spec, v, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("%s: spec: %w", d.Resource.ID, err)
	}
	return nil
}

// State decodes the observed state (nothing when there is none).
func (d Desired) State(v any) error {
	if len(d.Observed.State) == 0 {
		return nil
	}
	return json.Unmarshal(d.Observed.State, v)
}

// Dep is the observation of a dependency, by kind and name: an error tells the person to declare
// it, and this resource after it.
func (d Desired) Dep(k protocol.ResourceKind, name string) (protocol.Observed, error) {
	o, ok := d.Dependencies[string(k)+" "+name]
	if !ok || !o.Exists {
		return o, fmt.Errorf("%s: %s %s is not among its dependencies (declare it, and this one after it)", d.Resource.ID, k, name)
	}
	return o, nil
}

// Provider is a provider made of kinds. It implements sdk.Provider, sdk.Preparer, sdk.Guarder and
// sdk.Scripter.
type Provider struct {
	Name, Version string
	// API is the API it talks to; APIs, the APIs of a provider that uses several (ADR 0007).
	API  string
	APIs []string
	// ObserveNeeds are what observing needs of the credentials (shown by mamela check).
	ObserveNeeds []protocol.Need
	Kinds        []Kind
	// GuardFunc, when set, is the provider's safety check around applies (ADR 0006), with the reads
	// it sends (GuardCalls, beside every kind's reads).
	GuardFunc  func(c *Client, p protocol.GuardParams) (protocol.GuardResult, error)
	GuardCalls []protocol.Call
	// Scripts are setup scripts it writes (ScriptFunc writes them).
	Scripts    []protocol.Script
	ScriptFunc func(p protocol.ScriptParams) (protocol.ScriptResult, error)
	// ParseError makes an API's answer that is not a success an error (default: *APIError).
	ParseError func(status int, reason, body string) error
	// AlreadyDone says whether an error means the request's effect is there already (a create
	// sent again after its answer was lost: "already exists"). Apply then goes on.
	AlreadyDone func(err error) bool
	// AfterSend runs after each request is sent (to wait for an asynchronous task the answer names).
	AfterSend func(c *Client, ch protocol.Change, a protocol.Approved, body []byte) error
}

func (p *Provider) kind(k protocol.ResourceKind) (Kind, bool) {
	for _, x := range p.Kinds {
		if x.Kind() == k {
			return x, true
		}
	}
	return nil, false
}

// reads are the reads of every kind, each once.
func (p *Provider) reads() []protocol.Call {
	var out []protocol.Call
	for _, k := range p.Kinds {
		for _, c := range k.Reads() {
			if !slices.ContainsFunc(out, c.Same) {
				out = append(out, c)
			}
		}
	}
	return out
}

// Manifest implements sdk.Provider.
func (p *Provider) Manifest() protocol.Manifest {
	m := protocol.Manifest{Schema: protocol.Schema, Provider: p.Name, Version: p.Version, API: p.API, APIs: p.APIs,
		Observe: p.ObserveNeeds, ObserveCalls: p.reads(), Scripts: p.Scripts}
	if p.GuardFunc != nil {
		calls := p.reads()
		for _, c := range p.GuardCalls {
			if !slices.ContainsFunc(calls, c.Same) {
				calls = append(calls, c)
			}
		}
		m.Guard = &protocol.Guard{Calls: calls}
	}
	for _, k := range p.Kinds {
		mk := protocol.Kind{Kind: k.Kind()}
		if n, ok := k.(Node); ok {
			mk.Node = n.IsNode()
		}
		if d, ok := k.(Documented); ok {
			mk.Doc = d.Doc()
		}
		if im, ok := k.(Importer); ok {
			mk.Imports = im.Imports()
		}
		if s, ok := k.(Specified); ok {
			mk.Spec = SpecOf(s.Spec())
		}
		for _, o := range k.Operations() {
			if o.Support == protocol.Supported {
				// An operation reads what it changes: to skip what is in effect, and to look it up.
				var calls []protocol.Call
				for _, c := range append(slices.Clone(k.Reads()), o.Calls...) {
					if !slices.ContainsFunc(calls, c.Same) {
						calls = append(calls, c)
					}
				}
				o.Calls = calls
			}
			mk.Operations = append(mk.Operations, o)
		}
		m.Kinds = append(m.Kinds, mk)
	}
	return m
}

func (p *Provider) client(ctx context.Context, h *sdk.Host, claimed []map[string]string) *Client {
	var calls []protocol.Call
	for _, k := range p.Kinds {
		calls = append(calls, k.Reads()...)
		for _, o := range k.Operations() {
			calls = append(calls, o.Calls...)
		}
	}
	calls = append(calls, p.GuardCalls...)
	return &Client{ctx: ctx, h: h, calls: calls, Claimed: claimed, parse: p.ParseError}
}

// Observe implements sdk.Provider: every object of every kind is listed; a declared resource is
// the object of its kind with its name (and scope); what does not exist is told what creating it
// needs (Missing); what the provider made and is no longer declared is reported, for Mamela to keep.
func (p *Provider) Observe(ctx context.Context, h *sdk.Host, in protocol.ObserveParams) (protocol.ObserveResult, error) {
	c := p.client(ctx, h, in.Claimed)
	var out protocol.ObserveResult
	byKind := map[protocol.ResourceKind][]Object{}
	for _, k := range p.Kinds {
		objs, err := k.List(c)
		if err != nil {
			return out, fmt.Errorf("%s: %w", k.Kind(), err)
		}
		byKind[k.Kind()] = objs
	}
	reported := map[string]bool{}
	taken := map[protocol.ResourceKind]map[int]bool{}
	adopting := map[string]map[string]string{}
	for _, a := range in.Adopting {
		adopting[a.Resource] = a.IDs
	}
	for _, r := range in.Resources {
		k, ok := p.kind(r.Kind)
		if !ok {
			return out, fmt.Errorf("%s: kind %s is not this provider's", r.ID, r.Kind)
		}
		scope := ""
		if s, ok := k.(Scoped); ok {
			var err error
			if scope, err = s.Scope(r); err != nil {
				return out, fmt.Errorf("%s: %w", r.ID, err)
			}
		}
		o := protocol.Observed{ID: r.ID, Kind: r.Kind, Name: r.Name}
		if taken[r.Kind] == nil {
			taken[r.Kind] = map[int]bool{}
		}
		i := slices.IndexFunc(byKind[r.Kind], func(x Object) bool { return x.Name == r.Name && x.Scope == scope })
		j := -1
		if ids, ok := adopting[r.ID]; ok && (i < 0 || byKind[r.Kind][i].Foreign) {
			j = adoptable(byKind[r.Kind], taken[r.Kind], ids)
			if f, ok := k.(Finder); ok && j < 0 {
				// Not listed (it carries no mark of Mamela's yet): found by its IDs.
				x, err := f.Find(c, ids)
				if err != nil {
					return out, fmt.Errorf("%s: finding the imported object: %w", r.ID, err)
				}
				if x != nil && protocol.Targets(x.IDs, ids) {
					byKind[r.Kind] = append(byKind[r.Kind], *x)
					j = len(byKind[r.Kind]) - 1
				}
			}
		}
		switch {
		case j >= 0:
			// Imported to adopt (ADR 0009): made by another tool, and Mamela's once adopted.
			taken[r.Kind][j] = true
			fill(&o, byKind[r.Kind][j])
			o.Owned, o.Unfinished, o.Adoptable = false, false, true
		case i >= 0:
			taken[r.Kind][i] = true
			fill(&o, byKind[r.Kind][i])
		default:
			if a, ok := k.(Absent); ok {
				o.Version, o.IDs = a.Absent(r)
			}
			if m, ok := k.(Missing); ok {
				st, err := m.Missing(c, r)
				if err != nil {
					return out, fmt.Errorf("%s: %w", r.ID, err)
				}
				if st != nil {
					o.State, _ = json.Marshal(st, json.Deterministic(true))
				}
			}
		}
		reported[r.ID] = true
		out.Resources = append(out.Resources, o)
	}
	for _, k := range p.Kinds {
		for i, x := range byKind[k.Kind()] {
			id := string(k.Kind()) + " " + x.Name
			if taken[k.Kind()][i] || reported[id] || x.Adoptable || x.Foreign {
				continue
			}
			reported[id] = true
			o := protocol.Observed{ID: id, Kind: k.Kind(), Name: x.Name}
			fill(&o, x)
			out.Resources = append(out.Resources, o)
		}
	}
	return out, nil
}

func fill(o *protocol.Observed, x Object) {
	o.Exists, o.Version = true, x.Version
	o.Owned, o.Unfinished, o.Adoptable = !x.Unfinished && !x.Adoptable && !x.Foreign, x.Unfinished && !x.Foreign, x.Adoptable && !x.Foreign
	o.IDs = maps.Clone(x.IDs)
	if x.State != nil {
		o.State, _ = json.Marshal(x.State, json.Deterministic(true))
	}
}

// Diff implements sdk.Provider: each declared resource in order of its ID, except what exists and
// is not Mamela's.
func (p *Provider) Diff(_ context.Context, in protocol.DiffParams) (protocol.DiffResult, error) {
	obs := map[string]protocol.Observed{}
	for _, o := range in.Observed {
		obs[o.ID] = o
	}
	desired := slices.Clone(in.Desired)
	slices.SortFunc(desired, func(a, b protocol.Resource) int { return strings.Compare(a.ID, b.ID) })
	var out protocol.DiffResult
	for _, r := range desired {
		k, ok := p.kind(r.Kind)
		if !ok {
			return out, fmt.Errorf("%s: kind %s is not this provider's", r.ID, r.Kind)
		}
		o := obs[r.ID]
		switch {
		case !o.Exists || o.Owned || o.Unfinished:
		case o.Adoptable:
			continue // to adopt (an explicit operation): nothing is planned for it until then
		default:
			return out, fmt.Errorf("%s exists and Mamela did not create it: Mamela changes only what it created; "+
				"declare it under another name, or remove the declaration", r.ID)
		}
		cs, err := k.Diff(Desired{Resource: r, Observed: o, Dependencies: in.Dependencies})
		if err != nil {
			return out, err
		}
		out.Changes = append(out.Changes, cs...)
	}
	return out, nil
}

// Prepare implements sdk.Preparer.
func (p *Provider) Prepare(_ context.Context, in protocol.PrepareParams) (protocol.PrepareResult, error) {
	k, ok := p.kind(in.Observed.Kind)
	if !ok || !in.Observed.Exists {
		return protocol.PrepareResult{}, fmt.Errorf("%s: nothing to %s", in.Observed.ID, in.Op)
	}
	x, ok := k.(Explicit)
	if !ok {
		return protocol.PrepareResult{}, fmt.Errorf("%s has no explicit operations", in.Observed.Kind)
	}
	ch, err := x.Prepare(in.Op, in.Observed, in.Desired)
	return protocol.PrepareResult{Change: ch}, err
}

// Apply implements sdk.Provider: the approved requests in order, each at most once, skipping
// those in effect already (an earlier apply that was interrupted), with the change's artifacts.
func (p *Provider) Apply(ctx context.Context, h *sdk.Host, in protocol.ApplyParams) (protocol.ApplyResult, error) {
	c := p.client(ctx, h, nil)
	k, ok := p.kind(in.Change.Kind)
	if !ok {
		return protocol.ApplyResult{}, fmt.Errorf("kind %s is not this provider's", in.Change.Kind)
	}
	ready := map[string]protocol.ArtifactInfo{}
	for _, a := range in.Change.Artifacts {
		info, err := h.Artifact(ctx, a.ID)
		if err != nil {
			return protocol.ApplyResult{}, err
		}
		ready[a.ID] = info
	}
	c.ready = ready
	var sent []string
	for _, a := range in.Change.Requests {
		done, err := k.Done(c, in.Change, a)
		if err != nil {
			return protocol.ApplyResult{}, err
		}
		if done {
			continue
		}
		body, err := c.Send(in.Change, a)
		if err != nil {
			if p.AlreadyDone != nil && p.AlreadyDone(err) {
				continue
			}
			return protocol.ApplyResult{}, err
		}
		if p.AfterSend != nil {
			if err := p.AfterSend(c, in.Change, a, body); err != nil {
				return protocol.ApplyResult{}, err
			}
		}
		sent = append(sent, Describe(a))
	}
	return protocol.ApplyResult{Message: fmt.Sprintf("%s %s: %s", in.Change.Op, in.Change.Resource, strings.Join(sent, ", "))}, nil
}

// Looker is a kind that says itself whether a change is in effect (an operation that leaves no
// trace to count: a restart).
type Looker interface {
	Lookup(c *Client, ch protocol.Change) (protocol.LookupStatus, bool, error)
}

// Lookup implements sdk.Provider: how many of the change's requests are in effect.
func (p *Provider) Lookup(ctx context.Context, h *sdk.Host, in protocol.LookupParams) (protocol.LookupResult, error) {
	c := p.client(ctx, h, nil)
	k, ok := p.kind(in.Change.Kind)
	if !ok {
		return protocol.LookupResult{}, fmt.Errorf("kind %s is not this provider's", in.Change.Kind)
	}
	if l, ok := k.(Looker); ok {
		if status, decided, err := l.Lookup(c, in.Change); err != nil || decided {
			return protocol.LookupResult{Status: status}, err
		}
	}
	n := 0
	for _, a := range in.Change.Requests {
		done, err := k.Done(c, in.Change, a)
		if err != nil {
			return protocol.LookupResult{}, err
		}
		if done {
			n++
		}
	}
	switch {
	case len(in.Change.Requests) > 0 && n == len(in.Change.Requests):
		return protocol.LookupResult{Status: protocol.LookupDone}, nil
	case n == 0:
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	return protocol.LookupResult{Status: protocol.LookupPartly}, nil
}

// Guard implements sdk.Guarder.
func (p *Provider) Guard(ctx context.Context, h *sdk.Host, in protocol.GuardParams) (protocol.GuardResult, error) {
	if p.GuardFunc == nil {
		return protocol.GuardResult{}, errors.New("this provider has no guard")
	}
	return p.GuardFunc(p.client(ctx, h, nil), in)
}

// Script implements sdk.Scripter.
func (p *Provider) Script(_ context.Context, in protocol.ScriptParams) (protocol.ScriptResult, error) {
	if p.ScriptFunc == nil {
		return protocol.ScriptResult{}, errors.New("this provider writes no scripts")
	}
	return p.ScriptFunc(in)
}

// Describe is a request as people read it: its method, path and fixed parameters (an action).
func Describe(a protocol.Approved) string {
	s := a.Method + " " + a.Path
	for _, k := range []string{"Action", "X-Amz-Target"} {
		if v := a.Params[k]; len(v) == 1 {
			s += " " + v[0]
		}
	}
	return s
}
