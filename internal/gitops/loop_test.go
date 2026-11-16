package gitops

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
	"github.com/ieyoukan/mamela/internal/gitops/fakegithub"
)

// pollsOf runs n polls, ignoring their errors (a poll can fail and the next one goes on).
func (f *fixture) pollsOf(n int) {
	f.t.Helper()
	for range n {
		f.e.Once(context.Background())
	}
}

// statuses of a commit as one string, for a message.
func (f *fixture) statusLines(sha string) string {
	var b strings.Builder
	all := f.gh.Statuses(sha)
	fmt.Fprintf(&b, "\n  (%d statuses; the last ones)", len(all))
	for _, s := range all[max(0, len(all)-4):] {
		fmt.Fprintf(&b, "\n  %s %s: %s", s.State, s.Context, s.Description)
	}
	return b.String()
}

// A7a: the world changes under every apply (stale), and sync = "push" approves each new plan. That
// must stop after retry_max + 1 applies in a row, however many polls follow, and say why: on the
// commit's status, in a commit comment (no pull request) and in the log. Before: 12 applies per
// poll, for ever.
func TestAlwaysStaleStopsAcrossPolls(t *testing.T) {
	f := pushFixture(t)
	var logs []string
	f.e.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	f.m.alwaysStale = true
	head := f.push("v1", "alice")
	f.pollsOf(5)
	if want := f.e.Config.retryMax() + 1; f.m.applyCalls != want {
		t.Fatalf("%d applies, want %d (retry_max + 1)%s", f.m.applyCalls, want, f.statusLines(head))
	}
	s := f.lastStatus(head)
	if s.State != "failure" || !strings.Contains(s.Description, "MAM-GIT-0101") || len(s.Description) > 140 {
		t.Fatalf("the status does not say why it stopped: %+v", s)
	}
	cc := f.gh.CommitComments(head)
	if len(cc) != 1 || !strings.Contains(cc[0].Body, "MAM-GIT-0101") || !strings.Contains(cc[0].Body, "how") && !strings.Contains(cc[0].Body, "push") {
		t.Fatalf("the commit comment: %+v", cc)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "MAM-GIT-0101") {
		t.Fatalf("the log does not say why it stopped: %v", logs)
	}
	if f.e.Busy() {
		t.Fatal("still busy after giving up")
	}
	// Another commit starts over.
	f.m.alwaysStale = false
	f.push("v2", "alice")
	f.once()
	if f.m.world != "v2" {
		t.Fatalf("a new commit did not start over: %q", f.m.world)
	}
}

// retry_max counts here too: with retry_max = 0 the first stale apply is the last.
func TestStaleLimitFollowsRetryMax(t *testing.T) {
	f := pushFixture(t)
	zero := 0
	f.e.Config.RetryMax = &zero
	f.m.alwaysStale = true
	f.push("v1", "alice")
	f.pollsOf(3)
	if f.m.applyCalls != 1 {
		t.Fatalf("%d applies with retry_max = 0", f.m.applyCalls)
	}
}

// The same change applied again and again, successfully, while the plan still shows it (the world
// does not show the change): it is not applied for ever either.
func TestAppliedButNeverConvergingStops(t *testing.T) {
	f := pushFixture(t)
	f.m.state = func(rev string) api.Planned { return api.Planned{Text: "~ Item web: x -> y", Changes: 1} }
	head := f.push("v1", "alice")
	f.pollsOf(4)
	if want := f.e.Config.retryMax() + 1; f.m.applyCalls != want {
		t.Fatalf("%d applies, want %d%s", f.m.applyCalls, want, f.statusLines(head))
	}
	if s := f.lastStatus(head); !strings.Contains(s.Description, "MAM-GIT-0101") {
		t.Fatalf("status: %+v", s)
	}
}

// A phase after phase of different changes is not a repeat: the chain goes on.
func TestDifferentPhasesAreNotARepeat(t *testing.T) {
	f := pushFixture(t)
	f.m.state = func(rev string) api.Planned {
		if f.m.applyCalls < 8 {
			return api.Planned{Text: fmt.Sprintf("phase %d", f.m.applyCalls), Changes: 1}
		}
		return api.Planned{Text: "done"}
	}
	head := f.push("v1", "alice")
	f.pollsOf(3)
	if f.m.applyCalls != 8 || f.lastStatus(head).State != "success" {
		t.Fatalf("%d applies%s", f.m.applyCalls, f.statusLines(head))
	}
}

