// Package plan computes what would change to bring the observed world to the desired graph.
//
// Build is a pure function of (desired graph, observation snapshot, policy): it reads no clock,
// no randomness and no map iteration order, so the same inputs always give the same plan and the
// same plan digest. A plan only describes; it changes nothing.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Status summarizes one resource in the plan.
type Status string

const (
	StatusInSync   Status = "in-sync"  // nothing to do
	StatusChange   Status = "change"   // operations below
	StatusWaiting  Status = "waiting"  // depends on something not ready yet
	StatusBlocked  Status = "blocked"  // cannot decide safely (stale or missing observation)
	StatusRetained Status = "retained" // removed from the declaration, kept as is (Note says why)
)

// Inputs identify everything the plan was computed from.
type Inputs struct {
	RevisionDigest string `json:"revisionDigest"`
	SnapshotDigest string `json:"snapshotDigest"`
	PolicyDigest   string `json:"policyDigest"`
	// ProvidersDigest identifies the providers' contracts (spec.Providers.Digest) the plan was made
	// for. Applying with other providers (an updated Mamela or provider) needs a new plan and a new
	// approval.
	ProvidersDigest string    `json:"providersDigest,omitempty"`
	AsOf            time.Time `json:"asOf"`
}

// Diff is one field that differs.
type Diff struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Operation is one real-world change with its policy decision.
type Operation struct {
	Op       policy.Op       `json:"op"`
	Decision policy.Decision `json:"decision"`
	// Late operations run after every other operation of the apply (protocol.Change.Late).
	Late bool `json:"late,omitempty"`
	// Deletes: the operation deletes something that was not made by this resource's declaration
	// being removed (a Kubernetes node). Like every deletion it is approved only on the Control
	// Plane's host (ADR 0013).
	Deletes bool `json:"deletes,omitempty"`
}

// Note explains a status that has no operation (waiting, blocked, retained).
type Note struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Change is the plan for one resource.
type Change struct {
	Kind       graph.Kind  `json:"kind"`
	Name       string      `json:"name"`
	Status     Status      `json:"status"`
	Diffs      []Diff      `json:"diffs"`
	Operations []Operation `json:"operations"`
	// Verdict is the strictest decision among the operations; empty when there are none.
	Verdict policy.Verdict `json:"verdict,omitempty"`
	Note    *Note          `json:"note,omitempty"`
	// Provider is set for a resource of a provider's kind.
	Provider string `json:"provider,omitempty"`
	// ProviderChanges are the provider's changes, one per operation, with exactly the requests
	// each sends: approving the plan approves these and nothing else.
	ProviderChanges []protocol.Change `json:"providerChanges,omitempty"`
	// Creates: a provider's change to an object that does not exist yet.
	Creates bool `json:"creates,omitempty"`
	// Removes: the deletion of an object that no declaration has any more (ADR 0013). It runs after
	// every other change, newest object first, and only a plan approved on the Control Plane's
	// host runs it.
	Removes bool `json:"removes,omitempty"`
}

// Deletes is whether the change deletes anything: an object no declaration has any more, or an
// operation that deletes (a Kubernetes node). Only a plan approved on the Control Plane's host
// runs it.
func (c Change) Deletes() bool {
	return c.Removes || slices.ContainsFunc(c.Operations, func(o Operation) bool { return o.Deletes })
}

// Deleted names what the change deletes, for a person: the object, or the Kubernetes node.
func (c Change) Deleted() []string {
	if c.Removes {
		return []string{c.ID()}
	}
	var out []string
	for _, d := range c.Diffs {
		if d.Field == "remove-node" && c.Deletes() {
			out = append(out, "Kubernetes node "+d.To)
		}
	}
	if len(out) == 0 && c.Deletes() {
		out = append(out, c.ID())
	}
	return out
}

// ID is the resource identity ("Kind name").
func (c Change) ID() string { return string(c.Kind) + " " + c.Name }

// Plan is the full result.
type Plan struct {
	Inputs  Inputs   `json:"inputs"`
	Changes []Change `json:"changes"`
	Digest  string   `json:"-"`
}

// Build computes the plan, for no particular providers.
func Build(g *graph.Graph, snap state.Snapshot, pol policy.Policy) *Plan {
	return BuildFor(g, snap, pol, "")
}

