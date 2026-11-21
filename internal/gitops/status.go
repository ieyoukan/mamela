package gitops

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// headMeaning says, for a state of a declaration at the head, what it means and what to do.
var headMeaning = map[string]string{
	HeadInSync:    "nothing to change",
	HeadPending:   "changes planned, waiting for an approval of the plan",
	HeadWaiting:   "waiting for observation or dependencies (planned again at each poll)",
	HeadBlocked:   "refused by a guard or a policy (planned again at each poll)",
	HeadUnplanned: "could not be planned (tried again at each poll)",
	HeadFailed:    "the apply failed: see the commit status; it is retried by retry/retry_max",
	HeadError:     "the declaration could not be evaluated: fix it and push another commit",
	HeadEvalRetry: "the evaluator was not available (MAM-GIT-0201): evaluated again after the retry interval",
	HeadStopped:   "the same change keeps coming back (MAM-GIT-0101): push another commit, or approve the plan on the host",
	HeadUntrusted: "not merged from a trusted author's pull request (or pushed by one): not planned",
	HeadHeld:      "what differs is held by observe-only (MAM-POL-0312): shown, never applied; lifted on the Control Plane's host (planned again at the resync interval)",
}

// openExisting opens mamelacd's database if it exists; it is never made here (a database made by
// whoever runs a command, root for one, could not be opened by mamelacd's own user).
func openExisting(ctx context.Context, cfg Config, now func() time.Time) (*Store, error) {
	if _, err := os.Stat(cfg.StateDB); err != nil {
		return nil, fmt.Errorf("no state database at %s: mamelacd has not run with this configuration yet (or state_db is not the one it uses)", cfg.StateDB)
	}
	return OpenStore(ctx, cfg.StateDB, now)
}

