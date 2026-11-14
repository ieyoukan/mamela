package gitops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
	"github.com/ieyoukan/mamela/internal/gitops/fakegithub"
)

// fakeMamela is the Control Plane's API. A declaration's revision is the content of <dir>/rev in
// the tree; the world is the revision last applied. A plan has one change while the world is not
// its revision (state overrides that).
type fakeMamela struct {
	files     map[string]map[string]string // tree -> path -> content
	approved  map[string]bool
	plans     map[string]string // digest -> revision
	world     string
	applied   []string // revisions applied, in order
	planned   int
	state     func(rev string) api.Planned // optional
	stale     bool                         // the next apply finds the world changed
	down      bool                         // the API does not answer
	planErr   string                       // planning fails (an observation that fails)
	attempts  int                          // plans asked for
	nextPlans int
}

func newFakeMamela() *fakeMamela {
	return &fakeMamela{files: map[string]map[string]string{}, approved: map[string]bool{}, plans: map[string]string{}}
}

var errDown = errors.New("dial unix /run/mamela/api.sock: connect: no such file or directory")

func (m *fakeMamela) Tree(_ context.Context, tarball []byte) (string, error) {
	if m.down {
		return "", errDown
	}
	zr, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return "", &api.Error{Status: 422, Message: err.Error()}
	}
	tr := tar.NewReader(zr)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		_, rest, _ := strings.Cut(h.Name, "/")
		b, _ := io.ReadAll(tr)
		files[rest] = string(b)
	}
	name := fmt.Sprintf("sha256:%x", sha256.Sum256(tarball))
	m.files[name] = files
	return name, nil
}

func (m *fakeMamela) Revision(_ context.Context, tree, dir string) (string, error) {
	if m.down {
		return "", errDown
	}
	rev, ok := m.files[tree][dir+"/rev"]
	if !ok {
		return "", &api.Error{Status: 422, Message: "MAM-EVAL: no declaration in " + dir}
	}
	return rev, nil
}

func (m *fakeMamela) Plan(ctx context.Context, tree, dir string) (api.Planned, error) {
	rev, err := m.Revision(ctx, tree, dir)
	if err != nil {
		return api.Planned{}, err
	}
	m.attempts++
	if m.planErr != "" {
		return api.Planned{}, &api.Error{Status: 422, Message: m.planErr}
	}
	m.planned++
	m.nextPlans++
	p := api.Planned{Revision: rev, Text: "~ Item web: " + m.world + " -> " + rev}
	if m.state != nil {
		p = m.state(rev)
		p.Revision = rev
	} else if m.world != rev {
		p.Changes = 1
	}
	p.Digest = fmt.Sprintf("sha256:%064x", m.nextPlans)
	m.plans[p.Digest] = rev
	return p, nil
}

func (m *fakeMamela) Approve(_ context.Context, digest, by string, _ time.Duration) error {
	if m.down {
		return errDown
	}
	if _, ok := m.plans[digest]; !ok {
		return &api.Error{Status: 422, Message: "no such plan"}
	}
	if !strings.HasPrefix(by, "github:") {
		return fmt.Errorf("by %q", by)
	}
	m.approved[digest] = true
	return nil
}

func (m *fakeMamela) Approved(_ context.Context, digest string) (bool, error) {
	return m.approved[digest], nil
}

func (m *fakeMamela) Apply(_ context.Context, digest string) (api.Applied, error) {
	if !m.approved[digest] {
		return api.Applied{Text: "MAM-REC-0001: not approved"}, nil
	}
	if m.stale {
		m.stale = false
		return api.Applied{Text: "stopped: MAM-REC-0002: the world changed", Stale: true}, nil
	}
	m.world = m.plans[digest]
	m.applied = append(m.applied, m.world)
	return api.Applied{OK: true, Text: "done"}, nil
}

