package linux

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// fakeSystemd records what mamelet runs (systemctl only: linux runs nothing else).
type fakeSystemd struct {
	commands []string
	fail     string // a verb that exits 1
}

func (f *fakeSystemd) Run(_ context.Context, program string, args []string, _ []string) ([]byte, []byte, int, error) {
	if program != progSystemctl {
		return nil, nil, -1, fmt.Errorf("mamelet ran %s", program)
	}
	if args[0] == "show" {
		return []byte("LoadState=loaded\nUnitFileState=enabled\nActiveState=active\n"), nil, 0, nil
	}
	f.commands = append(f.commands, strings.Join(args, " "))
	if args[0] == f.fail {
		return nil, []byte("Job failed"), 1, nil
	}
	return nil, nil, 0, nil
}

type memCP struct {
	signed  noderev.Signed
	reports []noderev.Report
}

func (c *memCP) Revision(context.Context) (noderev.Signed, error) { return c.signed, nil }
func (c *memCP) Report(_ context.Context, r noderev.Report) error {
	c.reports = append(c.reports, r)
	return nil
}

var mt0 = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

// node is a mamelet on a node whose file system is a directory of the test, with root (and only
// root) being the test's own user, so that mamelet can give files to "root" without being root.
type mnode struct {
	t      *testing.T
	root   string
	key    ed25519.PrivateKey
	cp     *memCP
	sys    *fakeSystemd
	a      *mamelet.Agent
	serial int64
}

func newNode(t *testing.T) *mnode {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "etc/passwd"), fmt.Sprintf("root:x:%d:%d:root:/root:/bin/bash\n", os.Getuid(), os.Getgid()))
	write(t, filepath.Join(root, "etc/group"), fmt.Sprintf("root:x:%d:\n", os.Getgid()))
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	n := &mnode{t: t, root: root, key: key, cp: &memCP{}, sys: &fakeSystemd{}}
	n.a = &mamelet.Agent{
		Config: mamelet.Config{Node: "web", VMID: 7001, RevisionKey: key.Public().(ed25519.PublicKey), StateDir: "/var/lib/mamelet"},
		CP:     n.cp, System: &mamelet.System{Runner: n.sys, Root: root},
		State: mamelet.FileState{Path: filepath.Join(t.TempDir(), "state.json")}, Now: func() time.Time { return mt0 }, Version: "test",
	}
	return n
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// grant is an approved write of path.
func grant(path, content, op string, attempt int) File {
	return File{Path: path, Owner: "root", Group: "root", Write: &Write{Content: content, SHA256: ContentSHA256(content), Mode: "0640",
		Owner: "root", Group: "root", Grant: noderev.Once{Operation: op, Attempt: attempt, Action: ActionWrite}}}
}

// offer signs a revision with the part (and the other parts, unknown ones too) for mamelet.
func (n *mnode) offer(p Part, more map[string]jsontext.Value) {
	n.t.Helper()
	n.serial++
	r := noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: n.serial, IssuedAt: mt0, ExpiresAt: mt0.Add(time.Hour),
		Packages: []noderev.Package{}, Services: []noderev.Service{}}
	setPart(&r, p)
	for k, v := range more {
		if r.Ext == nil {
			r.Ext = map[string]jsontext.Value{}
		}
		r.Ext[k] = v
	}
	// Signed by hand: Sign refuses a part this Control Plane does not know, and the tests offer one.
	payload, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		n.t.Fatal(err)
	}
	n.cp.signed = noderev.Signed{Payload: payload, Signature: ed25519.Sign(n.key, payload)}
}

func (n *mnode) round() noderev.Report {
	n.t.Helper()
	n.sys.commands = nil
	rep, err := n.a.Round(context.Background())
	if err != nil {
		n.t.Fatal(err)
	}
	return rep
}

func (n *mnode) read(path string) string {
	n.t.Helper()
	b, err := os.ReadFile(filepath.Join(n.root, path))
	if err != nil {
		n.t.Fatal(err)
	}
	return string(b)
}

func observedOf(t *testing.T, rep noderev.Report) Observed {
	t.Helper()
	var o Observed
	if err := decodeStrict(rep.Ext[Name], &o); err != nil {
		t.Fatalf("the report's linux part: %v (%s)", err, rep.Ext[Name])
	}
	return o
}

func result(t *testing.T, rep noderev.Report, op string) noderev.Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.Operation == op {
			return r
		}
	}
	t.Fatalf("no result of %s in %+v", op, rep.Results)
	return noderev.Result{}
}

