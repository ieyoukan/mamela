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
	// Warnings are things a person should read before approving that neither stop nor change
	// anything: the declaration is valid, but probably not what was meant (a k3s agent newer than
	// its server, a k3s that nothing starts). They are part of the plan, so the plan that was read is
	// the plan that is approved.
	Warnings []Note `json:"warnings,omitempty"`
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
	if p, ok := planners[c.Kind]; ok && p.Deleted != nil && c.Deletes() {
		out = p.Deleted(c)
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
	kit := newKit(b)
	p := &Plan{Inputs: Inputs{
		RevisionDigest:  g.Digest(),
		SnapshotDigest:  snap.Digest(),
		PolicyDigest:    pol.Digest(),
		ProvidersDigest: providers,
		AsOf:            snap.AsOf.UTC(),
	}}
	// What is valid in the declaration but probably not meant goes with the plan of its resource.
	warned := map[string][]Note{}
	for _, w := range g.Warnings() {
		warned[w.Resource] = append(warned[w.Resource], Note{Code: string(w.Code), Message: w.Message, Hint: w.Hint})
	}
	for _, r := range g.Sorted() {
		switch r.Kind {
		case graph.KindNodePackage:
			p.Changes = append(p.Changes, b.onNode(r.NodePackage.Node, b.pkg(r)))
		case graph.KindNodeService:
			p.Changes = append(p.Changes, b.onNode(r.NodeService.Node, b.service(r)))
		case graph.KindNodeMamelet:
			p.Changes = append(p.Changes, b.onNode(r.NodeMamelet.Node, b.mamelet(r)))
		default:
			if pl, ok := planners[r.Kind]; ok {
				// A kind of a node extension: its planner says what changes; the core still decides
				// whether the node can be changed at all (onNode), and adds the declaration's warnings.
				c := b.onNode(r.Node(), pl.Plan(kit, r))
				c.Warnings = slices.Concat(warned[r.ID()], c.Warnings)
				p.Changes = append(p.Changes, c)
			} else if r.Provided != nil {
				p.Changes = append(p.Changes, b.guard(r.Name, b.provided(r)))
			}
		}
	}
	declared := map[string]bool{}
	for _, r := range g.Resources {
		declared[r.ID()] = true
	}
	p.Changes = append(p.Changes, b.undeclared(declared)...)
	p.Changes = append(p.Changes, b.extUndeclared()...)
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
	c := NewChange(r)
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
	case got.Kept != nil && len(got.Changes) == 0:
		// Declared here and made for another declaration, but as both want it (a shared template):
		// nothing is to change, so it is in sync, with a note of whose it is.
		c = b.withOps(c)
		c.Note = &Note{Code: got.Kept.Code, Message: got.Kept.Message, Hint: got.Kept.Hint}
		return c
	case got.Kept != nil:
		// Declared here, but made for another declaration (B4): Mamela changes nothing of it, and a
		// person decides where it belongs. Waiting would never end by itself.
		c.Status, c.Note = StatusBlocked, &Note{Code: got.Kept.Code, Message: got.Kept.Message, Hint: got.Kept.Hint}
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
			w := NewChange(r)
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
	case b.machineOfAnother(node) != nil:
		k := b.machineOfAnother(node)
		return false, &Note{Code: k.Code, Message: fmt.Sprintf("machine %s: %s", node, k.Message), Hint: k.Hint}
	case m.stopped:
		return false, &Note{Code: "MAM-PLAN-0111", Message: fmt.Sprintf("waits for machine %s to be started", node)}
	}
	return true, nil
}