type fixture struct {
	t  *testing.T
	e  *Engine
	m  *fakeMamela
	gh *fakegithub.GitHub
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	st, err := OpenStore(ctx, filepath.Join(t.TempDir(), "mamelacd.db"), func() time.Time { now = now.Add(time.Second); return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gh := fakegithub.New("me/infra", "token")
	server := httptest.NewServer(gh)
	t.Cleanup(server.Close)
	m := newFakeMamela()
	e := &Engine{Store: st, Mamela: m, GitHub: &GitHub{API: server.URL, Repo: "me/infra", Token: "token"},
		Config: Config{Repo: "me/infra", Branch: "main", Dirs: []string{"infra"}, Authors: []string{"alice"}, Approvers: []string{"alice", "bob"},
			MamelaSocket: "x", StateDB: "x", TokenFile: "x"}}
	f := &fixture{t: t, e: e, m: m, gh: gh}
	gh.SetBranch("main", f.commit("v0")) // a head no pull request made: not planned
	return f
}

// commit makes a commit whose declaration infra has the revision rev.
func (f *fixture) commit(rev string) string {
	f.t.Helper()
	dir := f.t.TempDir()
	os.MkdirAll(filepath.Join(dir, "infra"), 0o755)
	os.WriteFile(filepath.Join(dir, "infra/rev"), []byte(rev), 0o644)
	os.WriteFile(filepath.Join(dir, "infra/nonce"), []byte(fmt.Sprint(time.Now().UnixNano())), 0o644)
	sha, err := f.gh.AddTree(dir)
	if err != nil {
		f.t.Fatal(err)
	}
	return sha
}

func (f *fixture) once() {
	f.t.Helper()
	if err := f.e.Once(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// lastPlan is the digest of the last plan Mamela commented on a pull request.
func (f *fixture) lastPlan(pr int) string {
	f.t.Helper()
	cs := f.gh.Comments(pr)
	for i := len(cs) - 1; i >= 0; i-- {
		if _, d, ok := strings.Cut(cs[i].Body, "/mamela approve "); ok && cs[i].Bot {
			return d[:71]
		}
	}
	f.t.Fatalf("#%d: no plan commented: %+v", pr, cs)
	return ""
}

func (f *fixture) said(pr int, want string) bool {
	for _, c := range f.gh.Comments(pr) {
		if c.Bot && strings.Contains(c.Body, want) {
			return true
		}
	}
	return false
}

func (f *fixture) lastStatus(sha string) fakegithub.Status {
	s := f.gh.Statuses(sha)
	if len(s) == 0 {
		f.t.Fatalf("no status of %s", sha)
	}
	return s[len(s)-1]
}

// A pull request's plan, approved at its head, is applied once the merge is the head; the head is
// then planned again and found in sync; nothing is done twice.
func TestApprovedAtTheHead(t *testing.T) {
	f := newFixture(t)
	head := f.commit("v1")
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: head})
	f.once()
	if !f.said(1, "It is applied when the pull request is merged") || f.lastStatus(head).State != "pending" {
		t.Fatalf("the plan: %+v", f.gh.Comments(1))
	}
	f.gh.Say(1, "bob", "/mamela approve "+f.lastPlan(1))
	f.once()
	if !f.said(1, "Approved `") || len(f.m.applied) != 0 {
		t.Fatal("applied before the merge")
	}
	merge := f.commit("v1")
	f.gh.Merge(1, merge)
	f.once()
	if len(f.m.applied) != 1 || f.m.world != "v1" || !f.said(1, "### Mamela apply: `infra` applied") {
		t.Fatalf("after the merge: %v %+v", f.m.applied, f.gh.Comments(1))
	}
	if s := f.lastStatus(merge); s.State != "success" {
		t.Fatalf("the head's status: %+v", s)
	}
	n, planned := len(f.gh.Comments(1)), f.m.planned
	f.once()
	f.once()
	if len(f.gh.Comments(1)) != n || len(f.m.applied) != 1 || f.m.planned != planned {
		t.Fatal("did something again for the same head")
	}
}

// Only the declaration at the head is applied. A merge that is no longer the head (another was
// merged after it) is never applied, even approved: it would undo the later one.
func TestOnlyTheHeadIsApplied(t *testing.T) {
	f := newFixture(t)
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: f.commit("v1")})
	f.gh.Open(fakegithub.PR{Number: 2, Author: "alice", Base: "main", HeadSHA: f.commit("v2")})
	f.once()
	f.gh.Say(1, "alice", "/mamela approve "+f.lastPlan(1))
	f.once()
	f.gh.Merge(1, f.commit("v1"))
	f.gh.Merge(2, f.commit("v2")) // before the next poll: the head is #2's
	f.once()
	if len(f.m.applied) != 0 {
		t.Fatalf("applied a merge that is not the head: %v", f.m.applied)
	}
	if !f.said(2, "head of `main`") {
		t.Fatalf("#2: %+v", f.gh.Comments(2))
	}
	// #2's head plan, approved, is applied.
	f.gh.Say(2, "alice", "/mamela approve "+f.lastPlan(2))
	f.once()
	if len(f.m.applied) != 1 || f.m.applied[0] != "v2" {
		t.Fatalf("applied: %v", f.m.applied)
	}
	// #1's plan, approved again, is still not the head's declaration.
	f.gh.Say(1, "bob", "/mamela approve "+f.lastPlan(1))
	f.once()
	if len(f.m.applied) != 1 || f.m.world != "v2" {
		t.Fatalf("applied an old merge: %v", f.m.applied)
	}
}