// With approval comments a person approves each plan, so a stale apply is planned again and waits
// for the person (as before): nothing loops.
func TestStaleWithApprovalCommentsWaits(t *testing.T) {
	f := newFixture(t)
	f.m.alwaysStale = true
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: f.commit("v1")})
	f.once()
	f.gh.Say(1, "alice", "/mamela approve "+f.lastPlan(1))
	f.gh.Merge(1, f.commit("v1"))
	f.pollsOf(4)
	if f.m.applyCalls != 1 {
		t.Fatalf("%d applies without a new approval", f.m.applyCalls)
	}
}

// A7b: the apply is done, and then the result comment fails (a 502). The head must not stay
// "pending" with a status that says "applying" for ever: the next poll goes on (plans again, and
// the comment that failed is posted then).
func TestFailedReportAfterApplyIsRedone(t *testing.T) {
	f := newFixture(t)
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: f.commit("v1")})
	f.once()
	f.gh.Say(1, "alice", "/mamela approve "+f.lastPlan(1))
	f.once()
	merge := f.commit("v1")
	f.gh.Merge(1, merge)
	f.gh.Fail("POST", "/issues/", 502, 1, "Bad Gateway", nil)
	f.e.Once(context.Background()) // applies; the comment fails
	if f.m.world != "v1" {
		t.Fatal("not applied")
	}
	f.pollsOf(2)
	s := f.lastStatus(merge)
	if s.State != "success" || strings.Contains(s.Description, "applying") {
		t.Fatalf("the head is stuck: %+v%s", s, f.statusLines(merge))
	}
	n := 0
	for _, c := range f.gh.Comments(1) {
		if c.Bot && strings.Contains(c.Body, "### Mamela apply: `infra` applied") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the result comment was posted %d times: %+v", n, f.gh.Comments(1))
	}
	if f.m.applyCalls != 1 {
		t.Fatalf("applied %d times", f.m.applyCalls)
	}
}

// A7b: the plan after the apply fails (the Control Plane's API stops answering): the next poll
// plans again, though the apply is recorded.
func TestFailedReplanAfterApplyIsRedone(t *testing.T) {
	f := pushFixture(t)
	f.m.downAfterApply = true
	head := f.push("v1", "alice")
	if err := f.e.Once(context.Background()); err == nil {
		t.Fatal("no error while the API is down")
	}
	if f.m.world != "v1" {
		t.Fatal("not applied")
	}
	if s := f.lastStatus(head); strings.HasPrefix(s.Description, "applying") {
		t.Fatalf("the status says applying though the apply is done: %+v", s)
	}
	f.m.down = false
	f.pollsOf(2)
	if s := f.lastStatus(head); s.State != "success" {
		t.Fatalf("not planned again: %+v%s", s, f.statusLines(head))
	}
	if f.m.applyCalls != 1 {
		t.Fatalf("applied %d times", f.m.applyCalls)
	}
}

// A7c: a poll reads the two lists and the head, not every trusted pull request's comments. Closed
// pull requests are looked at again only when they changed.
func TestPollRequestsDoNotGrowWithPullRequests(t *testing.T) {
	f := pushFixture(t)
	old := f.commit("old") // what the merged pull requests were: not the head, so nothing is done for them
	for n := 1; n <= 30; n++ {
		f.gh.Open(fakegithub.PR{Number: n, Author: "alice", Base: "main", HeadSHA: old})
		f.gh.Merge(n, old)
	}
	f.push("v31", "alice")
	f.once()
	f.gh.ResetRequests()
	f.once()
	f.once()
	if n := f.gh.CountRequests("GET"); n > 2*6 {
		t.Fatalf("%d requests in two idle polls: %v", n, f.gh.Requests())
	}
	// An approval on an old pull request that was commented is still found.
	f.gh.Say(7, "alice", "/mamela approve sha256:"+strings.Repeat("ab", 32))
	f.once()
	if !f.said(7, "not a plan Mamela posted") {
		t.Fatalf("a comment on an old closed pull request was missed: %+v", f.gh.Comments(7))
	}
	f.gh.ResetRequests()
	f.once()
	f.once()
	if n := f.gh.CountRequests("GET /repos/me/infra/issues/"); n > 4 {
		t.Fatalf("the comments of the pull requests were read %d times in two idle polls", n)
	}
}

