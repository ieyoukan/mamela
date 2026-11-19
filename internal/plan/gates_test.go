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
	if st, n := k.ExtNotTaken("app-server", "linux", 1, "0.10.0"); st != StatusBlocked || n == nil || n.Code != "MAM-PLAN-0122" || !strings.Contains(n.Message, "a release that does not say") {
		t.Errorf("a mamelet that reports and lists nothing: %s %+v", st, n)
	}
	if _, n := k.ExtNotTaken("silent", "linux", 1, "0.10.0"); n != nil {
		t.Errorf("a node that never reported: %+v", n)
	}
	s.Mamelets = map[string]state.Mamelet{"app-server/mamelet": {Version: "0.9.2", ObservedAt: t0}}
	s.Ext = map[string]map[string]state.ExtObservation{"app-server": {"linux": {Accepts: 1, ObservedAt: t0}}}
	k = newKit(builder{snap: s, pol: policy.Default, g: desired(t)})
	if _, n := k.ExtNotTaken("app-server", "linux", 1, "0.10.0"); n != nil {
		t.Errorf("takes it: %+v", n)
	}
	if st, n := k.ExtNotTaken("app-server", "linux", 2, "0.11.0"); st != StatusBlocked || n == nil || !strings.Contains(n.Message, "runs 0.9.2") || !strings.Contains(n.Hint, "0.11.0") {
		t.Errorf("an older version of the part: %s %+v", st, n)
	}
	s.Ext["app-server"]["linux"] = state.ExtObservation{Accepts: 1, Refused: "cannot read it", ObservedAt: t0}
	k = newKit(builder{snap: s, pol: policy.Default, g: desired(t)})
	if st, n := k.ExtNotTaken("app-server", "linux", 1, "0.10.0"); st != StatusBlocked || n == nil || !strings.Contains(n.Message, "cannot read it") {
		t.Errorf("refused: %s %+v", st, n)
	}
	// The hint follows the reason: a part mamelet could not read asks for its update; a part it read
	// and refused for what it says does not send the operator to update a mamelet new enough.
	for reason, hint := range map[string]string{
		"the linux part of the revision cannot be read: unknown member \"x\"": "it is older than this Control Plane",
		"this mamelet does not know the extension linux; it went on":          "it is older than this Control Plane",
		"invalid linux part: it grants changes of 300 files":                  "change what the reason names",
	} {
		s.Ext["app-server"]["linux"] = state.ExtObservation{Accepts: 1, Refused: reason, ObservedAt: t0}
		k = newKit(builder{snap: s, pol: policy.Default, g: desired(t)})
		if st, n := k.ExtNotTaken("app-server", "linux", 1, "0.10.0"); st != StatusBlocked || n == nil || !strings.Contains(n.Message, reason) || !strings.Contains(n.Hint, hint) {
			t.Errorf("refused (%s): %s %+v", reason, st, n)
		}
	}
	s.Ext["app-server"]["linux"] = state.ExtObservation{Accepts: 1, Refused: "cannot read it", ObservedAt: t0}
	k = newKit(builder{snap: s, pol: policy.Default, g: desired(t)})
	// The kit's copy is the planner's own.
	o, _ := k.Ext("app-server", "linux")
	o.Refused = "changed"
	if again, _ := k.Ext("app-server", "linux"); again.Refused != "cannot read it" {
		t.Error("a planner changed the observation")
	}
}

