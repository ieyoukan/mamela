package plan

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// removable is a machine a provider reports that Mamela made for the declaration being planned, and
// that no declaration has any more: the provider prepared its deletion.
func removable(s *state.Snapshot, name string, order int, effects ...protocol.Effect) {
	undeclared(s, name, true)
	id := "TestVM " + name
	p := s.Provided[id]
	p.Removal = &state.ProvidedChange{
		Change: protocol.Change{Resource: id, Kind: "TestVM", Op: "delete-vm", IDs: map[string]string{"name": name},
			Reasons: []string{"delete " + name}, Requests: []protocol.Approved{{Method: "DELETE", Path: "/vms/{name}"}}},
		Verified: protocol.Verified{Effects: effects}}
	p.RemovalOrder = order
	s.Provided[id] = p
}

// What a declaration made and no longer has is deleted, and the plan says so: the newest first, a
// deletion that needs an approval (which the Control Plane's host gives), after what it replaces.
func TestRemovalPlansTheDeletion(t *testing.T) {
	s := converged()
	s.Stack = "infra"
	removable(&s, "old-db", 1, protocol.EffectLoseData)
	removable(&s, "old-web", 2, protocol.EffectLoseData)
	undeclared(&s, "other-stacks", true) // kept: no deletion was prepared for it
	p := Build(desired(t), s, policy.Default)

	var removals []string
	for _, c := range p.Changes {
		if c.Removes {
			removals = append(removals, c.Name)
			if c.Status != StatusChange || c.Verdict != policy.ApprovalRequired || len(c.ProviderChanges) != 1 ||
				c.Operations[0].Op != "delete-vm" || c.Operations[0].Decision.Code != "MAM-POL-0310" {
				t.Errorf("%s: %+v", c.Name, c)
			}
		}
	}
	if strings.Join(removals, " ") != "old-web old-db" {
		t.Errorf("deletions %v: the newest object goes first, what it depends on after it", removals)
	}
	if p.Removals() != 2 {
		t.Errorf("Removals() = %d", p.Removals())
	}
	if c := find(t, p, "TestVM other-stacks"); c.Status != StatusRetained || c.Removes {
		t.Errorf("%+v", c)
	}
	var buf bytes.Buffer
	Render(&buf, p)
	for _, want := range []string{"- TestVM old-web  [needs approval]", "delete-vm: needs approval (MAM-POL-0310", "sends DELETE /vms/{name}",
		"removed: Mamela made it for this declaration (infra), which no longer has it"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("render lacks %q:\n%s", want, buf.String())
		}
	}
}

// A deletion is shown even when a request is not known to the host's catalog, but never carried
// out; the Control Plane's own host and the policy's "retain" are never deleted.
func TestRemovalIsRefusedOrKept(t *testing.T) {
	s := converged()
	s.Stack = "infra"
	removable(&s, "unknown-call", 1)
	r := s.Provided["TestVM unknown-call"]
	r.Removal.Verified = protocol.Verified{Unclassified: []protocol.Call{{Method: "DELETE", Path: "/vms/{name}"}}}
	s.Provided["TestVM unknown-call"] = r
	removable(&s, "cp-host", 2, protocol.EffectLoseData)

	pol := policy.Default
	pol.ProtectedNodes = []string{"cp-host"}
	p := Build(desired(t), s, pol)
	if c := find(t, p, "TestVM unknown-call"); c.Verdict != policy.Denied || c.Operations[0].Decision.Code != "MAM-POL-0311" {
		t.Errorf("a deletion through a call the catalog does not know: %+v", c)
	}
	if c := find(t, p, "TestVM cp-host"); c.Removes || c.Status != StatusRetained || c.Note.Code != "MAM-POL-0308" {
		t.Errorf("the Control Plane's own host: %+v", c)
	}

	retain := policy.Default
	retain.Removal = "retain"
	p = Build(desired(t), s, retain)
	if p.Removals() != 0 {
		t.Error("the policy retains what a declaration no longer has")
	}
	if c := find(t, p, "TestVM unknown-call"); c.Status != StatusRetained || c.Note.Code != "MAM-PLAN-0305" {
		t.Errorf("%+v", c)
	}
}

// Why an object is kept comes from what observed it, and shows in the plan.
func TestKeptSaysWhy(t *testing.T) {
	s := converged()
	undeclared(&s, "elsewhere", true)
	e := s.Provided["TestVM elsewhere"]
	e.Kept = &state.Kept{Code: "MAM-PLAN-0302", Message: "not in this declaration; kept: it was made for another declaration (a), not for this one (b)", Hint: "remove it from the declaration that made it"}
	s.Provided["TestVM elsewhere"] = e
	c := find(t, Build(desired(t), s, policy.Default), "TestVM elsewhere")
	if c.Status != StatusRetained || c.Note.Code != "MAM-PLAN-0302" || !strings.Contains(c.Note.Message, "another declaration") || c.Note.Hint == "" {
		t.Errorf("%+v", c.Note)
	}
}

// The plan is the same each time it is built: deletions have an order that does not depend on maps.
func TestRemovalPlanIsDeterministic(t *testing.T) {
	s := converged()
	s.Stack = "infra"
	for i, n := range []string{"a", "b", "c", "d"} {
		removable(&s, n, i%2, protocol.EffectLoseData) // equal orders: by name
	}
	first := Build(desired(t), s, policy.Default).Digest
	for range 20 {
		if got := Build(desired(t), s, policy.Default).Digest; got != first {
			t.Fatal("the plan changed between builds")
		}
	}
}

// A declared object that Mamela made for another declaration is blocked, with the reason, not
// changed with an ordinary approval (B4 of the 0.3.2 audit); what runs on a machine of another
// declaration waits too.
func TestObjectOfAnotherDeclarationIsBlocked(t *testing.T) {
	g := desired(t)
	s := converged()
	// The machine is declared here, and was made for another declaration.
	for id, p := range s.Provided {
		if p.Declared {
			p.Kept = &state.Kept{Code: "MAM-PLAN-0307", Message: "it is in this declaration (shop), but Mamela made it for another declaration (blog)", Hint: "declare it in one place"}
			p.Changes = []state.ProvidedChange{providedChange(id, "resize-vm", protocol.EffectRestart)}
			s.Provided[id] = p
		}
	}
	s.AddProvided(s.Provided)
	p := Build(g, s, policy.Default)
	m := find(t, p, "TestVM app-server")
	if m.Status != StatusBlocked || m.Note == nil || m.Note.Code != "MAM-PLAN-0307" || !strings.Contains(m.Note.Message, "blog") ||
		m.Note.Hint == "" || len(m.Operations) != 0 || len(m.ProviderChanges) != 0 || m.Creates {
		t.Fatalf("the machine of another declaration: %+v", m)
	}
	for _, id := range []string{"NodePackage app-server/nginx", "NodeService app-server/nginx"} {
		c := find(t, p, id)
		if c.Status != StatusWaiting || c.Note == nil || c.Note.Code != "MAM-PLAN-0307" || !strings.Contains(c.Note.Message, "app-server") {
			t.Errorf("%s on it: %+v", id, c)
		}
	}
	if p.Removals() != 0 {
		t.Errorf("deletions: %d", p.Removals())
	}
	var buf bytes.Buffer
	Render(&buf, p)
	if !strings.Contains(buf.String(), "! TestVM app-server") || !strings.Contains(buf.String(), "MAM-PLAN-0307") {
		t.Errorf("render:\n%s", buf.String())
	}
}
