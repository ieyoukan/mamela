package linux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/reconcile"
)

// offline makes the flow's mamelet silent: an apply grants and then waits in vain (its outcome is
// unknown), and the grant stays in the revision until its deadline.
func (f *flow) offline() {
	f.nodes.Sleep = func(_ context.Context, d time.Duration) error { f.now = f.now.Add(d); return nil }
}

// online makes it answer again, one round each time an apply waits.
func (f *flow) online() {
	f.nodes.Sleep = func(ctx context.Context, d time.Duration) error {
		f.now = f.now.Add(d)
		f.node.a.Round(ctx)
		return nil
	}
}

// applyUnknown approves and applies p while mamelet does not answer.
func (f *flow) applyUnknown(p *plan.Plan) {
	f.t.Helper()
	f.offline()
	defer f.online()
	if _, err := f.st.Approve(f.ctx, p.Digest, "test", time.Hour); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.rec.Apply(f.ctx, p.Digest); err != nil {
		f.t.Fatal(err)
	}
}

func (f *flow) serial() int64 {
	f.t.Helper()
	return f.revision().Serial
}

// A plan of another declaration (a pull request's) of a user whose change main's apply is handing out
// does not take the change away, and mamelet carries it out as approved: the change carries every value
// it needs, and what the pull request asks mamelet to observe is added beside main's.
func TestAPlanOfAnotherDeclarationKeepsALiveGrant(t *testing.T) {
	main := userRes("web/user/app", `{"node":"web","name":"app","groups":["docker"]}`)
	pr := userRes("web/user/app", `{"node":"web","name":"app","shell":"/bin/sh","groups":["www-data"],"uid":1600}`)
	f, accounts := userFlow(t, main)
	f.publish()
	f.node.round()
	f.publish()
	f.node.round()
	c := f.userChange()
	if c.Status != plan.StatusChange {
		t.Fatalf("the plan %+v", c)
	}
	p, _ := f.plan()
	f.applyUnknown(p)
	part := partOf(f.revision())
	if u := part.user("app"); u == nil || u.Change == nil || !u.InForce {
		t.Fatalf("no grant: %+v", u)
	}
	f.declare(pr)
	if c := f.userChange(); c.Status == plan.StatusBlocked {
		t.Fatalf("the pull request's plan: %+v", c)
	}
	part = partOf(f.revision())
	u := part.user("app")
	if u.Change == nil || u.Change.Create == nil || u.Change.Create.Shell != DefaultShell || !slices.Equal(u.Change.Groups, []string{"docker"}) {
		t.Fatalf("the grant after the pull request's plan: %+v", u.Change)
	}
	if !slices.Equal(u.Shells, []string{"/bin/bash", "/bin/sh"}) || !slices.Equal(u.Groups, []string{"docker", "www-data"}) || !slices.Equal(u.UIDs, []int{1600}) {
		t.Errorf("what mamelet observes: %+v", u)
	}
	rep := f.node.round()
	if r := result(t, rep, u.Change.Grant.Operation); r.Outcome != noderev.OutcomeDone {
		t.Fatalf("mamelet: %+v", r)
	}
	if !slices.Contains(accounts.commands, "useradd --create-home --shell /bin/bash --home-dir /home/app --user-group -- app") {
		t.Errorf("ran %q", accounts.commands)
	}
}