// BuildFor computes the plan for the providers whose contracts have the given digest.
func BuildFor(g *graph.Graph, snap state.Snapshot, pol policy.Policy, providers string) *Plan {
	b := builder{snap: snap, pol: pol, g: g}
	p := &Plan{Inputs: Inputs{
		RevisionDigest:  g.Digest(),
		SnapshotDigest:  snap.Digest(),
		PolicyDigest:    pol.Digest(),
		ProvidersDigest: providers,
		AsOf:            snap.AsOf.UTC(),
	}}
	for _, r := range g.Sorted() {
		switch r.Kind {
		case graph.KindNodePackage:
			p.Changes = append(p.Changes, b.guard(r.NodePackage.Node, b.pkg(r)))
		case graph.KindNodeService:
			p.Changes = append(p.Changes, b.guard(r.NodeService.Node, b.service(r)))
		case graph.KindNodeK3s:
			p.Changes = append(p.Changes, b.guard(r.NodeK3s.Node, b.k3s(r)))
		case graph.KindNodeK3sManifest:
			p.Changes = append(p.Changes, b.guard(r.NodeK3sManifest.Node, b.k3sManifest(r)))
		default:
			if r.Provided != nil {
				p.Changes = append(p.Changes, b.guard(r.Name, b.provided(r)))
			}
		}
	}
	declared := map[string]bool{}
	for _, r := range g.Resources {
		declared[r.ID()] = true
	}
	p.Changes = append(p.Changes, b.undeclared(declared)...)
	p.Digest = digest(p)
	return p
}

// undeclared plans what a provider found that Mamela made and no declaration has: the deletion of
// what the declaration being planned made (ADR 0013), newest first; everything else is kept, and
// the note says why.
func (b builder) undeclared(declared map[string]bool) []Change {
	var removals, kept []Change
	for _, id := range slices.Sorted(maps.Keys(b.snap.Provided)) {
		o := b.snap.Provided[id]
		if o.Declared || declared[id] || !o.Observed.Exists || !o.Observed.Owned {
			continue
		}
		c := Change{Kind: o.Kind, Name: o.Name, Provider: o.Provider, Diffs: []Diff{}, Operations: []Operation{}}
		switch {
		case o.Removal == nil:
			c.Status = StatusRetained
			c.Note = keptNote(o)
			kept = append(kept, c)
		case b.pol.Protected(o.Name):
			c.Status, c.Note = StatusRetained, &Note{Code: string(policy.ProtectedDecision.Code), Message: policy.ProtectedDecision.Message, Hint: policy.ProtectedDecision.Hint}
			kept = append(kept, c)
		case !b.pol.Deletes():
			c.Status = StatusRetained
			c.Note = &Note{Code: "MAM-PLAN-0305", Message: "not in this declaration; kept: the policy retains what a declaration no longer has (removal = retain)"}
			kept = append(kept, c)
		default:
			d := b.pol.DecideRemoval(o.Removal.Verified)
			c.Status, c.Verdict, c.Removes = StatusChange, d.Verdict, true
			c.Diffs = []Diff{{Field: "removed", To: "Mamela made it for this declaration (" + b.snap.Stack + "), which no longer has it"}}
			c.Operations = []Operation{{Op: o.Removal.Change.Op, Decision: d}}
			c.ProviderChanges = []protocol.Change{o.Removal.Change}
			removals = append(removals, c)
		}
	}
	// Newest object first: what depends on an object was made after it.
	slices.SortStableFunc(removals, func(x, y Change) int {
		return b.snap.Provided[y.ID()].RemovalOrder - b.snap.Provided[x.ID()].RemovalOrder
	})
	return append(removals, kept...)
}

func keptNote(o state.Provided) *Note {
	if o.Kept != nil {
		return &Note{Code: o.Kept.Code, Message: o.Kept.Message, Hint: o.Kept.Hint}
	}
	return &Note{Code: "MAM-PLAN-0301", Message: "not in this declaration; kept unchanged",
		Hint: "its provider's explicit delete operation removes it (mamela act)"}
}

// Removals counts the deletions in the plan.
func (p *Plan) Removals() int {
	n := 0
	for _, c := range p.Changes {
		if c.Deletes() {
			n++
		}
	}
	return n
}

