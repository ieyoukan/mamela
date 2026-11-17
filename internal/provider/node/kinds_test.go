package node_test

import (
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/provider/spec/spectest"
)

// Every kind that lives on a node is covered by the node provider's contract, and its requirements
// name the node and the object (audit B17: a manifest's requirements named "{node}", and the
// resource's name where the manifest's was meant). A kind that is added and left out fails here.
func TestEveryNodeKindHasRequirementsThatNameTheirNode(t *testing.T) {
	providers := spec.Providers{node.Manifest()}
	for _, r := range spectest.NodeResources(t) {
		reqs := providers.Analyze(&graph.Graph{Resources: []graph.Resource{r}}, nil)
		if len(reqs) == 0 {
			t.Errorf("%s: no requirement at all", r.Kind)
		}
		for _, q := range reqs {
			switch {
			case q.Support == spec.Unsupported:
				t.Errorf("%s: %s: no provider handles it: %s", r.Kind, q.Op, q.Why)
			case q.Target != spectest.Node || !q.Resolved:
				t.Errorf("%s %s: target is %q (resolved %v), want the node %q", r.Kind, q.Capability, q.Target, q.Resolved, spectest.Node)
			case strings.Contains(q.Object, "{") || strings.Contains(q.Object, r.Name):
				t.Errorf("%s %s: object is %q: a template that was not filled in, or the resource's name where a value of its spec is meant", r.Kind, q.Capability, q.Object)
			}
		}
	}
}
