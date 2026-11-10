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
	StatusRetained Status = "retained" // removed from the declaration, kept as is
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
	b := builder{snap: snap, pol: pol}
	p := &Plan{Inputs: Inputs{
		RevisionDigest:  g.Digest(),
		SnapshotDigest:  snap.Digest(),
		PolicyDigest:    pol.Digest(),
		ProvidersDigest: providers,
		AsOf:            snap.AsOf.UTC(),
	}}
	desiredVMs := map[string]bool{}
	for _, r := range g.Sorted() {
		switch r.Kind {
		case graph.KindVirtualMachine:
			desiredVMs[r.Name] = true
			p.Changes = append(p.Changes, b.vm(r))
		case graph.KindNodePackage:
			p.Changes = append(p.Changes, b.pkg(r))
		case graph.KindNodeService:
			p.Changes = append(p.Changes, b.service(r))
		case graph.KindNodeK3s:
			p.Changes = append(p.Changes, b.k3s(r))
		default:
			if r.Provided != nil {
				p.Changes = append(p.Changes, b.provided(r))
			}
		}
	}
	// Managed VMs that are no longer declared are kept: removal is not deletion.
	for _, name := range slices.Sorted(maps.Keys(snap.VMs)) {
		if vm := snap.VMs[name]; vm.Managed && !desiredVMs[name] {
			p.Changes = append(p.Changes, Change{Kind: graph.KindVirtualMachine, Name: name, Status: StatusRetained,
				Diffs: []Diff{}, Operations: []Operation{},
				Note: &Note{Code: "MAM-PLAN-0301", Message: "removed from the declaration; Mamela keeps it and never deletes it",
					Hint: "delete it by hand if it is no longer needed"}})
		}
	}
	// The same for the resources of providers: a provider reports what Mamela created.
	declared := map[string]bool{}
	for _, r := range g.Resources {
		declared[r.ID()] = true
	}
	for _, id := range slices.Sorted(maps.Keys(snap.Provided)) {
		if o := snap.Provided[id]; !o.Declared && !declared[id] && o.Observed.Exists && o.Observed.Owned {
			p.Changes = append(p.Changes, Change{Kind: o.Kind, Name: o.Name, Status: StatusRetained, Provider: o.Provider,
				Diffs: []Diff{}, Operations: []Operation{},
				Note: &Note{Code: "MAM-PLAN-0301", Message: "removed from the declaration; Mamela keeps it and never deletes it",
					Hint: "delete it by hand if it is no longer needed"}})
		}
	}
	p.Digest = digest(p)
	return p
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
	case got.Observed.Exists && !got.Observed.Owned:
		return b.withOps(c, policy.Subject{Op: policy.OpChangeForeign})
	}
	if len(got.Changes) == 0 {
		return b.withOps(c)
	}
	c.Status, c.Verdict = StatusChange, policy.Auto
	for _, pc := range got.Changes {
		for _, reason := range pc.Change.Reasons {
			c.Diffs = append(c.Diffs, Diff{Field: string(pc.Change.Op), To: reason})
		}
		d := b.pol.DecideProvided(pc.Verified)
		c.Operations = append(c.Operations, Operation{Op: pc.Change.Op, Decision: d})
		c.Verdict = policy.Stricter(c.Verdict, d.Verdict)
		c.ProviderChanges = append(c.ProviderChanges, pc.Change)
	}
	return c
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
}

func (b builder) stale(observedAt time.Time) bool {
	return b.snap.AsOf.Sub(observedAt) > b.pol.MaxObservationAge()
}

func (b builder) staleNote(observedAt time.Time) *Note {
	return &Note{Code: "MAM-PLAN-0201",
		Message: fmt.Sprintf("the last observation is from %s, older than %s", observedAt.UTC().Format(time.RFC3339), b.pol.MaxObservationAge()),
		Hint:    "observe again before planning; Mamela does not act on stale information"}
}