// provided plans a resource of a provider from the provider's observation and diff.
func (b builder) provided(r graph.Resource) Change {
	c := newChange(r)
	c.Provider = r.Provided.Provider
	got, observed := b.snap.Provided[r.ID()]
	switch {
	case !observed || got.Provider != r.Provided.Provider || !got.Declared:
		c.Status, c.Note = StatusBlocked, &Note{Code: "MAM-PLAN-0107",
			Message: fmt.Sprintf("provider %s has not observed it", r.Provided.Provider),
			Hint:    "plan with the declaration's Pome so that its providers observe (mamela plan <pome-dir>)"}
		return c
	case got.SpecDigest != state.SpecDigest(r.Provided.Spec):
		c.Status, c.Note = StatusBlocked, &Note{Code: "MAM-PLAN-0203",
			Message: "the provider's changes were computed for another declaration of it",
			Hint:    "observe again (make the plan again)"}
		return c
	case b.stale(got.ObservedAt):
		c.Status, c.Note = StatusBlocked, b.staleNote(got.ObservedAt)
		return c
	case got.Waiting != "":
		c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0113", Message: got.Waiting,
			Hint: "apply the plan that makes it, then plan again (ADR 0007)"}
		return c
	case got.Observed.Exists && got.Observed.Adoptable && !got.Observed.Owned:
		// Imported (ADR 0009), or recognized by its provider: a person adopts it first.
		c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0114",
			Message: "it exists and is to be adopted: Mamela changes nothing of it until a person adopts it",
			Hint:    fmt.Sprintf("adopt it with its provider's adopt operation: mamela act --confirm %s <adopt-op> %q <pome-dir>", r.Name, r.ID())}
		return c
	case got.Observed.Exists && !got.Observed.Owned:
		return b.withOps(c, policy.Subject{Op: policy.OpChangeForeign})
	}
	if len(got.Changes) == 0 {
		return b.withOps(c)
	}
	c.Status, c.Verdict, c.Creates = StatusChange, policy.Auto, !got.Observed.Exists
	for _, pc := range got.Changes {
		for _, reason := range pc.Change.Reasons {
			c.Diffs = append(c.Diffs, Diff{Field: string(pc.Change.Op), To: reason})
		}
		// The files the change sends are approved with it: where a download comes from and its
		// sha256, and for whom and with which network a seed is made (ADR 0004).
		for _, a := range pc.Change.Artifacts {
			c.Diffs = append(c.Diffs, Diff{Field: string(pc.Change.Op) + " artifact", To: a.Describe()})
		}
		d := b.pol.DecideProvided(pc.Verified)
		if pc.Unsupported != "" {
			d = policy.Decision{Verdict: policy.Denied, Code: "MAM-POL-0309",
				Message: fmt.Sprintf("provider %s does not carry out %s: %s", r.Provided.Provider, pc.Change.Op, pc.Unsupported),
				Hint:    "restore the declared value to what the resource has, or change the resource by hand"}
		}
		c.Operations = append(c.Operations, Operation{Op: pc.Change.Op, Decision: d, Late: pc.Change.Late})
		c.Verdict = policy.Stricter(c.Verdict, d.Verdict)
		c.ProviderChanges = append(c.ProviderChanges, pc.Change)
	}
	if got.Observed.Exists && b.restartsMachine(got) {
		if note := b.machineRestartGate(r); note != nil {
			w := newChange(r)
			w.Provider, w.Status, w.Note = c.Provider, StatusWaiting, note
			return w
		}
	}
	return c
}

// restartsMachine is whether a provider's changes of an existing machine interrupt it (reboot, stop).
func (b builder) restartsMachine(got state.Provided) bool {
	return slices.ContainsFunc(got.Changes, func(pc state.ProvidedChange) bool {
		return slices.Contains(pc.Verified.Effects, protocol.EffectRestart)
	})
}

// machineRestartGate says what a restart of a machine that runs a k3s node waits for (nil: nothing,
// or the machine runs none). The machines of a cluster are restarted one at a time (in the order of
// their names, so the agents come before the server), and only while every Kubernetes node is
// Ready: a plan that restarts them all at once would take the cluster down.
func (b builder) machineRestartGate(r graph.Resource) *Note {
	var mine *graph.NodeK3sSpec
	for _, o := range b.g.Sorted() {
		if o.NodeK3s != nil && o.NodeK3s.Node == r.Name {
			mine = o.NodeK3s
		}
	}
	if mine == nil || !b.snap.K3s[mine.Node+"/k3s"].Running {
		return nil
	}
	server := mine.Server
	if mine.Role == graph.K3sServer {
		server = mine.Node
	}
	for _, o := range b.g.Sorted() {
		k := o.NodeK3s
		if k == nil || k.Node >= r.Name || (k.Node != server && k.Server != server) {
			continue
		}
		for _, m := range b.g.Sorted() {
			if m.Provided != nil && m.Name == k.Node && b.restartsMachine(b.snap.Provided[m.ID()]) && b.snap.Provided[m.ID()].Observed.Exists {
				return &Note{Code: "MAM-PLAN-0118", Message: fmt.Sprintf("waits for machine %s to be restarted before %s (the machines of a cluster are restarted one at a time)", m.Name, r.Name),
					Hint: "apply the plan that restarts it, then plan again"}
			}
		}
	}
	nodes := b.declaredNodes(b.snap.K3s[server+"/k3s"].Nodes, &graph.NodeK3sSpec{Node: server, Role: graph.K3sServer})
	var notReady []string
	for _, n := range nodes {
		if !n.Ready {
			notReady = append(notReady, n.Name)
		}
	}
	switch {
	case len(nodes) == 0:
		return &Note{Code: "MAM-PLAN-0115", Message: fmt.Sprintf("waits for the k3s server to report its Kubernetes nodes before machine %s is restarted", r.Name),
			Hint: "the server reports them while it runs; if it never does, look at k3s on the server"}
	case len(notReady) > 0:
		return &Note{Code: "MAM-PLAN-0115", Message: fmt.Sprintf("waits for every Kubernetes node to be Ready before machine %s is restarted (not Ready: %s)", r.Name, strings.Join(notReady, ", ")),
			Hint: "a restart goes on only on a healthy cluster; find out why the node is not Ready"}
	}
	return nil
}

