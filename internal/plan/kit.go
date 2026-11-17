package plan

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
)

// A node extension (k3s) plans the resources of its kinds through a Kit: a read-only view of what
// the plan is made of (the declaration, the observation, the policy) and the core's judgements
// about the nodes (is the machine ready, is the observation stale, has the agent reported). It
// cannot change the inputs, and it decides nothing itself: an operation's decision is the policy's
// (Decide, WithOps), a change on a node the Control Plane must not touch is denied after the
// planner (onNode), and a deletion is approved only on the Control Plane's host (Operation.Deletes,
// enforced where plans are approved).
//
// Extensions are compiled in (docs/design/node-extensions.md): they register in init, and the
// registrations are fixed before any plan is made, so a plan is still a pure function of its inputs.
type Kit struct {
	b    builder
	snap state.Snapshot
}

func newKit(b builder) *Kit {
	s := b.snap
	// The maps are the planner's own copies: what it does to them does not reach the core's view.
	s.Packages, s.Services, s.Provided = maps.Clone(s.Packages), maps.Clone(s.Services), maps.Clone(s.Provided)
	s.K3s, s.Mamelets, s.K3sManifests = maps.Clone(s.K3s), maps.Clone(s.Mamelets), maps.Clone(s.K3sManifests)
	s.Nodes, s.AgentRegistrations, s.NodeRefusals = maps.Clone(s.Nodes), maps.Clone(s.AgentRegistrations), maps.Clone(s.NodeRefusals)
	return &Kit{b: b, snap: s}
}

// Resources are the declared resources, in canonical order.
func (k *Kit) Resources() []graph.Resource {
	if k.b.g == nil {
		return nil
	}
	return k.b.g.Sorted()
}

// StaticIP is the declared static address of machine name ("" for DHCP or none).
func (k *Kit) StaticIP(name string) string {
	if k.b.g == nil {
		return ""
	}
	return k.b.g.StaticIP(name)
}

// Snapshot is the observation the plan is made of (the planner's own copy).
func (k *Kit) Snapshot() state.Snapshot { return k.snap }

// Policy is the policy the plan is made with.
func (k *Kit) Policy() policy.Policy { return k.b.pol }

// Decide is the policy's decision for an operation.
func (k *Kit) Decide(s policy.Subject) policy.Decision { return k.b.pol.Decide(s) }

// NodeReady is whether the node's machine exists, is Mamela's and runs; if not, the note says what
// a resource on it waits for.
func (k *Kit) NodeReady(node string) (bool, *Note) { return k.b.nodeReady(node) }

// AwaitAgent is how a resource waits for the first report of its node's agent (patiently on a young
// machine, blocked on one whose agent has been silent for long).
func (k *Kit) AwaitAgent(node string, waiting *Note) (Status, *Note) {
	return k.b.awaitAgent(node, waiting)
}

// Stale is whether an observation made at observedAt is too old to act on.
func (k *Kit) Stale(observedAt time.Time) bool { return k.b.stale(observedAt) }

// StaleNote is the note of a resource whose observation is too old.
func (k *Kit) StaleNote(observedAt time.Time) *Note { return k.b.staleNote(observedAt) }

// WithOps is c with the operations of the subjects, each decided by the policy (in sync without).
func (k *Kit) WithOps(c Change, subjects ...policy.Subject) Change {
	return k.b.withOps(c, subjects...)
}

// MachineLeaves is whether this plan deletes the machine called name (ADR 0013).
func (k *Kit) MachineLeaves(name string) bool { return k.b.machineLeaves(name) }

// MachineExists is whether any machine called name is observed.
func (k *Kit) MachineExists(name string) bool { return k.b.machineExists(name) }

// MachineRestarts is whether this plan restarts (reboots, stops) the existing machine called name
// that the declaration has.
func (k *Kit) MachineRestarts(name string) bool {
	for _, m := range k.Resources() {
		if m.Provided != nil && m.Name == name && k.b.restartsMachine(k.b.snap.Provided[m.ID()]) && k.b.snap.Provided[m.ID()].Observed.Exists {
			return true
		}
	}
	return false
}

