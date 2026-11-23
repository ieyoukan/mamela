package linux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// checkingSystemd is systemd and nginx as mamelet sees them in the tests of checks: nginx -t passes,
// or fails saying fail; a service of inactive is not active; one of stopsOnReload stops when it is
// reloaded. onCheck runs when nginx -t runs, before it answers.
type checkingSystemd struct {
	*fakeSystemd
	tt            *testing.T
	checks        int
	fail          string
	inactive      map[string]bool
	stopsOnReload map[string]bool
	onCheck       func()
}

func (c *checkingSystemd) Run(ctx context.Context, program string, args []string, env []string) ([]byte, []byte, int, error) {
	if program == "/usr/sbin/nginx" {
		if !slices.Equal(args, []string{"-t", "-q"}) {
			c.tt.Errorf("nginx ran with %q", args)
		}
		c.checks++
		if c.onCheck != nil {
			c.onCheck()
		}
		if c.fail != "" {
			return nil, []byte(c.fail), 1, nil
		}
		return nil, nil, 0, nil
	}
	unit := args[len(args)-1]
	if program == progSystemctl && args[0] == "show" && c.inactive[unit] {
		return []byte("LoadState=loaded\nUnitFileState=enabled\nActiveState=inactive\n"), nil, 0, nil
	}
	if program == progSystemctl && args[0] == "reload" && c.stopsOnReload[unit] {
		c.inactive[unit] = true
	}
	return c.fakeSystemd.Run(ctx, program, args, env)
}

// newCheckingNode is a mamelet whose node has nginx (/etc/nginx/nginx.conf, 0644 root's).
func newCheckingNode(t *testing.T) (*mnode, *checkingSystemd) {
	t.Helper()
	n := newNode(t)
	c := &checkingSystemd{fakeSystemd: n.sys, tt: t, inactive: map[string]bool{}, stopsOnReload: map[string]bool{}}
	n.a.System.Runner = c
	write(t, filepath.Join(n.root, "etc/nginx/nginx.conf"), oldNginx)
	return n, c
}

const oldNginx = "events {}\nhttp {}\n"

// checked is an approved write of path, checked by nginx -t and reloading nginx, against before.
func checked(path, content, op string, before Before) File {
	f := grant(path, content, op, 1)
	f.Write.Check, f.Write.Reload, f.Write.Before = "nginx", []string{"nginx"}, before
	return f
}

// wasThere is the before-state of a file of this content.
func wasThere(content string) Before { return Before{Exists: true, SHA256: ContentSHA256(content)} }

func (n *mnode) stateFile(name string) string { return filepath.Join(n.root, "var/lib/mamelet", name) }

// nginx's files of a group, written: nginx.conf over the old one, and a new site.
func nginxGroup(op string) []File {
	return []File{
		checked("/etc/nginx/conf.d/site.conf", siteConf, op, Before{}),
		checked("/etc/nginx/nginx.conf", nginxConf, op, wasThere(oldNginx)),
	}
}

// The check runs once, after every file of the group is written (two files of nginx may be valid
// only together), then nginx is reloaded, once, and found still active.
func TestMameletChecksAGroupOnceThenReloads(t *testing.T) {
	n, c := newCheckingNode(t)
	c.onCheck = func() {
		if n.read("/etc/nginx/nginx.conf") != nginxConf || n.read("/etc/nginx/conf.d/site.conf") != siteConf {
			t.Error("the check ran before every file of its group was written")
		}
	}
	n.offer(Part{Files: nginxGroup(opA)}, nil)
	rep := n.round()
	if c.checks != 1 {
		t.Errorf("the check ran %d times", c.checks)
	}
	if want := []string{"reload -- nginx"}; !slices.Equal(n.sys.commands, want) {
		t.Errorf("ran %q, want %q", n.sys.commands, want)
	}
	r := result(t, rep, opA)
	if r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "nginx -t passed") || !strings.Contains(r.Detail, "reloaded nginx") ||
		!strings.Contains(r.Detail, "wrote /etc/nginx/conf.d/site.conf") || !strings.Contains(r.Detail, "wrote /etc/nginx/nginx.conf") {
		t.Errorf("result %+v", r)
	}
	for _, f := range observedOf(t, rep).Files {
		if f.RolledBack != nil || f.Unchecked != nil || f.Pending != nil {
			t.Errorf("observed %+v", f)
		}
	}
	// Nothing is left of the check in mamelet's state directory.
	for _, name := range []string{checksName, keptDirName, pendingName} {
		if _, err := os.Lstat(n.stateFile(name)); err == nil {
			t.Errorf("%s is left", name)
		}
	}
	// The grant is done: the next round does nothing.
	n.round()
	if c.checks != 1 || len(n.sys.commands) != 0 {
		t.Errorf("again: %d checks, %q", c.checks, n.sys.commands)
	}
}