// Two declarations planned in turn (mamelacd plans a pull request and its branch alternately): after
// each has been planned once, the revision does not change any more, and each plan finds its own
// observation in the report (the owner it names, the shell and groups of its user).
func TestPlansInTurnDoNotChangeTheRevision(t *testing.T) {
	mainDecl := []string{
		fileRes("web/file/etc/app.conf", `{"node":"web","path":"/etc/app.conf","content":"x\n","owner":"root","group":"root"}`),
		userRes("web/user/app", `{"node":"web","name":"app","groups":["docker"]}`),
	}
	prDecl := []string{
		fileRes("web/file/etc/app.conf", `{"node":"web","path":"/etc/app.conf","content":"x\n","owner":"app","group":"www-data"}`),
		userRes("web/user/app", `{"node":"web","name":"app","shell":"/bin/sh","groups":["www-data"]}`),
	}
	f, _ := userFlow(t, mainDecl...)
	f.publish()
	f.node.round()
	planOf := func(decl []string) *plan.Plan {
		f.declare(decl...)
		p, _ := f.plan()
		f.node.round()
		return p
	}
	planOf(mainDecl)
	planOf(prDecl)
	serial := f.serial()
	for i := range 4 {
		decl := mainDecl
		if i%2 == 1 {
			decl = prDecl
		}
		p := planOf(decl)
		for _, c := range p.Changes {
			if c.Note != nil && (c.Note.Code == codeFileAwait || c.Note.Code == codeUserAwait) {
				t.Errorf("plan %d waits for a report: %s %+v", i, c.ID(), c.Note)
			}
		}
	}
	if s := f.serial(); s != serial {
		t.Errorf("the revision changed from %d to %d with plans alone", serial, s)
	}
}

// The observation of a source says which URIs and suites it was made for: a plan uses only the one of
// its own declaration, and waits for a report while there is none (it never judges its source by
// another declaration's).
func TestAnAptRepoIsJudgedByItsOwnURIs(t *testing.T) {
	g, err := decodeGraph(dockerRepo)
	if err != nil {
		t.Fatal(err)
	}
	inSync := dockerState(present("", dockerSources(true)), present("", testKeyArmored), FileState{})
	other := inSync
	other.Where = []AptWhereState{{AptWhere: AptWhere{URIs: []string{"https://download.docker.com/linux/debian"}, Suites: []string{"bookworm"}},
		Unread: []string{"https://download.docker.com/linux/debian bookworm"}, Conflicts: []string{"/etc/apt/sources.list.d/docker.list"}}}
	p := plan.Build(g, world{mamelet: Release, accepts: Version, observed: &Observed{AptRepos: []AptRepoState{other}}}.snapshot(g), policy.Default)
	if c := findChange(t, p, repoID); c.Status != plan.StatusWaiting || c.Note.Code != codeAptRepoAwait {
		t.Errorf("only another declaration's URIs observed: %+v %+v", c, c.Note)
	}
	both := inSync
	both.Where = append(slices.Clone(other.Where), inSync.Where...)
	p = plan.Build(g, world{mamelet: Release, accepts: Version, observed: &Observed{AptRepos: []AptRepoState{both}}}.snapshot(g), policy.Default)
	if c := findChange(t, p, repoID); c.Status != plan.StatusInSync {
		t.Errorf("its own observed too: %+v %+v", c, c.Note)
	}
}

// What a plan shows as kept (retained) is what the declaration in force has (Mamela may have written
// it) and this one does not: never what only another declaration planned.
func TestOnlyWhatIsInForceIsKept(t *testing.T) {
	g, err := decodeGraph()
	if err != nil {
		t.Fatal(err)
	}
	o := &Observed{
		Files:      []FileState{present("/etc/a.conf", "a\n"), present("/etc/b.conf", "b\n")},
		UserStates: []UserState{{Name: "a", Exists: true, UID: 1001}, {Name: "b", Exists: true, UID: 1002}},
		Artifacts:  []ArtifactState{{FileState: present("/usr/local/bin/a", "a")}, {FileState: present("/usr/local/bin/b", "b")}},
		AptRepos: []AptRepoState{{Name: "a", Sources: present(AptSourcesPath("a"), "x")},
			{Name: "b", Sources: present(AptSourcesPath("b"), "x")}},
		InForce: &InForce{Files: []string{"/etc/a.conf"}, Users: []string{"a"}, Artifacts: []string{"/usr/local/bin/a"}, AptRepos: []string{"a"}},
	}
	p := plan.Build(g, world{mamelet: Release, accepts: Version, observed: o}.snapshot(g), policy.Default)
	var kept []string
	for _, c := range p.Changes {
		if c.Status == plan.StatusRetained {
			kept = append(kept, c.ID())
		}
	}
	want := []string{"NodeUser web/user/a", "NodeAptRepo web/apt-repo/a", "NodeArtifact web/artifact/usr/local/bin/a", "NodeFile web/file/etc/a.conf"}
	if !slices.Equal(kept, want) {
		t.Errorf("kept %q\nwant %q", kept, want)
	}
}

