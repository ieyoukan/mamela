package linux

import (
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/provider/spec/spectest"
)

// Every linux kind is in the node provider's contract, its requirements name the node and the
// object, and every operation needs an approval (an extension's operation is never automatic).
func TestEveryKindIsInTheContract(t *testing.T) {
	providers := spec.Providers{node.Manifest()}
	found := 0
	for _, r := range spectest.NodeResources(t) {
		if _, ok := kindOf(r.Kind); !ok {
			continue
		}
		found++
		reqs := providers.Analyze(&graph.Graph{Resources: []graph.Resource{r}}, nil)
		if len(reqs) == 0 {
			t.Errorf("%s: no requirement", r.Kind)
		}
		for _, q := range reqs {
			if q.Support == spec.Unsupported || q.Target != spectest.Node || !q.Resolved || strings.Contains(q.Object, "{") || strings.Contains(q.Object, r.Name) {
				t.Errorf("%s: %+v", r.Kind, q)
			}
		}
	}
	if found != len(kinds) {
		t.Errorf("%d of %d kinds", found, len(kinds))
	}
	for _, k := range kinds {
		for _, o := range k.ops {
			if d := policy.Default.Decide(policy.Subject{Op: o.op}); d.Verdict != policy.ApprovalRequired || d.Code != o.decision.Code {
				t.Errorf("%s: %+v", o.op, d)
			}
			if !strings.HasPrefix(string(o.op), Name+".") {
				t.Errorf("%s is not named linux.<verb>-<object>", o.op)
			}
		}
	}
	// The kinds keep their places: the order is in every graph's digest.
	if d, _ := graph.KindOf(KindFile); d.Order != 9 {
		t.Errorf("NodeFile is at %d", d.Order)
	}
}
