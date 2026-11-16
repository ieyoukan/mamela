package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/version"
)

// defaultAliveEvery is how often a quiet mamelacd says it is alive.
const defaultAliveEvery = 10 * time.Minute

// stalledAfter is how many intervals without a finished poll make `mamelacd status` suspect that
// mamelacd is stopped or stuck.
const stalledAfter = 3

// ReloadToken reads the token file again, and says whether the token changed. A token file that is
// unreadable or empty leaves the token in use as it is (and says why).
func (e *Engine) ReloadToken() (changed bool, err error) {
	b, err := os.ReadFile(e.Config.TokenFile)
	if err != nil {
		return false, fmt.Errorf("the GitHub token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return false, fmt.Errorf("the GitHub token file %s is empty: the token in use stays", e.Config.TokenFile)
	}
	if token == e.GitHub.Token {
		return false, nil
	}
	e.GitHub.SetToken(token)
	return true, nil
}

func (e *Engine) sleep(ctx context.Context, d time.Duration, reload <-chan struct{}) (reloaded, ok bool) {
	if e.Sleep != nil {
		return e.Sleep(ctx, d, reload)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false, false
	case <-reload:
		return true, true
	case <-t.C:
		return false, true
	}
}

// loop is what Run keeps between polls: what it has said, and what it has seen.
type loop struct {
	e         *Engine
	started   time.Time
	polls     int64
	quiet     int64 // polls since the last line of the log
	failing   int64 // polls that failed in a row
	failSince time.Time
	lastKey   string // the error the failing polls have (it is said once, then summarised)
	repeated  int64
	lastLine  time.Time
	hb        Heartbeat
}

// errKey tells whether two polls failed in the same way: the text, but not a wait that counts down.
func errKey(err error) string {
	if _, ok := IsRateLimit(err); ok {
		return "rate limit"
	}
	return err.Error()
}

// Run polls until ctx is done: every interval (the shorter active interval while something is under
// way, and not before GitHub's rate limit is over). It is what the daemon is.
//
// It does not talk while nothing happens, but it must be told apart from a hang. So each poll leaves
// a heartbeat in the database (`mamelacd status` shows it); a quiet mamelacd says it is alive every
// AliveEvery; an error that repeats is said once, summarised at the same rate, and its end is said.
// A SIGHUP (reload) or a 401 reads the token file again, so a rotated token needs no restart.
func (e *Engine) Run(ctx context.Context, reload <-chan struct{}) {
	now := e.Store.Now
	l := &loop{e: e, started: now(), lastLine: now()}
	l.hb = Heartbeat{PID: os.Getpid(), Version: version.Current(), StartedMs: l.started.UnixMilli(), IntervalMs: e.Config.IntervalOrDefault().Milliseconds()}
	inner := e.Logf
	say := func(format string, args ...any) {
		l.lastLine, l.quiet = now(), 0
		if inner != nil {
			inner(format, args...)
		}
	}
	e.Logf = say
	defer func() { e.Logf = inner }()
	for ctx.Err() == nil {
		l.poll(ctx)
		reloaded, ok := e.sleep(ctx, e.NextWait(), reload)
		if !ok {
			return
		}
		if reloaded {
			l.reload()
		}
	}
}

// reload reads the token file again (SIGHUP).
func (l *loop) reload() {
	switch changed, err := l.e.ReloadToken(); {
	case err != nil:
		l.e.logf("mamelacd: SIGHUP: %v", err)
	case changed:
		l.e.logf("mamelacd: SIGHUP: the token file was read again: a new token is in use")
	default:
		l.e.logf("mamelacd: SIGHUP: the token file was read again: the token is the same")
	}
}

// poll does one poll and leaves the heartbeat and the log as they should be after it.
func (l *loop) poll(ctx context.Context) {
	e := l.e
	start := e.Store.Now()
	l.polls++
	l.quiet++
	l.hb.Polls, l.hb.PollStartMs, l.hb.PollEndMs, l.hb.Result = l.polls, start.UnixMilli(), 0, "running"
	l.beat(ctx)

	err := e.Once(ctx)
	if IsAuth(err) {
		// The token file may hold the new token already (it was rotated): use it at once.
		if changed, rerr := e.ReloadToken(); rerr == nil && changed {
			e.logf("mamelacd: GitHub refused the token, and the token file holds another one: using it")
			err = e.Once(ctx)
		}
	}
	end := e.Store.Now()
	took := end.Sub(start)
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return // stopping: not a failed poll
	}
	l.finish(ctx, err, took)
	l.hb.PollEndMs, l.hb.DurationMs = end.UnixMilli(), took.Milliseconds()
	l.hb.NextMs = end.Add(e.NextWait()).UnixMilli()
	l.beat(ctx)
}

