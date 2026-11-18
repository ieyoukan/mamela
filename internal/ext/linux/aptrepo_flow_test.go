package linux

import (
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/reconcile"
)

func change(t *testing.T, p *plan.Plan, id string) plan.Change {
	t.Helper()
	if p == nil {
		t.Fatalf("no plan to find %s in", id)
	}
	for _, c := range p.Changes {
		if c.ID() == id {
			return c
		}
	}
	t.Fatalf("no %s in the plan", id)
	return plan.Change{}
}

const (
	repoID = "NodeAptRepo web/apt-repo/docker"
	pkgID  = "NodePackage web/docker-ce"
)

// aptFlow is a flow whose node has apt and reaches the key server, with Docker's source (its key on
// the test's server) and docker-ce declared.
func aptFlow(t *testing.T) (*flow, *fakeApt, *keyServer) {
	k := newKeyServer(t)
	repo := aptRes("web/apt-repo/docker", strings.Replace(dockerSpec(""), "https://download.docker.com/linux/ubuntu/gpg", k.URL+"/armored", 1))
	f := newFlow(t, repo, dockerCE)
	apt := &fakeApt{root: f.node.root, simulate: map[string]string{
		"https://download.docker.com/linux/ubuntu": "Inst docker-ce (5:28.3.3-1~ubuntu.24.04~noble Docker CE:noble [amd64])"}}
	f.node.a.System.Runner = apt
	f.node.a.System.HTTP = k.Client()
	return f, apt, k
}

// The way of a source: the package waits for it; once the source is applied, the round that added it
// reports what installing the package changes (mamelet observes again after it changed something), so
// the next plan has the installation without waiting for another round. Taken out of the declaration,
// the source stays on the node.
func TestAnAptRepoFromDeclarationToNode(t *testing.T) {
	f, apt, k := aptFlow(t)
	f.publish()
	f.node.round() // it says it takes linux 1
	f.publish()
	f.node.round() // it observes the source
	p, _ := f.plan()
	if c := change(t, p, repoID); c.Status != plan.StatusChange || c.Verdict != policy.ApprovalRequired {
		t.Fatalf("the source: %+v", c)
	}
	if c := change(t, p, pkgID); c.Status != plan.StatusWaiting || c.Note.Code != codeAptRepoPackageGate {
		t.Fatalf("the package: %+v", c)
	}
	rep := f.apply(p)
	if len(rep.Steps) != 1 || rep.Steps[0].Outcome != reconcile.OutcomeDone {
		t.Fatalf("apply %+v", rep)
	}
	if !strings.Contains(f.node.read("/etc/apt/sources.list.d/mamela-docker.sources"), "Signed-By: /etc/apt/keyrings/mamela-docker.asc") ||
		f.node.read("/etc/apt/keyrings/mamela-docker.asc") != testKeyArmored || k.count("/armored") != 1 {
		t.Fatal("the files on the node")
	}
	// No other round of mamelet: the change set came with the result.
	p, _ = f.plan()
	if c := change(t, p, repoID); c.Status != plan.StatusInSync {
		t.Fatalf("the source after apply: %+v", c)
	}
	c := change(t, p, pkgID)
	if c.Status != plan.StatusChange || len(c.Diffs) < 3 || !strings.Contains(c.Diffs[2].To, "docker-ce 5:28.3.3-1~ubuntu.24.04~noble (Docker CE:noble)") {
		t.Fatalf("the package after the source: %+v", c)
	}
	// The default policy allows Ubuntu's packages only: Docker's origin is for the operator to allow.
	if c.Verdict != policy.Denied || c.Operations[0].Decision.Code != "MAM-POL-0305" {
		t.Errorf("the package's decision: %+v", c.Operations)
	}
	if got := strings.Join(apt.commands, ","); got != "apt-get -o DPkg::Lock::Timeout=300 update" {
		t.Errorf("ran %s", got)
	}

	// Taken out: the source stays, mamelet stops watching it.
	f.declare()
	p, _ = f.plan()
	kept := change(t, p, repoID)
	if kept.Status != plan.StatusRetained || kept.Note.Code != codeAptRepoKept || len(kept.Operations) != 0 {
		t.Fatalf("kept %+v", kept)
	}
	f.apply(p)
	if r := f.revision(); len(r.Ext) != 0 {
		t.Errorf("still watched: %s", r.Content())
	}
	if !strings.Contains(f.node.read("/etc/apt/sources.list.d/mamela-docker.sources"), "docker") {
		t.Error("the source was touched")
	}
}