// Status writes what mamelacd is doing, from its database (`mamelacd status`): the last poll and the
// next, each declaration's state, the pinned accounts. It says what looks wrong; healthy is false
// then (a stalled mamelacd, a poll that keeps failing). Run it as the user mamelacd runs as, so the
// database's files keep their owner.
func Status(ctx context.Context, cfg Config, now time.Time, w io.Writer) (healthy bool, err error) {
	st, err := openExisting(ctx, cfg, func() time.Time { return now })
	if err != nil {
		return false, err
	}
	defer st.Close()
	hb, hasBeat, err := st.LastHeartbeat(ctx)
	if err != nil {
		return false, err
	}
	heads, err := st.Heads(ctx, cfg.Repo)
	if err != nil {
		return false, err
	}
	pins, err := st.Principals(ctx)
	if err != nil {
		return false, err
	}
	healthy = true
	problem := func(format string, args ...any) {
		healthy = false
		fmt.Fprintf(w, "  PROBLEM: "+format+"\n", args...)
	}
	ts := func(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05Z") }
	ago := func(ms int64) string { return now.Sub(time.UnixMilli(ms)).Round(time.Second).String() }

	fmt.Fprintf(w, "mamelacd status: %s (%s), dirs %s\n", cfg.Repo, cfg.Branch, strings.Join(cfg.Dirs, ", "))
	if !hasBeat {
		fmt.Fprintf(w, "  no poll has been recorded in %s\n", cfg.StateDB)
		problem("mamelacd has not finished a poll with this database (it was never started, or it is older than 0.3.2, which kept no heartbeat). Start it: systemctl start mamelacd (MAM-GIT-0501)")
	} else {
		interval := time.Duration(hb.IntervalMs) * time.Millisecond
		if interval <= 0 {
			interval = cfg.IntervalOrDefault()
		}
		limit := stalledAfter * interval
		switch {
		case hb.PollEndMs == 0:
			run := now.Sub(time.UnixMilli(hb.PollStartMs)).Round(time.Second)
			fmt.Fprintf(w, "  poll:    running since %s (%s)\n", ts(hb.PollStartMs), run)
			if run > limit {
				fmt.Fprintf(w, "  note:    a poll that takes longer than %d intervals is a long apply or a call that hangs (the Control Plane's API, GitHub); if it does not end, look at the journal\n", stalledAfter)
			}
		default:
			fmt.Fprintf(w, "  poll:    last finished %s (%s ago), %s, took %s\n", ts(hb.PollEndMs), ago(hb.PollEndMs), hb.Result, time.Duration(hb.DurationMs)*time.Millisecond)
			next := time.UnixMilli(hb.NextMs)
			if next.After(now) {
				fmt.Fprintf(w, "  next:    due %s (in %s); every %s\n", ts(hb.NextMs), next.Sub(now).Round(time.Second), interval)
			} else {
				fmt.Fprintf(w, "  next:    was due %s; every %s\n", ts(hb.NextMs), interval)
			}
			// Not after 3 intervals since the last poll; nor while it waits on purpose (a rate limit).
			deadline := time.UnixMilli(hb.PollEndMs).Add(limit)
			if d := next.Add(interval); d.After(deadline) {
				deadline = d
			}
			if now.After(deadline) {
				problem("no poll finished for %s (stalled after %d x the interval %s): mamelacd may be stopped or stuck (MAM-GIT-0501). Look: systemctl status mamelacd; journalctl -u mamelacd -n 100",
					now.Sub(time.UnixMilli(hb.PollEndMs)).Round(time.Second), stalledAfter, interval)
			}
		}
		if hb.Failing > 0 {
			problem("the last %d poll(s) failed, since %s: %s", hb.Failing, ts(hb.FailingSince), hb.Detail)
		}
		fmt.Fprintf(w, "  process: pid %d, %s, up %s, %d poll(s) since it started\n", hb.PID, hb.Version, now.Sub(time.UnixMilli(hb.StartedMs)).Round(time.Second), hb.Polls)
	}
	if len(heads) == 0 {
		fmt.Fprintln(w, "  heads:   none seen yet")
	} else {
		fmt.Fprintln(w, "  heads:")
		for _, h := range heads {
			fmt.Fprintf(w, "    %-16s %-10s at %s (since %s ago) %s\n", h.Dir, h.State, short7(h.SHA), ago(h.UpdatedMs), headDetail(h))
		}
	}
	if len(pins) > 0 {
		fmt.Fprintln(w, "  accounts (the GitHub id each login was pinned to):")
		for _, p := range pins {
			fmt.Fprintf(w, "    @%s id %d, pinned %s\n", p.Login, p.ID, ts(p.PinnedMs))
			if p.ConflictID != 0 {
				problem("MAM-GIT-0301: account id %d acted under the login @%s at %s and is not trusted; the pinned one is id %d. If the pin is wrong: mamelacd repin --config <mamelacd.toml> --login %s", p.ConflictID, p.Login, ts(p.ConflictMs), p.ID, p.Login)
			}
		}
	}
	return healthy, nil
}

func headDetail(h Head) string {
	var parts []string
	if h.Digest != "" {
		parts = append(parts, "plan "+short(h.Digest))
	}
	if h.Drift {
		parts = append(parts, "undoing a drift")
	}
	if m := headMeaning[h.State]; m != "" && h.State != HeadInSync && h.State != HeadPending {
		parts = append(parts, "- "+m)
	}
	return strings.Join(parts, " ")
}

// Repin forgets the account pinned for a login; the next poll pins the account the login has then
// (B11: after a rename or a recreated account, on purpose). It says what it did.
func Repin(ctx context.Context, cfg Config, login string, w io.Writer) error {
	st, err := openExisting(ctx, cfg, time.Now)
	if err != nil {
		return err
	}
	defer st.Close()
	ok, err := st.Unpin(ctx, login)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no account is pinned for @%s (see `mamelacd status`)", login)
	}
	fmt.Fprintf(w, "forgot the account pinned for @%s: the next poll pins the account that has the login then (check it at https://github.com/%s). A running mamelacd needs no restart\n", login, login)
	return nil
}
