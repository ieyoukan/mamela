package gitops

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/gitops/fakegithub"
)

// logged collects what the engine logs.
func (f *fixture) logged() *[]string {
	logs := &[]string{}
	f.e.Logf = func(format string, args ...any) { *logs = append(*logs, fmt.Sprintf(format, args...)) }
	return logs
}

func count(logs []string, sub string) int {
	n := 0
	for _, l := range logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// A7c: GitHub's rate limit. When the budget is used up, the poll stops (no more requests are sent),
// the error is one line that says when reading resumes, and the next poll waits for it.
func TestRateLimitIsRespected(t *testing.T) {
	f := newFixture(t)
	f.e.GitHub.Now = f.gh.Now
	f.gh.SetRateLimit(3, f.gh.Now().Add(10*time.Minute))
	err := f.e.Once(context.Background())
	rl, ok := IsRateLimit(err)
	if !ok {
		t.Fatalf("not a rate limit: %v", err)
	}
	if msg := err.Error(); strings.ContainsAny(msg, "\n{") || !strings.Contains(msg, "MAM-GIT-0003") || !strings.Contains(msg, "in 9m") {
		t.Fatalf("not one readable line: %q", msg)
	}
	if n := f.gh.CountRequests(""); n != 3 {
		t.Fatalf("%d requests were sent with a budget of 3: %v", n, f.gh.Requests())
	}
	if w := f.e.NextWait(); w < rl.Wait || w < 9*time.Minute {
		t.Fatalf("waits %v for a limit that lasts %v", w, rl.Wait)
	}
	// Still limited: not even asked.
	f.e.Once(context.Background())
	if n := f.gh.CountRequests(""); n != 3 {
		t.Fatalf("asked while limited: %d requests", n)
	}
	f.gh.Advance(11 * time.Minute)
	f.gh.SetRateLimit(0, time.Time{})
	f.once()
	if f.e.NextWait() != time.Minute {
		t.Fatalf("still waiting after the limit: %v", f.e.NextWait())
	}
}

// The secondary limit (403 with Retry-After) is a wait as well, not "403 forbidden".
func TestSecondaryRateLimitUsesRetryAfter(t *testing.T) {
	f := newFixture(t)
	f.e.GitHub.Now = f.gh.Now
	f.gh.Fail("GET", "/pulls", 403, 1, "You have exceeded a secondary rate limit.", map[string]string{"Retry-After": "120"})
	err := f.e.Once(context.Background())
	rl, ok := IsRateLimit(err)
	if !ok || !rl.Secondary || rl.Wait != 120*time.Second {
		t.Fatalf("%v (%+v)", err, rl)
	}
	if strings.Contains(err.Error(), "exceeded a secondary") || f.e.NextWait() < 2*time.Minute {
		t.Fatalf("%q, waits %v", err, f.e.NextWait())
	}
}

// A refused token is said once, with what to do, and the poll stops at it.
func TestRefusedTokenSaysWhatToDo(t *testing.T) {
	f := newFixture(t)
	f.e.GitHub.TokenFile = "/etc/mamelacd/github-token"
	f.gh.SetToken("another")
	err := f.e.Once(context.Background())
	if !IsAuth(err) {
		t.Fatalf("not a refused token: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"MAM-GIT-0001", "revoked or has expired", "/etc/mamelacd/github-token", "SIGHUP"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q is missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "\n") || strings.Contains(msg, "Bad credentials") {
		t.Errorf("not one line, or GitHub's own words: %q", msg)
	}
	if n := f.gh.CountRequests(""); n != 1 {
		t.Fatalf("%d requests after the token was refused", n)
	}
}

// A 5xx is GitHub's, in a line, with no body.
func TestServerErrorIsOneLine(t *testing.T) {
	f := newFixture(t)
	f.gh.Fail("GET", "/pulls", 502, 1, "<html>Bad Gateway: request id 8F2C...</html>", nil)
	err := f.e.Once(context.Background())
	msg := fmt.Sprint(err)
	if err == nil || !strings.Contains(msg, "MAM-GIT-0004") || strings.Contains(msg, "<html>") || strings.Contains(msg, "request id") || strings.Contains(msg, "\n") {
		t.Fatalf("%q", msg)
	}
}

// B11: PR lists and comments are read in full: more open pull requests than a page, more comments
// than a page.
func TestListsAreReadInFull(t *testing.T) {
	f := newFixture(t)
	for n := 1; n <= 32; n++ {
		f.gh.Open(fakegithub.PR{Number: n, Author: "alice", Base: "main", HeadSHA: f.commit(fmt.Sprintf("v%d", n))})
	}
	f.once()
	for _, n := range []int{1, 32} { // #1 is the least recently updated: the 32nd on the list, past the first 30
		if !f.said(n, "/mamela approve") {
			t.Errorf("pull request #%d was not planned (it is beyond the first page of open pull requests)", n)
		}
	}
	digest := f.lastPlan(1)
	for i := 0; i < 130; i++ {
		f.gh.Say(1, "carol", fmt.Sprintf("chatter %d", i))
	}
	f.gh.Say(1, "bob", "/mamela approve "+digest)
	f.once()
	if !f.said(1, "Approved `") || !f.m.approved[digest] {
		t.Fatalf("an approval after 130 comments was not read (it is beyond the first page of comments)")
	}
}

// B11: a head merged by a pull request that is older than the closed ones read each poll is found
// by looking further back, once; a head no pull request of an author made is still not planned.
func TestOlderMergeIsFound(t *testing.T) {
	f := newFixture(t)
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: f.commit("v1")})
	f.once()
	f.gh.Say(1, "alice", "/mamela approve "+f.lastPlan(1))
	f.once()
	merge := f.commit("v1")
	f.gh.Merge(1, merge)
	// Before mamelacd looks, 35 other pull requests (into other branches) were touched.
	other := f.commit("x")
	for n := 100; n < 135; n++ {
		f.gh.Open(fakegithub.PR{Number: n, Author: "alice", Base: "other", HeadSHA: other})
		f.gh.Merge(n, other)
	}
	f.once()
	if f.m.world != "v1" || !f.said(1, "### Mamela apply: `infra` applied") {
		t.Fatalf("the merge was not found among the older closed pull requests: applied %v, %+v", f.m.applied, f.gh.Comments(1))
	}
	// Not looked for again while the head is the same.
	f.gh.ResetRequests()
	f.once()
	if n := f.gh.CountRequests("GET /repos/me/infra/pulls?direction=desc&page=2"); n != 0 {
		t.Fatalf("looked further back again: %v", f.gh.Requests())
	}
}