// SameProviderChanges reports whether every change of fresh is one of approved, exactly: the
// same operation, objects, requests and precondition.
func SameProviderChanges(fresh, approved []protocol.Change) bool {
	for _, f := range fresh {
		fj, _ := json.Marshal(f, json.Deterministic(true))
		if !slices.ContainsFunc(approved, func(a protocol.Change) bool {
			aj, _ := json.Marshal(a, json.Deterministic(true))
			return string(aj) == string(fj)
		}) {
			return false
		}
	}
	return true
}

type builder struct {
	snap state.Snapshot
	pol  policy.Policy
	g    *graph.Graph
}

func (b builder) stale(observedAt time.Time) bool {
	return b.snap.AsOf.Sub(observedAt) > b.pol.MaxObservationAge()
}

func (b builder) staleNote(observedAt time.Time) *Note {
	return &Note{Code: "MAM-PLAN-0201",
		Message: fmt.Sprintf("the last observation is from %s, older than %s", observedAt.UTC().Format(time.RFC3339), b.pol.MaxObservationAge()),
		Hint:    "observe again before planning; Mamela does not act on stale information"}
}

// nodeReady reports whether the node's machine exists, is managed and running. Without that,
// node resources wait: there is nothing (or nothing trustworthy) to observe them on.
func (b builder) nodeReady(node string) (bool, *Note) {
	m, ok := b.machine(node)
	switch {
	case !ok:
		return false, &Note{Code: "MAM-PLAN-0101", Message: fmt.Sprintf("waits for machine %s to be created", node)}
	case !m.managed:
		return false, &Note{Code: "MAM-PLAN-0102", Message: fmt.Sprintf("machine %s is not managed by Mamela (Foreign)", node),
			Hint: "Mamela does not configure machines it did not create; a provider may offer to adopt one (mamela act)"}
	case m.stopped:
		return false, &Note{Code: "MAM-PLAN-0111", Message: fmt.Sprintf("waits for machine %s to be started", node)}
	}
	return true, nil
}

// machine is what plans need of a node: a provider's machine (ADR 0004).
type machine struct {
	managed, stopped bool
	uptime           time.Duration
}

func (b builder) machine(node string) (machine, bool) {
	n, ok := b.snap.Nodes[node]
	if !ok {
		return machine{}, false
	}
	return machine{managed: n.Managed, stopped: !n.Running, uptime: time.Duration(n.UptimeSeconds) * time.Second}, true
}

// SilentAgentAfter is how long a machine may run before Mamela says that its agent has never reported.
// A first boot with cloud-init takes a few minutes; this is well beyond it.
const SilentAgentAfter = 15 * time.Minute

// awaitAgent is how a node resource waits for its agent's first report: patiently while the
// machine is young, but once it has been running long past a first boot, the agent is not coming
// by itself (the machine may be stuck at boot, for example in emergency mode, or cannot reach the Control
// Plane), and the plan says so.
func (b builder) awaitAgent(node string, waiting *Note) (Status, *Note) {
	m, _ := b.machine(node)
	if m.uptime < SilentAgentAfter {
		return StatusWaiting, waiting
	}
	return StatusBlocked, &Note{Code: "MAM-PLAN-0110",
		Message: fmt.Sprintf("machine %s has been running for %s, but its agent has never reported: it may be stuck at boot or unable to reach the Control Plane",
			node, m.uptime.Truncate(time.Minute)),
		Hint: "look at its console; if it is stuck, restart it with its provider's explicit operation (mamela act, for example reboot-vm or reset-vm of Proxmox)"}
}

