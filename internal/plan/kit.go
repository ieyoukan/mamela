package plan

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
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
	if s.Ext != nil {
		ext := make(map[string]map[string]state.ExtObservation, len(s.Ext))
		for node, exts := range s.Ext {
			ext[node] = make(map[string]state.ExtObservation, len(exts))
			for name, o := range exts {
				o.Payload = o.Payload.Clone()
				ext[node][name] = o
			}
		}
		s.Ext = ext
	}
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

// Ext is what the mamelet of node reported of the node extension ext (stage 5 of
// docs/design/node-extensions.md): the version of its part it takes, what it observed (the
// extension's own JSON, a copy) and whether it refused the part. ok is false when it reported
// nothing of the extension.
func (k *Kit) Ext(node, ext string) (state.ExtObservation, bool) {
	o, ok := k.snap.Ext[node][ext]
	o.Payload = o.Payload.Clone()
	return o, ok
}

// ExtNotTaken says why the mamelet of node cannot carry out anything of the node extension ext,
// whose part of a revision is at version (nil when it can, or when the node's agent has not reported
// at all: a resource then waits for it, AwaitAgent). It does not list the extension, or lists an
// older version of its part (it is older than the extension: release is the mamelet release that
// brought this version), or it refused the extension's part of its revision. The Control Plane
// gives such a node no part of the extension (capability negotiation), so a change of a resource of
// the extension there is not approved and then never carried out:
//
//   - Waiting (MAM-PLAN-0123) when the same plan updates the node's mamelet (its NodeMamelet is a
//     change, or waits for the nodes before it): that is the way to the extension
//     (docs/mamelet-update.md), and the plan after the update has the extension's part. An update
//     to a release whose label says it is older than release does not bring it.
//   - Blocked (MAM-PLAN-0122) otherwise, and always when the mamelet refused the part.
//
// It is blocked too when the node's mamelet takes the extension but the declaration puts a release on
// the node whose label says it is older than release (a downgrade): after it, the node would get
// nothing of the extension.
func (k *Kit) ExtNotTaken(node, ext string, version int, release string) (Status, *Note) {
	o, listed := k.snap.Ext[node][ext]
	if listed && o.Refused != "" {
		hint := fmt.Sprintf("mamelet read the part and refused what it says (the reason above; journalctl -u mamelet on the node logs it too), and goes on with the rest of the revision: "+
			"change what the reason names (a value, how many) and plan again; if it names nothing the declaration has, this Control Plane and the mamelet on %s are of different releases: update them (the Control Plane first, docs/mamelet-update.md)", node)
		if strings.Contains(o.Refused, "does not know the extension") || strings.Contains(o.Refused, "cannot be read") {
			hint = "mamelet could not read the part (an extension or a member it does not know): it is older than this Control Plane, so update it (docs/mamelet-update.md); it goes on with the rest of the revision meanwhile"
		}
		return StatusBlocked, &Note{Code: "MAM-PLAN-0122",
			Message: fmt.Sprintf("mamelet on node %s refused the part of the %s extension in its revision: %s", node, ext, o.Refused),
			Hint:    hint}
	}
	if listed && o.Accepts >= version {
		if pinned := k.mameletPinned(node); releaseOlder(pinned, release) {
			// It takes the extension now, but the declaration puts an older mamelet on the node: its
			// update would come first, and nothing of the extension would reach the node after it.
			return StatusBlocked, &Note{Code: "MAM-PLAN-0122",
				Message: fmt.Sprintf("this declaration puts mamelet %s on node %s, which does not carry out the %s extension (mamelet %s and later do): once it runs there, Mamela gives the node nothing of it, and an approved change of it is never carried out (MAM-NODE-0104)",
					pinned, node, ext, release),
				Hint: fmt.Sprintf("declare mamelet %s or later on %s (Mamela.mamelet), or take the %s resources off the node (they stay on it as they are)", release, node, ext)}
		}
		return "", nil
	}
	m, known := k.snap.Mamelets[node+"/mamelet"]
	if !known && !listed && !k.agentReported(node) {
		return "", nil
	}
	running := "a release that does not say which"
	if m.Version != "" {
		running = m.Version
	}
	if to, ok := k.mameletUpdate(node, release); ok {
		return StatusWaiting, &Note{Code: "MAM-PLAN-0123",
			Message: fmt.Sprintf("waits for mamelet on node %s to be updated to %s by this plan: it runs %s, which does not carry out the %s extension (mamelet %s and later do)",
				node, to, running, ext, release),
			Hint: "apply the plan that updates mamelet; the plan after it has what the extension does on " + node}
	}
	return StatusBlocked, &Note{Code: "MAM-PLAN-0122",
		Message: fmt.Sprintf("mamelet on node %s runs %s, which does not carry out the %s extension (mamelet %s and later do): Mamela gives the node nothing of it",
			node, running, ext, release),
		Hint: fmt.Sprintf("update mamelet on %s first, to %s or later: put it at the site's controlplane.mamelet_binary, declare it with Mamela.mamelet and apply (docs/mamelet-update.md; a Control Plane is updated before its mamelets); then plan again",
			node, release)}
}

