package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/store"
)

// Config is the gitops configuration (TOML, ADR 0010).
type Config struct {
	// Repo is the GitHub repository of the declarations ("owner/name"), Branch the branch merges
	// into which are applied, Dirs the declarations (Pome directories) in it.
	Repo   string   `toml:"repo"`
	Branch string   `toml:"branch"`
	Dirs   []string `toml:"dirs"`
	// API is GitHub's API (default https://api.github.com); TokenFile holds a token limited to
	// the repository: contents read, pull requests and commit statuses read and write.
	API       string `toml:"api_url"`
	TokenFile string `toml:"token_file"`
	// Authors are whose pull requests are planned; Approvers who may approve (GitHub logins).
	// SelfApproval lets an author approve their own pull request's plan.
	Authors      []string `toml:"authors"`
	Approvers    []string `toml:"approvers"`
	SelfApproval *bool    `toml:"self_approval"`
	// ApprovalTTL is how long an approval lasts (default 24h); Interval how often GitHub is
	// read (default 1m).
	ApprovalTTL string `toml:"approval_ttl"`
	Interval    string `toml:"interval"`
	// WorkDir is where trees are fetched (one directory per commit).
	WorkDir string `toml:"work_dir"`
}

// Check says what is wrong with the configuration.
func (c Config) Check() error {
	switch {
	case !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(c.Repo):
		return fmt.Errorf("repo %q: owner/name", c.Repo)
	case c.Branch == "" || len(c.Dirs) == 0:
		return fmt.Errorf("branch and dirs are required")
	case c.TokenFile == "":
		return fmt.Errorf("token_file is required (a token limited to %s)", c.Repo)
	case len(c.Authors) == 0 || len(c.Approvers) == 0:
		return fmt.Errorf("authors and approvers are required: whose pull requests are planned, and who approves")
	case c.WorkDir == "":
		return fmt.Errorf("work_dir is required")
	}
	for _, d := range c.Dirs {
		if filepath.IsAbs(d) || d == ".." || strings.HasPrefix(filepath.Clean(d), "..") {
			return fmt.Errorf("dir %q: a directory in the repository", d)
		}
	}
	if _, err := c.ttl(); err != nil {
		return err
	}
	_, err := c.interval()
	return err
}

func (c Config) ttl() (time.Duration, error) {
	if c.ApprovalTTL == "" {
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(c.ApprovalTTL)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("approval_ttl %q", c.ApprovalTTL)
	}
	return d, nil
}

func (c Config) interval() (time.Duration, error) {
	if c.Interval == "" {
		return time.Minute, nil
	}
	d, err := time.ParseDuration(c.Interval)
	if err != nil || d < 10*time.Second {
		return 0, fmt.Errorf("interval %q: 10s or more", c.Interval)
	}
	return d, nil
}

// Interval is how often GitHub is read.
func (c Config) IntervalOrDefault() time.Duration {
	d, _ := c.interval()
	return d
}

// Planned is what planning a declaration gave.
type Planned struct {
	Digest   string
	Revision string // the declaration's digest
	Changes  int    // changes to make (not in sync)
	Text     string // the plan as people read it
	Programs []string
}

// Mamela is what gitops asks of Mamela: exactly what a person does with the CLI.
type Mamela interface {
	// Plan plans a declaration and saves the plan.
	Plan(ctx context.Context, dir string) (Planned, error)
	// Revision evaluates a declaration (its digest).
	Revision(ctx context.Context, dir string) (string, error)
	// Approve approves a saved plan.
	Approve(ctx context.Context, digest, by string, ttl time.Duration) error
	// Apply applies an approved plan; the text is what it did.
	Apply(ctx context.Context, digest string) (bool, string, error)
}

// Engine follows the repository.
type Engine struct {
	Config Config
	GitHub *GitHub
	Store  *store.Store
	Mamela Mamela
	Logf   func(format string, args ...any)
}

