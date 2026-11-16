package gitops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ieyoukan/mamela/internal/api"
)

// errNotPlanned: the declaration could not be planned, and that was reported.
var errNotPlanned = errors.New("could not be planned")

func statusContext(dir string) string { return "mamela/" + dir }

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
			if !ok || !stillWaiting(last, pr.Head.SHA) {
				continue
			}
		}
		if _, err := e.planAndPost(ctx, pr.Number, pr.Head.SHA, d, false); err != nil && !errors.Is(err, errNotPlanned) {
			return err
		}
	}
	_, err = e.Store.See(ctx, key)
	return err
}

// stillWaiting says whether the last plan of a pull request's head waits or is blocked with
// nothing to approve: what it waits for may be there at the next poll.
func stillWaiting(last Plan, sha string) bool {
	return last.SHA == sha && !last.AtHead && last.Changes == 0 && last.Waiting+last.Blocked > 0
}

// planAndPost plans one declaration of a commit, records the plan, sets the commit's status, and
// comments the plan on the pull request (pr > 0). A plan that waits or is blocked as it did before
// is not posted again. errNotPlanned: it could not be planned, and that was reported.
func (e *Engine) planAndPost(ctx context.Context, pr int, sha, dir string, atHead bool) (api.Planned, error) {
	tree, err := e.tree(ctx, sha)
	var p api.Planned
	if err == nil {
		p, err = e.Mamela.Plan(ctx, tree, dir)
	}
	if err != nil {
		return p, e.planFailed(ctx, pr, sha, dir, err)
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
	if hadPrevious && repeats(previous, p, sha, atHead) && !e.statusIsStale(ctx, sha, dir) {
		return p, nil
	}
	state, desc := planStatus(p)
	if err := e.GitHub.Status(ctx, sha, state, statusContext(dir), desc); err != nil {
		e.logf("mamelacd: status of %s: %v", short(sha), err)
	}
	e.logf("mamelacd: #%d %s at %s: plan %s (%d changes, %d waiting, %d blocked)", pr, dir, short(sha), short(p.Digest), p.Changes, p.Waiting, p.Blocked)
	if pr == 0 {
		return p, nil
	}
	return p, e.GitHub.Comment(ctx, pr, e.planComment(p, dir, sha, atHead))
}

// planFailed reports a plan that did not come back. The API refusing it is about the declaration:
// the commit's status says so, and the pull request is told once per commit (planning observes
// the world, so it can fail for a while, and the head is planned again each poll). Any other error
// is the API not answering: it is returned, to try again at the next poll.
func (e *Engine) planFailed(ctx context.Context, pr int, sha, dir string, err error) error {
	if !api.Refused(err) {
		return err
	}
	e.GitHub.Status(ctx, sha, "failure", statusContext(dir), "could not be planned")
	fresh, serr := e.Store.See(ctx, fmt.Sprintf("plan error %s %s %s", e.Config.Repo, sha, dir))
	if serr != nil {
		return serr
	}
	if pr > 0 && fresh {
		body := fmt.Sprintf("### Mamela: `%s` at %s could not be planned\n\n```text\n%s\n```\n", dir, short(sha), clip(err.Error()))
		if err := e.GitHub.Comment(ctx, pr, body); err != nil {
			return err
		}
	}
	return errNotPlanned
}

// statusIsStale says whether the head's commit status is the failure of a plan that could not be
// made (its state is "unplanned"): the plan that follows replaces it, even if it says what the last
// saved plan did.
func (e *Engine) statusIsStale(ctx context.Context, sha, dir string) bool {
	h, known, err := e.Store.Head(ctx, e.Config.Repo, dir)
	return err == nil && known && h.SHA == sha && h.State == HeadUnplanned
}

// repeats says whether p says what the previous plan of the same commit did, with nothing to
// approve: waiting or blocked for the same reasons.
func repeats(previous Plan, p api.Planned, sha string, atHead bool) bool {
	return !previous.Applied && previous.SHA == sha && previous.AtHead == atHead && previous.Revision == p.Revision &&
		previous.Changes == 0 && p.Changes == 0 && previous.Waiting == p.Waiting && previous.Blocked == p.Blocked
}

// planStatus is the commit status a plan sets.
func planStatus(p api.Planned) (state, description string) {
	switch {
	case p.Blocked > 0:
		return "failure", fmt.Sprintf("%d blocked, %d waiting", p.Blocked, p.Waiting)
	case p.Removals > 0:
		// ADR 0013: only the Control Plane's host approves a deletion; the status says how.
		return "pending", fmt.Sprintf("deletes %d: on the host, mamela approve %s", p.Removals, p.Digest)
	case p.Changes > 0:
		return "pending", fmt.Sprintf("%d changes, approve %s", p.Changes, short(p.Digest))
	case p.Waiting > 0:
		return "pending", fmt.Sprintf("%d waiting for observation or dependencies", p.Waiting)
	}
	return "success", "in sync"
}

// planComment is the pull request comment of a plan: the plan, and how to approve it.
func (e *Engine) planComment(p api.Planned, dir, sha string, atHead bool) string {
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
		if p.Removals > 0 {
			// ADR 0013: a comment from GitHub's side cannot approve a deletion.
			fmt.Fprintf(&b, "**This plan deletes %d object(s).** A comment cannot approve it: a person approves it on the Control Plane's host, with\n\n```\nmamela approve %s\n```\n\n", p.Removals, p.Digest)
		} else {
			fmt.Fprintf(&b, "To approve exactly this plan, an approver comments:\n\n```\n/mamela approve %s\n```\n\n", p.Digest)
		}
		if atHead {
			fmt.Fprintf(&b, "It is the declaration at the head of `%s`: it is applied as soon as it is approved.\n", e.Config.Branch)
		} else {
			fmt.Fprintf(&b, "It is applied when the pull request is merged, if this declaration is then the one at the head of `%s`.\n", e.Config.Branch)
		}
	}
	return b.String()
}
