package plan

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
)

func refusing(reason string) state.Snapshot {
	s := converged()
	s.NodeRefusals = map[string]state.NodeRefusal{"app-server": {Serial: 7, WorksFrom: 5, Reason: reason, ObservedAt: t0}}
	return s
}

// A node whose mamelet refuses the Control Plane's revision is named, with what to do about that
// kind of refusal (MAM-PLAN-0130). The reasons are the ones mamelet gives (noderev.Verify).
func TestRefusalNamesTheNodeTheReasonAndWhatToDo(t *testing.T) {
	for _, tc := range []struct{ reason, hint string }{
		{fmt.Errorf("%w: serial 7 < 9", noderev.ErrRolledBack).Error(), "database"},
		{fmt.Errorf("%w: 2026-11-17T00:00:00Z", noderev.ErrFromFuture).Error(), "clock"},
		{fmt.Errorf("%w at 2026-11-17T00:00:00Z", noderev.ErrExpired).Error(), "clock"},
		{noderev.ErrBadSignature.Error(), "key"},
		{fmt.Errorf("%w: web (VMID 1), this is db (VMID 2)", noderev.ErrOtherNode).Error(), "another machine"},
		{`decode revision: json: unknown name "mamelet"`, "older than this Control Plane"},
		{`invalid revision: service "a:b" is not a valid service name`, "declaration"},
		{"something no release said", "journalctl"},
	} {
		s := refusing(tc.reason)
		// The node reported, and is refusing: a package it has not simulated yet, a change that waits
		// for approval, and a service that waits for one, are all blocked by it.
		delete(s.Packages, "app-server/nginx")
		p := Build(desired(t), s, policy.Default)
		c := find(t, p, "NodePackage app-server/nginx")
		if c.Status != StatusBlocked || c.Note == nil || c.Note.Code != "MAM-PLAN-0130" ||
			!strings.Contains(c.Note.Message, "node app-server") || !strings.Contains(c.Note.Message, "revision 7") ||
			!strings.Contains(c.Note.Message, "revision 5") || !strings.Contains(c.Note.Message, tc.reason) || !strings.Contains(c.Note.Hint, tc.hint) {
			t.Errorf("%q: %+v", tc.reason, c)
		}
	}
}

// A change that needs a permission is Blocked while the node refuses, and has no operation to wait
// for approval for; what is in sync stays in sync; nothing else changes (a node that accepts, or a
// node that is not the one that refuses, is as before).
func TestRefusalBlocksChangesAndOnlyThose(t *testing.T) {
	apt := noderev.NewChangeSet([]noderev.Change{{Action: noderev.ChangeInstall, Package: "nginx", Version: "1", Origins: []string{"Ubuntu:24.04/noble"}}})
	s := refusing(noderev.ErrBadSignature.Error())
	s.Packages["app-server/nginx"] = state.Package{ChangeSet: &apt, ObservedAt: t0} // not installed: a change
	s.Services["app-server/nginx"] = state.Service{Exists: true, Enabled: true, Running: true, ObservedAt: t0}
	p := Build(desired(t), s, policy.Default)
	pkg := find(t, p, "NodePackage app-server/nginx")
	if pkg.Status != StatusBlocked || len(pkg.Operations) != 0 || pkg.Verdict != "" || pkg.Note.Code != "MAM-PLAN-0130" {
		t.Errorf("a change on a node that refuses: %+v", pkg)
	}
	if svc := find(t, p, "NodeService app-server/nginx"); svc.Status != StatusInSync {
		t.Errorf("in sync stays in sync: %+v", svc)
	}
	// Without the refusal the same observation is a change that needs an approval.
	s.NodeRefusals = nil
	if pkg := find(t, Build(desired(t), s, policy.Default), "NodePackage app-server/nginx"); pkg.Status != StatusChange || pkg.Verdict != policy.ApprovalRequired {
		t.Errorf("without a refusal: %+v", pkg)
	}
	// Another node's refusal is not this node's.
	s.NodeRefusals = map[string]state.NodeRefusal{"db": {Serial: 1, Reason: noderev.ErrBadSignature.Error(), ObservedAt: t0}}
	if pkg := find(t, Build(desired(t), s, policy.Default), "NodePackage app-server/nginx"); pkg.Status != StatusChange {
		t.Errorf("another node's refusal: %+v", pkg)
	}
}

// A plan of a world where nothing refuses is the plan it always was: its digest is the same with and
// without the new field, so no approval of a stored plan is lost.
func TestARefusalFreeSnapshotKeepsItsPlanDigest(t *testing.T) {
	a := converged()
	b := converged()
	b.NodeRefusals = map[string]state.NodeRefusal{}
	if Build(desired(t), a, policy.Default).Digest != Build(desired(t), b, policy.Default).Digest {
		t.Fatal("an empty refusal map changes the digest")
	}
}