// Planner plans the resources of one kind of a node extension.
type Planner struct {
	// Plan is the change of a resource of the kind. Its warnings come after the declaration's.
	Plan func(k *Kit, r graph.Resource) Change
	// Reported is whether the node's agent has reported the resource (so the agent is not silent).
	Reported func(k *Kit, r graph.Resource) bool
	// Deleted names, for a person, what a change that deletes deletes ("Kubernetes node a2").
	Deleted func(c Change) []string
}

var (
	planners     = map[graph.Kind]Planner{}
	restartGates []func(k *Kit, machine graph.Resource) *Note
	startGates   []func(k *Kit, node, service string) *Note
	// codeOwners: which extension each note code is ("core" for the core's own). Set before any
	// init runs, so an extension's registration sees the core's codes.
	codeOwners = func() map[string]string {
		m := map[string]string{}
		for _, c := range coreCodes {
			m[c] = "core"
		}
		return m
	}()
)

// coreCodes are the codes of the core's notes. A note's code is in the plan's JSON, so it never
// changes meaning, and no extension may take one.
var coreCodes = []string{"MAM-PLAN-0101", "MAM-PLAN-0102", "MAM-PLAN-0103", "MAM-PLAN-0104", "MAM-PLAN-0105", "MAM-PLAN-0106",
	"MAM-PLAN-0107", "MAM-PLAN-0110", "MAM-PLAN-0111", "MAM-PLAN-0113", "MAM-PLAN-0114", "MAM-PLAN-0119", "MAM-PLAN-0120",
	"MAM-PLAN-0130", "MAM-PLAN-0201", "MAM-PLAN-0202", "MAM-PLAN-0203", "MAM-PLAN-0301", "MAM-PLAN-0305"}

// Extension is what a node extension adds to planning.
type Extension struct {
	Name string
	// Planners by kind (the kinds the extension registered with graph).
	Planners map[graph.Kind]Planner
	// RestartGate says what the restart of a machine waits for (nil: nothing).
	RestartGate func(k *Kit, machine graph.Resource) *Note
	// StartGate says what the start of a service on a node waits for (nil: nothing).
	StartGate func(k *Kit, node, service string) *Note
	// Codes are the codes of the extension's notes: each is the extension's alone.
	Codes []string
}

// Register adds a node extension's planning. It panics on a kind that has a planner already and on
// a note code that is another's: both are programming errors, found when the program starts.
func Register(e Extension) {
	if e.Name == "" || e.Name == "core" {
		panic("plan: an extension needs a name of its own")
	}
	for _, c := range e.Codes {
		if owner, taken := codeOwners[c]; taken && owner != e.Name {
			panic(fmt.Sprintf("plan: extension %s: code %s is %s's", e.Name, c, owner))
		}
	}
	for _, kind := range slices.Sorted(maps.Keys(e.Planners)) {
		if _, taken := planners[kind]; taken || graph.CoreKind(kind) {
			panic(fmt.Sprintf("plan: extension %s: kind %s has a planner already", e.Name, kind))
		}
		if e.Planners[kind].Plan == nil {
			panic(fmt.Sprintf("plan: extension %s: kind %s has no Plan", e.Name, kind))
		}
	}
	for _, c := range e.Codes {
		codeOwners[c] = e.Name
	}
	maps.Copy(planners, e.Planners)
	if e.RestartGate != nil {
		restartGates = append(restartGates, e.RestartGate)
	}
	if e.StartGate != nil {
		startGates = append(startGates, e.StartGate)
	}
}

// machineRestartGate says what a restart of the machine r waits for, by the extensions' gates.
func (b builder) machineRestartGate(r graph.Resource) *Note {
	for _, gate := range restartGates {
		if note := gate(newKit(b), r); note != nil {
			return note
		}
	}
	return nil
}

// serviceStartGate says what the start of a service waits for, by the extensions' gates.
func (b builder) serviceStartGate(node, service string) *Note {
	for _, gate := range startGates {
		if note := gate(newKit(b), node, service); note != nil {
			return note
		}
	}
	return nil
}