// Main moved under an approved pull request: the merge is another declaration; the head is
// planned and applied once that plan is approved.
func TestMergedDeclarationDiffers(t *testing.T) {
	f := newFixture(t)
	f.gh.Open(fakegithub.PR{Number: 4, Author: "alice", Base: "main", HeadSHA: f.commit("v2")})
	f.once()
	f.gh.Say(4, "alice", "/mamela approve "+f.lastPlan(4))
	f.once()
	f.gh.Merge(4, f.commit("v3"))
	f.once()
	if !f.said(4, "is not the one planned") || len(f.m.applied) != 0 {
		t.Fatalf("a moved main: %+v %v", f.gh.Comments(4), f.m.applied)
	}
	f.gh.Say(4, "alice", "/mamela approve "+f.lastPlan(4))
	f.once()
	if len(f.m.applied) != 1 || f.m.world != "v3" {
		t.Fatalf("after approving the head: %v", f.m.applied)
	}
	f.gh.Say(4, "alice", "/mamela approve "+f.lastPlan(4))
	f.once()
	if !f.said(4, "it was applied already") {
		t.Fatalf("approving an applied plan: %+v", f.gh.Comments(4))
	}
}

// A head that no trusted author's pull request made (a push, another author's merge) is not planned.
func TestUntrustedHeadIsNotPlanned(t *testing.T) {
	f := newFixture(t)
	f.gh.Open(fakegithub.PR{Number: 3, Author: "mallory", Base: "main", HeadSHA: f.commit("evil")})
	f.gh.Merge(3, f.commit("evil"))
	f.once()
	f.gh.SetBranch("main", f.commit("pushed"))
	f.once()
	if f.m.planned != 0 || len(f.gh.Comments(3)) != 0 {
		t.Fatalf("planned an untrusted head: %d %+v", f.m.planned, f.gh.Comments(3))
	}
}

// A plan that waits or is blocked is never reported in sync, is planned again each poll without
// being commented again, and its next phase needs its own approval.
func TestWaitingIsPlannedAgain(t *testing.T) {
	for _, unresolved := range []api.Planned{{Waiting: 1}, {Blocked: 1}} {
		f := newFixture(t)
		f.m.state = func(string) api.Planned { return unresolved }
		f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: f.commit("v1")})
		merge := f.commit("v1")
		f.gh.Merge(1, merge)
		f.once()
		for _, s := range f.gh.Statuses(merge) {
			if s.State == "success" {
				t.Fatalf("%+v: reported success", unresolved)
			}
		}
		if f.said(1, "in sync") {
			t.Fatalf("%+v: said in sync", unresolved)
		}
		n, planned := len(f.gh.Comments(1)), f.m.planned
		f.once()
		if f.m.planned == planned || len(f.gh.Comments(1)) != n {
			t.Fatalf("%+v: not planned again, or commented again", unresolved)
		}
		f.m.state = nil // what it waited for is there: one change
		f.once()
		f.gh.Say(1, "alice", "/mamela approve "+f.lastPlan(1))
		f.once()
		if len(f.m.applied) != 1 || f.lastStatus(merge).State != "success" {
			t.Fatalf("%+v: next phase: %v %+v", unresolved, f.m.applied, f.gh.Statuses(merge))
		}
	}
}

// The world was not what an approved plan assumed: nothing was applied; the head is planned
// again, and the new plan needs its own approval.
func TestStalePlanIsPlannedAgain(t *testing.T) {
	f := newFixture(t)
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: f.commit("v1")})
	f.once()
	first := f.lastPlan(1)
	f.gh.Say(1, "alice", "/mamela approve "+first)
	f.m.stale = true
	f.gh.Merge(1, f.commit("v1"))
	f.once()
	if len(f.m.applied) != 0 || !f.said(1, "stale") || f.lastPlan(1) == first {
		t.Fatalf("stale: %v %+v", f.m.applied, f.gh.Comments(1))
	}
	f.once()
	if len(f.m.applied) != 0 {
		t.Fatal("applied without approving the new plan")
	}
	f.gh.Say(1, "alice", "/mamela approve "+f.lastPlan(1))
	f.once()
	if len(f.m.applied) != 1 {
		t.Fatalf("applied: %v", f.m.applied)
	}
}

