package gitops

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
)

// Reason codes of mamelacd's own decisions (docs/reason-codes.md, MAM-GIT).
const (
	CodeStopped     = "MAM-GIT-0101" // the same change came back too many times in a row: not applied again
	CodeEvalLater   = "MAM-GIT-0201" // the evaluator is unavailable for now: evaluated again later
	CodeAccount     = "MAM-GIT-0301" // a login is another GitHub account than the one pinned for it
	CodeNoSuchLogin = "MAM-GIT-0302" // a configured login does not exist on GitHub
)

// recentPRs is how many of the most recently updated closed pull requests are read each poll (the
// open ones are all read). A new head merged by a pull request older than these is looked for in
// older pages of closed pull requests, once (see olderMerge).
const recentPRs = 30

// olderPages bounds that search: 100 closed pull requests to a page.
const olderPages = 5

// Mamela is what mamelacd asks of the Control Plane: the local API (api.Client).
type Mamela interface {
	// Tree sends a commit's tarball; the answer names it.
	Tree(ctx context.Context, tarball []byte) (string, error)
	// Plan plans a declaration of a tree and saves the plan.
	Plan(ctx context.Context, tree, dir string) (api.Planned, error)
	// Revision evaluates a declaration of a tree (its digest).
	Revision(ctx context.Context, tree, dir string) (string, error)
	// Approve approves a saved plan.
	Approve(ctx context.Context, digest, by string, ttl time.Duration) error
	// Approved says whether a plan's approval is valid.
	Approved(ctx context.Context, digest string) (bool, error)
	// Apply applies an approved plan.
	Apply(ctx context.Context, digest string) (api.Applied, error)
}

// Engine follows the repository.
type Engine struct {
	Config Config
	GitHub *GitHub
	Store  *Store
	Mamela Mamela
	Logf   func(format string, args ...any)

	// Sleep waits d between polls (Run). It returns early with reloaded when the token is to be read
	// again, and with ok false when ctx is done. nil: really waits.
	Sleep func(ctx context.Context, d time.Duration, reload <-chan struct{}) (reloaded, ok bool)
	// AliveEvery is how often Run says it is alive when nothing else was said (default 10m).
	AliveEvery time.Duration

	trees map[string]string // commit -> the tree's name at the API
	// busy: the last poll left something under way that the next poll may move on.
	busy bool
	// rateWait: the last poll was stopped by GitHub's rate limit; the next one waits at least this.
	rateWait time.Duration
	// pins: the GitHub account (numeric id) each configured login was pinned to (B11).
	pins      map[string]int64
	conflicts map[string]User // the accounts that acted under a pinned login in this poll
	missing   map[string]time.Time
	// warnedCommitComments: said that the token cannot comment on commits.
	warnedCommitComments bool
}

// pollError is what a poll could not do: several things, each in its own words. It is one line.
type pollError struct{ errs []error }

func (p pollError) Error() string {
	var parts []string
	for i, err := range p.errs {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(p.errs)-3))
			break
		}
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "; ")
}

func (p pollError) Unwrap() []error { return p.errs }

func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return pollError{errs}
}

func anyFatal(errs []error) bool { return slices.ContainsFunc(errs, fatal) }

// NextWait is how long to wait before the next poll: the interval (the shorter active one while
// something is under way), or until GitHub's rate limit is over when that is later.
func (e *Engine) NextWait() time.Duration {
	wait := e.Config.IntervalOrDefault()
	if e.busy {
		wait = e.Config.ActiveIntervalOrDefault()
	}
	return max(wait, e.rateWait)
}

// Busy says whether the last poll left something under way (a declaration waits for what it depends
// on, or a chain of phases was cut short): the next poll should come after ActiveInterval.
func (e *Engine) Busy() bool { return e.busy }

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// Once reads GitHub once and does what is due, in three phases: plan the pull requests' new heads
// (plan.go), handle approval comments (comments.go), and bring the declarations at the head of the
// branch about (head.go). An error of one pull request or declaration does not stop the others;
// GitHub refusing the token, or its rate limit, stops the poll (every later request would fail).
func (e *Engine) Once(ctx context.Context) error {
	e.busy = false
	e.rateWait = 0
	e.trees = map[string]string{} // the API keeps a few recent trees only: send them again each poll
	e.conflicts = map[string]User{}
	err := e.poll(ctx)
	if err == nil {
		err = e.reportConflicts(ctx)
	} else {
		e.reportConflicts(ctx)
	}
	if rl, ok := IsRateLimit(err); ok {
		e.rateWait = rl.Wait + time.Second
	}
	return err
}

func (e *Engine) poll(ctx context.Context) error {
	if err := e.pinAccounts(ctx); err != nil {
		return err
	}
	open, err := e.GitHub.OpenPRs(ctx)
	if err != nil {
		return err
	}
	closed, err := e.GitHub.PRs(ctx, "closed", recentPRs)
	if err != nil {
		return err
	}
	var errs []error
	for _, phase := range []func() []error{
		func() []error { return e.flushReports(ctx) },
		func() []error { return e.planOpen(ctx, open) },
		func() []error { return e.handleComments(ctx, slices.Concat(open, closed)) },
		func() []error { return e.syncBranch(ctx, closed) },
	} {
		errs = append(errs, phase()...)
		if anyFatal(errs) {
			break
		}
	}
	return joinErrors(errs)
}