// A node whose mamelet is older than an extension, and whose declaration updates it in the same plan
// (the way docs/mamelet-update.md gives): the extension's resources wait for the update instead of
// being blocked, so that a plan pushed with mamelacd's sync = "push" (which approves no plan with a
// blocked change) applies the update. An update to a release that does not bring the extension, or
// none, is still blocked.
func TestExtNotTakenWaitsForTheMameletUpdate(t *testing.T) {
	const old, next = "1111111111111111111111111111111111111111111111111111111111111111", "2222222222222222222222222222222222222222222222222222222222222222"
	decl := func(version, sha string) *graph.Graph {
		v := ""
		if version != "" {
			v = `,"version":"` + version + `"`
		}
		g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + vm("app-server", `{"mode":"dhcp"}`) + `,` +
			`{"kind":"NodeMamelet","name":"app-server/mamelet","spec":{"node":"app-server","sha256":"` + sha + `"` + v + `}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	s := state.Empty(t0)
	machineObserved(&s, decl("", old), "app-server", true, true, true, 600)
	s.Mamelets = map[string]state.Mamelet{"app-server/mamelet": {SHA256: old, Version: "0.9.2", ObservedAt: t0}}
	for _, tc := range []struct {
		name, version, sha string
		status             Status
		code               string
	}{
		{"an update to the release of the extension", "0.10.0+849289e", next, StatusWaiting, "MAM-PLAN-0123"},
		{"an update that does not say its release", "", next, StatusWaiting, "MAM-PLAN-0123"},
		{"an update to a newer release", "v0.11.2", next, StatusWaiting, "MAM-PLAN-0123"},
		{"an update to an older release", "0.9.3", next, StatusBlocked, "MAM-PLAN-0122"},
		{"no update: the declared release runs", "0.9.2", old, StatusBlocked, "MAM-PLAN-0122"},
	} {
		k := newKit(builder{snap: s, pol: policy.Default, g: decl(tc.version, tc.sha)})
		if st, n := k.ExtNotTaken("app-server", "linux", 1, "0.10.0"); st != tc.status || n == nil || n.Code != tc.code {
			t.Errorf("%s: %s %+v", tc.name, st, n)
		}
	}
	// The node is protected (the Control Plane's host): its update is denied, so it does not come.
	protected := policy.Default
	protected.ProtectedNodes = []string{"app-server"}
	k := newKit(builder{snap: s, pol: protected, g: decl("0.10.0", next)})
	if st, n := k.ExtNotTaken("app-server", "linux", 1, "0.10.0"); st != StatusBlocked || n.Code != "MAM-PLAN-0122" {
		t.Errorf("protected: %s %+v", st, n)
	}
	// The node's update waits for another node's: it comes after it.
	two := `{"schema":"mamela-graph/v2","resources":[` + vm("a-server", `{"mode":"dhcp"}`) + `,` + vm("app-server", `{"mode":"dhcp"}`) + `,` +
		`{"kind":"NodeMamelet","name":"a-server/mamelet","spec":{"node":"a-server","sha256":"` + next + `"}},` +
		`{"kind":"NodeMamelet","name":"app-server/mamelet","spec":{"node":"app-server","sha256":"` + next + `"}}]}`
	g, err := graph.Decode([]byte(two))
	if err != nil {
		t.Fatal(err)
	}
	machineObserved(&s, g, "a-server", true, true, true, 600)
	s.Mamelets["a-server/mamelet"] = state.Mamelet{SHA256: old, Version: "0.9.2", ObservedAt: t0}
	k = newKit(builder{snap: s, pol: policy.Default, g: g})
	if st, n := k.ExtNotTaken("app-server", "linux", 1, "0.10.0"); st != StatusWaiting || n.Code != "MAM-PLAN-0123" {
		t.Errorf("after another node: %s %+v", st, n)
	}
}

func TestReleaseOlder(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		older bool
	}{
		{"0.9.2", "0.10.0", true}, {"0.10.0", "0.10.0", false}, {"0.10.0+849289e", "0.10.0", false}, {"v0.9.9", "0.10.0", true},
		{"1.0.0", "0.10.0", false}, {"", "0.10.0", false}, {"latest", "0.10.0", false}, {"0.10", "0.10.0", false},
	} {
		if got := releaseOlder(tc.a, tc.b); got != tc.older {
			t.Errorf("releaseOlder(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// A machine that has run for an hour, whose mamelet reports (which release it runs) but not yet the
// package a new declaration added: it is not silent, and the package waits for its next report.
func TestAMameletThatReportsIsNotSilent(t *testing.T) {
	g := desired(t)
	s := state.Empty(t0)
	machineObserved(&s, g, "app-server", true, true, true, 3600)
	if c := find(t, Build(g, s, policy.Default), "NodePackage app-server/nginx"); c.Status != StatusBlocked || c.Note.Code != "MAM-PLAN-0110" {
		t.Errorf("silent: %+v", c)
	}
	s.Mamelets = map[string]state.Mamelet{"app-server/mamelet": {SHA256: strings.Repeat("ab", 32), Version: "0.10.0", ObservedAt: t0}}
	if c := find(t, Build(g, s, policy.Default), "NodePackage app-server/nginx"); c.Status != StatusWaiting || c.Note.Code != "MAM-PLAN-0103" {
		t.Errorf("reports: %+v", c)
	}
	s.Mamelets = nil
	s.Ext = map[string]map[string]state.ExtObservation{"app-server": {"linux": {Accepts: 1, ObservedAt: t0}}}
	if c := find(t, Build(g, s, policy.Default), "NodePackage app-server/nginx"); c.Status != StatusWaiting || c.Note.Code != "MAM-PLAN-0103" {
		t.Errorf("reports an extension: %+v", c)
	}
}
