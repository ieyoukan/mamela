package site

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/provider/protocol"
)

const l2 = `{"schema":"mamela-graph/v2","resources":[
{"kind":"TestVM","name":"db","provider":"test","spec":{"cpu":1}},
{"kind":"NodePackage","name":"db/nginx","spec":{"node":"db","package":"nginx","state":"Present"}},
{"kind":"NodeService","name":"db/nginx","spec":{"node":"db","service":"nginx","enabled":true,"running":true}}]}`

var t0 = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

// world is machine db, Mamela's and running, with what its agent reported.
func world(t *testing.T, g *graph.Graph) state.Snapshot {
	t.Helper()
	snap := state.Empty(t0)
	m := g.Resources[0]
	snap.AddProvided(map[string]state.Provided{m.ID(): {Provider: "test", Kind: m.Kind, Name: m.Name, Declared: true,
		SpecDigest: state.SpecDigest(m.Provided.Spec), ObservedAt: t0,
		Observed: protocol.Observed{ID: m.ID(), Kind: m.Kind, Name: m.Name, Exists: true, Owned: true, IDs: map[string]string{"n": "1"},
			Node: &protocol.ObservedNode{ID: 7001, Running: true, UptimeSeconds: 60}}}})
	return snap
}

// What a plan uses of mamelet is concrete: each grant's conditions hold for exactly the approved
// operation, and a start the policy allows is a standing grant.
func TestPlannedRequirementsAreConcrete(t *testing.T) {
	g, err := graph.Decode([]byte(l2))
	if err != nil {
		t.Fatal(err)
	}
	snap := world(t, g)
	cs := noderev.NewChangeSet([]noderev.Change{{Action: "install", Package: "nginx", Version: "1", Origins: []string{"Ubuntu:24.04/noble"}}})
	snap.Packages["db/nginx"] = state.Package{ChangeSet: &cs, ObservedAt: t0}
	snap.Services["db/nginx"] = state.Service{Exists: true, ObservedAt: t0}
	pol := policy.Default
	pol.AutoStartServices = []string{"nginx"}
	p := plan.Build(g, snap, pol)

	var s *Site
	byOp := map[string][]spec.Requirement{}
	for _, q := range s.PlannedRequirements(p, g, pol) {
		k := q.Resource + " " + string(q.Op)
		byOp[k] = append(byOp[k], q)
	}
	mamelet := func(k, capability string) spec.Requirement {
		reqs := byOp[k]
		if len(reqs) != 1 || reqs[0].Provider != "mamelet" || reqs[0].Capability != capability || reqs[0].Target != "db" ||
			reqs[0].TargetType != "node" || reqs[0].Object != "nginx" {
			t.Errorf("%s: %+v", k, reqs)
			return spec.Requirement{}
		}
		return reqs[0]
	}
	// The install's conditions are data that hold exactly for the approved simulation.
	install := mamelet("NodePackage db/nginx install-package", "apt.install")
	approved := map[string][]string{
		node.KeyNode: {"db"}, node.KeyOperation: {"op-1"}, node.KeyNow: {"2026-11-10T01:00:00Z"},
		node.KeyChangeSet: {cs.Digest}, node.KeyAptOrigin: {"Ubuntu:24.04/noble"},
	}
	install.Fill(map[string][]string{node.FactOperationID: {"op-1"}, node.FactExpiresAt: {"2026-11-11T00:00:00Z"}})
	if !holdAll(install.Conditions, approved) {
		t.Errorf("the approved install does not satisfy its conditions: %+v", install.Conditions)
	}
	for name, edit := range map[string]func(map[string][]string){
		"another change set": func(f map[string][]string) { f[node.KeyChangeSet] = []string{"sha256:other"} },
		"a removal":          func(f map[string][]string) { f[node.KeyAptRemove] = []string{"libfoo"} },
		"another origin":     func(f map[string][]string) { f[node.KeyAptOrigin] = []string{"Ubuntu:24.04/noble", "ppa:evil"} },
		"another node":       func(f map[string][]string) { f[node.KeyNode] = []string{"web"} },
		"another operation":  func(f map[string][]string) { f[node.KeyOperation] = []string{"op-2"} },
		"expired":            func(f map[string][]string) { f[node.KeyNow] = []string{"2026-11-12T00:00:00Z"} },
	} {
		facts := maps.Clone(approved)
		edit(facts)
		if holdAll(install.Conditions, facts) {
			t.Errorf("%s satisfies the install's conditions", name)
		}
	}
	// Unfilled facts never hold.
	if raw := mamelet("NodePackage db/nginx install-package", "apt.install"); holdAll(raw.Conditions, approved) {
		t.Error("conditions with an unfilled operation and expiry hold")
	}
	if q := mamelet("NodeService db/nginx start-service", "systemd.start"); q.Grant != spec.Standing ||
		slices.ContainsFunc(q.Conditions, func(c spec.Condition) bool { return c.Key == node.KeyOperation }) {
		t.Errorf("auto start is not a standing grant: %+v", q)
	}
	if q := mamelet("NodeService db/nginx enable-service", "systemd.enable"); q.Grant != spec.Once || len(q.StandingIf) != 0 {
		t.Errorf("enable is standing: %+v", q)
	}
}

// Without approval by policy, a start is a one-time grant, not a standing one.
func TestStandingOnlyWhenThePolicyDecides(t *testing.T) {
	g, _ := graph.Decode([]byte(l2))
	snap := world(t, g)
	snap.Packages["db/nginx"] = state.Package{Installed: true, Version: "1", ObservedAt: t0}
	snap.Services["db/nginx"] = state.Service{Exists: true, Enabled: true, ObservedAt: t0}
	pol := policy.Default
	p := plan.Build(g, snap, pol)
	var s *Site
	starts := 0
	for _, q := range s.PlannedRequirements(p, g, pol) {
		if q.Op == policy.OpStartService {
			starts++
			if q.Grant != spec.Once {
				t.Errorf("standing without the policy: %+v", q)
			}
		}
	}
	if starts != 1 {
		t.Errorf("%d start requirements, want 1", starts)
	}
}

// Every operation the planner can produce for a built-in kind is declared by mamelet.
func TestManifestsDeclareEveryPlannedOperation(t *testing.T) {
	var s *Site
	ps := s.Providers()
	if err := ps.Check(); err != nil {
		t.Fatal(err)
	}
	for kind, ops := range map[graph.Kind][]policy.Op{
		graph.KindNodePackage: {policy.OpInstallPackage},
		graph.KindNodeService: {policy.OpEnableService, policy.OpStartService, policy.OpDisableService, policy.OpStopService},
		graph.KindNodeK3s:     {policy.OpInstallK3s},
	} {
		m, ok := ps.For(kind)
		if !ok {
			t.Errorf("no provider for %s", kind)
			continue
		}
		for _, op := range ops {
			if _, ok := m.Operation(kind, op); !ok {
				t.Errorf("%s does not declare %s %s", m.Provider, kind, op)
			}
		}
	}
}