// mameletPinned is the release label of the mamelet the declaration puts on node ("" when it declares
// none, or none with a label).
func (k *Kit) mameletPinned(node string) string {
	for _, r := range k.Resources() {
		if m := r.NodeMamelet; m != nil && m.Node == node {
			return m.Version
		}
	}
	return ""
}

// mameletUpdate says whether this plan updates the mamelet of node (its NodeMamelet is a change that
// is not denied, or waits for the nodes before it to run their declared release), and to which
// release (its label, or its sha256). A release whose label is older than atLeast is no update that
// helps: ok is false.
func (k *Kit) mameletUpdate(node, atLeast string) (to string, ok bool) {
	for _, r := range k.Resources() {
		m := r.NodeMamelet
		if m == nil || m.Node != node || releaseOlder(m.Version, atLeast) {
			continue
		}
		c := k.b.onNode(node, k.b.mamelet(r))
		pending := c.Status == StatusChange && c.Verdict != policy.Denied ||
			c.Status == StatusWaiting && c.Note != nil && c.Note.Code == "MAM-PLAN-0120"
		if !pending {
			continue
		}
		if m.Version != "" {
			return m.Version, true
		}
		return "the release of sha256 " + m.SHA256[:min(12, len(m.SHA256))], true
	}
	return "", false
}

// releaseOlder is whether the release label a ("0.9.2", "v0.9.2+abc") is older than b ("0.10.0"). A
// label that does not say (empty, or not major.minor.patch) is not older: it may be any release.
func releaseOlder(a, b string) bool {
	x, okA := releaseParts(a)
	y, okB := releaseParts(b)
	if !okA || !okB {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}

func releaseParts(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "+-"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// agentReported is whether the node's agent has reported anything at all.
func (k *Kit) agentReported(node string) bool {
	if _, ok := k.snap.Ext[node]; ok {
		return true
	}
	for key := range k.snap.Packages {
		if strings.HasPrefix(key, node+"/") {
			return true
		}
	}
	for key := range k.snap.Services {
		if strings.HasPrefix(key, node+"/") {
			return true
		}
	}
	return false
}

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
// that the declaration has. A machine the policy holds (the Control Plane's host, observe-only) is
// not restarted by any plan: nothing waits for it.
func (k *Kit) MachineRestarts(name string) bool {
	if k.b.pol.Holds(name) {
		return false
	}
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
	packageGates []func(k *Kit, node, pkg string) *Note
	extUndecl    []func(k *Kit) []Change
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
	"MAM-PLAN-0122", "MAM-PLAN-0123", "MAM-PLAN-0130", "MAM-PLAN-0201", "MAM-PLAN-0202", "MAM-PLAN-0203", "MAM-PLAN-0301", "MAM-PLAN-0305"}

// Extension is what a node extension adds to planning.
type Extension struct {
	Name string
	// Planners by kind (the kinds the extension registered with graph).
	Planners map[graph.Kind]Planner
	// RestartGate says what the restart of a machine waits for (nil: nothing).
	RestartGate func(k *Kit, machine graph.Resource) *Note
	// StartGate says what the start of a service on a node waits for (nil: nothing).
	StartGate func(k *Kit, node, service string) *Note
	// PackageGate says what the installation of a package on a node waits for (nil: nothing), for
	// example the APT source it comes from.
	PackageGate func(k *Kit, node, pkg string) *Note
	// Undeclared are the changes of what the extension observed on a node that no resource of the
	// declaration has any more: each retained (StatusRetained, with a note that says why and what
	// becomes of it), never an operation. They come after the declared resources and the providers'.
	Undeclared func(k *Kit) []Change
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
	if e.PackageGate != nil {
		packageGates = append(packageGates, e.PackageGate)
	}
	if e.Undeclared != nil {
		extUndecl = append(extUndecl, e.Undeclared)
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

// packageGate says what the installation of a package waits for, by the extensions' gates.
func (b builder) packageGate(node, pkg string) *Note {
	for _, gate := range packageGates {
		if note := gate(newKit(b), node, pkg); note != nil {
			return note
		}
	}
	return nil
}

// extUndeclared are the extensions' changes of what no declaration has any more: kept as they are.
// An extension cannot plan an operation this way (only a declared resource has one).
func (b builder) extUndeclared() []Change {
	var out []Change
	for _, u := range extUndecl {
		for _, c := range u(newKit(b)) {
			if c.Status != StatusRetained || len(c.Operations) > 0 || c.Removes || c.Verdict != "" {
				continue
			}
			if c.Diffs == nil {
				c.Diffs = []Diff{}
			}
			c.Operations = []Operation{}
			out = append(out, c)
		}
	}
	return out
}
