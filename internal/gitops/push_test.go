package gitops

import (
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
)

// pushFixture follows the branch with sync = "push" (ADR 0014): nobody comments an approval.
func pushFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.e.Config.Sync = SyncPush
	return f
}

// push puts a commit with revision rev on the branch, attributed to login.
func (f *fixture) push(rev, login string) string {
	f.t.Helper()
	sha := f.commit(rev)
	f.gh.SetBranch("main", sha)
	f.gh.SetAuthor(sha, login)
	return sha
}

// A commit an author pushed to the branch is planned, approved as the push, and applied at the next
// poll: no pull request, no comment. Nothing is done twice.
func TestPushIsTheApproval(t *testing.T) {
	f := pushFixture(t)
	head := f.push("v1", "alice")
	f.once()
	if len(f.m.applied) != 0 || f.lastStatus(head).State != "pending" {
		t.Fatalf("after the first poll: applied %v, status %+v", f.m.applied, f.gh.Statuses(head))
	}
	f.once()
	if len(f.m.applied) != 1 || f.m.world != "v1" {
		t.Fatalf("after the second poll: applied %v", f.m.applied)
	}
	if s := f.lastStatus(head); s.State != "success" {
		t.Fatalf("the head's status: %+v", s)
	}
	planned := f.m.planned
	f.once()
	f.once()
	if len(f.m.applied) != 1 || f.m.planned != planned {
		t.Fatal("did something again for the same head")
	}
	// The next push is the next approval.
	f.push("v2", "alice")
	f.once()
	f.once()
	if len(f.m.applied) != 2 || f.m.world != "v2" {
		t.Fatalf("second push: applied %v", f.m.applied)
	}
}

// A head whose author is not an author (or is no GitHub user at all) is not planned, and nothing
// approves it.
func TestPushByNoAuthorIsNotPlanned(t *testing.T) {
	for _, login := range []string{"mallory", ""} {
		f := pushFixture(t)
		head := f.push("v1", login)
		f.once()
		f.once()
		if f.m.planned != 0 || len(f.m.applied) != 0 || len(f.gh.Statuses(head)) != 0 {
			t.Fatalf("author %q: planned %d, applied %v, statuses %+v", login, f.m.planned, f.m.applied, f.gh.Statuses(head))
		}
	}
}

// Without sync = "push", an author's direct push is not planned (the default: ADR 0010).
func TestDirectPushNeedsPushSync(t *testing.T) {
	f := newFixture(t)
	f.push("v1", "alice")
	f.once()
	if f.m.planned != 0 {
		t.Fatal("planned a head that is no trusted pull request's merge")
	}
}

// A plan that deletes is not approved by a push: the status tells the host's approval; once the host
// approves it, it is applied.
func TestPushDoesNotApproveADeletion(t *testing.T) {
	f := pushFixture(t)
	f.m.state = func(rev string) api.Planned {
		return api.Planned{Text: "- Item web", Changes: 1, Removals: 1}
	}
	head := f.push("v1", "alice")
	f.once()
	f.once()
	s := f.lastStatus(head)
	if len(f.m.applied) != 0 || s.State != "pending" || !strings.Contains(s.Description, "on the host, mamela approve sha256:") {
		t.Fatalf("applied %v, status %+v", f.m.applied, s)
	}
	if len(f.m.approved) != 0 {
		t.Fatalf("approved by the push: %v", f.m.approved)
	}
	var digest string
	for d := range f.m.plans {
		digest = d // the plan of the head (the newest: the poll plans the head again only while it waits)
	}
	f.m.approved[digest] = true // `mamela approve` on the host
	f.once()
	if len(f.m.applied) != 1 {
		t.Fatalf("not applied after the host's approval: %v", f.m.applied)
	}
}

