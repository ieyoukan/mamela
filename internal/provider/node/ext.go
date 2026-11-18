package node

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
)

// A node extension (k3s, docs/design/node-extensions.md) adds to the node provider what is its:
// what mamelet's reports say of its kinds, what a node's revision carries for them, and the grants
// of its operations. The core keeps what makes a grant safe: it is for one approved operation and
// attempt, it ends with its deadline (and with the approval), it reaches only the node it is for
// (the registration the plan saw), a protected node gets none, and the core waits for mamelet's
// result.

// Extension is what a node extension adds to the node provider.
type Extension struct {
	Name string
	// Wire is the version of the extension's part of a revision (Revision.Ext[Name]) that the Control
	// Plane writes; 0 for an extension that has members of its own in the revision (k3s). A node gets
	// the part only when its mamelet takes this version or a later one (Report.Extensions): the core
	// takes the part out of the revision of any other node (capability negotiation, noderev's ext.go),
	// and observes what the node's mamelet reports of it (state.Snapshot.Ext).
	Wire int
	// Kinds are the extension's kinds in the provider's manifest (what each operation needs).
	Kinds []spec.Kind
	// Observe adds what a node's report says of the extension's kinds to the snapshot (received:
	// when the report arrived).
	Observe func(r noderev.Report, received time.Time, snap *state.Snapshot)
	// Publish works out what a node's revision gets from the declaration g, and returns the change
	// to make to it: it adds what is new and takes away nothing the declaration in force allows.
	Publish func(ctx context.Context, st *store.Store, g *graph.Graph, node string) (func(r *noderev.Revision), error)
	// Protect takes every grant of the extension out of the revision of a protected node.
	Protect func(r *noderev.Revision)
	// Prune takes out of node's revision what g no longer declares, with its grants.
	Prune func(g *graph.Graph, node string, r *noderev.Revision)
	// Grants are the extension's operations that mamelet carries out.
	Grants map[policy.Op]GrantFunc
}

// GrantFunc says how an approved operation is granted: ok is false when the resource is not one
// the operation is for. An error stops the operation before anything is granted.
type GrantFunc func(ctx context.Context, q GrantRequest) (g Grant, ok bool, err error)

// GrantRequest is an approved operation to grant.
type GrantRequest struct {
	Store *store.Store
	Step  reconcile.Step
	// Declaration is the declaration of the approved plan.
	Declaration *graph.Graph
}

// Approved is the value a field gets in the approved change ("" when it has no such diff): what the
// grant must match, or nothing happens.
func (q GrantRequest) Approved(field string) string { return approvedDiff(q.Step.Approved, field) }

// Grant is the grant of one operation on one node.
type Grant struct {
	Node string
	What string // for a person: "install k3s v1.37.2+k3s1"
	// Put puts the grant into the node's revision. once is the one-time permission of the
	// operation for an action, with the core's operation, attempt and deadline.
	Put func(rev *noderev.Revision, once func(action string) noderev.Once) error
}

var (
	extensions []Extension
	grants     = map[policy.Op]GrantFunc{}
)

// Register adds a node extension. It panics when its name, a kind or an operation is taken: a
// programming error, found when the program starts.
func Register(e Extension) {
	if e.Name == "" || slices.ContainsFunc(extensions, func(o Extension) bool { return o.Name == e.Name }) {
		panic(fmt.Sprintf("node: extension %q: a name of its own is needed", e.Name))
	}
	for _, k := range e.Kinds {
		if graph.CoreKind(k.Kind) || slices.ContainsFunc(extensions, func(o Extension) bool {
			return slices.ContainsFunc(o.Kinds, func(ok spec.Kind) bool { return ok.Kind == k.Kind })
		}) {
			panic(fmt.Sprintf("node: extension %s: kind %s is taken", e.Name, k.Kind))
		}
	}
	for op := range e.Grants {
		if _, taken := grants[op]; taken || slices.Contains([]policy.Op{policy.OpInstallPackage, policy.OpConfigureService, policy.OpUpdateMamelet}, op) || serviceActions[op] != "" {
			panic(fmt.Sprintf("node: extension %s: operation %s is taken", e.Name, op))
		}
	}
	if e.Wire > 0 && noderev.PartVersion(e.Name) != e.Wire {
		panic(fmt.Sprintf("node: extension %s: its part is registered with noderev at version %d, not %d", e.Name, noderev.PartVersion(e.Name), e.Wire))
	}
	for op, g := range e.Grants {
		grants[op] = g
		if e.Wire > 0 {
			wireOps[op] = e.Name
		}
	}
	extensions = append(extensions, e)
}

// wireOps names the extension of each operation whose grant goes into an extension's part.
var wireOps = map[policy.Op]string{}

// taken is the node's current report (nil when it has none under its current registration): what its
// mamelet takes is noderev.Takes of it.
func taken(ctx context.Context, st *store.Store, node string) (*noderev.Report, error) {
	nr, ok, err := st.CurrentNodeReport(ctx, node)
	if err != nil || !ok {
		return nil, err
	}
	return &nr.Report, nil
}

// observeExt adds to the snapshot what a node's report says of the extensions of the generic wire
// (received: when it arrived): only of the extensions this Control Plane has and the mamelet lists.
func observeExt(r noderev.Report, received time.Time, snap *state.Snapshot) {
	for _, e := range extensions {
		v, listed := r.Extensions[e.Name]
		if e.Wire == 0 || !listed {
			continue
		}
		o := state.ExtObservation{Accepts: v, ObservedAt: received}
		if why, refused := r.ExtRefused[e.Name]; refused {
			o.Refused = printable(why, maxRefusal)
			if o.Refused == "" {
				o.Refused = "(no reason given)"
			}
		}
		if p := r.Ext[e.Name]; len(p) > 0 {
			c := p.Clone()
			if c.Canonicalize() == nil && c.Kind() == '{' {
				o.Payload = c
			}
		}
		if snap.Ext == nil {
			snap.Ext = map[string]map[string]state.ExtObservation{}
		}
		if snap.Ext[r.Node] == nil {
			snap.Ext[r.Node] = map[string]state.ExtObservation{}
		}
		snap.Ext[r.Node][e.Name] = o
	}
}