// The way of 2(c) on a Control Plane: a pull request declares a file, mamelet observes it; the
// branch's plan does not show it. Once a plan that has it is applied, the branch's plan without it
// shows it kept.
func TestAFileOnlyAPullRequestDeclaredIsNotKept(t *testing.T) {
	file := fileRes("web/file/etc/pr.conf", `{"node":"web","path":"/etc/pr.conf","content":"pr\n"}`)
	f := newFlow(t)
	f.publish()
	f.node.round()
	f.declare(file)
	f.plan()
	f.node.round() // it observes the pull request's file
	write(t, filepath.Join(f.node.root, "etc/pr.conf"), "by hand\n")
	f.node.round()
	f.declare()
	p, _ := f.plan()
	for _, c := range p.Changes {
		if c.Kind == KindFile {
			t.Errorf("the branch's plan shows %+v", c)
		}
	}
	f.declare(file)
	p, _ = f.plan()
	f.apply(p)
	f.node.round()
	f.declare()
	p, _ = f.plan()
	if c := p.Changes[len(p.Changes)-1]; c.ID() != "NodeFile web/file/etc/pr.conf" || c.Status != plan.StatusRetained {
		t.Errorf("after it was applied: %+v", p.Changes)
	}
}

// A write approved and handed out while mamelet was away, whose declaration went back to the content
// the node has: the next approved plan applied withdraws it (Prune), and mamelet, back, writes nothing.
func TestAStaleWriteIsWithdrawnAtTheNextApply(t *testing.T) {
	v1 := fileRes("web/file/etc/app.conf", `{"node":"web","path":"/etc/app.conf","content":"v1\n","restart":["app"]}`)
	v2 := fileRes("web/file/etc/app.conf", `{"node":"web","path":"/etc/app.conf","content":"v2\n","restart":["app"]}`)
	f := newFlow(t, v2)
	write(t, filepath.Join(f.node.root, "etc/app.conf"), "v1\n")
	f.publish()
	f.node.round()
	f.publish()
	f.node.round()
	p, _ := f.plan()
	f.applyUnknown(p)
	part := partOf(f.revision())
	if w := part.file("/etc/app.conf").Write; w == nil || w.SHA256 != ContentSHA256("v2\n") || w.Before.SHA256 != ContentSHA256("v1\n") {
		t.Fatalf("the grant %+v", w)
	}
	f.declare(v1)
	p, c := f.plan()
	if c.Status != plan.StatusInSync {
		t.Fatalf("v1 on the node, v1 declared: %+v", c)
	}
	if part = partOf(f.revision()); part.file("/etc/app.conf").Write == nil {
		t.Fatal("a plan took the grant away")
	}
	f.apply(p) // approved and applied: what it does not write goes
	if part = partOf(f.revision()); part.file("/etc/app.conf").Write != nil {
		t.Fatalf("the stale write stays: %+v", part.file("/etc/app.conf").Write)
	}
	f.node.round()
	if got := f.node.read("/etc/app.conf"); got != "v1\n" || len(f.node.sys.commands) != 0 {
		t.Errorf("the node has %q, ran %q", got, f.node.sys.commands)
	}
}