func (b builder) pkg(r graph.Resource) Change {
	want := r.NodePackage
	c := newChange(r)
	if ok, note := b.nodeReady(want.Node); !ok {
		c.Status, c.Note = StatusWaiting, note
		return c
	}
	got, observed := b.snap.Packages[r.Name]
	if !observed {
		c.Status, c.Note = b.awaitAgent(want.Node, &Note{Code: "MAM-PLAN-0103",
			Message: fmt.Sprintf("waits for the agent on %s to report packages", want.Node)})
		return c
	}
	if b.stale(got.ObservedAt) {
		c.Status, c.Note = StatusBlocked, b.staleNote(got.ObservedAt)
		return c
	}
	if got.Installed {
		return b.withOps(c)
	}
	if got.Error != "" {
		c.Status, c.Note = StatusBlocked, &Note{Code: "MAM-PLAN-0202",
			Message: fmt.Sprintf("the agent on %s cannot observe %s: %s", want.Node, want.Package, got.Error),
			Hint:    "fix the cause on the node (for example apt sources), then plan again"}
		return c
	}
	cs := got.ChangeSet
	if cs == nil {
		c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0105",
			Message: fmt.Sprintf("waits for the agent on %s to work out what installing %s would change", want.Node, want.Package)}
		return c
	}
	// The diff names every package apt would touch, and the change set digest: approving the plan
	// approves exactly this set, and the agent refuses to install anything else.
	c.Diffs = []Diff{{"installed", "false", "true"}, {"changeSet", "", cs.Digest}}
	var origins []string
	for _, ch := range cs.Changes {
		line := ch.Action + " " + ch.Package
		if ch.Version != "" {
			line += " " + ch.Version
		}
		if len(ch.Origins) > 0 {
			line += " (" + strings.Join(ch.Origins, ", ") + ")"
		}
		c.Diffs = append(c.Diffs, Diff{"apt", "", line})
		origins = append(origins, ch.Origins...)
	}
	return b.withOps(c, policy.Subject{Op: policy.OpInstallPackage, Removes: cs.Removes(), Origins: origins})
}

func (b builder) service(r graph.Resource) Change {
	want := r.NodeService
	c := newChange(r)
	if ok, note := b.nodeReady(want.Node); !ok {
		c.Status, c.Note = StatusWaiting, note
		return c
	}
	got, observed := b.snap.Services[r.Name]
	if !observed {
		c.Status, c.Note = b.awaitAgent(want.Node, &Note{Code: "MAM-PLAN-0104",
			Message: fmt.Sprintf("waits for the agent on %s to report services", want.Node)})
		return c
	}
	if b.stale(got.ObservedAt) {
		c.Status, c.Note = StatusBlocked, b.staleNote(got.ObservedAt)
		return c
	}
	if !got.Exists {
		c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0106",
			Message: fmt.Sprintf("waits for service %s to exist on %s (what provides it, a package or k3s, is not installed yet)", want.Service, want.Node)}
		return c
	}
	var subjects []policy.Subject
	if got.Enabled != want.Enabled {
		c.Diffs = append(c.Diffs, Diff{"enabled", btoa(got.Enabled), btoa(want.Enabled)})
		op := policy.OpEnableService
		if !want.Enabled {
			op = policy.OpDisableService
		}
		subjects = append(subjects, policy.Subject{Op: op, Service: want.Service})
	}
	if got.Running != want.Running && want.Running {
		// A k3s agent starts only once its server runs: its start waits for the server (Type=notify),
		// and would hold its mamelet until then.
		if server := b.k3sServerOf(want.Node, want.Service); server != "" {
			if srv, ok := b.snap.Services[server+"/k3s"]; !ok || !srv.Running {
				c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0112",
					Message: fmt.Sprintf("waits for the k3s server %s to run before starting the agent on %s", server, want.Node)}
				return c
			}
		}
	}
	if got.Running != want.Running {
		c.Diffs = append(c.Diffs, Diff{"running", btoa(got.Running), btoa(want.Running)})
		op := policy.OpStartService
		if !want.Running {
			op = policy.OpStopService
		}
		subjects = append(subjects, policy.Subject{Op: op, Service: want.Service})
	}
	repair := want.Running && b.pol.Decide(policy.Subject{Op: policy.OpStartService, Service: want.Service}).Verdict == policy.Auto
	if got.RepairStart != repair {
		c.Diffs = append(c.Diffs, Diff{"automaticStartRepair", btoa(got.RepairStart), btoa(repair)})
		subjects = append(subjects, policy.Subject{Op: policy.OpConfigureService, Service: want.Service})
	}
	return b.withOps(c, subjects...)
}