func (e *Engine) planOpen(ctx context.Context, open []PR) (errs []error) {
	for _, pr := range open {
		if err := e.planHead(ctx, pr); err != nil {
			errs = append(errs, fmt.Errorf("#%d: %w", pr.Number, err))
			if fatal(err) {
				break
			}
		}
	}
	return errs
}

// handleComments handles the approval comments of the trusted pull requests, open and closed: an
// approval can come after the merge. A pull request whose updated_at is what it was when its
// comments were last read, and that was not changing then, is not read again: a poll costs the two
// lists and the head, not a request for every pull request (A7c).
func (e *Engine) handleComments(ctx context.Context, prs []PR) (errs []error) {
	for _, pr := range prs {
		if !e.trusted(pr) {
			continue
		}
		if err := e.comments(ctx, pr); err != nil {
			errs = append(errs, fmt.Errorf("#%d comments: %w", pr.Number, err))
			if fatal(err) {
				break
			}
		}
	}
	return errs
}

// syncBranch brings each declaration at the head of the branch about.
func (e *Engine) syncBranch(ctx context.Context, closed []PR) (errs []error) {
	head, err := e.GitHub.Branch(ctx, e.Config.Branch)
	if err != nil {
		return []error{err}
	}
	by, err := e.pusher(ctx, head)
	if err != nil {
		return []error{err}
	}
	owner := e.mergedAs(closed, head)
	if owner == 0 && by == "" {
		if owner, err = e.olderMerge(ctx, head); err != nil {
			return []error{err}
		}
	}
	for _, d := range e.Config.Dirs {
		if err := e.syncHead(ctx, head, owner, by, d); err != nil {
			errs = append(errs, fmt.Errorf("%s at the head: %w", d, err))
			if fatal(err) {
				break
			}
		}
	}
	return errs
}

// pusher is the author of the head commit when sync = "push" and the author is one of the
// configured authors, as the account that was pinned for the login ("": the head is not applied on
// its own). Whoever can push to the branch is trusted at GitHub; the author is what the commit
// says (ADR 0014).
func (e *Engine) pusher(ctx context.Context, head string) (string, error) {
	if !e.Config.PushSync() {
		return "", nil
	}
	login, id, err := e.GitHub.CommitAuthor(ctx, head)
	if err != nil || !slices.Contains(e.Config.Authors, login) || !e.sameAccount(User{Login: login, ID: id}) {
		return "", err
	}
	return login, nil
}

// trusted: a pull request of the repository itself (not a fork), into the branch, by an author.
func (e *Engine) trusted(pr PR) bool {
	return pr.Base.Ref == e.Config.Branch && pr.Head.Repo != nil && pr.Head.Repo.FullName == e.Config.Repo &&
		slices.Contains(e.Config.Authors, pr.User.Login) && e.sameAccount(pr.User)
}

// mergedAs is the number of the trusted pull request that was merged as the commit head (0: none
// among the closed ones read).
func (e *Engine) mergedAs(closed []PR, head string) int {
	for _, pr := range closed {
		if pr.MergedAt != nil && pr.MergeCommitSHA == head && e.trusted(pr) {
			return pr.Number
		}
	}
	return 0
}

// olderMerge looks for the pull request merged as a new head among the older closed pull requests
// (the recent ones were read already), a page of 100 at a time. Only for a head that is new to a
// declaration: it costs a request per page, once. It is how a head merged long ago (mamelacd was
// stopped; many pull requests were touched since) is still known to be a trusted author's.
func (e *Engine) olderMerge(ctx context.Context, head string) (int, error) {
	isNew := false
	for _, d := range e.Config.Dirs {
		last, known, err := e.Store.Head(ctx, e.Config.Repo, d)
		if err != nil {
			return 0, err
		}
		isNew = isNew || !known || last.SHA != head
	}
	if !isNew {
		return 0, nil
	}
	for page := 1; page <= olderPages; page++ {
		prs, err := e.GitHub.PRsPage(ctx, "closed", 100, page)
		if err != nil {
			return 0, err
		}
		if n := e.mergedAs(prs, head); n > 0 {
			return n, nil
		}
		if len(prs) < 100 {
			break
		}
	}
	return 0, nil
}

// tree sends a commit's tree to the Control Plane's API once per poll, and names it.
func (e *Engine) tree(ctx context.Context, sha string) (string, error) {
	if t, ok := e.trees[sha]; ok {
		return t, nil
	}
	data, err := e.GitHub.Tarball(ctx, sha)
	if err != nil {
		return "", err
	}
	t, err := e.Mamela.Tree(ctx, data)
	if err != nil {
		return "", err
	}
	if e.trees == nil {
		e.trees = map[string]string{}
	}
	e.trees[sha] = t
	return t, nil
}

func short(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// clip keeps a text within what a comment holds.
func clip(s string) string {
	const max = 50000
	s = strings.ReplaceAll(s, "```", "'''")
	if len(s) > max {
		return s[:max] + "\n... (cut)"
	}
	return s
}
