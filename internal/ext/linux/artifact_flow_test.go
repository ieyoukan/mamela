package linux

import (
	"archive/tar"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/reconcile"
)

func (f *flow) artifactChange() (*plan.Plan, plan.Change) {
	f.t.Helper()
	p, _ := f.plan()
	for _, c := range p.Changes {
		if c.Kind == KindArtifact {
			return p, c
		}
	}
	return p, plan.Change{}
}

// The whole way of an artifact: the plan asks for an approval of the download and its member, the
// apply grants it, mamelet installs it, and the next plan is in sync; a new version replaces it; out
// of the declaration, the file stays.
func TestAnArtifactFromDeclarationToNode(t *testing.T) {
	rel := newReleases(t)
	v1 := rel.put("/v1.tar.gz", tarGz(t, entry{name: "tool-1/tool", typeflag: tar.TypeReg, body: "tool 1"}))
	v2 := rel.put("/v2.tar.gz", tarGz(t, entry{name: "tool-2/tool", typeflag: tar.TypeReg, body: "tool 2"}))
	declare := func(url, sum, member string) string {
		return artifactRes("web/artifact/usr/local/bin/tool", `{"node":"web","path":"/usr/local/bin/tool","url":"`+url+`","sha256":"`+sum+`","extract":"`+member+`","mode":"0750"}`)
	}
	f := newFlow(t, declare(rel.url("/v1.tar.gz"), v1, "tool-1/tool"))
	f.node.a.System.HTTP = rel.srv.Client()
	f.publish()
	f.node.round() // it says it takes linux
	f.publish()
	f.node.round() // it observes the artifact
	p, c := f.artifactChange()
	if c.Status != plan.StatusChange || c.Verdict != policy.ApprovalRequired || c.Operations[0].Op != OpInstallArtifact {
		t.Fatalf("the plan %+v", c)
	}
	rep := f.apply(p)
	if len(rep.Steps) != 1 || rep.Steps[0].Outcome != reconcile.OutcomeDone {
		t.Fatalf("apply %+v", rep)
	}
	if got := f.node.read("/usr/local/bin/tool"); got != "tool 1" {
		t.Fatalf("on the node: %q", got)
	}
	f.node.round()
	if _, c := f.artifactChange(); c.Status != plan.StatusInSync {
		t.Fatalf("after the installation: %+v", c)
	}

	// A new version: the plan shows the old download and the new one.
	f.declare(declare(rel.url("/v2.tar.gz"), v2, "tool-2/tool"))
	f.publish()
	f.node.round()
	p, c = f.artifactChange()
	if c.Status != plan.StatusChange || !hasDiff(c, "sha256", v1, v2) || !hasDiff(c, "extract", "tool-1/tool", "tool-2/tool") || c.Note != nil {
		t.Fatalf("a new version: %+v", c)
	}
	f.apply(p)
	if got := f.node.read("/usr/local/bin/tool"); got != "tool 2" {
		t.Fatalf("on the node: %q", got)
	}

	// Taken out of the declaration: the plan says it stays; applying it stops the watching.
	f.declare()
	p, _ = f.plan()
	kept := p.Changes[len(p.Changes)-1]
	if kept.ID() != "NodeArtifact web/artifact/usr/local/bin/tool" || kept.Status != plan.StatusRetained || kept.Note.Code != codeArtifactKept {
		t.Fatalf("kept %+v", p.Changes)
	}
	f.apply(p)
	if r := f.revision(); len(r.Ext) != 0 {
		t.Errorf("still watched: %s", r.Content())
	}
	if got := f.node.read("/usr/local/bin/tool"); got != "tool 2" {
		t.Errorf("the file was touched: %q", got)
	}
}

// An approval is of exactly what the plan showed: a declaration changed since grants nothing.
func TestAnArtifactGrantIsOfWhatThePlanShowed(t *testing.T) {
	rel := newReleases(t)
	v1 := rel.put("/v1", []byte("one"))
	res := func(sum string) string {
		return artifactRes("web/artifact/usr/local/bin/tool", `{"node":"web","path":"/usr/local/bin/tool","url":"`+rel.url("/v1")+`","sha256":"`+sum+`"}`)
	}
	f := newFlow(t, res(v1))
	f.publish()
	f.node.round()
	f.publish()
	f.node.round()
	p, c := f.artifactChange()
	if c.Status != plan.StatusChange {
		t.Fatalf("the plan %+v", c)
	}
	if _, err := f.st.Approve(f.ctx, p.Digest, "test", time.Hour); err != nil {
		t.Fatal(err)
	}
	step := reconcile.Step{OperationID: "op-aaaaaaaaaaaaaaaaaaaaaaaa", PlanDigest: p.Digest, Op: OpInstallArtifact, Approved: c}
	f.declare(res(sumB))
	step.Resource = f.g.Resources[1]
	if _, err := f.st.Intend(f.ctx, step.OperationID, p.Digest, step.Resource.ID(), string(OpInstallArtifact)); err != nil {
		t.Fatal(err)
	}
	if err := f.nodes.Apply(f.ctx, step); err == nil || !strings.Contains(err.Error(), "not the one the approved plan showed") {
		t.Errorf("granted: %v", err)
	}
}

func hasDiff(c plan.Change, field, from, to string) bool {
	for _, d := range c.Diffs {
		if d.Field == field && d.From == from && d.To == to {
			return true
		}
	}
	return false
}
