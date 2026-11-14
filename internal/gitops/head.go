package gitops

import (
	"context"
	"errors"
	"fmt"

	"github.com/ieyoukan/mamela/internal/api"
)

// The head of the branch is the only thing applied. A plan of a merge that is no longer the head's
// declaration is never applied: it would undo what was merged after it.
//
// Each declaration at the head has a state (Head, HeadInSync...), kept between polls. A poll
// starts from the last state and the head's commit:
//
//	a new head, neither merged from a trusted pull request nor (sync = "push") by an author
//	                                                      -> HeadUntrusted (not planned)
//	a new head that cannot be evaluated                  -> HeadError (until another commit)
//	an approved plan of exactly the head's declaration   -> applied: HeadFailed, or planned again
//	waiting, blocked or unplanned (same head)            -> planned again
//	pending, in sync or failed (same head)               -> left alone
//	a new head, the declaration in sync before           -> HeadInSync, nothing to plan
//	any other new head                                   -> planned
//
// "Planned" ends in HeadInSync, HeadPending, HeadWaiting or HeadBlocked (the plan's counts), or
// HeadUnplanned when the plan could not be made. With sync = "push" (ADR 0014) a plan with
// changes and no deletion is approved right after it is planned, as the head's author: the next
// poll applies it.

// Outcomes of applying a plan, kept with it (and said in the comment).
const (
	outcomeApplied = "applied"
	outcomeStale   = "stale"  // the world was not as planned: planned again, the new plan needs its own approval
	outcomeFailed  = "failed" // investigate
)

// errNotEvaluated: the declaration at the head could not be evaluated, and that was reported.
var errNotEvaluated = errors.New("could not be evaluated")

// headSync is one declaration at the head of the branch, being brought about.
type headSync struct {
	e     *Engine
	sha   string // the head commit
	owner int    // the trusted pull request merged as the head (0: none among the recent ones)
	by    string // sync = "push": the author of the head commit, who approves its plans ("": no one)
	dir   string
}

// syncHead brings one declaration at the head of the branch about. owner is the trusted pull
// request merged as the head, by the author of the head commit when sync = "push" (ADR 0014);
// without either, a new head is not planned (ADR 0010).
func (e *Engine) syncHead(ctx context.Context, sha string, owner int, by, dir string) error {
	if !validSHA(sha) {
		return fmt.Errorf("the head of %s: commit %q", e.Config.Branch, sha)
	}
	h := headSync{e: e, sha: sha, owner: owner, by: by, dir: dir}
	last, known, err := e.Store.Head(ctx, e.Config.Repo, dir)
	if err != nil {
		return err
	}
	sameHead := known && last.SHA == sha
	switch {
	case !sameHead && owner == 0 && by == "":
		e.logf("mamelacd: %s: the head %s of %s was not merged from a trusted author's pull request: not planned", dir, short(sha), e.Config.Branch)
		return h.record(ctx, "", HeadUntrusted, "")
	case sameHead && (last.State == HeadUntrusted || last.State == HeadError):
		return nil // nothing changes until another commit arrives
	}

	rev := last.Revision
	if !sameHead {
		rev, err = h.evaluate(ctx)
		if errors.Is(err, errNotEvaluated) {
			return h.record(ctx, "", HeadError, "")
		}
		if err != nil {
			return err
		}
	}

	// An approved plan of exactly this declaration is applied.
	p, approved, err := e.approvedPlan(ctx, dir, rev)
	if err != nil {
		return err
	}
	if approved {
		return h.apply(ctx, rev, p)
	}

	switch nextStep(last, known, sameHead, rev) {
	case stepLeave:
		return nil
	case stepUnchanged:
		e.GitHub.Status(ctx, sha, "success", statusContext(dir), "in sync (declaration unchanged)")
		return h.record(ctx, rev, HeadInSync, last.Digest)
	case stepPlanNew:
		if err := h.noteMovedDeclaration(ctx, rev); err != nil {
			return err
		}
	}
	return h.plan(ctx, rev)
}

