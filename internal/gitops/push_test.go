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
	f.once() // planned, approved by the push, and applied: no waiting for the next poll
	if len(f.m.applied) != 1 || f.m.world != "v1" {
		t.Fatalf("after the first poll: applied %v, statuses %+v", f.m.applied, f.gh.Statuses(head))
	}
	if f.e.Busy() {
		t.Fatal("busy after converging")
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

// later makes time pass for mamelacd's store (not for the fakes).
func (f *fixture) later(d time.Duration) {
	f.t.Helper()
	clock := f.e.Store.clock
	f.e.Store.clock = func() time.Time { return clock().Add(d) }
}

// The phases of one change (each needs its plan approved and applied before the next can be
// planned) follow each other in one poll: the push is the approval of all of them.
func TestPhasesFollowEachOtherInOnePoll(t *testing.T) {
	f := pushFixture(t)
	f.m.state = func(rev string) api.Planned {
		if len(f.m.applied) < 3 {
			return api.Planned{Text: "phase", Changes: 1}
		}
		return api.Planned{Text: "converged"}
	}
	head := f.push("v1", "alice")
	f.once()
	if len(f.m.applied) != 3 || f.lastStatus(head).State != "success" || f.e.Busy() {
		t.Fatalf("applied %d, status %+v, busy %v", len(f.m.applied), f.lastStatus(head), f.e.Busy())
	}
}

// A chain that never ends is cut at maxChain per poll, and the next poll comes soon.
func TestChainIsBounded(t *testing.T) {
	f := pushFixture(t)
	f.m.state = func(rev string) api.Planned { return api.Planned{Text: "again", Changes: 1} }
	f.push("v1", "alice")
	f.once()
	if n := len(f.m.applied); n < 2 || n > maxChain || !f.e.Busy() {
		t.Fatalf("applied %d (max %d), busy %v", n, maxChain, f.e.Busy())
	}
}

// A declaration that waits for what it depends on makes the next poll come sooner; one that waits
// for a person's approval does not.
func TestBusyWhileWaiting(t *testing.T) {
	f := pushFixture(t)
	f.m.state = func(rev string) api.Planned { return api.Planned{Text: "waiting", Waiting: 1} }
	f.push("v1", "alice")
	f.once()
	if !f.e.Busy() {
		t.Fatal("not busy while waiting")
	}
	f = pushFixture(t)
	f.m.state = func(rev string) api.Planned { return api.Planned{Text: "del", Changes: 1, Removals: 1} }
	f.push("v1", "alice")
	f.once()
	if f.e.Busy() {
		t.Fatal("busy while waiting for the host's approval")
	}
	var c Config
	if c.ActiveIntervalOrDefault() != 10*time.Second {
		t.Fatalf("default active interval %v", c.ActiveIntervalOrDefault())
	}
}

// A failed apply is tried again after the retry interval, up to retry_max times; the status says so.
func TestFailedApplyIsRetried(t *testing.T) {
	f := pushFixture(t)
	f.e.Config.Retry = "1m"
	f.m.applyFails = 2
	head := f.push("v1", "alice")
	f.once()
	if s := f.lastStatus(head); s.State != "failure" || !strings.Contains(s.Description, "(retry 1/3 in 1m0s)") || len(s.Description) > 140 {
		t.Fatalf("after the first failure: %+v", s)
	}
	planned := f.m.planned
	f.once()
	if f.m.planned != planned {
		t.Fatal("tried again before the retry interval")
	}
	f.later(2 * time.Minute)
	f.once() // the second failure
	if s := f.lastStatus(head); s.State != "failure" || !strings.Contains(s.Description, "(retry 2/3 in 1m0s)") {
		t.Fatalf("after the second failure: %+v", s)
	}
	f.later(2 * time.Minute)
	f.once() // succeeds
	if f.m.world != "v1" || f.lastStatus(head).State != "success" {
		t.Fatalf("not retried to success: world %q, status %+v", f.m.world, f.lastStatus(head))
	}
}

// After retry_max retries a failed head is left alone until another commit; that commit starts over.
func TestRetryGivesUp(t *testing.T) {
	f := pushFixture(t)
	f.e.Config.Retry = "1m"
	one := 1
	f.e.Config.RetryMax = &one
	f.m.applyFails = 100
	head := f.push("v1", "alice")
	f.once()
	f.later(2 * time.Minute)
	f.once()
	if s := f.lastStatus(head); !strings.Contains(s.Description, "gave up after 2 tries") {
		t.Fatalf("status: %+v", s)
	}
	planned := f.m.planned
	f.later(2 * time.Minute)
	f.once()
	f.once()
	if f.m.planned != planned {
		t.Fatal("tried again after giving up")
	}
	f.m.applyFails = 0
	f.push("v2", "alice")
	f.once()
	if f.m.world != "v2" {
		t.Fatalf("a new commit did not start over: %q", f.m.world)
	}
}

// retry = "0" never tries again.
func TestRetryCanBeOff(t *testing.T) {
	f := pushFixture(t)
	f.e.Config.Retry = "0"
	f.m.applyFails = 1
	head := f.push("v1", "alice")
	f.once()
	if s := f.lastStatus(head); strings.Contains(s.Description, "retry") {
		t.Fatalf("status: %+v", s)
	}
	planned := f.m.planned
	f.later(time.Hour)
	f.once()
	if f.m.planned != planned {
		t.Fatal("retried with retry = 0")
	}
}

func TestActiveIntervalAndRetryConfig(t *testing.T) {
	c := Config{Repo: "me/infra", Branch: "main", Dirs: []string{"infra"}, TokenFile: "x", Authors: []string{"alice"}, Approvers: []string{"alice"}, MamelaSocket: "x", StateDB: "x"}
	for _, tc := range []struct {
		name string
		set  func(*Config)
		ok   bool
	}{
		{"active 5s", func(c *Config) { c.ActiveInterval = "5s" }, true},
		{"active 2s", func(c *Config) { c.ActiveInterval = "2s" }, false},
		{"active more than interval", func(c *Config) { c.ActiveInterval = "2m" }, false},
		{"retry 1m", func(c *Config) { c.Retry = "1m" }, true},
		{"retry 10s", func(c *Config) { c.Retry = "10s" }, false},
		{"retry 0", func(c *Config) { c.Retry = "0" }, true},
		{"retry_max 21", func(c *Config) { n := 21; c.RetryMax = &n }, false},
		{"retry_max 0", func(c *Config) { n := 0; c.RetryMax = &n }, true},
	} {
		cc := c
		tc.set(&cc)
		if err := cc.Check(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// A plan that could not be made sets a failure status; the plan that comes next (in sync as the one
// before it was) replaces it, instead of leaving "could not be planned" on a head that is in sync.
func TestFailedPlanStatusIsReplaced(t *testing.T) {
	f := pushFixture(t)
	head := f.push("v1", "alice")
	f.once() // converged: in sync
	f.m.planErr = "an observation failed"
	f.later(time.Hour) // the resync interval has passed
	f.once()
	if s := f.lastStatus(head); s.State != "failure" || s.Description != "could not be planned" {
		t.Fatalf("after the failed plan: %+v", s)
	}
	f.m.planErr = ""
	f.once()
	if s := f.lastStatus(head); s.State != "success" {
		t.Fatalf("the failure was left on a head that is in sync: %+v", s)
	}
}