// The check fails: every file of the group is put back as it was (the one that did not exist is
// removed), nothing is reloaded or restarted, and the outcome and the observation say what the check
// said (escaped, bounded). The files without a check are written as before.
func TestMameletPutsTheGroupBackWhenTheCheckFails(t *testing.T) {
	n, c := newCheckingNode(t)
	c.fail = "nginx: [emerg] unknown directive \"rot\" in /etc/nginx/conf.d/site.conf:3\x1b[2J\nnginx: configuration file /etc/nginx/nginx.conf test failed\n" + strings.Repeat("x", 2000)
	files := nginxGroup(opA)
	files[0].Write.Restart = nil
	files[1].Write.Reload, files[1].Write.Restart = nil, []string{"app"}
	other := grant("/etc/app.conf", "a\n", opB, 1)
	n.offer(Part{Files: append(files, other)}, nil)
	rep := n.round()

	if got := n.read("/etc/nginx/nginx.conf"); got != oldNginx {
		t.Errorf("nginx.conf is %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(n.root, "etc/nginx/nginx.conf")); fi.Mode().Perm() != 0o644 {
		t.Errorf("nginx.conf's mode %v", fi.Mode())
	}
	if n.exists("/etc/nginx/conf.d/site.conf") {
		t.Error("the new site is still there")
	}
	if n.read("/etc/app.conf") != "a\n" {
		t.Error("a file without a check was not written")
	}
	if slices.ContainsFunc(n.sys.commands, func(c string) bool { return strings.HasPrefix(c, "reload") || strings.Contains(c, "-- app") }) {
		t.Errorf("ran %q", n.sys.commands)
	}
	r := result(t, rep, opA)
	for _, want := range []string{"nginx -t failed", `unknown directive "rot"`, `\x1b[2J`, "put back /etc/nginx/nginx.conf as it was",
		"removed /etc/nginx/conf.d/site.conf (it did not exist)", "nothing was reloaded or restarted", "fix the declaration"} {
		if r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, want) {
			t.Errorf("result %+v, want it to say %q", r, want)
		}
	}
	if strings.ContainsRune(r.Detail, 0x1b) || len(r.Detail) > 2*maxCheckOutput+1000 {
		t.Errorf("the output is not escaped and bounded: %d bytes", len(r.Detail))
	}
	o := observedOf(t, rep)
	for path, content := range map[string]string{"/etc/nginx/conf.d/site.conf": siteConf, "/etc/nginx/nginx.conf": nginxConf} {
		f := o.file(path)
		if f == nil || f.RolledBack == nil {
			t.Fatalf("observed %s: %+v", path, f)
		}
		rb := f.RolledBack
		if rb.SHA256 != ContentSHA256(content) || rb.Check != "nginx" || !strings.Contains(rb.Output, "unknown directive") || len(rb.Output) > maxCheckOutput+3 ||
			!slices.Equal(rb.Files, []string{"/etc/nginx/conf.d/site.conf", "/etc/nginx/nginx.conf"}) {
			t.Errorf("observed %s: %+v", path, rb)
		}
	}
	if o.file("/etc/nginx/conf.d/site.conf").Exists || o.file("/etc/app.conf").RolledBack != nil {
		t.Errorf("observed %+v", o.Files)
	}
	// The kept content is gone with the group; what was put back is recorded until the file changes.
	if _, err := os.Lstat(n.stateFile(keptDirName)); err == nil {
		t.Error("the kept contents are left")
	}

	// A new approval of the same content (what the check reads on the node was fixed): written again,
	// checked, and kept this time; the record of the put back is gone.
	c.fail = ""
	n.offer(Part{Files: nginxGroup(opC)}, nil)
	rep = n.round()
	if r := result(t, rep, opC); r.Outcome != noderev.OutcomeDone || n.read("/etc/nginx/nginx.conf") != nginxConf {
		t.Errorf("again: %+v", r)
	}
	for _, f := range observedOf(t, rep).Files {
		if f.RolledBack != nil {
			t.Errorf("still put back: %+v", f)
		}
	}
}