// k3s plans a k3s server on a node from mamelet's report of it.
func (b builder) k3s(r graph.Resource) Change {
	want := r.NodeK3s
	c := newChange(r)
	if ok, note := b.nodeReady(want.Node); !ok {
		c.Status, c.Note = StatusWaiting, note
		return c
	}
	got, observed := b.snap.K3s[r.Name]
	switch {
	case !observed:
		c.Status, c.Note = b.awaitAgent(want.Node, &Note{Code: "MAM-PLAN-0108",
			Message: fmt.Sprintf("waits for the agent on %s to report k3s", want.Node)})
		return c
	case b.stale(got.ObservedAt):
		c.Status, c.Note = StatusBlocked, b.staleNote(got.ObservedAt)
		return c
	case got.Error != "":
		c.Status, c.Note = StatusBlocked, &Note{Code: "MAM-PLAN-0204",
			Message: fmt.Sprintf("the agent on %s cannot observe k3s: %s", want.Node, got.Error),
			Hint:    "fix it on the node, then plan again"}
		return c
	case got.Installed && got.SHA256 == want.SHA256 && !got.Stale:
		return b.retireNode(r, got, b.withOps(c))
	}
	// A k3s whose binary is in place is changed in place: an agent only after its server runs the
	// declared release; and, if it runs (it is restarted), the agents one at a time and only while
	// every Kubernetes node is Ready.
	upgrade := got.Running
	if got.SHA256 != "" {
		if note := b.upgradeGate(r, got); note != nil {
			c.Status, c.Note = StatusWaiting, note
			return c
		}
	}
	server := ""
	if want.Role == graph.K3sAgent && b.g.StaticIP(want.Server) != "" {
		// The server's declared address: an agent never waits for, or follows, what is reported.
		server = graph.K3sServerURL(b.g.StaticIP(want.Server))
	} else if want.Role == graph.K3sAgent {
		srv := b.snap.K3s[want.Server+"/k3s"]
		if len(srv.Addresses) == 0 {
			c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0109",
				Message: fmt.Sprintf("waits for the k3s server %s to report its address", want.Server)}
			return c
		}
		server = "https://" + srv.Addresses[0] + ":6443"
	}
	// The diff says exactly what is approved: the release, the binary's sha256, where it comes
	// from, how it runs and, for an agent, the server it joins. The join token is made by the
	// Control Plane and given only to the cluster's nodes; it is never shown.
	c.Diffs = []Diff{{"version", b.k3sRunning(r, got), want.Version}, {"sha256", got.SHA256, want.SHA256},
		{"from", "", noderev.K3sReleases + "/" + noderev.K3sTag(want.Version) + "/k3s (k3s-arm64 on arm64)"},
		{"role", "", string(want.Role)}}
	if server != "" {
		// Always shown, changed or not: the grant checks the server in the revision against this
		// approved value, so it is what the approval says the agent joins.
		c.Diffs = append(c.Diffs, Diff{"server", got.Server, server})
	}
	c.Diffs = append(c.Diffs, Diff{"runs", "", "/usr/local/bin/k3s " + strings.Join(noderev.K3sArgs(string(want.Role), server), " ")},
		Diff{"token", "", "the cluster's join token, made by the Control Plane, in " + noderev.K3sTokenFile + " (0600)"})
	if upgrade {
		c.Diffs = append(c.Diffs, Diff{"restart", "", "k3s on " + want.Node + " restarts into this release; its Kubernetes node is not Ready for a while"})
		return b.withOps(c, policy.Subject{Op: policy.OpUpgradeK3s})
	}
	return b.withOps(c, policy.Subject{Op: policy.OpInstallK3s})
}

// retireNode adds to the plan of a k3s server that is in sync the removal of a Kubernetes node
// that no declaration of this cluster names any more: a node whose machine this plan deletes (it
// is drained first, while its machine still runs), or a Not Ready node whose machine is gone (a
// left-over). One node per plan; the server's own node, a node whose machine still exists, and a
// cluster whose node names do not include the server's own are never touched. It is a deletion:
// the policy's removal = retain keeps it, and only the Control Plane's host approves it.
func (b builder) retireNode(r graph.Resource, got state.K3s, c Change) Change {
	want := r.NodeK3s
	if want.Role != graph.K3sServer || !got.Running || !b.pol.Deletes() ||
		!slices.ContainsFunc(got.Nodes, func(n state.K3sNode) bool { return n.Name == want.Node }) {
		return c
	}
	declared := map[string]bool{}
	for _, o := range b.g.Sorted() {
		if k := o.NodeK3s; k != nil && (k.Node == want.Node || k.Server == want.Node) {
			declared[k.Node] = true
		}
	}
	byName := slices.SortedFunc(slices.Values(got.Nodes), func(x, y state.K3sNode) int { return strings.Compare(x.Name, y.Name) })
	for _, n := range byName {
		if declared[n.Name] || b.pol.Protected(n.Name) {
			continue
		}
		var why string
		switch {
		case b.machineLeaves(n.Name):
			why = "its machine is deleted by this plan; its pods are moved to other nodes first, then the node is deleted"
			if !n.Ready {
				why = "its machine is deleted by this plan and the node is not Ready (it cannot be drained); the node is deleted"
			}
		case !n.Ready && !b.machineExists(n.Name):
			why = "the node is not Ready and no machine of that name exists any more (a left-over); the node is deleted"
		default:
			continue
		}
		// The approval names the node: the grant takes it from this diff, and no other.
		c.Diffs = append(c.Diffs, Diff{"remove-node", "", n.Name}, Diff{"remove-node because", "", why})
		d := b.pol.Decide(policy.Subject{Op: policy.OpRemoveK3sNode})
		c.Status = StatusChange
		c.Verdict = policy.Stricter(c.Verdict, d.Verdict)
		c.Operations = append(c.Operations, Operation{Op: policy.OpRemoveK3sNode, Decision: d, Late: true, Deletes: true})
		return c
	}
	return c
}

