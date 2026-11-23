package linux

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/reconcile"
)

// newCheckingFlow is the way from a declaration to a node that has nginx, whose nginx -t the test
// answers.
func newCheckingFlow(t *testing.T, resources ...string) (*flow, *checkingSystemd) {
	t.Helper()
	f := newFlow(t, resources...)
	c := &checkingSystemd{fakeSystemd: f.node.sys, tt: t, inactive: map[string]bool{}, stopsOnReload: map[string]bool{}}
	f.node.a.System.Runner = c
	write(t, filepath.Join(f.node.root, "etc/nginx/nginx.conf"), oldNginx)
	f.publish()
	f.node.round() // it says it takes linux
	f.publish()
	f.node.round() // it observes the files
	return f, c
}

func (f *flow) change(p *plan.Plan, name string) plan.Change {
	f.t.Helper()
	for _, c := range p.Changes {
		if c.Name == name {
			return c
		}
	}
	f.t.Fatalf("no change of %s", name)
	return plan.Change{}
}

func reloads(commands []string) int {
	n := 0
	for _, c := range commands {
		if strings.HasPrefix(c, "reload ") {
			n++
		}
	}
	return n
}

// Two files of nginx, valid only together, in one plan: the reconciler applies one operation at a
// time, so the first one's grant writes both, and nginx -t runs once both are written. It fails: both
// are put back, nginx is not reloaded, the apply stops, and the next plan says why. Once what the check
// reads is fixed, the same plan's approval writes both, checks them, and reloads nginx once.
func TestNginxFilesFromDeclarationToNodeWithTheirCheck(t *testing.T) {
	f, c := newCheckingFlow(t, siteFile, nginxFile)
	p, _ := f.plan()
	for _, name := range []string{"web/file/etc/nginx/conf.d/site.conf", "web/file/etc/nginx/nginx.conf"} {
		if ch := f.change(p, name); ch.Status != plan.StatusChange || ch.Verdict != policy.ApprovalRequired {
			t.Fatalf("%s: %+v", name, ch)
		}
	}
	c.fail = `nginx: [emerg] "server" directive is not allowed here in /etc/nginx/conf.d/site.conf:1`
	c.onCheck = func() {
		if f.node.read("/etc/nginx/nginx.conf") != nginxConf || f.node.read("/etc/nginx/conf.d/site.conf") != siteConf {
			t.Error("the check ran before both files were written")
		}
	}
	rep := f.apply(p)
	if len(rep.Steps) != 1 || rep.Steps[0].Outcome != reconcile.OutcomeFailed || !strings.Contains(rep.Steps[0].Detail, "nginx -t failed") ||
		!strings.Contains(rep.Steps[0].Detail, "removed /etc/nginx/conf.d/site.conf") || !strings.Contains(rep.Steps[0].Detail, "put back /etc/nginx/nginx.conf as it was") {
		t.Fatalf("apply %+v", rep)
	}
	if c.checks != 1 || reloads(f.node.sys.commands) != 0 {
		t.Errorf("checks %d, ran %q", c.checks, f.node.sys.commands)
	}
	if f.node.exists("/etc/nginx/conf.d/site.conf") || f.node.read("/etc/nginx/nginx.conf") != oldNginx {
		t.Error("not put back")
	}
	f.node.round()
	p, _ = f.plan()
	for _, name := range []string{"web/file/etc/nginx/conf.d/site.conf", "web/file/etc/nginx/nginx.conf"} {
		ch := f.change(p, name)
		if ch.Status != plan.StatusChange || ch.Note == nil || ch.Note.Code != codeFileRolledBack || !strings.Contains(ch.Note.Message, `"server" directive is not allowed`) ||
			!strings.Contains(ch.Note.Hint, "fix the declaration") {
			t.Errorf("%s after the put back: %+v %+v", name, ch.Status, ch.Note)
		}
	}

	c.fail, c.onCheck = "", nil
	f.node.sys.commands = nil
	rep = f.apply(p)
	if len(rep.Steps) != 2 || rep.Steps[0].Outcome != reconcile.OutcomeDone || rep.Steps[1].Outcome != reconcile.OutcomeDone {
		t.Fatalf("apply %+v", rep)
	}
	// The first operation wrote both, checked and reloaded; the second found its file written, checked
	// again, and had nothing pending.
	if c.checks != 3 || reloads(f.node.sys.commands) != 1 {
		t.Errorf("checks %d, ran %q", c.checks, f.node.sys.commands)
	}
	f.node.round()
	p, _ = f.plan()
	for _, ch := range p.Changes {
		if ch.Kind == KindFile && ch.Status != plan.StatusInSync {
			t.Errorf("after the apply: %+v", ch)
		}
	}
}

// An observe-only node (MAM-POL-0312): the plan shows the write with its check and reload, denied;
// applying it writes, checks and reloads nothing.
func TestAnObserveOnlyNodeIsNotCheckedNorReloaded(t *testing.T) {
	f, c := newCheckingFlow(t, siteFile)
	held := policy.Default
	held.ObserveOnly = []string{"web"}
	snap, err := f.rec.Provider.Observe(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Build(f.g, snap, held)
	if err := f.st.SavePlan(f.ctx, f.g, snap, held, p); err != nil {
		t.Fatal(err)
	}
	ch := f.change(p, "web/file/etc/nginx/conf.d/site.conf")
	if ch.Verdict != policy.Denied || ch.Operations[0].Decision.Code != "MAM-POL-0312" {
		t.Fatalf("the plan %+v", ch)
	}
	rep := f.apply(p)
	if len(rep.Steps) != 1 || rep.Steps[0].Outcome != reconcile.OutcomeDenied {
		t.Fatalf("apply %+v", rep)
	}
	f.node.round()
	if f.node.exists("/etc/nginx/conf.d/site.conf") || c.checks != 0 || reloads(f.node.sys.commands) != 0 {
		t.Errorf("something was done: checks %d, ran %q", c.checks, f.node.sys.commands)
	}
}
