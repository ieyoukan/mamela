package plan

import (
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
)

// A test extension of the generic wire: the installation of package "gated" waits for it, and what
// it observed on node gate-test that nothing declares is kept (an operation it plans that way is
// dropped). It does nothing to any other plan of this package's tests (their golden files).
func init() {
	Register(Extension{
		Name: "test-gates",
		PackageGate: func(k *Kit, node, pkg string) *Note {
			if pkg != "gated" {
				return nil
			}
			return &Note{Code: "MAM-PLAN-0998", Message: "waits for the source of " + pkg + " on " + node}
		},
		Undeclared: func(k *Kit) []Change {
			if _, ok := k.Ext("gate-test", "test-gates"); !ok {
				return nil
			}
			return []Change{
				{Kind: "NodeThing", Name: "gate-test/kept", Status: StatusRetained, Note: &Note{Code: "MAM-PLAN-0999", Message: "kept"}},
				{Kind: "NodeThing", Name: "gate-test/sneaky", Status: StatusRetained, Operations: []Operation{{Op: "delete-thing", Decision: policy.Decision{Verdict: policy.Auto}}}},
				{Kind: "NodeThing", Name: "gate-test/changed", Status: StatusChange},
			}
		},
		Codes: []string{"MAM-PLAN-0998", "MAM-PLAN-0999"},
	})
}

// The installation of a package waits for what an extension's gate says; an installed package is in
// sync whatever the gate says.
func TestPackageGate(t *testing.T) {
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + vm("app-server", `{"mode":"dhcp"}`) + `,` +
		`{"kind":"NodePackage","name":"app-server/gated","spec":{"node":"app-server","package":"gated","state":"Present"}},` +
		`{"kind":"NodePackage","name":"app-server/free","spec":{"node":"app-server","package":"free","state":"Present"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s := state.Empty(t0)
	machineObserved(&s, g, "app-server", true, true, true, 600)
	s.Packages["app-server/gated"] = state.Package{Error: "E: Unable to locate package gated", ObservedAt: t0}
	s.Packages["app-server/free"] = state.Package{Error: "E: Unable to locate package free", ObservedAt: t0}
	p := Build(g, s, policy.Default)
	if c := find(t, p, "NodePackage app-server/gated"); c.Status != StatusWaiting || c.Note.Code != "MAM-PLAN-0998" {
		t.Errorf("gated: %+v", c)
	}
	if c := find(t, p, "NodePackage app-server/free"); c.Status != StatusBlocked || c.Note.Code != "MAM-PLAN-0202" {
		t.Errorf("free: %+v", c)
	}
	s.Packages["app-server/gated"] = state.Package{Installed: true, ObservedAt: t0}
	if c := find(t, Build(g, s, policy.Default), "NodePackage app-server/gated"); c.Status != StatusInSync {
		t.Errorf("installed: %+v", c)
	}
}

// What an extension keeps is in the plan, after everything else; it cannot plan an operation that way.
func TestExtensionUndeclaredIsOnlyKept(t *testing.T) {
	s := converged()
	s.Ext = map[string]map[string]state.ExtObservation{"gate-test": {"test-gates": {Accepts: 1, ObservedAt: t0}}}
	p := Build(desired(t), s, policy.Default)
	last := p.Changes[len(p.Changes)-1]
	if last.ID() != "NodeThing gate-test/kept" || last.Status != StatusRetained || last.Diffs == nil || last.Operations == nil {
		t.Errorf("last %+v", last)
	}
	for _, c := range p.Changes {
		if strings.Contains(c.Name, "sneaky") || strings.Contains(c.Name, "changed") {
			t.Errorf("an extension planned %+v without a declaration", c)
		}
	}
}

// Whether a node's mamelet takes an extension, and what the plan says when it does not.
func TestExtNotTaken(t *testing.T) {
	s := converged()
	k := newKit(builder{snap: s, pol: policy.Default, g: desired(t)})
	if n := k.ExtNotTaken("app-server", "linux", 1, "0.10.0"); n == nil || n.Code != "MAM-PLAN-0122" || !strings.Contains(n.Message, "a release that does not say") {
		t.Errorf("a mamelet that reports and lists nothing: %+v", n)
	}
	if n := k.ExtNotTaken("silent", "linux", 1, "0.10.0"); n != nil {
		t.Errorf("a node that never reported: %+v", n)
	}
	s.Mamelets = map[string]state.Mamelet{"app-server/mamelet": {Version: "0.9.2", ObservedAt: t0}}
	s.Ext = map[string]map[string]state.ExtObservation{"app-server": {"linux": {Accepts: 1, ObservedAt: t0}}}
	k = newKit(builder{snap: s, pol: policy.Default, g: desired(t)})
	if n := k.ExtNotTaken("app-server", "linux", 1, "0.10.0"); n != nil {
		t.Errorf("takes it: %+v", n)
	}
	if n := k.ExtNotTaken("app-server", "linux", 2, "0.11.0"); n == nil || !strings.Contains(n.Message, "runs 0.9.2") || !strings.Contains(n.Hint, "0.11.0") {
		t.Errorf("an older version of the part: %+v", n)
	}
	s.Ext["app-server"]["linux"] = state.ExtObservation{Accepts: 1, Refused: "cannot read it", ObservedAt: t0}
	k = newKit(builder{snap: s, pol: policy.Default, g: desired(t)})
	if n := k.ExtNotTaken("app-server", "linux", 1, "0.10.0"); n == nil || !strings.Contains(n.Message, "cannot read it") {
		t.Errorf("refused: %+v", n)
	}
	// The kit's copy is the planner's own.
	o, _ := k.Ext("app-server", "linux")
	o.Refused = "changed"
	if again, _ := k.Ext("app-server", "linux"); again.Refused != "cannot read it" {
		t.Error("a planner changed the observation")
	}
}