// machineLeaves is whether this plan deletes the machine called name (ADR 0013).
func (b builder) machineLeaves(name string) bool {
	if !b.pol.Deletes() || b.pol.Protected(name) {
		return false
	}
	for id, o := range b.snap.Provided {
		if o.Name == name && o.Removal != nil && !o.Declared && o.Observed.Exists && o.Observed.Owned &&
			!slices.ContainsFunc(b.g.Resources, func(r graph.Resource) bool { return r.ID() == id }) {
			return true
		}
	}
	return false
}

// machineExists is whether any machine called name is observed.
func (b builder) machineExists(name string) bool {
	if _, ok := b.snap.Nodes[name]; ok {
		return true
	}
	for _, o := range b.snap.Provided {
		if o.Name == name && o.Observed.Exists {
			return true
		}
	}
	return false
}

// k3sManifest plans a manifest file in a k3s server's auto-deploy directory. Its whole content is
// in the diff: that is what is approved, and what k3s then applies with cluster-admin rights.
func (b builder) k3sManifest(r graph.Resource) Change {
	want := r.NodeK3sManifest
	c := newChange(r)
	if ok, note := b.nodeReady(want.Node); !ok {
		c.Status, c.Note = StatusWaiting, note
		return c
	}
	got, observed := b.snap.K3sManifests[r.Name]
	switch {
	case !observed:
		c.Status, c.Note = b.awaitAgent(want.Node, &Note{Code: "MAM-PLAN-0117",
			Message: fmt.Sprintf("waits for the agent on %s to report the manifest %s", want.Node, want.Name)})
		return c
	case b.stale(got.ObservedAt):
		c.Status, c.Note = StatusBlocked, b.staleNote(got.ObservedAt)
		return c
	case got.Error != "":
		c.Status, c.Note = StatusBlocked, &Note{Code: "MAM-PLAN-0204",
			Message: fmt.Sprintf("the agent on %s cannot observe the manifest %s: %s", want.Node, want.Name, got.Error),
			Hint:    "fix it on the node, then plan again"}
		return c
	}
	sum := noderev.ManifestSHA256(want.Content)
	if got.SHA256 == sum {
		return b.withOps(c)
	}
	c.Diffs = []Diff{{"file", "", noderev.K3sManifestDir + "/" + noderev.K3sManifestFile(want.Name)},
		{"sha256", got.SHA256, sum}, {"content", "", want.Content}}
	return b.withOps(c, policy.Subject{Op: policy.OpWriteK3sManifest})
}

// k3sCurrent is whether a node's k3s is what its declaration says: the declared binary, running.
func k3sCurrent(got state.K3s, want *graph.NodeK3sSpec) bool {
	return got.Installed && got.SHA256 == want.SHA256 && !got.Stale
}

// k3sRunning is the release a node's kubelet runs, as the cluster's server reports it ("" when
// unknown).
func (b builder) k3sRunning(r graph.Resource, got state.K3s) string {
	nodes := got.Nodes
	if r.NodeK3s.Role == graph.K3sAgent {
		nodes = b.snap.K3s[r.NodeK3s.Server+"/k3s"].Nodes
	}
	for _, n := range nodes {
		if n.Name == r.NodeK3s.Node {
			return n.Version
		}
	}
	return ""
}