const opA, opB = "op-aaaaaaaaaaaaaaaaaaaaaaaa", "op-bbbbbbbbbbbbbbbbbbbbbbbb"

// An approved write: the directories it needs, the content, mode and owner, and the services it
// names restarted if they run; once.
func TestMameletWritesAnApprovedFileOnce(t *testing.T) {
	n := newNode(t)
	f := grant("/etc/app/app.conf", "listen 8080\n", opA, 1)
	f.Write.Restart = []string{"app", "nginx"}
	n.offer(Part{Files: []File{f}}, nil)
	rep := n.round()
	if got := n.read("/etc/app/app.conf"); got != "listen 8080\n" {
		t.Fatalf("content %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(n.root, "etc/app/app.conf")); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(n.root, "etc/app")); fi.Mode().Perm() != 0o755 {
		t.Errorf("the directory's mode %v", fi.Mode())
	}
	if want := []string{"try-restart -- app", "try-restart -- nginx"}; !slices.Equal(n.sys.commands, want) {
		t.Errorf("ran %q, want %q", n.sys.commands, want)
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "made /etc/app") {
		t.Errorf("result %+v", r)
	}
	if rep.Extensions[Name] != Version {
		t.Errorf("the report lists %v", rep.Extensions)
	}
	o := observedOf(t, rep)
	if len(o.Files) != 1 || !o.Files[0].Exists || o.Files[0].SHA256 != ContentSHA256("listen 8080\n") || o.Files[0].Mode != "0640" || o.Files[0].Owner != "root" || o.Files[0].Group != "root" {
		t.Errorf("observed %+v", o.Files)
	}
	if !o.Users["root"] || !o.Groups["root"] {
		t.Errorf("accounts %v %v", o.Users, o.Groups)
	}

	// Changed by hand afterwards: the same grant does not write again (a new approval does).
	write(t, filepath.Join(n.root, "etc/app/app.conf"), "changed by hand\n")
	rep = n.round()
	if got := n.read("/etc/app/app.conf"); got != "changed by hand\n" {
		t.Errorf("the grant wrote twice: %q", got)
	}
	if len(n.sys.commands) != 0 {
		t.Errorf("ran %q again", n.sys.commands)
	}
	if o := observedOf(t, rep); o.Files[0].SHA256 != ContentSHA256("changed by hand\n") {
		t.Errorf("observed %+v", o.Files[0])
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone {
		t.Errorf("the result is not reported again: %+v", r)
	}
}

// A unit: systemd reads it again before the services are restarted.
func TestMameletReloadsSystemdForAUnit(t *testing.T) {
	n := newNode(t)
	f := grant("/etc/systemd/system/app.service", "[Service]\nExecStart=/usr/local/bin/app\n", opA, 1)
	f.Write.Restart = []string{"app"}
	n.offer(Part{Files: []File{f}}, nil)
	n.round()
	if want := []string{"daemon-reload", "try-restart -- app"}; !slices.Equal(n.sys.commands, want) {
		t.Errorf("ran %q, want %q", n.sys.commands, want)
	}
}

// mamelet never writes through a symbolic link: not at the path, not above it.
func TestMameletDoesNotWriteThroughALink(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(n *mnode)
		path  string
	}{
		{"the file is a link", func(n *mnode) {
			write(t, filepath.Join(n.root, "etc/target"), "untouched\n")
			if err := os.Symlink("target", filepath.Join(n.root, "etc/app.conf")); err != nil {
				t.Fatal(err)
			}
		}, "/etc/app.conf"},
		{"a directory above is a link", func(n *mnode) {
			write(t, filepath.Join(n.root, "etc/real/x"), "")
			write(t, filepath.Join(n.root, "etc/real/app.conf"), "untouched\n")
			if err := os.Symlink("real", filepath.Join(n.root, "etc/app")); err != nil {
				t.Fatal(err)
			}
		}, "/etc/app/app.conf"},
		{"the path is a directory", func(n *mnode) {
			write(t, filepath.Join(n.root, "etc/app.conf/x"), "")
		}, "/etc/app.conf"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := newNode(t)
			c.setup(n)
			n.offer(Part{Files: []File{grant(c.path, "new\n", opA, 1)}}, nil)
			rep := n.round()
			if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused {
				t.Errorf("result %+v", r)
			}
			if o := observedOf(t, rep); o.Files[0].Unsafe == "" {
				t.Errorf("observed %+v", o.Files[0])
			}
			for _, p := range []string{"etc/target", "etc/real/app.conf"} {
				if b, err := os.ReadFile(filepath.Join(n.root, p)); err == nil && string(b) != "untouched\n" {
					t.Errorf("%s was written: %q", p, b)
				}
			}
		})
	}
}

