package gitops

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
)

// recentPRs is how many of the most recently updated pull requests (of each state) are read each
// poll. A head merged by a pull request older than the last recentPRs closed ones is not found
// among them, so it is taken as not merged from a trusted author's (see mergedAs).
const recentPRs = 30

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

	trees map[string]string // commit -> the tree's name at the API
	// busy: the last poll left something under way that the next poll may move on.
	busy bool
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
// branch about (head.go). An error of one pull request or declaration does not stop the others.
func (e *Engine) Once(ctx context.Context) error {
	e.busy = false
	e.trees = map[string]string{} // the API keeps a few recent trees only: send them again each poll
	open, err := e.GitHub.PRs(ctx, "open", recentPRs)
	if err != nil {
		return err
	}
	closed, err := e.GitHub.PRs(ctx, "closed", recentPRs)
	if err != nil {
		return err
	}
	var errs []string
	errs = append(errs, e.planOpen(ctx, open)...)
	errs = append(errs, e.handleComments(ctx, slices.Concat(open, closed))...)
	errs = append(errs, e.syncBranch(ctx, closed)...)
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (e *Engine) planOpen(ctx context.Context, open []PR) (errs []string) {
	for _, pr := range open {
		if err := e.planHead(ctx, pr); err != nil {
			errs = append(errs, fmt.Sprintf("#%d: %v", pr.Number, err))
		}
	}
	return errs
}

// handleComments handles the approval comments of the trusted pull requests, open and closed: an
// approval can come after the merge.
func (e *Engine) handleComments(ctx context.Context, prs []PR) (errs []string) {
	for _, pr := range prs {
		if !e.trusted(pr) {
			continue
		}
		if err := e.comments(ctx, pr); err != nil {
			errs = append(errs, fmt.Sprintf("#%d comments: %v", pr.Number, err))
		}
	}
	return errs
}

// syncBranch brings each declaration at the head of the branch about.
func (e *Engine) syncBranch(ctx context.Context, closed []PR) (errs []string) {
	head, err := e.GitHub.Branch(ctx, e.Config.Branch)
	if err != nil {
		return []string{err.Error()}
	}
	owner := e.mergedAs(closed, head)
	by, err := e.pusher(ctx, head)
	if err != nil {
		return []string{err.Error()}
	}
	for _, d := range e.Config.Dirs {
		if err := e.syncHead(ctx, head, owner, by, d); err != nil {
			errs = append(errs, fmt.Sprintf("%s at the head: %v", d, err))
		}
	}
	return errs
}

// pusher is the author of the head commit when sync = "push" and the author is one of the
// configured authors ("": the head is not applied on its own). Whoever can push to the branch is
// trusted at GitHub; the author is what the commit says (ADR 0014).
func (e *Engine) pusher(ctx context.Context, head string) (string, error) {
	if !e.Config.PushSync() {
		return "", nil
	}
	login, err := e.GitHub.CommitAuthor(ctx, head)
	if err != nil || !slices.Contains(e.Config.Authors, login) {
		return "", err
	}
	return login, nil
}

// trusted: a pull request of the repository itself (not a fork), into the branch, by an author.
func (e *Engine) trusted(pr PR) bool {
	return pr.Base.Ref == e.Config.Branch && pr.Head.Repo != nil && pr.Head.Repo.FullName == e.Config.Repo &&
		slices.Contains(e.Config.Authors, pr.User.Login)
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
