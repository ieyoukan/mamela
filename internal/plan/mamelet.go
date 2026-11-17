package plan

import (
	"fmt"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
)

// mamelet plans the update of a node's mamelet to the release its declaration pins. Nodes are
// updated one at a time, in the order of their names, and the next only after the ones before it
// run what is declared: an update that does not work stops at the first node it was tried on.
func (b builder) mamelet(r graph.Resource) Change {
	want := r.NodeMamelet
	c := NewChange(r)
	if ok, note := b.nodeReady(want.Node); !ok {
		c.Status, c.Note = StatusWaiting, note
		return c
	}
	got, observed := b.snap.Mamelets[r.Name]
	switch {
	case !observed:
		c.Status, c.Note = b.awaitAgent(want.Node, &Note{Code: "MAM-PLAN-0119",
			Message: fmt.Sprintf("waits for the agent on %s to report which mamelet it runs", want.Node),
			Hint:    "a mamelet from before updates were declared does not report it: replace it by hand once (docs/mamelet-update.md)"})
		return c
	case b.stale(got.ObservedAt):
		c.Status, c.Note = StatusBlocked, b.staleNote(got.ObservedAt)
		return c
	case got.SHA256 == want.SHA256:
		return b.withOps(c)
	}
	for _, other := range b.g.Sorted() {
		o := other.NodeMamelet
		if o == nil || o.Node >= want.Node || b.pol.Protected(o.Node) {
			continue
		}
		if og, seen := b.snap.Mamelets[other.Name]; seen && og.SHA256 != o.SHA256 {
			c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0120",
				Message: fmt.Sprintf("waits for mamelet on %s to run its declared release before %s is updated (nodes are updated one at a time)", o.Node, want.Node),
				Hint:    "if the update of " + o.Node + " failed, find out why (the plan of " + o.Node + " says); the others wait"}
			return c
		}
	}
	c.Diffs = []Diff{{"sha256", got.SHA256, want.SHA256}, {"version", got.Version, want.Version},
		{"from", "", "the Control Plane: /v1/mamelet/" + want.SHA256 + " (the binary of controlplane.mamelet_binary)"},
		{"restart", "", "mamelet on " + want.Node + " restarts into it and reports again in a few seconds; the previous binary is kept as " + noderev.MameletPrevPath +
			", and mamelet goes back to it if the new one has not reported within 10 minutes"}}
	return b.withOps(c, policy.Subject{Op: policy.OpUpdateMamelet})
}