type headStep int

const (
	stepLeave     headStep = iota // pending an approval, in sync, or failed: nothing to do
	stepPlanAgain                 // waiting, blocked or unplanned: what it waits for may be there now
	stepUnchanged                 // another commit, the same declaration, in sync before: nothing to plan
	stepPlanNew                   // a declaration not planned yet
)

// nextStep says what to do with a declaration at the head that has no approved plan to apply.
func nextStep(last Head, known, sameHead bool, rev string) headStep {
	switch {
	case sameHead && (last.State == HeadWaiting || last.State == HeadBlocked || last.State == HeadUnplanned):
		return stepPlanAgain
	case sameHead:
		return stepLeave
	case known && last.Revision == rev && last.State == HeadInSync:
		return stepUnchanged
	}
	return stepPlanNew
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

// record records the state of the declaration at the head.
func (h headSync) record(ctx context.Context, rev, state, digest string) error {
	return h.e.Store.SetHead(ctx, h.e.Config.Repo, Head{Dir: h.dir, SHA: h.sha, Revision: rev, State: state, Digest: digest})
}

// recordPlanned records the state after the head was planned (planned: its result, err: how it went).
func (h headSync) recordPlanned(ctx context.Context, rev string, planned api.Planned, err error) error {
	switch {
	case errors.Is(err, errNotPlanned):
		return h.record(ctx, rev, HeadUnplanned, "")
	case err != nil:
		return err
	}
	if err := h.record(ctx, rev, headState(planned), planned.Digest); err != nil {
		return err
	}
	return h.approveByPush(ctx, planned)
}

// approveByPush approves a plan of the head as the head's author (sync = "push", ADR 0014): the
// push is the approval. A plan that deletes is not: it is approved on the Control Plane's host (the
// API refuses it too). A blocked plan is not approved either.
func (h headSync) approveByPush(ctx context.Context, p api.Planned) error {
	if h.by == "" || p.Changes == 0 || p.Blocked > 0 || p.Removals > 0 {
		return nil
	}
	e := h.e
	ttl, _ := e.Config.ttl()
	if err := e.Mamela.Approve(ctx, p.Digest, "push:"+h.by, ttl); err != nil {
		if !api.Refused(err) {
			return err
		}
		e.logf("mamelacd: %s plan %s was not approved: %v", h.dir, short(p.Digest), err)
		e.GitHub.Status(ctx, h.sha, "failure", statusContext(h.dir), "could not be approved")
		return nil
	}
	e.logf("mamelacd: %s plan %s approved by the push of %s", h.dir, short(p.Digest), h.by)
	return nil
}

// plan plans the head and records its state.
func (h headSync) plan(ctx context.Context, rev string) error {
	p, err := h.e.planAndPost(ctx, h.owner, h.sha, h.dir, true)
	return h.recordPlanned(ctx, rev, p, err)
}

// evaluate evaluates the head's declaration at the Control Plane: its revision. If the API refuses
// it, that is reported on the pull request that was merged as the head (errNotEvaluated).
func (h headSync) evaluate(ctx context.Context) (string, error) {
	e := h.e
	tree, err := e.tree(ctx, h.sha)
	var rev string
	if err == nil {
		rev, err = e.Mamela.Revision(ctx, tree, h.dir)
	}
	if err == nil {
		return rev, nil
	}
	if !api.Refused(err) {
		return "", err
	}
	e.GitHub.Status(ctx, h.sha, "failure", statusContext(h.dir), "could not be evaluated")
	body := fmt.Sprintf("### Mamela: `%s` at the head %s of `%s` could not be evaluated\n\n```text\n%s\n```\n",
		h.dir, short(h.sha), e.Config.Branch, clip(err.Error()))
	if err := e.GitHub.Comment(ctx, h.owner, body); err != nil {
		return "", err
	}
	return "", errNotEvaluated
}

// noteMovedDeclaration tells the merged pull request that the head is not the declaration that
// was planned for it (the branch moved before the merge): the head is planned instead.
func (h headSync) noteMovedDeclaration(ctx context.Context, rev string) error {
	e := h.e
	prev, ok, err := e.Store.LatestPlan(ctx, e.Config.Repo, h.owner, h.dir)
	if err != nil || !ok || prev.AtHead || prev.Revision == rev {
		return err
	}
	return e.GitHub.Comment(ctx, h.owner, fmt.Sprintf("The declaration `%s` at the head %s of `%s` is not the one planned at %s: planning the head.",
		h.dir, short(h.sha), e.Config.Branch, short(prev.SHA)))
}

// approvedPlan is the newest plan of the declaration (by its revision) that is approved and not
// applied yet.
func (e *Engine) approvedPlan(ctx context.Context, dir, rev string) (Plan, bool, error) {
	plans, err := e.Store.Unapplied(ctx, e.Config.Repo, dir, rev)
	if err != nil {
		return Plan{}, false, err
	}
	for _, p := range plans {
		ok, err := e.Mamela.Approved(ctx, p.Digest)
		if err != nil {
			return Plan{}, false, err
		}
		if ok {
			return p, true, nil
		}
	}
	return Plan{}, false, nil
}

// apply applies an approved plan of the declaration at the head, reports it, and plans the head
// again: the end of one phase is not convergence (a next phase may need its own approval).
func (h headSync) apply(ctx context.Context, rev string, p Plan) error {
	e := h.e
	e.GitHub.Status(ctx, h.sha, "pending", statusContext(h.dir), "applying "+short(p.Digest))
	a, err := e.Mamela.Apply(ctx, p.Digest)
	if err != nil && !api.Refused(err) {
		return err // the API did not answer: the next poll asks again (apply is safe to repeat)
	}
	outcome, text := applyOutcome(a, err)
	if err := e.Store.Applied(ctx, p.Digest, outcome); err != nil {
		return err
	}
	e.logf("mamelacd: %s plan %s %s", h.dir, short(p.Digest), outcome)
	pr := h.owner
	if pr == 0 {
		pr = p.PR // the merged pull request is not among the recent ones any more
	}
	if err := h.reportApply(ctx, pr, p, outcome, text); err != nil {
		return err
	}
	if outcome == outcomeFailed {
		e.GitHub.Status(ctx, h.sha, "failure", statusContext(h.dir), "failed "+short(p.Digest))
		return h.record(ctx, rev, HeadFailed, p.Digest)
	}
	next, err := e.planAndPost(ctx, pr, h.sha, h.dir, true)
	return h.recordPlanned(ctx, rev, next, err)
}

// applyOutcome is how an apply went, from the API's answer (err: the API refused it), and the
// text to show.
func applyOutcome(a api.Applied, err error) (outcome, text string) {
	switch {
	case err != nil:
		return outcomeFailed, a.Text + "\n" + err.Error()
	case a.Stale:
		return outcomeStale, a.Text
	case !a.OK:
		return outcomeFailed, a.Text
	}
	return outcomeApplied, a.Text
}

// reportApply comments the apply on pull request pr, and on the pull request the plan was posted
// on when that is another.
func (h headSync) reportApply(ctx context.Context, pr int, p Plan, outcome, text string) error {
	e := h.e
	if pr > 0 {
		comment := fmt.Sprintf("### Mamela apply: `%s` %s (plan `%s`, the head %s of `%s`)\n\n```text\n%s\n```\n",
			p.Dir, outcome, p.Digest, short(h.sha), e.Config.Branch, clip(text))
		if err := e.GitHub.Comment(ctx, pr, comment); err != nil {
			return err
		}
	}
	if p.PR != pr && p.PR > 0 {
		e.GitHub.Comment(ctx, p.PR, fmt.Sprintf("Plan `%s` was applied as the head %s of `%s` (%s): see #%d.", p.Digest, short(h.sha), e.Config.Branch, outcome, pr))
	}
	return nil
}
