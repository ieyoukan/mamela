package linux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
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

// A plan's observation targets are bounded: a list past maxTargets keeps the declaration in force's
// values, then the planned declaration's, then the others', and a kind's entries are never more than
// mamelet observes on a node: those in force or granted first, then the planned declaration's, then
// what other plans added, each by name.
func TestObservationTargetsAreBounded(t *testing.T) {
	var many []string
	for i := range maxTargets {
		many = append(many, "u"+strings.Repeat("x", i))
	}
	if got := union(nil, many, []string{"root"}); len(got) != maxTargets || !slices.Contains(got, "root") || slices.Contains(got, many[maxTargets-1]) {
		t.Errorf("past the bound, nothing in force: %q", got)
	}
	if got := union(many[:2], many, []string{"root"}); len(got) != maxTargets || !slices.Contains(got, "root") || !slices.Contains(got, many[0]) || !slices.Contains(got, many[1]) {
		t.Errorf("past the bound, two in force: %q", got)
	}
	if got := union(nil, many[:3], []string{"root", many[0]}); len(got) != 4 {
		t.Errorf("within it: %q", got)
	}
	full := func() Part {
		p := Part{}
		for i := range MaxNodeFiles - 6 {
			p.Files = append(p.Files, File{Path: fmt.Sprintf("/etc/f%03d", i), InForce: true})
		}
		p.Files = append(p.Files, File{Path: "/etc/granted", Write: &Write{}})
		for i := range 10 {
			p.Files = append(p.Files, File{Path: fmt.Sprintf("/etc/left%02d", i)})
		}
		return p
	}
	declare := func(n int) []string {
		var rs []string
		for i := range n {
			rs = append(rs, fileRes(fmt.Sprintf("web/file/etc/mine%d", i), fmt.Sprintf(`{"node":"web","path":"/etc/mine%d","content":"x\n"}`, i)))
		}
		return rs
	}
	kept := func(p Part, prefix string) []string {
		var out []string
		for _, f := range p.Files {
			if strings.HasPrefix(f.Path, prefix) {
				out = append(out, f.Path)
			}
		}
		slices.Sort(out)
		return out
	}
	for _, c := range []struct {
		mine       int
		wantMine   []string
		wantOthers []string
	}{
		{8, []string{"/etc/mine0", "/etc/mine1", "/etc/mine2", "/etc/mine3", "/etc/mine4"}, nil},
		{2, []string{"/etc/mine0", "/etc/mine1"}, []string{"/etc/left00", "/etc/left01", "/etc/left02"}},
	} {
		g, err := decodeGraph(declare(c.mine)...)
		if err != nil {
			t.Fatal(err)
		}
		p := full()
		publishFiles(g, "web", &p)
		if len(p.Files) != MaxNodeFiles || len(kept(p, "/etc/f")) != MaxNodeFiles-6 || p.file("/etc/granted") == nil {
			t.Errorf("%d declared: %d files, those in force and granted are not all kept", c.mine, len(p.Files))
		}
		if got := kept(p, "/etc/mine"); !slices.Equal(got, c.wantMine) {
			t.Errorf("%d declared: kept %q of them, want %q", c.mine, got, c.wantMine)
		}
		if got := kept(p, "/etc/left"); !slices.Equal(got, c.wantOthers) {
			t.Errorf("%d declared: kept %q of other plans, want %q", c.mine, got, c.wantOthers)
		}
	}
}

// A user's groups in force are never taken out by a declaration nobody approved: a pull request that
// adds 7 groups to the 10 of the declaration in force gets those that fit (6), and main's user, in
// sync, does not wait again for its own (it did, in turn with the pull request's plans). Plans of the
// two in turn do not change the revision; a third declaration takes the room of the second, never of
// the one in force; and the next apply of a declaration makes its own the ones kept.
func TestTheTargetsInForceAreKeptPastTheBound(t *testing.T) {
	user := func(groups ...string) string {
		return fmt.Sprintf(`{"kind":"NodeUser","name":"web/user/app","spec":{"node":"web","name":"app","groups":[%s]}}`, `"`+strings.Join(groups, `","`)+`"`)
	}
	names := func(prefix string, n int) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("%s%02d", prefix, i))
		}
		return out
	}
	graphOf := func(groups []string) *graph.Graph {
		g, err := decodeGraph(user(groups...))
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	mainG, pr, pr2 := graphOf(names("g", 10)), graphOf(names("p", 7)), graphOf(names("q", 5))
	p := Part{}
	pruneUsers(mainG, "web", &p) // main applied: in force
	publishUsers(pr, "web", &p)
	got := p.user("app").Groups
	for _, g := range names("g", 10) {
		if !slices.Contains(got, g) {
			t.Fatalf("the pull request took %s of the declaration in force out: %q", g, got)
		}
	}
	if len(got) != maxTargets || !slices.Contains(got, "p05") || slices.Contains(got, "p06") {
		t.Errorf("the pull request's that fit: %q", got)
	}
	// main and the pull request planned in turn: the part does not change.
	before := string(encode(p))
	for _, g := range []*graph.Graph{mainG, pr, mainG, pr} {
		publishUsers(g, "web", &p)
		if after := string(encode(p)); after != before {
			t.Fatalf("the part changed:\n%s\n%s", before, after)
		}
	}
	// Another pull request: its own fit after main's, in place of the first's.
	publishUsers(pr2, "web", &p)
	got = p.user("app").Groups
	for _, g := range slices.Concat(names("g", 10), names("q", 5)) {
		if !slices.Contains(got, g) {
			t.Fatalf("%s is not kept: %q", g, got)
		}
	}
	// What mamelet gets has no bookkeeping of the Control Plane.
	rev := &noderev.Revision{}
	setPart(rev, p)
	out, err := partForMamelet(rev.Ext[Name], time.Now())
	if err != nil || strings.Contains(string(out), `"held"`) {
		t.Errorf("mamelet gets %s (%v)", out, err)
	}
	// The pull request applied: its groups are the ones in force.
	pruneUsers(pr, "web", &p)
	if got := p.user("app"); !slices.Equal(got.Groups, names("p", 7)) || !slices.Equal(got.Held.Groups, names("p", 7)) {
		t.Errorf("after the apply: %+v", got)
	}

	// The URIs and suites of a source: the same.
	held := AptWhere{URIs: []string{"https://example.com/main"}, Suites: []string{"stable"}}
	cur := []AptWhere{held}
	for i := range maxTargets - 1 {
		cur = addWhere(&held, cur, AptWhere{URIs: []string{fmt.Sprintf("https://example.com/pr%02d", i)}, Suites: []string{"stable"}})
	}
	w := AptWhere{URIs: []string{"https://example.com/zz"}, Suites: []string{"stable"}}
	cur = addWhere(&held, cur, w)
	if len(cur) != maxTargets || !slices.ContainsFunc(cur, held.same) || !slices.ContainsFunc(cur, w.same) {
		t.Errorf("the URIs and suites past the bound: %+v", cur)
	}
}

var _ = reconcile.OutcomeDone