var approveRe = regexp.MustCompile(`^/mamela approve (sha256:[0-9a-f]{64})$`)

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// Once reads GitHub once and does what is due: plans new heads, handles approval comments, and
// applies what was merged. An error of one pull request does not stop the others.
func (e *Engine) Once(ctx context.Context) error {
	open, err := e.GitHub.PRs(ctx, "open", 30)
	if err != nil {
		return err
	}
	closed, err := e.GitHub.PRs(ctx, "closed", 30)
	if err != nil {
		return err
	}
	var errs []string
	for _, pr := range open {
		if err := e.planHead(ctx, pr); err != nil {
			errs = append(errs, fmt.Sprintf("#%d: %v", pr.Number, err))
		}
	}
	for _, pr := range append(slices.Clone(open), closed...) {
		if !e.trusted(pr) {
			continue
		}
		if err := e.comments(ctx, pr); err != nil {
			errs = append(errs, fmt.Sprintf("#%d comments: %v", pr.Number, err))
		}
	}
	for _, pr := range closed {
		if pr.MergedAt == nil || pr.Base.Ref != e.Config.Branch || !e.trusted(pr) {
			continue
		}
		if err := e.merged(ctx, pr); err != nil {
			errs = append(errs, fmt.Sprintf("#%d merge: %v", pr.Number, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// trusted: a pull request of the repository itself (not a fork), into the branch, by an author.
func (e *Engine) trusted(pr PR) bool {
	return pr.Base.Ref == e.Config.Branch && pr.Head.Repo != nil && pr.Head.Repo.FullName == e.Config.Repo &&
		slices.Contains(e.Config.Authors, pr.User.Login)
}

func (e *Engine) checkout(ctx context.Context, sha string) (string, error) {
	dir := filepath.Join(e.Config.WorkDir, sha)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(e.Config.WorkDir, 0o700); err != nil {
		return "", err
	}
	return dir, e.GitHub.Checkout(ctx, sha, dir)
}

// planHead plans a trusted pull request's head, once per head and declaration.
func (e *Engine) planHead(ctx context.Context, pr PR) error {
	if !e.trusted(pr) || !validSHA(pr.Head.SHA) {
		return nil
	}
	key := fmt.Sprintf("head %s %s", e.Config.Repo, pr.Head.SHA)
	if seen, err := e.Store.GitopsSeen(ctx, key); err != nil || seen {
		return err
	}
	tree, err := e.checkout(ctx, pr.Head.SHA)
	if err != nil {
		return err
	}
	for _, d := range e.Config.Dirs {
		if err := e.planAndPost(ctx, pr, pr.Head.SHA, tree, d, false); err != nil {
			return err
		}
	}
	_, err = e.Store.GitopsSee(ctx, key)
	return err
}

func statusContext(dir string) string { return "mamela/" + dir }

// planAndPost plans one declaration of a tree and comments the plan on the pull request.
func (e *Engine) planAndPost(ctx context.Context, pr PR, sha, tree, dir string, merged bool) error {
	p, err := e.Mamela.Plan(ctx, filepath.Join(tree, dir))
	if err != nil {
		body := fmt.Sprintf("### Mamela: `%s` at %s could not be planned\n\n```text\n%s\n```\n", dir, short(sha), clip(err.Error()))
		e.GitHub.Status(ctx, sha, "failure", statusContext(dir), "could not be planned")
		return e.GitHub.Comment(ctx, pr.Number, body)
	}
	if err := e.Store.SaveGitopsPlan(ctx, store.GitopsPlan{Digest: p.Digest, Repo: e.Config.Repo, PR: pr.Number, Dir: dir, SHA: sha,
		Merged: merged, Revision: p.Revision, Changes: p.Changes}); err != nil {
		return err
	}
	var b strings.Builder
	what := "head"
	if merged {
		what = "merge"
	}
	fmt.Fprintf(&b, "### Mamela plan: `%s` at %s (%s)\n\n", dir, short(sha), what)
	if p.Changes == 0 {
		b.WriteString("Nothing to change: the declaration is in sync.\n\n")
	}
	fmt.Fprintf(&b, "```text\n%s\n```\n\n", clip(p.Text))
	if len(p.Programs) > 0 {
		fmt.Fprintf(&b, "Provider programs: %s\n\n", strings.Join(p.Programs, ", "))
	}
	if p.Changes > 0 {
		fmt.Fprintf(&b, "To approve exactly this plan, an approver comments:\n\n```\n/mamela approve %s\n```\n\n", p.Digest)
		if merged {
			b.WriteString("It is the merged declaration: it is applied as soon as it is approved.\n")
		} else {
			b.WriteString("It is applied when the pull request is merged, if the merged declaration is this one.\n")
		}
	}
	state, desc := "success", "in sync"
	if p.Changes > 0 {
		state, desc = "pending", fmt.Sprintf("%d changes, approve %s", p.Changes, short(p.Digest))
	}
	if err := e.GitHub.Status(ctx, sha, state, statusContext(dir), desc); err != nil {
		e.logf("gitops: status of %s: %v", short(sha), err)
	}
	e.logf("gitops: #%d %s at %s: plan %s (%d changes)", pr.Number, dir, short(sha), short(p.Digest), p.Changes)
	return e.GitHub.Comment(ctx, pr.Number, b.String())
}

// comments handles approval comments, each once.
func (e *Engine) comments(ctx context.Context, pr PR) error {
	cs, err := e.GitHub.Comments(ctx, pr.Number)
	if err != nil {
		return err
	}
	for _, c := range cs {
		m := approveRe.FindStringSubmatch(strings.TrimSpace(c.Body))
		if m == nil {
			continue
		}
		fresh, err := e.Store.GitopsSee(ctx, fmt.Sprintf("comment %s %d", e.Config.Repo, c.ID))
		if err != nil || !fresh {
			if err != nil {
				return err
			}
			continue
		}
		if err := e.approve(ctx, pr, c, m[1]); err != nil {
			e.logf("gitops: #%d approval %d: %v", pr.Number, c.ID, err)
			e.GitHub.Comment(ctx, pr.Number, fmt.Sprintf("Mamela did not approve `%s`: %v", m[1], err))
		}
	}
	return nil
}

func (e *Engine) approve(ctx context.Context, pr PR, c Comment, digest string) error {
	switch {
	case c.User.Type != "User" || !slices.Contains(e.Config.Approvers, c.User.Login):
		return fmt.Errorf("@%s is not an approver of %s", strings.TrimPrefix(c.User.Login, "@"), e.Config.Repo)
	case c.User.Login == pr.User.Login && e.Config.SelfApproval != nil && !*e.Config.SelfApproval:
		return fmt.Errorf("the author of the pull request cannot approve its plans here (self_approval = false)")
	}
	gp, ok, err := e.Store.GitopsPlan(ctx, digest)
	if err != nil {
		return err
	}
	if !ok || gp.Repo != e.Config.Repo || gp.PR != pr.Number {
		return fmt.Errorf("it is not a plan Mamela posted on this pull request")
	}
	if gp.Applied {
		return fmt.Errorf("it was applied already (%s)", gp.Outcome)
	}
	ttl, _ := e.Config.ttl()
	if err := e.Mamela.Approve(ctx, digest, "github:"+c.User.Login, ttl); err != nil {
		return err
	}
	e.logf("gitops: #%d plan %s approved by %s", pr.Number, short(digest), c.User.Login)
	if gp.Merged {
		return e.apply(ctx, pr, gp)
	}
	return e.GitHub.Comment(ctx, pr.Number, fmt.Sprintf("Approved `%s` (by @%s, for %s). It is applied when the pull request is merged, if the merged declaration is the one planned.", digest, c.User.Login, ttl))
}

// merged applies what was approved for a merged pull request, or plans the merged declaration
// again when it is not the one approved.
func (e *Engine) merged(ctx context.Context, pr PR) error {
	if !validSHA(pr.MergeCommitSHA) {
		return fmt.Errorf("merge commit %q", pr.MergeCommitSHA)
	}
	key := fmt.Sprintf("merge %s %d %s", e.Config.Repo, pr.Number, pr.MergeCommitSHA)
	if seen, err := e.Store.GitopsSeen(ctx, key); err != nil || seen {
		return err
	}
	plans, err := e.Store.GitopsPlans(ctx, e.Config.Repo, pr.Number)
	if err != nil {
		return err
	}
	tree, err := e.checkout(ctx, pr.MergeCommitSHA)
	if err != nil {
		return err
	}
	for _, d := range e.Config.Dirs {
		rev, err := e.Mamela.Revision(ctx, filepath.Join(tree, d))
		if err != nil {
			e.GitHub.Comment(ctx, pr.Number, fmt.Sprintf("### Mamela: `%s` at the merge %s could not be evaluated\n\n```text\n%s\n```\n", d, short(pr.MergeCommitSHA), clip(err.Error())))
			continue
		}
		// The newest plan of this declaration for the pull request.
		i := slices.IndexFunc(plans, func(p store.GitopsPlan) bool { return p.Dir == d })
		switch {
		case i < 0:
			err = e.planAndPost(ctx, pr, pr.MergeCommitSHA, tree, d, true)
		case plans[i].Revision != rev:
			// main moved, or the head was not planned: the merged declaration is another one.
			e.GitHub.Comment(ctx, pr.Number, fmt.Sprintf("The merged declaration `%s` is not the one planned at %s: planning the merge.", d, short(plans[i].SHA)))
			err = e.planAndPost(ctx, pr, pr.MergeCommitSHA, tree, d, true)
		case plans[i].Applied:
		case plans[i].Changes == 0:
			e.GitHub.Status(ctx, pr.MergeCommitSHA, "success", statusContext(d), "in sync")
		default:
			ok, err2 := e.approved(ctx, plans[i].Digest)
			switch {
			case err2 != nil:
				err = err2
			case ok:
				err = e.apply(ctx, pr, plans[i])
			default:
				// Not approved (or the approval expired): plan the merge, which can be approved.
				e.GitHub.Comment(ctx, pr.Number, fmt.Sprintf("Plan `%s` of `%s` was not approved (or its approval expired) when the pull request was merged: planning the merge.", plans[i].Digest, d))
				err = e.planAndPost(ctx, pr, pr.MergeCommitSHA, tree, d, true)
			}
		}
		if err != nil {
			return err
		}
	}
	_, err = e.Store.GitopsSee(ctx, key)
	return err
}

func (e *Engine) approved(ctx context.Context, digest string) (bool, error) {
	a, err := e.Store.ValidApproval(ctx, digest)
	if err != nil {
		return false, nil // none, or expired
	}
	return a != nil, nil
}

func (e *Engine) apply(ctx context.Context, pr PR, gp store.GitopsPlan) error {
	sha := gp.SHA
	if !gp.Merged {
		sha = pr.MergeCommitSHA
	}
	e.GitHub.Status(ctx, sha, "pending", statusContext(gp.Dir), "applying "+short(gp.Digest))
	ok, text, err := e.Mamela.Apply(ctx, gp.Digest)
	outcome := "applied"
	state := "success"
	if err != nil || !ok {
		outcome, state = "failed", "failure"
	}
	if err != nil {
		text += "\n" + err.Error()
	}
	if err := e.Store.GitopsApplied(ctx, gp.Digest, outcome); err != nil {
		return err
	}
	e.GitHub.Status(ctx, sha, state, statusContext(gp.Dir), outcome+" "+short(gp.Digest))
	e.logf("gitops: #%d plan %s %s", pr.Number, short(gp.Digest), outcome)
	return e.GitHub.Comment(ctx, pr.Number, fmt.Sprintf("### Mamela apply: `%s` %s (plan `%s`)\n\n```text\n%s\n```\n", gp.Dir, outcome, gp.Digest, clip(text)))
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
