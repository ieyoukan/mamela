package gitops

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
)

// Config is mamelacd's configuration (TOML, ADR 0010 and 0012).
type Config struct {
	// Repo is the GitHub repository of the declarations ("owner/name"), Branch the branch whose
	// head is applied, Dirs the declarations (Pome directories) in it.
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
	// MamelaSocket is the Control Plane's local API (`mamela api`); StateDB mamelacd's own database.
	MamelaSocket string `toml:"mamela_socket"`
	StateDB      string `toml:"state_db"`
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
	case c.MamelaSocket == "":
		return fmt.Errorf("mamela_socket is required (the Control Plane's local API, `mamela api --socket`)")
	case c.StateDB == "":
		return fmt.Errorf("state_db is required (mamelacd's own database)")
	}
	for _, d := range c.Dirs {
		if !localDir(d) {
			return fmt.Errorf("dir %q: a directory in the repository", d)
		}
	}
	if _, err := c.ttl(); err != nil {
		return err
	}
	_, err := c.interval()
	return err
}

func localDir(d string) bool {
	return d != "" && !path.IsAbs(d) && !strings.Contains(d, "\\") && path.Clean(d) == d && d != "." && d != ".." && !strings.HasPrefix(d, "../")
}

func (c Config) ttl() (time.Duration, error) {
	if c.ApprovalTTL == "" {
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(c.ApprovalTTL)
	if err != nil || d <= 0 || d > api.MaxTTL {
		return 0, fmt.Errorf("approval_ttl %q: a duration up to %s", c.ApprovalTTL, api.MaxTTL)
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

// IntervalOrDefault is how often GitHub is read.
func (c Config) IntervalOrDefault() time.Duration {
	d, _ := c.interval()
	return d
}

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
}

var approveRe = regexp.MustCompile(`^/mamela approve (sha256:[0-9a-f]{64})$`)

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// Once reads GitHub once and does what is due: plans new heads of pull requests, handles approval
// comments, and brings the declarations at the head of the branch about: it applies an approved
// plan of exactly that declaration, or plans it. An error of one pull request or declaration does
// not stop the others.
func (e *Engine) Once(ctx context.Context) error {
	e.trees = map[string]string{} // the API keeps a few recent trees only: send them again each poll
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
	if head, err := e.GitHub.Branch(ctx, e.Config.Branch); err != nil {
		errs = append(errs, err.Error())
	} else {
		owner := 0 // the trusted pull request merged as the head
		for _, pr := range closed {
			if pr.MergedAt != nil && pr.MergeCommitSHA == head && e.trusted(pr) {
				owner = pr.Number
				break
			}
		}
		for _, d := range e.Config.Dirs {
			if err := e.syncHead(ctx, head, owner, d); err != nil {
				errs = append(errs, fmt.Sprintf("%s at the head: %v", d, err))
			}
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

// tree sends a commit's tree to the Control Plane's API once, and names it.
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

// planHead plans a trusted pull request's head, once per head and declaration; again while the
// plan waits or is blocked.
func (e *Engine) planHead(ctx context.Context, pr PR) error {
	if !e.trusted(pr) || !validSHA(pr.Head.SHA) {
		return nil
	}
	key := fmt.Sprintf("head %s %s", e.Config.Repo, pr.Head.SHA)
	seen, err := e.Store.Seen(ctx, key)
	if err != nil {
		return err
	}
	for _, d := range e.Config.Dirs {
		if seen {
			last, ok, err := e.Store.LatestPlan(ctx, e.Config.Repo, pr.Number, d)
			if err != nil {
				return err
			}
			if !ok || last.SHA != pr.Head.SHA || last.AtHead || last.Changes > 0 || last.Waiting+last.Blocked == 0 {
				continue
			}
		}
		if _, err := e.planAndPost(ctx, pr.Number, pr.Head.SHA, d, false); err != nil && err != errPlanned {
			return err
		}
	}
	_, err = e.Store.See(ctx, key)
	return err
}

func statusContext(dir string) string { return "mamela/" + dir }

// errPlanned: the declaration could not be planned, and that was reported.
var errPlanned = fmt.Errorf("could not be planned")

// reported says whether an error of the Control Plane's API is about the declaration (to report
// on GitHub) rather than the API not answering (to try again next time).
func reported(err error) bool {
	return api.Refused(err)
}

// planAndPost plans one declaration of a commit, records the plan, sets the commit's status, and
// comments the plan on the pull request (pr > 0). A plan that waits or is blocked as it did before
// is not commented again. errPlanned: it could not be planned, and that was reported.
func (e *Engine) planAndPost(ctx context.Context, pr int, sha, dir string, atHead bool) (api.Planned, error) {
	tree, err := e.tree(ctx, sha)
	var p api.Planned
	if err == nil {
		p, err = e.Mamela.Plan(ctx, tree, dir)
	}
	if err != nil {
		if !reported(err) {
			return p, err
		}
		e.GitHub.Status(ctx, sha, "failure", statusContext(dir), "could not be planned")
		// Planning observes the world: it can fail for a while (an API down). The head is planned
		// again each poll; the failure is commented once per commit.
		fresh, serr := e.Store.See(ctx, fmt.Sprintf("plan error %s %s %s", e.Config.Repo, sha, dir))
		if serr != nil {
			return p, serr
		}
		if pr > 0 && fresh {
			body := fmt.Sprintf("### Mamela: `%s` at %s could not be planned\n\n```text\n%s\n```\n", dir, short(sha), clip(err.Error()))
			if err := e.GitHub.Comment(ctx, pr, body); err != nil {
				return p, err
			}
		}
		return p, errPlanned
	}
	previous, hadPrevious, err := e.Store.LatestPlan(ctx, e.Config.Repo, pr, dir)
	if err != nil {
		return p, err
	}
	if err := e.Store.SavePlan(ctx, Plan{Digest: p.Digest, Repo: e.Config.Repo, PR: pr, Dir: dir, SHA: sha, AtHead: atHead,
		Revision: p.Revision, Changes: p.Changes, Waiting: p.Waiting, Blocked: p.Blocked}); err != nil {
		return p, err
	}
	// Re-observation can give a new plan while still waiting for the same thing: keep the new
	// plan, but do not post the same pending status and comment every poll.
	if hadPrevious && !previous.Applied && previous.SHA == sha && previous.AtHead == atHead && previous.Revision == p.Revision &&
		previous.Changes == 0 && p.Changes == 0 && previous.Waiting == p.Waiting && previous.Blocked == p.Blocked {
		return p, nil
	}
	var b strings.Builder
	what := "pull request head"
	if atHead {
		what = "head of `" + e.Config.Branch + "`"
	}
	fmt.Fprintf(&b, "### Mamela plan: `%s` at %s (%s)\n\n", dir, short(sha), what)
	if p.Changes == 0 && p.Waiting == 0 && p.Blocked == 0 {
		b.WriteString("Nothing to change: the declaration is in sync.\n\n")
	}
	fmt.Fprintf(&b, "```text\n%s\n```\n\n", clip(p.Text))
	if len(p.Programs) > 0 {
		fmt.Fprintf(&b, "Provider programs: %s\n\n", strings.Join(p.Programs, ", "))
	}
	if p.Changes > 0 {
		fmt.Fprintf(&b, "To approve exactly this plan, an approver comments:\n\n```\n/mamela approve %s\n```\n\n", p.Digest)
		if atHead {
			fmt.Fprintf(&b, "It is the declaration at the head of `%s`: it is applied as soon as it is approved.\n", e.Config.Branch)
		} else {
			fmt.Fprintf(&b, "It is applied when the pull request is merged, if this declaration is then the one at the head of `%s`.\n", e.Config.Branch)
		}
	}
	state, desc := "success", "in sync"
	switch {
	case p.Blocked > 0:
		state, desc = "failure", fmt.Sprintf("%d blocked, %d waiting", p.Blocked, p.Waiting)
	case p.Changes > 0:
		state, desc = "pending", fmt.Sprintf("%d changes, approve %s", p.Changes, short(p.Digest))
	case p.Waiting > 0:
		state, desc = "pending", fmt.Sprintf("%d waiting for observation or dependencies", p.Waiting)
	}
	if err := e.GitHub.Status(ctx, sha, state, statusContext(dir), desc); err != nil {
		e.logf("mamelacd: status of %s: %v", short(sha), err)
	}
	e.logf("mamelacd: #%d %s at %s: plan %s (%d changes, %d waiting, %d blocked)", pr, dir, short(sha), short(p.Digest), p.Changes, p.Waiting, p.Blocked)
	if pr == 0 {
		return p, nil
	}
	return p, e.GitHub.Comment(ctx, pr, b.String())
}

// headState is a plan's state at the head of the branch.
func headState(p api.Planned) string {
	switch {
	case p.Blocked > 0:
		return HeadBlocked
	case p.Changes > 0:
		return HeadPending
	case p.Waiting > 0:
		return HeadWaiting
	}
	return HeadInSync
}

// syncHead brings one declaration at the head of the branch about. Only the head is applied: a
// plan of a merge that is no longer the head's declaration is never applied (it would undo what
// was merged after it). owner is the trusted pull request merged as the head (0: none; then the
// head is not planned, ADR 0010).
func (e *Engine) syncHead(ctx context.Context, head string, owner int, dir string) error {
	if !validSHA(head) {
		return fmt.Errorf("the head of %s: commit %q", e.Config.Branch, head)
	}
	last, known, err := e.Store.Head(ctx, e.Config.Repo, dir)
	if err != nil {
		return err
	}
	set := func(rev, state, digest string) error {
		return e.Store.SetHead(ctx, e.Config.Repo, Head{Dir: dir, SHA: head, Revision: rev, State: state, Digest: digest})
	}
	sameHead := known && last.SHA == head
	if !sameHead && owner == 0 {
		e.logf("mamelacd: %s: the head %s of %s was not merged from a trusted author's pull request: not planned", dir, short(head), e.Config.Branch)
		return set("", HeadUntrusted, "")
	}
	if sameHead && (last.State == HeadUntrusted || last.State == HeadError) {
		return nil
	}
	rev := last.Revision
	if !sameHead {
		tree, err := e.tree(ctx, head)
		if err == nil {
			rev, err = e.Mamela.Revision(ctx, tree, dir)
		}
		if err != nil {
			if !reported(err) {
				return err
			}
			e.GitHub.Status(ctx, head, "failure", statusContext(dir), "could not be evaluated")
			if err := e.GitHub.Comment(ctx, owner, fmt.Sprintf("### Mamela: `%s` at the head %s of `%s` could not be evaluated\n\n```text\n%s\n```\n",
				dir, short(head), e.Config.Branch, clip(err.Error()))); err != nil {
				return err
			}
			return set("", HeadError, "")
		}
	}

	// An approved plan of exactly this declaration is applied.
	plans, err := e.Store.Unapplied(ctx, e.Config.Repo, dir, rev)
	if err != nil {
		return err
	}
	for _, p := range plans {
		ok, err := e.Mamela.Approved(ctx, p.Digest)
		if err != nil {
			return err
		}
		if ok {
			return e.apply(ctx, head, owner, rev, p)
		}
	}

	switch {
	case sameHead && (last.State == HeadWaiting || last.State == HeadBlocked || last.State == HeadUnplanned):
		// Observe and plan again: what it waits for may be there now.
	case sameHead:
		return nil // pending an approval, in sync, or failed (investigate)
	case known && last.Revision == rev && last.State == HeadInSync:
		// Another commit, the same declaration, in sync before: nothing to plan.
		e.GitHub.Status(ctx, head, "success", statusContext(dir), "in sync (declaration unchanged)")
		return set(rev, HeadInSync, last.Digest)
	default:
		prev, ok, err := e.Store.LatestPlan(ctx, e.Config.Repo, owner, dir)
		if err != nil {
			return err
		}
		if ok && !prev.AtHead && prev.Revision != rev {
			if err := e.GitHub.Comment(ctx, owner, fmt.Sprintf("The declaration `%s` at the head %s of `%s` is not the one planned at %s: planning the head.",
				dir, short(head), e.Config.Branch, short(prev.SHA))); err != nil {
				return err
			}
		}
	}
	p, err := e.planAndPost(ctx, owner, head, dir, true)
	if err == errPlanned {
		return set(rev, HeadUnplanned, "")
	}
	if err != nil {
		return err
	}
	return set(rev, headState(p), p.Digest)
}

// apply applies an approved plan of the declaration at the head, reports it, and plans the head
// again: the end of one phase is not convergence (a next phase may need its own approval).
func (e *Engine) apply(ctx context.Context, head string, owner int, rev string, p Plan) error {
	e.GitHub.Status(ctx, head, "pending", statusContext(p.Dir), "applying "+short(p.Digest))
	a, err := e.Mamela.Apply(ctx, p.Digest)
	if err != nil && !reported(err) {
		return err // the API did not answer: the next poll asks again (apply is safe to repeat)
	}
	outcome, text := "applied", a.Text
	switch {
	case err != nil:
		outcome, text = "failed", text+"\n"+err.Error()
	case a.Stale:
		outcome = "stale" // planned again below; the new plan needs its own approval
	case !a.OK:
		outcome = "failed"
	}
	if err := e.Store.Applied(ctx, p.Digest, outcome); err != nil {
		return err
	}
	e.logf("mamelacd: %s plan %s %s", p.Dir, short(p.Digest), outcome)
	pr := owner
	if pr == 0 {
		pr = p.PR
	}
	comment := fmt.Sprintf("### Mamela apply: `%s` %s (plan `%s`, the head %s of `%s`)\n\n```text\n%s\n```\n",
		p.Dir, outcome, p.Digest, short(head), e.Config.Branch, clip(text))
	if pr > 0 {
		if err := e.GitHub.Comment(ctx, pr, comment); err != nil {
			return err
		}
	}
	if p.PR != pr && p.PR > 0 {
		e.GitHub.Comment(ctx, p.PR, fmt.Sprintf("Plan `%s` was applied as the head %s of `%s` (%s): see #%d.", p.Digest, short(head), e.Config.Branch, outcome, pr))
	}
	set := func(state, digest string) error {
		return e.Store.SetHead(ctx, e.Config.Repo, Head{Dir: p.Dir, SHA: head, Revision: rev, State: state, Digest: digest})
	}
	if outcome == "failed" {
		e.GitHub.Status(ctx, head, "failure", statusContext(p.Dir), "failed "+short(p.Digest))
		return set(HeadFailed, p.Digest)
	}
	next, err := e.planAndPost(ctx, pr, head, p.Dir, true)
	if err == errPlanned {
		return set(HeadUnplanned, "")
	}
	if err != nil {
		return err
	}
	return set(headState(next), next.Digest)
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
		key := fmt.Sprintf("comment %s %d", e.Config.Repo, c.ID)
		if seen, err := e.Store.Seen(ctx, key); err != nil || seen {
			if err != nil {
				return err
			}
			continue
		}
		aerr := e.approve(ctx, pr, c, m[1])
		var down unreachable
		if errors.As(aerr, &down) {
			return aerr // the Control Plane did not answer: the comment is handled next time
		}
		if _, err := e.Store.See(ctx, key); err != nil {
			return err
		}
		if aerr != nil {
			e.logf("mamelacd: #%d approval %d: %v", pr.Number, c.ID, aerr)
			e.GitHub.Comment(ctx, pr.Number, fmt.Sprintf("Mamela did not approve `%s`: %v", m[1], aerr))
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
	gp, ok, err := e.Store.Plan(ctx, digest)
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
		if !reported(err) {
			return unreachable{err}
		}
		return err
	}
	e.logf("mamelacd: #%d plan %s approved by %s", pr.Number, short(digest), c.User.Login)
	return e.GitHub.Comment(ctx, pr.Number, fmt.Sprintf("Approved `%s` (by @%s, for %s). It is applied while `%s` at the head of `%s` is this declaration.",
		digest, c.User.Login, ttl, gp.Dir, e.Config.Branch))
}

// unreachable: the Control Plane's API did not answer.
type unreachable struct{ err error }

func (u unreachable) Error() string { return u.err.Error() }
func (u unreachable) Unwrap() error { return u.err }

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
