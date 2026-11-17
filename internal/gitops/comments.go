package gitops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
)

var approveRe = regexp.MustCompile(`^/mamela approve (sha256:[0-9a-f]{64})$`)

// unreachable: the Control Plane's API did not answer.
type unreachable struct{ err error }

func (u unreachable) Error() string { return u.err.Error() }
func (u unreachable) Unwrap() error { return u.err }

// comments handles a pull request's approval comments, each once. A comment that cannot be
// approved is answered on the pull request; one that could not be handled because the API did
// not answer is left for the next poll.
func (e *Engine) comments(ctx context.Context, pr PR) error {
	// Unchanged since the comments were read after its last change: nothing new to read. The key
	// holds the pull request's updated_at; GitHub's Date of the answer decides whether it was read
	// after the change (a comment in the same second as the read would not move updated_at again).
	readKey := ""
	if !pr.UpdatedAt.IsZero() {
		readKey = fmt.Sprintf("comments %s #%d %s", e.Config.Repo, pr.Number, pr.UpdatedAt.UTC().Format(time.RFC3339))
		if read, err := e.Store.Seen(ctx, readKey); err != nil || read {
			return err
		}
	}
	cs, asOf, err := e.GitHub.Comments(ctx, pr.Number)
	if err != nil {
		return err
	}
	if err := e.handleOf(ctx, pr, cs); err != nil {
		return err
	}
	if readKey != "" && asOf.After(pr.UpdatedAt) {
		_, err = e.Store.See(ctx, readKey)
	}
	return err
}

// handleOf handles the approval comments among a pull request's comments.
func (e *Engine) handleOf(ctx context.Context, pr PR, cs []Comment) error {
	for _, c := range cs {
		m := approveRe.FindStringSubmatch(strings.TrimSpace(c.Body))
		if m == nil {
			continue
		}
		key := fmt.Sprintf("comment %s %d", e.Config.Repo, c.ID)
		seen, err := e.Store.Seen(ctx, key)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		aerr := e.approve(ctx, pr, c, m[1])
		var down unreachable
		if errors.As(aerr, &down) {
			return aerr
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

// approve approves the plan a comment names, if the commenter may and the plan is one mamelacd
// posted on this pull request that is not applied yet.
func (e *Engine) approve(ctx context.Context, pr PR, c Comment, digest string) error {
	switch {
	case c.User.Type != "User" || !slices.Contains(e.Config.Approvers, c.User.Login):
		return fmt.Errorf("@%s is not an approver of %s", strings.TrimPrefix(c.User.Login, "@"), e.Config.Repo)
	case !e.sameAccount(c.User):
		return e.accountError(c.User)
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
		if !api.Refused(err) {
			return unreachable{err}
		}
		return err
	}
	e.logf("mamelacd: #%d plan %s approved by %s", pr.Number, short(digest), c.User.Login)
	return e.GitHub.Comment(ctx, pr.Number, fmt.Sprintf("Approved `%s` (by @%s, for %s). It is applied while `%s` at the head of `%s` is this declaration.",
		digest, c.User.Login, ttl, gp.Dir, e.Config.Branch))
}