// B11: a login that was left by a rename and taken by another account is not trusted, though it is
// the same string. Pull requests, approvals and pushes of the new owner are ignored, and it is said
// once (MAM-GIT-0301), with how to move the pin on purpose.
func TestLoginTakenByAnotherAccountIsNotTrusted(t *testing.T) {
	f := newFixture(t)
	logs := f.logged()
	f.gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: f.commit("v1")})
	f.once() // pins alice and bob
	bobID := f.gh.UserID("bob")
	digest := f.lastPlan(1)

	// bob renames his account; somebody else takes the login "bob" and approves.
	f.gh.Rename("bob", "bob-new")
	f.gh.Register("bob")
	if f.gh.UserID("bob") == bobID {
		t.Fatal("the fake gave the old id to the new account")
	}
	f.gh.Say(1, "bob", "/mamela approve "+digest)
	f.once()
	f.once()
	if f.m.approved[digest] {
		t.Fatal("an account that only has the login of an approver approved a plan")
	}
	var refusal string
	for _, c := range f.gh.Comments(1) {
		if c.Bot && strings.Contains(c.Body, "did not approve") {
			refusal = c.Body
		}
	}
	for _, want := range []string{"MAM-GIT-0301", "@bob", "repin"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal %q is missing %q", refusal, want)
		}
	}
	// The same for an author: the old owner renames, a new account takes "alice".
	f.gh.Rename("alice", "alice-new")
	f.gh.Register("alice")
	f.gh.Open(fakegithub.PR{Number: 2, Author: "alice", Base: "main", HeadSHA: f.commit("evil")})
	f.once()
	f.once()
	if len(f.gh.Comments(2)) != 0 {
		t.Fatalf("a pull request of a login's new owner was planned: %+v", f.gh.Comments(2))
	}
	if n := count(*logs, "mamelacd: MAM-GIT-0301"); n != 2 { // one for each account, not for each poll
		t.Fatalf("said %d times: %v", n, *logs)
	}
	ps, _ := f.e.Store.Principals(context.Background())
	for _, p := range ps {
		if p.Login == "alice" && (p.ConflictID == 0 || p.ID == p.ConflictID) {
			t.Errorf("the conflict is not recorded for status: %+v", p)
		}
	}
	// The operator moves the pin on purpose: the new account is trusted.
	if ok, err := f.e.Store.Unpin(context.Background(), "alice"); err != nil || !ok {
		t.Fatalf("unpin: %v %v", ok, err)
	}
	f.once()
	if !f.said(2, "/mamela approve") {
		t.Fatalf("not trusted after repinning: %+v", f.gh.Comments(2))
	}
}

