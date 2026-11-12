package spec

import (
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
)

func manifest() Manifest {
	return Manifest{Schema: Schema, Provider: "test", Version: "1.0.0",
		Observe: []Need{{Capability: "list", Target: Target{Type: "acl", Ref: "pool"}, Always: true}},
		Kinds: []Kind{{Kind: graph.KindNodePackage, Operations: []Operation{
			{Op: policy.OpInstallPackage, Support: Supported, Needs: []Need{
				{Capability: "apt.install", Target: Target{Type: "node", Ref: "{node}"}, Object: "{package}", Always: true},
				{Capability: "mirror.read", Target: Target{Type: "acl", Ref: "mirror"}},
			}},
		}}}}
}

type sites map[string]string

func (s sites) ResolveTarget(provider string, t Target) (string, bool) {
	v, ok := s[t.Ref]
	return v, ok
}

func TestCheckRejectsInconsistentManifests(t *testing.T) {
	for name, edit := range map[string]func(*Manifest){
		"no version":             func(m *Manifest) { m.Version = "" },
		"other schema":           func(m *Manifest) { m.Schema = "mamela-provider/v0" },
		"twice":                  func(m *Manifest) { m.Kinds[0].Operations = append(m.Kinds[0].Operations, m.Kinds[0].Operations[0]) },
		"unsupported with needs": func(m *Manifest) { m.Kinds[0].Operations[0].Support, m.Kinds[0].Operations[0].Why = Unsupported, "x" },
		"unknown support":        func(m *Manifest) { m.Kinds[0].Operations[0].Support = "maybe" },
		"need without target":    func(m *Manifest) { m.Kinds[0].Operations[0].Needs[0].Target = Target{} },
		"supported with why":     func(m *Manifest) { m.Kinds[0].Operations[0].Why = "because" },
	} {
		m := manifest()
		edit(&m)
		if m.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := manifest().Check(); err != nil {
		t.Fatal(err)
	}
	other := manifest()
	other.Provider = "other"
	if err := (Providers{manifest(), other}).Check(); err == nil || !strings.Contains(err.Error(), "both test and other") {
		t.Errorf("a kind with two providers: %v", err)
	}
}

func TestAnalyzeResolvesEachResource(t *testing.T) {
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[
{"kind":"TestVM","name":"a","provider":"vms","spec":{"cpu":1}},
{"kind":"TestVM","name":"b","provider":"vms","spec":{"cpu":1}},
{"kind":"NodePackage","name":"a/nginx","spec":{"node":"a","package":"nginx","state":"Present"}},
{"kind":"NodePackage","name":"b/curl","spec":{"node":"b","package":"curl","state":"Present"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ps := Providers{manifest()}
	reqs := ps.Analyze(g, sites{"pool": "/pool/lab"})
	got := map[string]Requirement{}
	for _, q := range reqs {
		got[q.Resource+" "+q.Capability] = q
	}
	for _, want := range []Requirement{
		{Resource: "NodePackage a/nginx", Capability: "apt.install", Target: "a", Object: "nginx", Resolved: true, Always: true},
		{Resource: "NodePackage b/curl", Capability: "apt.install", Target: "b", Object: "curl", Resolved: true, Always: true},
		{Resource: "NodePackage b/curl", Capability: "mirror.read", Target: "<mirror>"}, // the site does not know it
	} {
		q := got[want.Resource+" "+want.Capability]
		if q.Target != want.Target || q.Object != want.Object || q.Resolved != want.Resolved || q.Always != want.Always ||
			q.Op != policy.OpInstallPackage || q.Provider != "test" {
			t.Errorf("%s %s: %+v", want.Resource, want.Capability, q)
		}
	}
	// The machines' provider is not among these: said so, once per machine.
	for _, id := range []string{"TestVM a", "TestVM b"} {
		if q := got[id+" "]; q.Support != Unsupported || !strings.Contains(q.Why, "no provider") {
			t.Errorf("%s: %+v", id, q)
		}
	}
	if obs := ps.Observation(sites{"pool": "/pool/lab"}); len(obs) != 1 || obs[0].Target != "/pool/lab" || !obs[0].Resolved {
		t.Errorf("observation: %+v", obs)
	}
	if q := ps.OperationRequirements(g.Resources[2], policy.OpEnableService, nil); len(q) != 1 || q[0].Support != Unsupported {
		t.Errorf("an operation the provider does not declare: %+v", q)
	}
}