// B7: self_heal = false holds for a head that was in sync and whose plan then waited, was blocked
// or could not be made: the plan that undoes the drift is not approved by a push.
func TestDriftIsNotApprovedAfterWaitingOrBlockedOrUnplanned(t *testing.T) {
	for name, hitch := range map[string]func(f *fixture){
		"unplanned": func(f *fixture) { f.m.planErr = "an observation failed" },
		"waiting": func(f *fixture) {
			f.m.state = func(string) api.Planned { return api.Planned{Text: "waiting", Waiting: 1} }
		},
		"blocked": func(f *fixture) {
			f.m.state = func(string) api.Planned { return api.Planned{Text: "blocked", Blocked: 1} }
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := pushFixture(t)
			f.push("v1", "alice")
			f.once() // in sync
			if f.m.world != "v1" {
				t.Fatal("not in sync")
			}
			f.drifted()
			hitch(f)
			f.once() // the resync finds the head waiting, blocked or not plannable
			f.m.planErr, f.m.state = "", nil
			f.once() // the world is as it drifted: one change that undoes the drift
			f.once()
			f.once()
			if f.m.world != "changed under it" || len(f.m.applied) != 1 {
				t.Fatalf("the drift was undone by a push's approval: %q %v", f.m.world, f.m.applied)
			}
			if len(f.m.approved) != 1 {
				t.Fatalf("approved %v", f.m.approved)
			}
		})
	}
}

// With self_heal = true the same drift is undone.
func TestDriftIsHealedAfterWaiting(t *testing.T) {
	f := pushFixture(t)
	f.e.Config.SelfHeal = true
	f.push("v1", "alice")
	f.once()
	f.drifted()
	f.m.state = func(string) api.Planned { return api.Planned{Text: "waiting", Waiting: 1} }
	f.once()
	f.m.state = nil
	f.pollsOf(3)
	if f.m.world != "v1" {
		t.Fatalf("not healed: %q", f.m.world)
	}
}

// A phase of a new commit that waited is not drift: the push approves it as before.
func TestPhaseAfterWaitingIsStillApprovedByPush(t *testing.T) {
	f := pushFixture(t)
	f.m.state = func(string) api.Planned { return api.Planned{Text: "waiting", Waiting: 1} }
	f.push("v1", "alice")
	f.once()
	f.m.state = nil
	f.pollsOf(2)
	if f.m.world != "v1" {
		t.Fatalf("a phase of the push was not approved: %q", f.m.world)
	}
}

// A drift repair that the host approved is applied; its next phase is not approved by a push either.
func TestDriftNextPhaseNeedsTheHostToo(t *testing.T) {
	f := pushFixture(t)
	head := f.push("v1", "alice")
	f.once()
	f.drifted()
	phases := 0
	f.m.state = func(rev string) api.Planned {
		if phases < 2 {
			return api.Planned{Text: fmt.Sprintf("repair %d", phases), Changes: 1}
		}
		return api.Planned{Text: "done"}
	}
	f.once() // finds the drift
	var digest string
	for d := range f.m.plans {
		if f.m.plans[d] == "v1" {
			digest = d
		}
	}
	h, _, _ := f.e.Store.Head(context.Background(), "me/infra", "infra")
	f.m.approved[h.Digest] = true // `mamela approve` on the host
	phases = 1
	f.once() // applies it; the next phase is planned
	approved := len(f.m.approved)
	f.pollsOf(2)
	if len(f.m.approved) != approved {
		t.Fatalf("the next phase of a drift repair was approved by a push (%s)%s", digest, f.statusLines(head))
	}
}