// A path mamelet refuses is refused whatever the revision says (the graph refuses it first); the
// other files go on.
func TestMameletRefusesForbiddenPathsWhateverTheRevisionSays(t *testing.T) {
	n := newNode(t)
	ops := map[string]string{
		"/etc/sudoers.d/x":                 "op-cccccccccccccccccccccccc",
		"/var/lib/mamelet/state.json":      "op-dddddddddddddddddddddddd",
		"/root/.ssh/authorized_keys":       "op-eeeeeeeeeeeeeeeeeeeeeeee",
		"/etc/../etc/shadow":               "op-ffffffffffffffffffffffff",
		"/etc/rancher/k3s/cluster-token":   "op-abababababababababababab",
		"/etc/apt/sources.list.d/mamela-x": "op-acacacacacacacacacacacac",
	}
	files := []File{grant("/etc/fine.conf", "fine\n", opA, 1)}
	for p, op := range ops {
		files = append(files, grant(p, "evil\n", op, 1))
	}
	n.offer(Part{Files: files}, nil)
	rep := n.round()
	for p, op := range ops {
		if r := result(t, rep, op); r.Outcome != noderev.OutcomeRefused {
			t.Errorf("%s: %+v", p, r)
		}
		if _, err := os.Stat(filepath.Join(n.root, p)); err == nil {
			t.Errorf("%s was written", p)
		}
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || n.read("/etc/fine.conf") != "fine\n" {
		t.Errorf("the allowed file: %+v", r)
	}
	for _, s := range observedOf(t, rep).Files {
		if _, refused := ops[s.Path]; refused && !strings.Contains(s.Error, "refuses") {
			t.Errorf("observed %+v", s)
		}
	}
}

// A user that does not exist: refused, nothing written; a later attempt, after it exists, writes.
func TestMameletRetriesWithANewAttempt(t *testing.T) {
	n := newNode(t)
	f := grant("/etc/app.conf", "x\n", opA, 1)
	f.Write.Owner = "app"
	n.offer(Part{Files: []File{f}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "user app does not exist") {
		t.Errorf("result %+v", r)
	}
	if o := observedOf(t, rep); o.Users["app"] || !o.Users["root"] {
		t.Errorf("accounts %v", o.Users)
	}
	write(t, filepath.Join(n.root, "etc/passwd"), fmt.Sprintf("root:x:%d:%d::/root:/bin/sh\napp:x:%d:%d::/:/bin/false\n", 99999, os.Getgid(), os.Getuid(), os.Getgid()))
	rep = n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused {
		t.Errorf("the same attempt ran again: %+v", r)
	}
	f.Write.Grant.Attempt = 2
	n.offer(Part{Files: []File{f}}, nil)
	rep = n.round()
	if n.read("/etc/app.conf") != "x\n" {
		t.Errorf("attempt 2 did not write")
	}
	if o := observedOf(t, rep); o.Files[0].Owner != "app" {
		t.Errorf("observed %+v", o.Files[0])
	}
}

// A write that was interrupted (mamelet killed after it made its temporary file, before it said
// what it did) is made again by the same attempt, and leaves no temporary file.
func TestMameletFinishesAnInterruptedWrite(t *testing.T) {
	n := newNode(t)
	tmp := filepath.Join(n.root, "etc/app", tempName("app.conf"))
	write(t, tmp, "half a fi")
	write(t, filepath.Join(n.root, "etc/app/app.conf"), "old\n")
	n.offer(Part{Files: []File{grant("/etc/app/app.conf", "new\n", opA, 1)}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || n.read("/etc/app/app.conf") != "new\n" {
		t.Fatalf("result %+v", r)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Error("the temporary file is still there")
	}
	entries, _ := os.ReadDir(filepath.Join(n.root, "etc/app"))
	if len(entries) != 1 {
		t.Errorf("the directory has %v", entries)
	}
}

// Written already, but not said so (the attempt was interrupted after the rename): the attempt still
// restarts what it names, and says the file was in place.
func TestMameletRestartsWhenTheFileIsInPlaceAlready(t *testing.T) {
	n := newNode(t)
	write(t, filepath.Join(n.root, "etc/app.conf"), "new\n")
	if err := os.Chmod(filepath.Join(n.root, "etc/app.conf"), 0o640); err != nil {
		t.Fatal(err)
	}
	f := grant("/etc/app.conf", "new\n", opA, 1)
	f.Write.Restart = []string{"app"}
	n.offer(Part{Files: []File{f}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "already as approved") {
		t.Errorf("result %+v", r)
	}
	if !slices.Equal(n.sys.commands, []string{"try-restart -- app"}) {
		t.Errorf("ran %q", n.sys.commands)
	}
}

// A failed restart is reported (the file stays written).
func TestMameletReportsAFailedRestart(t *testing.T) {
	n := newNode(t)
	n.sys.fail = "try-restart"
	f := grant("/etc/app.conf", "new\n", opA, 1)
	f.Write.Restart = []string{"app"}
	n.offer(Part{Files: []File{f}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "restart app") || !strings.Contains(r.Detail, "wrote /etc/app.conf") {
		t.Errorf("result %+v", r)
	}
}

// A part mamelet cannot use is refused alone: the report says why, and the rest goes on. A part of
// an extension it does not know likewise.
func TestMameletRefusesOnlyThePartItCannotUse(t *testing.T) {
	n := newNode(t)
	bad := grant("/etc/app.conf", "x\n", opA, 1)
	bad.Write.SHA256 = ContentSHA256("something else")
	n.offer(Part{Files: []File{bad}}, map[string]jsontext.Value{"zzz": jsontext.Value(`{"a":1}`)})
	rep := n.round()
	if rep.Refused != "" {
		t.Fatalf("the whole revision was refused: %s", rep.Refused)
	}
	if !strings.Contains(rep.ExtRefused[Name], "not the one approved") || !strings.Contains(rep.ExtRefused["zzz"], "does not know") {
		t.Errorf("refused %v", rep.ExtRefused)
	}
	if rep.Serial != n.serial || rep.Ext[Name] != nil {
		t.Errorf("serial %d, ext %s", rep.Serial, rep.Ext)
	}
	if _, err := os.Stat(filepath.Join(n.root, "etc/app.conf")); err == nil {
		t.Error("the file was written")
	}
}

// A one-time grant needs a revision fetched in this round: from the last one kept, mamelet only
// observes.
func TestMameletWritesOnlyFromAFreshRevision(t *testing.T) {
	n := newNode(t)
	n.offer(Part{Files: []File{grant("/etc/app.conf", "x\n", opA, 1)}}, nil)
	// The revision was accepted in an earlier round, and the Control Plane cannot be reached now.
	signed := n.cp.signed
	if err := n.a.State.Save(mamelet.State{LastSerial: n.serial, Revision: &signed, Attempts: map[string]noderev.Result{}}); err != nil {
		t.Fatal(err)
	}
	n.a.CP = failingCP{n.cp}
	rep, _ := n.a.Round(context.Background())
	if _, err := os.Stat(filepath.Join(n.root, "etc/app.conf")); err == nil {
		t.Error("written from a cached revision")
	}
	if o := observedOf(t, rep); len(o.Files) != 1 || o.Files[0].Exists {
		t.Errorf("observed %+v", o.Files)
	}
}

type failingCP struct{ *memCP }

func (failingCP) Revision(context.Context) (noderev.Signed, error) {
	return noderev.Signed{}, fmt.Errorf("unreachable")
}

// A file right under the root, and one in a directory three levels down that does not exist yet.
func TestMameletWritesAtAnyDepth(t *testing.T) {
	n := newNode(t)
	n.offer(Part{Files: []File{grant("/top.conf", "top\n", opA, 1), grant("/srv/a/b/c.conf", "deep\n", opB, 1)}}, nil)
	rep := n.round()
	for _, op := range []string{opA, opB} {
		if r := result(t, rep, op); r.Outcome != noderev.OutcomeDone {
			t.Errorf("%s: %+v", op, r)
		}
	}
	if n.read("/top.conf") != "top\n" || n.read("/srv/a/b/c.conf") != "deep\n" {
		t.Error("not written")
	}
	for _, d := range []string{"srv", "srv/a", "srv/a/b"} {
		if fi, err := os.Lstat(filepath.Join(n.root, d)); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: %v %v", d, fi, err)
		}
	}
}