// A write handed out while mamelet was away, and the file changed by hand meanwhile: mamelet compares
// and refuses (it never overwrites what the approved plan did not show); the file already as approved
// is done.
func TestMameletComparesBeforeItWrites(t *testing.T) {
	n := newNode(t)
	write(t, filepath.Join(n.root, "etc/app.conf"), "edited by hand\n")
	g := grant("/etc/app.conf", "v2\n", opA, 1)
	g.Write.Before = Before{Exists: true, SHA256: ContentSHA256("v1\n")}
	n.offer(Part{Files: []File{g}}, nil)
	if r := result(t, n.round(), opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "changed since the plan") {
		t.Errorf("result %+v", r)
	}
	if n.read("/etc/app.conf") != "edited by hand\n" {
		t.Error("overwritten")
	}
	write(t, filepath.Join(n.root, "etc/app.conf"), "v2\n")
	g = grant("/etc/app.conf", "v2\n", opB, 1)
	g.Write.Before = Before{Exists: true, SHA256: ContentSHA256("v1\n")}
	n.offer(Part{Files: []File{g}}, nil)
	if r := result(t, n.round(), opB); r.Outcome != noderev.OutcomeDone {
		t.Errorf("already as approved: %+v", r)
	}
}

// A file too large to hash has no sha256 to tell it from another: the plan's before-state carries its
// stamp (inode, size, times), and a large file rewritten since (here the same size, with its mtime put
// back) is not the one the plan saw. One without a stamp is never taken for it.
func TestMameletComparesALargeFileByItsStamp(t *testing.T) {
	n := newNode(t)
	big := filepath.Join(n.root, "etc/app/big.conf")
	write(t, big, strings.Repeat("a", MaxContent+1))
	r, err := os.OpenRoot(n.root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	seen := inspect(r, "/etc/app/big.conf", readAccounts(r))
	if !seen.Large || (seen.Stamp == "") != !runtimeStamps() {
		t.Fatalf("observed %+v", seen)
	}
	if !runtimeStamps() {
		t.Skip("this system gives files no stamp")
	}
	fi, _ := os.Stat(big)
	f, _ := os.OpenFile(big, os.O_WRONLY, 0)
	f.WriteAt([]byte("b"), 0)
	f.Close()
	os.Chtimes(big, fi.ModTime(), fi.ModTime())
	g := grant("/etc/app/big.conf", "small\n", opA, 1)
	g.Write.Before = beforeOf(seen)
	n.offer(Part{Files: []File{g}}, nil)
	if res := result(t, n.round(), opA); res.Outcome != noderev.OutcomeRefused || !strings.Contains(res.Detail, "changed since the plan (it was a file too large to hash (inode") {
		t.Errorf("result %+v", res)
	}
	if b, _ := os.ReadFile(big); len(b) != MaxContent+1 {
		t.Error("overwritten")
	}
	// Seen again, unchanged since: written.
	g = grant("/etc/app/big.conf", "small\n", opB, 1)
	g.Write.Before = beforeOf(inspect(r, "/etc/app/big.conf", readAccounts(r)))
	n.offer(Part{Files: []File{g}}, nil)
	if res := result(t, n.round(), opB); res.Outcome != noderev.OutcomeDone || n.read("/etc/app/big.conf") != "small\n" {
		t.Errorf("result %+v", res)
	}
	// A large file without a stamp is never the one the plan saw.
	if (Before{Exists: true, Large: true}).Is(FileState{Exists: true, Large: true}) {
		t.Error("two large files without stamps are the same")
	}
	for _, b := range []Before{{Exists: true, SHA256: sumA, Stamp: "1:2:3:4:5"}, {Exists: true, Large: true, Stamp: "x"}, {Stamp: "1:2:3:4:5"}} {
		if b.check() == "" {
			t.Errorf("%+v accepted", b)
		}
	}
	if b := (Before{Exists: true, Large: true, Stamp: "66306:1234:262145:-1:1763550000000000000"}); b.check() != "" {
		t.Errorf("%+v refused: %s", b, b.check())
	}
}

// The same compare and swap for an artifact, an APT source and a user's shell and keys.
func TestMameletComparesBeforeTheOtherKinds(t *testing.T) {
	t.Run("artifact", func(t *testing.T) {
		rel := newReleases(t)
		sum := rel.put("/tool", []byte("tool"))
		n := artifactNode(t, rel)
		write(t, filepath.Join(n.root, "usr/local/bin/tool"), "put by hand")
		n.offer(Part{Artifacts: []Artifact{install("/usr/local/bin/tool", rel.url("/tool"), sum, "", opA, 1)}}, nil)
		if r := result(t, n.round(), opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "changed since the plan") || rel.count("/tool") != 0 {
			t.Errorf("result %+v, %d downloads", r, rel.count("/tool"))
		}
	})
	t.Run("apt source", func(t *testing.T) {
		n, apt, k := aptNode(t)
		write(t, filepath.Join(n.root, AptSourcesPath("docker")), "Types: deb\nURIs: https://evil.example\n")
		n.offer(Part{AptRepos: []AptRepo{addGrant(k, "/armored", testKeyArmored, opA, 1)}}, nil)
		if r := result(t, n.round(), opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "changed since the plan") || len(apt.commands) != 0 {
			t.Errorf("result %+v, ran %q", r, apt.commands)
		}
	})
	t.Run("user", func(t *testing.T) {
		n, f := newUserNode(t)
		addUser(t, n, "app:x:%d:%d::/home/app:/bin/sh")
		write(t, filepath.Join(n.root, "home/app/.ssh/authorized_keys"), keyBob+"\n")
		os.Chmod(filepath.Join(n.root, "bin/zsh"), 0o755)
		content := KeysContent([]string{keyAlice})
		change := func(op string, before UserBefore) User {
			return User{Name: "app", Shells: []string{"/bin/bash"}, Keys: true, Change: &UserChange{Shell: "/bin/bash", Keys: &KeysWrite{Content: content, SHA256: ContentSHA256(content)},
				Before: before, Grant: noderev.Once{Operation: op, Attempt: 1, Action: ActionChangeUser}}}
		}
		// The plan saw /bin/zsh: the shell changed since.
		n.offer(Part{Users: []User{change(opA, UserBefore{Exists: true, Shell: "/bin/zsh", Keys: &Before{Exists: true, SHA256: ContentSHA256(keyBob + "\n")}})}}, nil)
		if r := result(t, n.round(), opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "shell of app changed since the plan") || len(f.commands) != 0 {
			t.Errorf("shell: %+v, ran %q", r, f.commands)
		}
		// The plan saw no authorized_keys: they changed since.
		n.offer(Part{Users: []User{change(opB, UserBefore{Exists: true, Shell: "/bin/sh", Keys: &Before{}})}}, nil)
		if r := result(t, n.round(), opB); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "authorized_keys changed since the plan") {
			t.Errorf("keys: %+v", r)
		}
		if n.read("/home/app/.ssh/authorized_keys") != keyBob+"\n" {
			t.Error("the keys were overwritten")
		}
	})
}