// apt-get update fails: the apply says why, the files stay, the next plan has only the update left,
// and running apply again on the approved plan retries it (attempt 2) without downloading the key
// again.
func TestAFailedAptUpdateIsRetriedByApply(t *testing.T) {
	f, apt, k := aptFlow(t)
	f.declare(aptRes("web/apt-repo/docker", strings.Replace(dockerSpec(""), "https://download.docker.com/linux/ubuntu/gpg", k.URL+"/armored", 1)))
	f.publish()
	f.node.round()
	f.publish()
	f.node.round()
	p, _ := f.plan()
	apt.updateFail = "E: Failed to fetch https://download.docker.com/linux/ubuntu/dists/noble/InRelease  Could not resolve 'download.docker.com'"
	rep := f.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0004" || !strings.Contains(rep.Stopped.Message, "Could not resolve") {
		t.Fatalf("apply %+v", rep)
	}
	snap, err := f.rec.Provider.Observe(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := change(t, plan.Build(f.g, snap, policy.Default), repoID); c.Status != plan.StatusChange || c.Note == nil || c.Note.Code != codeAptRepoUnread {
		t.Fatalf("what remains: %+v", c)
	}
	apt.updateFail = ""
	again, err := f.rec.Apply(f.ctx, p.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Steps) != 1 || again.Steps[0].Outcome != reconcile.OutcomeDone || again.Stopped != nil {
		t.Fatalf("apply again %+v", again)
	}
	if k.count("/armored") != 1 {
		t.Errorf("the key was downloaded %d times", k.count("/armored"))
	}
	if c := change(t, again.Remaining, repoID); c.Status != plan.StatusInSync {
		t.Errorf("after the retry: %+v", c)
	}
}

// A mamelet older than the extension gets nothing of the source, and no grant: the source is
// blocked, and the package is not held by it (it shows its own problem).
func TestAnOldMameletGetsNoAptRepo(t *testing.T) {
	f, _, _ := aptFlow(t)
	f.publish()
	old := noderev.Report{Node: "web", VMID: 7001, Version: "0.9.2", MameletSHA256: strings.Repeat("ab", 32),
		Packages: []noderev.PackageState{{Name: "docker-ce", Error: "simulate: E: Unable to locate package docker-ce"}}, Services: []noderev.ServiceState{}, Results: []noderev.Result{}}
	if err := f.st.PutNodeReport(f.ctx, old); err != nil {
		t.Fatal(err)
	}
	p, _ := f.plan()
	if c := change(t, p, repoID); c.Status != plan.StatusBlocked || c.Note.Code != "MAM-PLAN-0122" {
		t.Fatalf("the source %+v", c)
	}
	if c := change(t, p, pkgID); c.Status != plan.StatusBlocked || c.Note.Code != "MAM-PLAN-0202" {
		t.Fatalf("the package %+v", c)
	}
	var res graph.Resource
	for _, r := range f.g.Resources {
		if r.Kind == KindAptRepo {
			res = r
		}
	}
	step := reconcile.Step{OperationID: opA, PlanDigest: p.Digest, Op: OpAddAptRepo, Resource: res}
	if _, err := f.st.Intend(f.ctx, step.OperationID, p.Digest, step.Resource.ID(), string(OpAddAptRepo)); err != nil {
		t.Fatal(err)
	}
	if err := f.nodes.Apply(f.ctx, step); err == nil || !strings.Contains(err.Error(), "MAM-NODE-0104") {
		t.Errorf("granted: %v", err)
	}
	if r := f.revision(); len(r.Ext) != 0 {
		t.Errorf("a part for an old mamelet: %s", r.Content())
	}
}