// A7d: the evaluator is unavailable for a while (the toolchain is being replaced: MAM-EVAL-0001; a
// build that timed out: 0102; a full disk). The API answers 422 to all of them. That is not the
// declaration's fault: the head is evaluated again after the retry interval.
func TestTransientEvaluationFailureIsRetried(t *testing.T) {
	for _, msg := range []string{
		"MAM-EVAL-0001: the toolchain does not match toolchain.lock",
		"MAM-EVAL-0102: the build did not finish in time",
		"write /var/lib/mamela/tmp/main.wasm: no space left on device",
	} {
		t.Run(strings.Fields(msg)[0], func(t *testing.T) {
			f := pushFixture(t)
			f.e.Config.Retry = "1m"
			f.m.revErr = msg
			head := f.push("v1", "alice")
			f.once()
			s := f.lastStatus(head)
			if strings.Contains(s.Description, "could not be evaluated") && !strings.Contains(s.Description, "again") {
				t.Fatalf("a transient failure is reported as the declaration's: %+v", s)
			}
			if !strings.Contains(s.Description, "again in 1m0s") {
				t.Fatalf("the status does not say it will try again: %+v", s)
			}
			planned := f.m.attempts
			f.once()
			if f.m.attempts != planned {
				t.Fatal("tried again before the retry interval")
			}
			f.m.revErr = "" // the toolchain is back
			f.later(2 * time.Minute)
			f.once()
			if f.m.world != "v1" || f.lastStatus(head).State != "success" {
				t.Fatalf("not evaluated again: world %q%s", f.m.world, f.statusLines(head))
			}
		})
	}
}

// A declaration that is wrong stays an error until another commit, and says why where the author
// looks: the status's description, a commit comment (there is no pull request) and the log.
func TestEvaluationErrorOfAPushIsExplained(t *testing.T) {
	f := pushFixture(t)
	var logs []string
	f.e.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	f.m.revErr = "MAM-EVAL-0101: the build failed\n  detail:\n    main.emel:3:1: unexpected token"
	head := f.push("v1", "alice")
	f.once()
	f.once()
	s := f.lastStatus(head)
	if s.State != "failure" || !strings.Contains(s.Description, "MAM-EVAL-0101") || len(s.Description) > 140 {
		t.Fatalf("status: %+v", s)
	}
	cc := f.gh.CommitComments(head)
	if len(cc) != 1 || !strings.Contains(cc[0].Body, "main.emel:3:1") || !strings.Contains(cc[0].Body, "MAM-EVAL-0101") {
		t.Fatalf("commit comments: %+v", cc)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "MAM-EVAL-0101") {
		t.Fatalf("the log does not say why: %v", logs)
	}
	if n := f.gh.CountRequests("POST /repos/me/infra/issues/0"); n != 0 {
		t.Fatalf("commented on issue 0 %d times", n)
	}
	f.m.revErr = ""
	f.push("v2", "alice")
	f.once()
	if f.m.world != "v2" {
		t.Fatalf("a new commit did not start over: %q", f.m.world)
	}
}

// A token that cannot comment on commits does not break anything: the reason is in the status and
// the log.
func TestCommitCommentsMayBeDenied(t *testing.T) {
	f := pushFixture(t)
	f.gh.DenyCommitComments()
	f.m.revErr = "MAM-EVAL-0101: the build failed"
	head := f.push("v1", "alice")
	f.once()
	f.once()
	if s := f.lastStatus(head); !strings.Contains(s.Description, "MAM-EVAL-0101") {
		t.Fatalf("status: %+v", s)
	}
	if n := f.gh.CountRequests("POST /repos/me/infra/commits/"); n != 1 {
		t.Fatalf("asked %d times to comment on a commit after it was refused", n)
	}
}

// A plan that cannot be made, for a push (no pull request), says why on the status, in the log and
// in a commit comment, once; it is planned again at each poll, and the status is replaced when it works.
func TestPlanFailureOfAPushIsExplained(t *testing.T) {
	f := pushFixture(t)
	logs := f.logged()
	f.m.planErr = "MAM-OBS-0001: the API of provider kv did not answer"
	head := f.push("v1", "alice")
	f.pollsOf(3)
	s := f.lastStatus(head)
	if s.State != "failure" || !strings.Contains(s.Description, "MAM-OBS-0001") || len(s.Description) > 140 {
		t.Fatalf("status: %+v", s)
	}
	if cc := f.gh.CommitComments(head); len(cc) != 1 || !strings.Contains(cc[0].Body, "MAM-OBS-0001") {
		t.Fatalf("commit comments: %+v", cc)
	}
	if n := count(*logs, "could not be planned"); n != 1 {
		t.Fatalf("said %d times: %v", n, *logs)
	}
	f.m.planErr = ""
	f.once()
	if f.m.world != "v1" || f.lastStatus(head).State != "success" {
		t.Fatalf("not planned again: %q%s", f.m.world, f.statusLines(head))
	}
}