// upgradeGate says what a change of a k3s whose binary is in place waits for, or nil when it may go
// on. The server goes first, for an agent running or not (an agent never runs a newer release than
// its server: a stopped agent given a newer binary would start on it). A k3s that runs is restarted:
// the agents one at a time (in the order of their names), and each step only while every
// Kubernetes node is Ready, as the server sees it: a restart on top of an unhealthy cluster would
// take more of it down.
func (b builder) upgradeGate(r graph.Resource, got state.K3s) *Note {
	want := r.NodeK3s
	nodes := got.Nodes
	if want.Role == graph.K3sAgent {
		srv, ok := b.snap.K3s[want.Server+"/k3s"]
		for _, other := range b.g.Sorted() {
			o := other.NodeK3s
			if o == nil || o.Node != want.Server {
				continue
			}
			if !ok || b.stale(srv.ObservedAt) || !k3sCurrent(srv, o) {
				return &Note{Code: "MAM-PLAN-0116", Message: fmt.Sprintf("waits for the k3s server %s to run its declared release before %s is upgraded (an agent never runs a newer release than its server)", want.Server, want.Node),
					Hint: "the server's upgrade comes first in this or an earlier plan"}
			}
		}
		for _, other := range b.g.Sorted() {
			o := other.NodeK3s
			if !got.Running || o == nil || o.Role != graph.K3sAgent || o.Server != want.Server || o.Node >= want.Node {
				continue
			}
			if og, seen := b.snap.K3s[other.Name]; seen && og.Running && !k3sCurrent(og, o) {
				return &Note{Code: "MAM-PLAN-0116", Message: fmt.Sprintf("waits for the k3s agent %s to be upgraded before %s (agents are upgraded one at a time)", o.Node, want.Node)}
			}
		}
		nodes = srv.Nodes
	}
	if !got.Running {
		return nil // nothing restarts
	}
	nodes = b.declaredNodes(nodes, want)
	var notReady []string
	for _, n := range nodes {
		if !n.Ready {
			notReady = append(notReady, n.Name)
		}
	}
	switch {
	case len(nodes) == 0:
		return &Note{Code: "MAM-PLAN-0115", Message: fmt.Sprintf("waits for the k3s server to report its Kubernetes nodes before k3s on %s is restarted", want.Node),
			Hint: "the server reports them while it runs; if it never does, look at k3s on the server"}
	case len(notReady) > 0:
		return &Note{Code: "MAM-PLAN-0115", Message: fmt.Sprintf("waits for every Kubernetes node to be Ready before k3s on %s is restarted (not Ready: %s)", want.Node, strings.Join(notReady, ", ")),
			Hint: "an upgrade goes on only on a healthy cluster; find out why the node is not Ready"}
	}
	return nil
}

// declaredNodes are the Kubernetes nodes the declaration is about: those named like a k3s node of
// this cluster. A node the declaration does not name (a machine whose declaration was removed and
// left its node behind, or one joined by hand) is not Mamela's to wait for: it would stop every
// upgrade for good. When no name matches at all (the Kubernetes node names are not the declared
// ones), nothing is dropped: the gate stays as careful as it can be.
func (b builder) declaredNodes(nodes []state.K3sNode, want *graph.NodeK3sSpec) []state.K3sNode {
	server := want.Server
	if want.Role == graph.K3sServer {
		server = want.Node
	}
	declared := map[string]bool{}
	for _, r := range b.g.Sorted() {
		if o := r.NodeK3s; o != nil && (o.Node == server || o.Server == server) {
			declared[o.Node] = true
		}
	}
	var mine []state.K3sNode
	for _, n := range nodes {
		if declared[n.Name] {
			mine = append(mine, n)
		}
	}
	if len(mine) == 0 {
		return nodes
	}
	return mine
}

// k3sServerOf is the server of node's k3s agent, when service is that agent's k3s service.
func (b builder) k3sServerOf(node, service string) string {
	if service != "k3s" || b.g == nil {
		return ""
	}
	for _, r := range b.g.Resources {
		if r.NodeK3s != nil && r.NodeK3s.Node == node && r.NodeK3s.Role == graph.K3sAgent {
			return r.NodeK3s.Server
		}
	}
	return ""
}

// guard denies every operation of a change on a node the Control Plane must never change: its own
// host (ADR 0003). The change is still shown.
func (b builder) guard(node string, c Change) Change {
	if !b.pol.Protected(node) || c.Status != StatusChange {
		return c
	}
	for i := range c.Operations {
		c.Operations[i].Decision = policy.ProtectedDecision
	}
	c.Verdict = policy.Denied
	return c
}

func newChange(r graph.Resource) Change {
	return Change{Kind: r.Kind, Name: r.Name, Diffs: []Diff{}, Operations: []Operation{}}
}

func (b builder) withOps(c Change, subjects ...policy.Subject) Change {
	if len(subjects) == 0 {
		c.Status = StatusInSync
		return c
	}
	c.Status = StatusChange
	c.Verdict = policy.Auto
	for _, s := range subjects {
		d := b.pol.Decide(s)
		c.Operations = append(c.Operations, Operation{Op: s.Op, Decision: d})
		c.Verdict = policy.Stricter(c.Verdict, d.Verdict)
	}
	return c
}

func digest(p *Plan) string {
	out, err := json.Marshal(p, json.Deterministic(true))
	if err != nil {
		panic("plan: encoding failed: " + err.Error())
	}
	sum := sha256.Sum256(out)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Canonical is the deterministic JSON of the plan (what the digest covers).
func (p *Plan) Canonical() []byte {
	out, err := json.Marshal(p, json.Deterministic(true))
	if err != nil {
		panic("plan: encoding failed: " + err.Error())
	}
	return out
}

func btoa(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