// B11: with sync = "push", a commit attributed to a login of a new owner is not applied.
func TestPushByLoginsNewOwnerIsNotApplied(t *testing.T) {
	f := pushFixture(t)
	f.push("v1", "alice")
	f.once() // pins alice
	f.gh.Rename("alice", "alice-new")
	f.gh.Register("alice")
	f.push("v2", "alice")
	f.once()
	f.once()
	if f.m.world != "v1" || len(f.m.applied) != 1 {
		t.Fatalf("applied a push of an account that is not the pinned one: %v", f.m.applied)
	}
}

// An account that is renamed and not replaced is still the same account: the pin follows the id,
// and the configuration (a login) is what an operator updates.
func TestRenamedAccountKeepsItsPin(t *testing.T) {
	f := pushFixture(t)
	f.push("v1", "alice")
	f.once()
	f.gh.Rename("alice", "alice-new")
	f.e.Config.Authors = []string{"alice-new"}
	f.push("v2", "alice-new")
	f.once()
	// A new login is pinned to what GitHub says it is now: the same id.
	ps, _ := f.e.Store.Principals(context.Background())
	var ids = map[string]int64{}
	for _, p := range ps {
		ids[p.Login] = p.ID
	}
	if ids["alice"] == 0 || ids["alice-new"] != ids["alice"] || f.m.world != "v2" {
		t.Fatalf("pins %v, world %q", ids, f.m.world)
	}
}

// A login that does not exist is trusted with nothing, and said.
func TestUnknownLoginIsTrustedWithNothing(t *testing.T) {
	f := newFixture(t)
	logs := f.logged()
	f.gh.Delete("bob")
	f.once()
	f.gh.Say(1, "bob", "x")
	if count(*logs, "MAM-GIT-0302") != 1 || !strings.Contains(strings.Join(*logs, "\n"), "@bob") {
		t.Fatalf("%v", *logs)
	}
	f.once()
	f.once()
	if n := f.gh.CountRequests("GET /users/bob"); n != 1 {
		t.Fatalf("asked %d times about a login that does not exist (once per %v)", n, missingFor)
	}
}

// The client follows pages: 105 open pull requests are three pages of 100 at most (two), 250
// comments are three; and a list that is a whole number of pages ends with an empty one.
func TestPagesAreFollowed(t *testing.T) {
	f := newFixture(t)
	for n := 1; n <= 105; n++ {
		f.gh.Open(fakegithub.PR{Number: n, Author: "alice", Base: "main", HeadSHA: strings.Repeat("a", 40)})
	}
	f.gh.ResetRequests()
	prs, err := f.e.GitHub.OpenPRs(context.Background())
	if err != nil || len(prs) != 105 {
		t.Fatalf("%d open pull requests: %v", len(prs), err)
	}
	if n := f.gh.CountRequests("GET /repos/me/infra/pulls"); n != 2 {
		t.Fatalf("%d requests for 105 pull requests: %v", n, f.gh.Requests())
	}
	seen := map[int]bool{}
	for _, p := range prs {
		if seen[p.Number] {
			t.Fatalf("#%d twice", p.Number)
		}
		seen[p.Number] = true
	}
	for i := 0; i < 200; i++ {
		f.gh.Say(1, "carol", fmt.Sprint("c", i))
	}
	cs, asOf, err := f.e.GitHub.Comments(context.Background(), 1)
	if err != nil || len(cs) != 200 || asOf.IsZero() {
		t.Fatalf("%d comments (as of %v): %v", len(cs), asOf, err)
	}
}

// The tarball of a commit is a GitHub request like the others: a rate limit there is a wait, not a
// failure of the declaration.
func TestTarballFollowsTheRateLimit(t *testing.T) {
	f := newFixture(t)
	f.e.GitHub.Now = f.gh.Now
	sha := f.commit("v1")
	f.gh.Fail("GET", "/tarball/", 429, 1, "slow down", map[string]string{"Retry-After": "90"})
	_, err := f.e.GitHub.Tarball(context.Background(), sha)
	if rl, ok := IsRateLimit(err); !ok || rl.Wait != 90*time.Second {
		t.Fatalf("%v", err)
	}
	if _, err := f.e.GitHub.Tarball(context.Background(), sha); !strings.Contains(fmt.Sprint(err), "MAM-GIT-0003") {
		t.Fatalf("asked while told to wait: %v", err)
	}
	if n := f.gh.CountRequests("GET /repos/me/infra/tarball/"); n != 1 {
		t.Fatalf("%d tarball requests", n)
	}
}