// finish decides what a poll's end says in the log.
func (l *loop) finish(ctx context.Context, err error, took time.Duration) {
	e, now := l.e, l.e.Store.Now()
	every := e.AliveEvery
	if every <= 0 {
		every = defaultAliveEvery
	}
	if err == nil {
		l.hb.Result, l.hb.Detail = "ok", ""
		switch {
		case l.failing > 0:
			e.logf("mamelacd: polls work again after %d that failed since %s", l.failing, l.failSince.UTC().Format("15:04:05Z"))
			l.failing, l.lastKey, l.repeated = 0, "", 0
		case now.Sub(l.lastLine) >= every:
			e.logf("mamelacd: alive: %d poll(s) since the last line, all fine; the last took %s; next in %s%s",
				l.quiet, took.Round(time.Millisecond), e.NextWait(), l.headsSummary(ctx))
		}
		return
	}
	first := strings.Join(strings.Fields(strings.SplitN(err.Error(), "\n", 2)[0]), " ")
	l.hb.Result, l.hb.Detail = "error", clipDetail(first)
	if l.failing == 0 {
		l.failSince = now
	}
	l.failing++
	switch key := errKey(err); {
	case key != l.lastKey:
		if l.repeated > 0 {
			e.logf("mamelacd: the error before repeated %d more time(s)", l.repeated)
		}
		l.lastKey, l.repeated = key, 0
		e.logf("mamelacd: %v", err)
	default:
		l.repeated++
		if now.Sub(l.lastLine) >= every {
			e.logf("mamelacd: still failing the same way, %d poll(s) since %s (said once above): %s", l.failing, l.failSince.UTC().Format("15:04:05Z"), first)
			l.repeated = 0
		}
	}
	l.hb.Failing, l.hb.FailingSince = l.failing, l.failSince.UnixMilli()
}

func clipDetail(s string) string {
	if r := []rune(s); len(r) > 300 {
		return string(r[:299]) + "…"
	}
	return s
}

// headsSummary is " ; infra in-sync at 0a1b2c3, lab/web pending at 0a1b2c3", for the alive line.
func (l *loop) headsSummary(ctx context.Context) string {
	heads, err := l.e.Store.Heads(ctx, l.e.Config.Repo)
	if err != nil || len(heads) == 0 {
		return ""
	}
	var parts []string
	for _, h := range heads {
		parts = append(parts, fmt.Sprintf("%s %s at %s", h.Dir, h.State, short7(h.SHA)))
	}
	return "; " + strings.Join(parts, ", ")
}

func short7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// beat writes the heartbeat. Failing to is said, and does not stop the poll.
func (l *loop) beat(ctx context.Context) {
	l.hb.Failing, l.hb.FailingSince = l.failing, l.failSince.UnixMilli()
	if l.failing == 0 {
		l.hb.FailingSince = 0
	}
	if err := l.e.Store.SetHeartbeat(ctx, l.hb); err != nil && ctx.Err() == nil {
		l.e.logf("mamelacd: the heartbeat could not be written to the database: %v", err)
	}
}