// A blocked plan is not approved by a push.
func TestPushDoesNotApproveABlockedPlan(t *testing.T) {
	f := pushFixture(t)
	f.m.state = func(rev string) api.Planned { return api.Planned{Text: "blocked", Changes: 1, Blocked: 1} }
	f.push("v1", "alice")
	f.once()
	f.once()
	if len(f.m.approved) != 0 || len(f.m.applied) != 0 {
		t.Fatalf("approved %v, applied %v", f.m.approved, f.m.applied)
	}
}

func TestSyncConfig(t *testing.T) {
	c := Config{Repo: "me/infra", Branch: "main", Dirs: []string{"infra"}, TokenFile: "x", Authors: []string{"alice"}, MamelaSocket: "x", StateDB: "x"}
	if c.Check() == nil {
		t.Error("no approvers with the default sync")
	}
	c.Sync = SyncPush
	if err := c.Check(); err != nil {
		t.Errorf("push without approvers: %v", err)
	}
	c.Sync = "auto"
	if c.Check() == nil {
		t.Error("unknown sync accepted")
	}
}

// drifted makes the world differ from the head's declaration, and the resync interval pass.
func (f *fixture) drifted() {
	f.t.Helper()
	f.m.world = "changed under it"
	clock := f.e.Store.clock
	f.e.Store.clock = func() time.Time { return clock().Add(time.Hour) }
}

// A declaration in sync is planned again after the resync interval, to find drift: it is shown on
// the head, and without self_heal nothing undoes it by itself.
func TestDriftIsFoundAndWaitsForAnApproval(t *testing.T) {
	f := pushFixture(t)
	head := f.push("v1", "alice")
	f.once()
	f.once()
	f.once()
	if f.m.world != "v1" || f.lastStatus(head).State != "success" {
		t.Fatalf("not in sync: %v %+v", f.m.world, f.lastStatus(head))
	}
	planned := f.m.planned
	f.once()
	if f.m.planned != planned {
		t.Fatal("planned again before the resync interval")
	}

	f.drifted()
	f.once()
	if f.m.planned != planned+1 || f.lastStatus(head).State != "pending" {
		t.Fatalf("drift not found: planned %d, status %+v", f.m.planned, f.lastStatus(head))
	}
	f.once()
	f.once()
	if f.m.world != "changed under it" || len(f.m.applied) != 1 {
		t.Fatalf("a drift was undone without an approval: %v %v", f.m.world, f.m.applied)
	}
}

// With self_heal, the plan that undoes the drift is approved as the head's author and applied.
func TestSelfHealUndoesDrift(t *testing.T) {
	f := pushFixture(t)
	f.e.Config.SelfHeal = true
	head := f.push("v1", "alice")
	f.once()
	f.once()
	f.once()
	f.drifted()
	f.once() // finds the drift, approves the plan
	f.once() // applies it
	if f.m.world != "v1" || len(f.m.applied) != 2 {
		t.Fatalf("not healed: %v %v", f.m.world, f.m.applied)
	}
	if s := f.lastStatus(head); s.State != "success" {
		t.Fatalf("status: %+v", s)
	}
}

func TestResyncConfig(t *testing.T) {
	c := Config{Repo: "me/infra", Branch: "main", Dirs: []string{"infra"}, TokenFile: "x", Authors: []string{"alice"}, Approvers: []string{"alice"}, MamelaSocket: "x", StateDB: "x"}
	if d, err := c.resync(); err != nil || d != 10*time.Minute {
		t.Errorf("default: %v %v", d, err)
	}
	for resync, ok := range map[string]bool{"5m": true, "0": true, "30s": false, "soon": false} {
		c.Resync = resync
		if err := c.Check(); (err == nil) != ok {
			t.Errorf("resync %q: %v", resync, err)
		}
	}
	c.Resync, c.SelfHeal = "", true
	if c.Check() == nil {
		t.Error("self_heal without sync = push")
	}
	c.Sync = SyncPush
	if err := c.Check(); err != nil {
		t.Errorf("self_heal with push: %v", err)
	}
}
