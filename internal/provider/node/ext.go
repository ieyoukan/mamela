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
	for op, g := range e.Grants {
		grants[op] = g
	}
	extensions = append(extensions, e)
}
