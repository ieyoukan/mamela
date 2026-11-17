package plan

import (
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

// An extension cannot take a kind that is planned already, the core's or another's, nor a note code
// that is another's: a code in the plan's JSON keeps its meaning.
func TestRegisterRefusesWhatIsTaken(t *testing.T) {
	plan := Planner{Plan: func(*Kit, graph.Resource) Change { return Change{} }}
	for name, e := range map[string]Extension{
		"a core kind":         {Name: "x", Planners: map[graph.Kind]Planner{graph.KindNodePackage: plan}},
		"a kind planned":      {Name: "x", Planners: map[graph.Kind]Planner{graph.KindNodeK3s: plan}},
		"a core code":         {Name: "x", Codes: []string{"MAM-PLAN-0101"}},
		"another's code":      {Name: "x", Codes: []string{"MAM-PLAN-0108"}},
		"no name":             {Codes: []string{"MAM-PLAN-0999"}},
		"the core's name":     {Name: "core"},
		"a kind with no plan": {Name: "x", Planners: map[graph.Kind]Planner{"NodeOther": {}}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: registered", name)
				}
			}()
			Register(e)
		}()
	}
}
