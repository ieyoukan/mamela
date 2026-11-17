package gitops

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Who is trusted is configured as logins (authors, approvers). A login is not an identity: when an
// account is renamed, GitHub frees the old login, and anybody can take it; on a public repository
// that someone then has the old owner's say. The numeric id of an account stays. So each configured
// login is resolved to its id the first time it is seen, pinned in mamelacd's database, and every
// pull request, comment and commit that acts under the login must come from that id. When another
// account acts under a pinned login, mamelacd does not trust it, says why (MAM-GIT-0301) and how to
// move the pin on purpose (`mamelacd repin`). Nothing in the configuration changes, and the
// approvals the Control Plane records still say `github:<login>`.

// missingFor is how long a login that does not exist is not asked about again.
const missingFor = 10 * time.Minute

// principalLogins are the configured logins that are trusted with something.
func (e *Engine) principalLogins() []string {
	logins := slices.Concat(e.Config.Authors, e.Config.Approvers)
	slices.Sort(logins)
	return slices.Compact(logins)
}

// pinAccounts loads the pins and resolves the logins that have none. A pin that was removed from the
// database (`mamelacd repin`) is made again here, from GitHub's answer now.
func (e *Engine) pinAccounts(ctx context.Context) error {
	ps, err := e.Store.Principals(ctx)
	if err != nil {
		return err
	}
	e.pins = map[string]int64{}
	for _, p := range ps {
		e.pins[p.Login] = p.ID
	}
	now := e.Store.Now()
	for _, login := range e.principalLogins() {
		if _, ok := e.pins[login]; ok {
			continue
		}
		if until, ok := e.missing[login]; ok && now.Before(until) {
			continue
		}
		id, exists, err := e.GitHub.UserID(ctx, login)
		if fatal(err) {
			return err
		}
		if err != nil {
			return fmt.Errorf("resolving @%s: %w", login, err)
		}
		if !exists {
			if e.missing == nil {
				e.missing = map[string]time.Time{}
			}
			e.missing[login] = now.Add(missingFor)
			e.logf("mamelacd: %s: @%s (in authors or approvers) is not a GitHub account: it is trusted with nothing. Fix the login in the configuration", CodeNoSuchLogin, login)
			continue
		}
		if err := e.Store.Pin(ctx, login, id); err != nil {
			return err
		}
		e.pins[login] = id
		e.logf("mamelacd: pinned @%s to GitHub account id %d: only that account is trusted under this login from now on. Check at https://api.github.com/users/%s that it is the right one", login, id, login)
	}
	return nil
}

// sameAccount says whether u, who acts under a configured login, is the account pinned for it.
func (e *Engine) sameAccount(u User) bool {
	pin, ok := e.pins[u.Login]
	if !ok {
		return false
	}
	if pin == u.ID {
		return true
	}
	if e.conflicts == nil {
		e.conflicts = map[string]User{}
	}
	e.conflicts[fmt.Sprintf("%s/%d", u.Login, u.ID)] = u
	return false
}

// accountError says why an approval or a push of u is refused, when it is not the pinned account.
func (e *Engine) accountError(u User) error {
	return fmt.Errorf("%s: @%s is GitHub account id %d, not the account id %d that was pinned for this login: the login was renamed and taken by somebody else, or the account was recreated. Nothing was done for it. If the account is the right one now, run `mamelacd repin --config <mamelacd.toml> --login %s`",
		CodeAccount, u.Login, u.ID, e.pins[u.Login], u.Login)
}

// reportConflicts says, once for each, which accounts acted under a pinned login in this poll
// without being the pinned account: in the log, and in the database for `mamelacd status`.
func (e *Engine) reportConflicts(ctx context.Context) error {
	for _, u := range e.conflicts {
		fresh, err := e.Store.See(ctx, fmt.Sprintf("account %s %s %d", e.Config.Repo, u.Login, u.ID))
		if err != nil {
			return err
		}
		if !fresh {
			continue
		}
		if err := e.Store.NoteConflict(ctx, u.Login, u.ID); err != nil {
			return err
		}
		e.logf("mamelacd: %v", e.accountError(u))
	}
	return nil
}