// When the Control Plane's API does not answer, nothing is reported as a problem of the
// declaration, and the next poll tries again.
func TestAPIDown(t *testing.T) {
	f := newFixture(t)
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: f.commit("v1")})
	f.m.down = true
	if err := f.e.Once(context.Background()); err == nil {
		t.Fatal("no error while the API is down")
	}
	if len(f.gh.Comments(1)) != 0 {
		t.Fatalf("commented: %+v", f.gh.Comments(1))
	}
	f.m.down = false
	f.once()
	if !f.said(1, "/mamela approve") {
		t.Fatal("not planned once the API answered")
	}
}

// A declaration that cannot be evaluated is reported once, on the pull request.
func TestNotPlannable(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "README"), []byte("no declaration"), 0o644)
	sha, _ := f.gh.AddTree(dir)
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: sha})
	f.once()
	f.once()
	n := 0
	for _, c := range f.gh.Comments(1) {
		if strings.Contains(c.Body, "could not be planned") {
			n++
		}
	}
	if n != 1 || f.lastStatus(sha).State != "failure" {
		t.Fatalf("reported %d times: %+v", n, f.gh.Comments(1))
	}
}

// An approval comment written while the Control Plane's API does not answer is not lost.
func TestApprovalWhileAPIDown(t *testing.T) {
	f := newFixture(t)
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: f.commit("v1")})
	f.once()
	f.gh.Say(1, "alice", "/mamela approve "+f.lastPlan(1))
	f.m.down = true
	if err := f.e.Once(context.Background()); err == nil {
		t.Fatal("no error while the API is down")
	}
	if f.said(1, "did not approve") || len(f.m.approved) != 0 {
		t.Fatalf("decided while down: %+v", f.gh.Comments(1))
	}
	f.m.down = false
	f.once()
	if !f.said(1, "Approved `") || len(f.m.approved) != 1 {
		t.Fatalf("the approval was lost: %+v", f.gh.Comments(1))
	}
}

// Planning the head can fail for a while (an observation fails): it is planned again each poll,
// and the failure is commented once.
func TestHeadPlanFailureIsRetried(t *testing.T) {
	f := newFixture(t)
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: f.commit("v1")})
	f.once()
	f.m.planErr = "MAM-OBS: the API of provider kv did not answer"
	f.gh.Merge(1, f.commit("v1"))
	f.once()
	f.once()
	n := 0
	for _, c := range f.gh.Comments(1) {
		if strings.Contains(c.Body, "could not be planned") {
			n++
		}
	}
	if n != 1 || f.m.attempts < 3 {
		t.Fatalf("reported %d times, %d attempts", n, f.m.attempts)
	}
	f.m.planErr = ""
	f.once()
	if !f.said(1, "head of `main`") {
		t.Fatalf("not planned again: %+v", f.gh.Comments(1))
	}
}

func TestConfigCheck(t *testing.T) {
	ok := Config{Repo: "me/infra", Branch: "main", Dirs: []string{"infra", "lab/web"}, TokenFile: "t", Authors: []string{"a"}, Approvers: []string{"a"},
		MamelaSocket: "/run/mamela/api.sock", StateDB: "/var/lib/mamelacd/state.db"}
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*Config){
		"repo":      func(c *Config) { c.Repo = "infra" },
		"dirs":      func(c *Config) { c.Dirs = nil },
		"escape":    func(c *Config) { c.Dirs = []string{"../x"} },
		"absolute":  func(c *Config) { c.Dirs = []string{"/etc"} },
		"dot":       func(c *Config) { c.Dirs = []string{"."} },
		"unclean":   func(c *Config) { c.Dirs = []string{"a/../b"} },
		"token":     func(c *Config) { c.TokenFile = "" },
		"approvers": func(c *Config) { c.Approvers = nil },
		"authors":   func(c *Config) { c.Authors = nil },
		"socket":    func(c *Config) { c.MamelaSocket = "" },
		"state":     func(c *Config) { c.StateDB = "" },
		"ttl":       func(c *Config) { c.ApprovalTTL = "-1h" },
		"long ttl":  func(c *Config) { c.ApprovalTTL = "720h" },
		"interval":  func(c *Config) { c.Interval = "1s" },
	} {
		c := ok
		f(&c)
		if err := c.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if !strings.Contains(clip(strings.Repeat("a", 60000)), "(cut)") || strings.Contains(clip("```x```"), "```") {
		t.Error("clip")
	}
}