func (b builder) vm(r graph.Resource) Change {
	want := r.VirtualMachine
	c := newChange(r)
	got, exists := b.snap.VMs[r.Name]
	if !exists {
		c.Diffs = []Diff{
			{"cpu", "", itoa(want.CPU)}, {"memoryMiB", "", itoa(want.MemoryMiB)},
			{"diskGiB", "", itoa(want.DiskGiB)}, {"image", "", want.Image},
		}
		return b.withOps(c, policy.Subject{Op: policy.OpCreateVM})
	}
	if !got.Managed {
		c.Diffs = []Diff{}
		return b.withOps(c, policy.Subject{Op: policy.OpChangeForeign})
	}
	if b.stale(got.ObservedAt) {
		c.Status, c.Note = StatusBlocked, b.staleNote(got.ObservedAt)
		return c
	}
	var subjects []policy.Subject
	// The disk first: resizing CPU or memory restarts a running VM, and the guest grows its
	// filesystem to the disk at boot (cloud-init growpart).
	if got.DiskGiB != want.DiskGiB {
		c.Diffs = append(c.Diffs, Diff{"diskGiB", itoa(got.DiskGiB), itoa(want.DiskGiB)})
		op := policy.OpGrowVMDisk
		if want.DiskGiB < got.DiskGiB {
			op = policy.OpShrinkVMDisk
		}
		subjects = append(subjects, policy.Subject{Op: op})
	}
	if got.CPU != want.CPU {
		c.Diffs = append(c.Diffs, Diff{"cpu", itoa(got.CPU), itoa(want.CPU)})
		subjects = append(subjects, policy.Subject{Op: policy.OpResizeVMCPU})
	}
	if got.MemoryMiB != want.MemoryMiB {
		c.Diffs = append(c.Diffs, Diff{"memoryMiB", itoa(got.MemoryMiB), itoa(want.MemoryMiB)})
		subjects = append(subjects, policy.Subject{Op: policy.OpResizeVMMemory})
	}
	if got.Image != want.Image {
		c.Diffs = append(c.Diffs, Diff{"image", got.Image, want.Image})
		subjects = append(subjects, policy.Subject{Op: policy.OpReplaceVMImage})
	}
	return b.withOps(c, subjects...)
}

// nodeReady reports whether the node's VM exists, is managed and freshly observed. Without that,
// node resources wait: there is nothing (or nothing trustworthy) to observe them on.
func (b builder) nodeReady(node string) (bool, *Note) {
	vm, ok := b.snap.VMs[node]
	switch {
	case !ok:
		return false, &Note{Code: "MAM-PLAN-0101", Message: fmt.Sprintf("waits for VM %s to be created", node)}
	case !vm.Managed:
		return false, &Note{Code: "MAM-PLAN-0102", Message: fmt.Sprintf("VM %s is not managed by Mamela (Foreign)", node),
			Hint: "Mamela does not configure machines it did not create"}
	}
	return true, nil
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
		c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0103",
			Message: fmt.Sprintf("waits for the agent on %s to report packages", want.Node)}
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
		c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0104",
			Message: fmt.Sprintf("waits for the agent on %s to report services", want.Node)}
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
	if got.Running != want.Running {
		c.Diffs = append(c.Diffs, Diff{"running", btoa(got.Running), btoa(want.Running)})
		op := policy.OpStartService
		if !want.Running {
			op = policy.OpStopService
		}
		subjects = append(subjects, policy.Subject{Op: op, Service: want.Service})
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
		c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0108",
			Message: fmt.Sprintf("waits for the agent on %s to report k3s", want.Node)}
		return c
	case b.stale(got.ObservedAt):
		c.Status, c.Note = StatusBlocked, b.staleNote(got.ObservedAt)
		return c
	case got.Error != "":
		c.Status, c.Note = StatusBlocked, &Note{Code: "MAM-PLAN-0204",
			Message: fmt.Sprintf("the agent on %s cannot observe k3s: %s", want.Node, got.Error),
			Hint:    "fix it on the node, then plan again"}
		return c
	case got.Installed && got.SHA256 == want.SHA256:
		return b.withOps(c)
	}
	// The diff says exactly what is approved: the release, the binary's sha256, where it comes
	// from and how it runs.
	c.Diffs = []Diff{{"version", "", want.Version}, {"sha256", got.SHA256, want.SHA256},
		{"from", "", noderev.K3sReleases + "/" + noderev.K3sTag(want.Version) + "/k3s (k3s-arm64 on arm64)"},
		{"runs", "", "/usr/local/bin/k3s " + strings.Join(noderev.K3sArgs, " ")}}
	return b.withOps(c, policy.Subject{Op: policy.OpInstallK3s})
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

func itoa(n int64) string { return fmt.Sprintf("%d", n) }

func btoa(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