// A file changed by someone else while the check ran is not put back over: it is left as it is, and
// the outcome says so; the others are put back.
func TestMameletDoesNotPutBackOverAnotherChange(t *testing.T) {
	n, c := newCheckingNode(t)
	c.fail = "nginx: [emerg] bad"
	c.onCheck = func() { write(t, filepath.Join(n.root, "etc/nginx/conf.d/site.conf"), "by hand\n") }
	n.offer(Part{Files: nginxGroup(opA)}, nil)
	rep := n.round()
	if n.read("/etc/nginx/conf.d/site.conf") != "by hand\n" || n.read("/etc/nginx/nginx.conf") != oldNginx {
		t.Error("the files are not as expected")
	}
	if r := result(t, rep, opA); !strings.Contains(r.Detail, "/etc/nginx/conf.d/site.conf changed since mamelet wrote it: left as it is") {
		t.Errorf("result %+v", r)
	}
	if o := observedOf(t, rep); o.Files[0].RolledBack != nil || o.Files[1].RolledBack == nil {
		t.Errorf("observed %+v", o.Files)
	}
}

// One write of a group that mamelet refuses (the file changed since the plan) refuses the group:
// nothing of it is written.
func TestMameletRefusesTheWholeGroup(t *testing.T) {
	n, c := newCheckingNode(t)
	files := nginxGroup(opA)
	files[1].Write.Before = wasThere("another\n")
	n.offer(Part{Files: files}, nil)
	rep := n.round()
	if n.exists("/etc/nginx/conf.d/site.conf") || n.read("/etc/nginx/nginx.conf") != oldNginx || c.checks != 0 {
		t.Error("a part of the group was written, or checked")
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "changed since the plan") ||
		!strings.Contains(r.Detail, "nothing of /etc/nginx/conf.d/site.conf, /etc/nginx/nginx.conf (checked together by nginx -t) was written") {
		t.Errorf("result %+v", r)
	}
}

// mamelet stopped (SIGTERM) between the writes and the check: the attempt is not recorded, the
// observation says the check is unfinished, and when mamelet starts again it runs the check, puts the
// files back as it fails, and the grant's outcome says so (the files are not written again).
func TestMameletFinishesACheckItStoppedBefore(t *testing.T) {
	n, c := newCheckingNode(t)
	ctx, stop := context.WithCancel(context.Background())
	testHookBeforeCheck = stop
	t.Cleanup(func() { testHookBeforeCheck = nil })
	n.offer(Part{Files: nginxGroup(opA)}, nil)
	rep, err := n.a.Round(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.checks != 0 || n.read("/etc/nginx/conf.d/site.conf") != siteConf {
		t.Fatalf("checks %d", c.checks)
	}
	for _, r := range rep.Results {
		if r.Operation == opA {
			t.Errorf("the interrupted attempt was recorded: %+v", r)
		}
	}
	for _, f := range observedOf(t, rep).Files {
		if u := f.Unchecked; u == nil || u.Check != "nginx" || u.Failed != "" {
			t.Errorf("observed %s: %+v", f.Path, u)
		}
	}
	testHookBeforeCheck = nil

	c.fail = "nginx: [emerg] bad"
	rep = n.round()
	if c.checks != 1 {
		t.Errorf("the check ran %d times", c.checks)
	}
	if n.exists("/etc/nginx/conf.d/site.conf") || n.read("/etc/nginx/nginx.conf") != oldNginx || len(n.sys.commands) != 0 {
		t.Errorf("not put back (ran %q)", n.sys.commands)
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "mamelet stopped after it wrote") ||
		!strings.Contains(r.Detail, "when it started again, nginx -t failed: nginx: [emerg] bad") || !strings.Contains(r.Detail, "removed /etc/nginx/conf.d/site.conf") {
		t.Errorf("result %+v", r)
	}
	for _, f := range observedOf(t, rep).Files {
		if f.Unchecked != nil || f.RolledBack == nil {
			t.Errorf("observed %+v", f)
		}
	}
}

// mamelet killed between the writes and the check (nothing of the round saved but its records):
// when it starts again, whatever the revision (none here: the Control Plane is away and the one it had
// is not usable), it runs the check; as it passes, the files stay, and the reload is pending until the
// grant is carried out (the check runs again first).
func TestMameletFinishesACheckAfterItWasKilled(t *testing.T) {
	n, c := newCheckingNode(t)
	testHookBeforeCheck = func() { panic("killed") }
	t.Cleanup(func() { testHookBeforeCheck = nil })
	n.offer(Part{Files: nginxGroup(opA)}, nil)
	func() {
		defer func() { recover() }()
		n.a.Round(context.Background())
		t.Fatal("not killed")
	}()
	testHookBeforeCheck = nil
	if _, err := os.Lstat(n.stateFile(checksName)); err != nil {
		t.Fatalf("no record of the check: %v", err)
	}

	// Started again without a revision: the check runs, and passes.
	cp := n.a.CP
	n.a.CP = failingCP{}
	n.sys.commands = nil
	n.a.Round(context.Background())
	n.a.CP = cp
	if c.checks != 1 || n.read("/etc/nginx/conf.d/site.conf") != siteConf || len(n.sys.commands) != 0 {
		t.Fatalf("checks %d, ran %q", c.checks, n.sys.commands)
	}
	if _, err := os.Lstat(n.stateFile(checksName)); err == nil {
		t.Error("the record of the check is left")
	}
	// The grant is carried out: the files are as approved, the check runs again, nginx is reloaded.
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "was already as approved") || !strings.Contains(r.Detail, "reloaded nginx") {
		t.Errorf("result %+v", r)
	}
	if c.checks != 2 || !slices.Equal(n.sys.commands, []string{"reload -- nginx"}) {
		t.Errorf("checks %d, ran %q", c.checks, n.sys.commands)
	}
}