// A plan's observation targets are bounded: a list past maxTargets keeps only the planned
// declaration's, and entries no approved declaration has past maxCandidates go.
func TestObservationTargetsAreBounded(t *testing.T) {
	var many []string
	for i := range maxTargets {
		many = append(many, "u"+strings.Repeat("x", i))
	}
	if got := union(many, []string{"root"}); !slices.Equal(got, []string{"root"}) {
		t.Errorf("past the bound: %q", got)
	}
	if got := union(many[:3], []string{"root", many[0]}); len(got) != 4 {
		t.Errorf("within it: %q", got)
	}
	p := Part{}
	for i := range maxCandidates + 1 {
		p.Files = append(p.Files, File{Path: "/etc/c" + strings.Repeat("x", i)})
	}
	p.Files = append(p.Files, File{Path: "/etc/in-force", InForce: true}, File{Path: "/etc/granted", Write: &Write{}})
	g, err := decodeGraph(fileRes("web/file/etc/mine", `{"node":"web","path":"/etc/mine","content":"x\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	publishFiles(g, "web", &p)
	var paths []string
	for _, f := range p.Files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{"/etc/in-force", "/etc/granted", "/etc/mine"}) {
		t.Errorf("kept %q", paths)
	}
}

var _ = reconcile.OutcomeDone