// machineOfAnother says, for a machine Mamela made for another declaration (B4), why this one
// does not configure it; nil for any other machine.
func (b builder) machineOfAnother(node string) *state.Kept {
	for _, id := range slices.Sorted(maps.Keys(b.snap.Provided)) {
		if p := b.snap.Provided[id]; p.Name == node && p.Declared && p.Kept != nil && p.Observed.Node != nil {
			return p.Kept
		}
	}
	return nil
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

// nodeReported is whether the agent on node has reported anything of the declaration.
func (b builder) nodeReported(node string) bool {
	if b.g == nil {
		return false
	}
	for _, r := range b.g.Resources {
		var seen bool
		switch {
		case r.NodePackage != nil && r.NodePackage.Node == node:
			_, seen = b.snap.Packages[r.Name]
		case r.NodeService != nil && r.NodeService.Node == node:
			_, seen = b.snap.Services[r.Name]
		case r.NodeMamelet != nil && r.NodeMamelet.Node == node:
			_, seen = b.snap.Mamelets[r.Name]
		default:
			if pl, ok := planners[r.Kind]; ok && pl.Reported != nil && r.Node() == node {
				seen = pl.Reported(newKit(b), r)
			}
		}
		if seen {
			return true
		}
	}
	return false
}

// SilentAgentAfter is how long a machine may run before Mamela says that its agent has never reported.
// A first boot with cloud-init takes a few minutes; this is well beyond it.
const SilentAgentAfter = 15 * time.Minute

// awaitAgent is how a node resource waits for its agent's first report: patiently while the
// machine is young, but once it has been running long past a first boot, the agent is not coming
// by itself (the machine may be stuck at boot, for example in emergency mode, or cannot reach the Control
// Plane), and the plan says so.
//
// An agent that has reported other resources of the machine is not silent: it has only not
// reported this one yet (a resource declared after the machine was made is learned at its next poll),
// so the plan keeps waiting for it.
func (b builder) awaitAgent(node string, waiting *Note) (Status, *Note) {
	if note := b.refusal(node); note != nil {
		return StatusBlocked, note // it does report, and says it refuses what the Control Plane offers
	}
	m, _ := b.machine(node)
	if m.uptime < SilentAgentAfter || b.nodeReported(node) {
		return StatusWaiting, waiting
	}
	return StatusBlocked, &Note{Code: "MAM-PLAN-0110",
		Message: fmt.Sprintf("machine %s has been running for %s, but its agent has never reported: it may be stuck at boot or unable to reach the Control Plane",
			node, m.uptime.Truncate(time.Minute)),
		Hint: "look at its console; if it is stuck, restart it with its provider's explicit operation (mamela act, for example reboot-vm or reset-vm of Proxmox)"}
}

func (b builder) pkg(r graph.Resource) Change {
	want := r.NodePackage
	c := NewChange(r)
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
	// What an installation waits for is the node extensions' to say (an APT source it comes from).
	if note := b.packageGate(want.Node, want.Package); note != nil {
		c.Status, c.Note = StatusWaiting, note
		return c
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
	c := NewChange(r)
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
		// What a start waits for is the node extensions' to say (a k3s agent's waits for its server).
		if note := b.serviceStartGate(want.Node, want.Service); note != nil {
			c.Status, c.Note = StatusWaiting, note
			return c
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

// onNode is a change of a resource on a node. A grant travels in the node's revision, so while the
// node's mamelet refuses the revisions on offer no change can be carried out, whatever is approved:
// it is Blocked, with the node's own reason (MAM-PLAN-0130), instead of waiting for an approval and
// then for a result that does not come. A resource that is in sync stays in sync.
func (b builder) onNode(node string, c Change) Change {
	if note := b.refusal(node); note != nil && c.Status == StatusChange {
		c.Status, c.Note, c.Verdict = StatusBlocked, note, ""
		c.Operations, c.Diffs = []Operation{}, []Diff{}
		return c
	}
	return b.guard(node, c)
}

// refusal says that the mamelet of node refuses the Control Plane's revision (nil when it does not),
// and why, as the node reported it, with what to do about that kind of refusal.
func (b builder) refusal(node string) *Note {
	r, ok := b.snap.NodeRefusals[node]
	if !ok {
		return nil
	}
	from := "no revision"
	if r.WorksFrom > 0 {
		from = fmt.Sprintf("revision %d", r.WorksFrom)
	}
	hint := "see the reason; mamelet logs it too (journalctl -u mamelet on the node)"
	switch {
	case strings.Contains(r.Reason, noderev.ErrRolledBack.Error()):
		hint = "a mamelet never accepts a serial lower than one it accepted: the Control Plane's database may have been restored from an older copy; restore the newest one"
	case strings.Contains(r.Reason, noderev.ErrFromFuture.Error()), strings.Contains(r.Reason, noderev.ErrExpired.Error()):
		hint = "the clocks of the node and the Control Plane differ by more than a few minutes: set the node's clock (NTP)"
	case strings.Contains(r.Reason, noderev.ErrBadSignature.Error()):
		hint = "the node trusts another Control Plane's key than this one's: its configuration (revisionKey) or this Control Plane's keys are not the ones it was made with"
	case strings.Contains(r.Reason, noderev.ErrOtherNode.Error()):
		hint = "the revision is for another machine: the name and VMID in the node's configuration are not this node's"
	case strings.HasPrefix(r.Reason, "decode revision"):
		hint = "the revision has something this mamelet does not know: the node runs a mamelet older than this Control Plane; replace it by hand once and declare its release afterwards (docs/mamelet-update.md)"
	case strings.HasPrefix(r.Reason, "invalid revision"):
		hint = "mamelet found the revision invalid: change what the reason names in the declaration (a name or a value its checks do not allow)"
	}
	return &Note{Code: "MAM-PLAN-0130",
		Message: fmt.Sprintf("mamelet on node %s refuses the Control Plane's revision %d (it works from %s): %s", node, r.Serial, from, r.Reason),
		Hint:    hint}
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

// NewChange is the plan of a resource with nothing in it yet: no diff, no operation.
func NewChange(r graph.Resource) Change {
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