// A service that is not active is not reloaded (and the outcome says so); one that stops when it is
// reloaded is reported, not left pending.
func TestMameletReloadsOnlyAnActiveService(t *testing.T) {
	n, c := newCheckingNode(t)
	c.inactive["nginx"] = true
	n.offer(Part{Files: nginxGroup(opA)}, nil)
	rep := n.round()
	if len(n.sys.commands) != 0 {
		t.Errorf("ran %q", n.sys.commands)
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "nginx is not active: not reloaded") {
		t.Errorf("result %+v", r)
	}

	n, c = newCheckingNode(t)
	c.stopsOnReload["nginx"] = true
	n.offer(Part{Files: nginxGroup(opA)}, nil)
	rep = n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "nginx is not active after its reload") {
		t.Errorf("result %+v", r)
	}
	if o := observedOf(t, rep); o.Files[0].Pending != nil {
		t.Errorf("left pending: %+v", o.Files[0].Pending)
	}
}

// A reload that fails is pending (the next plan asks to approve it again), as a restart is.
func TestMameletKeepsAFailedReloadPending(t *testing.T) {
	n, _ := newCheckingNode(t)
	n.sys.fail = "reload"
	n.offer(Part{Files: nginxGroup(opA)}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "reload nginx") || !strings.Contains(r.Detail, "the files stay as written") {
		t.Errorf("result %+v", r)
	}
	for _, f := range observedOf(t, rep).Files {
		if p := f.Pending; p == nil || !slices.Equal(p.Reloads, []string{"nginx"}) {
			t.Errorf("pending %s: %+v", f.Path, p)
		}
	}
}

// mamelet cannot put a file back (what it kept is gone): the group stays in its record, the
// observation says so (the plan blocks), nothing is reloaded, and once what it needs is back, the
// next round puts the files back.
func TestMameletTriesAgainToPutBack(t *testing.T) {
	n, c := newCheckingNode(t)
	c.fail = "nginx: [emerg] bad"
	kept, away := n.stateFile(keptDirName), n.stateFile("away")
	c.onCheck = func() {
		if err := os.Rename(kept, away); err != nil {
			t.Fatal(err)
		}
	}
	n.offer(Part{Files: nginxGroup(opA)}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "mamelet tries again every round") {
		t.Errorf("result %+v", r)
	}
	o := observedOf(t, rep)
	if u := o.Files[1].Unchecked; u == nil || u.Failed == "" || !strings.Contains(u.Error, "put back /etc/nginx/nginx.conf") {
		t.Errorf("observed %+v", o.Files[1])
	}
	if n.read("/etc/nginx/nginx.conf") != nginxConf || len(n.sys.commands) != 0 {
		t.Error("nginx.conf was changed, or something ran")
	}
	c.onCheck = nil
	if err := os.Rename(away, kept); err != nil {
		t.Fatal(err)
	}
	rep = n.round()
	if n.read("/etc/nginx/nginx.conf") != oldNginx || c.checks != 1 {
		t.Errorf("not put back (checks %d)", c.checks)
	}
	if o := observedOf(t, rep); o.Files[1].Unchecked != nil || o.Files[1].RolledBack == nil {
		t.Errorf("observed %+v", o.Files[1])
	}
}

// A record of the checks that cannot be decoded is set aside, never written over, and reported.
func TestMameletSetsAsideARecordOfChecksItCannotRead(t *testing.T) {
	n, _ := newCheckingNode(t)
	write(t, n.stateFile(checksName), "{not json")
	n.offer(Part{Files: []File{{Path: "/etc/nginx/nginx.conf"}}}, nil)
	rep := n.round()
	if o := observedOf(t, rep); !slices.Contains(o.SetAside, checksName+corruptSuffix) {
		t.Errorf("set aside %v", o.SetAside)
	}
	if got, _ := os.ReadFile(n.stateFile(checksName + corruptSuffix)); string(got) != "{not json" {
		t.Errorf("what was set aside: %q", got)
	}
}
