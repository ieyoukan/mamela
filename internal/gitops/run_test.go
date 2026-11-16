package gitops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runFor runs the daemon's loop for n polls with a clock that only the loop's waits move, and
// returns the waits asked for. hook runs before each wait (the world changing between polls).
func (f *fixture) runFor(n int, reload func(i int) bool, hook func(i int)) (waits []time.Duration, end time.Time) {
	f.t.Helper()
	cur := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	f.e.Store.clock = func() time.Time { return cur }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.e.Sleep = func(ctx context.Context, d time.Duration, _ <-chan struct{}) (bool, bool) {
		waits = append(waits, d)
		i := len(waits)
		if hook != nil {
			hook(i)
		}
		cur = cur.Add(d)
		if i >= n {
			cancel()
			return false, false
		}
		return reload != nil && reload(i), true
	}
	f.e.GitHub.Now = func() time.Time { return cur }
	f.e.Run(ctx, nil)
	return waits, cur
}

// A8: every poll leaves a heartbeat that `mamelacd status` shows; the normal waiting says nothing
// else.
func TestHeartbeatAndStatus(t *testing.T) {
	f := pushFixture(t)
	logs := f.logged()
	f.push("v1", "alice")
	_, end := f.runFor(3, nil, nil)
	hb, ok, err := f.e.Store.LastHeartbeat(context.Background())
	if err != nil || !ok {
		t.Fatalf("no heartbeat: %v %v", ok, err)
	}
	if hb.Polls != 3 || hb.Result != "ok" || hb.PollEndMs == 0 || hb.PID != os.Getpid() || hb.Failing != 0 {
		t.Fatalf("%+v", hb)
	}
	if want := time.UnixMilli(hb.PollEndMs).Add(time.Minute).UnixMilli(); hb.NextMs != want {
		t.Fatalf("next poll %d, want %d", hb.NextMs, want)
	}
	// 30 seconds after the last poll: fine.
	var out bytes.Buffer
	healthy, err := Status(context.Background(), f.e.Config, end.Add(-30*time.Second), &out)
	if err != nil || !healthy {
		t.Fatalf("not healthy: %v\n%s", err, out.String())
	}
	for _, want := range []string{"me/infra (main)", "last finished", "ok, took", "in 30s", "infra", "in-sync", "@alice id ", "pid "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
	// Longer than 3 intervals without a poll: stalled.
	out.Reset()
	healthy, err = Status(context.Background(), f.e.Config, end.Add(10*time.Minute), &out)
	if err != nil || healthy || !strings.Contains(out.String(), "PROBLEM: no poll finished for") || !strings.Contains(out.String(), "MAM-GIT-0501") ||
		!strings.Contains(out.String(), "journalctl -u mamelacd") {
		t.Fatalf("a stalled mamelacd was not said to be: %v\n%s", healthy, out.String())
	}
	// Exactly at 3 intervals: not yet.
	out.Reset()
	if healthy, _ := Status(context.Background(), f.e.Config, time.UnixMilli(hb.PollEndMs).Add(2*time.Minute+30*time.Second), &out); !healthy {
		t.Fatalf("stalled too soon:\n%s", out.String())
	}
	// Nothing was said by the waiting itself.
	for _, l := range *logs {
		if strings.Contains(l, "alive") {
			t.Fatalf("said alive after 3 minutes: %v", *logs)
		}
	}
}

// A8: a poll that is running is shown as running, not as stalled.
func TestStatusOfARunningPoll(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	f.e.Store.SetHeartbeat(context.Background(), Heartbeat{PID: 1, Version: "x", StartedMs: now.Add(-time.Hour).UnixMilli(), PollStartMs: now.Add(-20 * time.Minute).UnixMilli(),
		Result: "running", IntervalMs: time.Minute.Milliseconds(), Polls: 5})
	var out bytes.Buffer
	healthy, err := Status(context.Background(), f.e.Config, now, &out)
	if err != nil || !healthy || !strings.Contains(out.String(), "running since") || !strings.Contains(out.String(), "a long apply or a call that hangs") {
		t.Fatalf("%v %v\n%s", healthy, err, out.String())
	}
}

// A8: no heartbeat, no database.
func TestStatusWithoutHeartbeatOrDatabase(t *testing.T) {
	f := newFixture(t)
	var out bytes.Buffer
	healthy, err := Status(context.Background(), f.e.Config, time.Now(), &out)
	if err != nil || healthy || !strings.Contains(out.String(), "no poll has been recorded") {
		t.Fatalf("%v %v\n%s", healthy, err, out.String())
	}
	cfg := f.e.Config
	cfg.StateDB = filepath.Join(t.TempDir(), "nothing.db")
	if _, err := Status(context.Background(), cfg, time.Now(), &out); err == nil || !strings.Contains(err.Error(), "no state database") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(cfg.StateDB); err == nil {
		t.Fatal("status created a database")
	}
}

// A8: a quiet mamelacd says it is alive every ten minutes, in one line, and not more.
func TestAliveLineIsRare(t *testing.T) {
	f := pushFixture(t)
	logs := f.logged()
	f.push("v1", "alice")
	f.runFor(25, nil, nil) // 25 minutes
	alive := 0
	for _, l := range *logs {
		if strings.Contains(l, "mamelacd: alive:") {
			alive++
			if strings.Contains(l, "\n") || !strings.Contains(l, "infra in-sync at") || !strings.Contains(l, "next in 1m0s") {
				t.Errorf("the alive line: %q", l)
			}
		}
	}
	if alive != 2 {
		t.Fatalf("%d alive lines in 25 minutes (every 10): %v", alive, *logs)
	}
	if len(*logs) > 8 {
		t.Fatalf("too many lines for a quiet mamelacd: %v", *logs)
	}
}

// A8 (A7c): a failure that goes on is said once, with what to do, and summarised at the alive rate;
// when it ends, that is said once.
func TestRepeatedErrorIsSaidOnce(t *testing.T) {
	f := pushFixture(t)
	logs := f.logged()
	f.e.GitHub.TokenFile = "/etc/mamelacd/github-token"
	f.gh.SetToken("another")
	f.runFor(35, nil, func(i int) {
		if i == 30 {
			f.gh.SetToken("token")
		}
	})
	if n := count(*logs, "mamelacd: MAM-GIT-0001"); n != 1 {
		t.Fatalf("the refused token was said %d times: %v", n, *logs)
	}
	if n := count(*logs, "still failing the same way"); n < 2 || n > 3 {
		t.Fatalf("summarised %d times in 30 minutes: %v", n, *logs)
	}
	if n := count(*logs, "polls work again after 30"); n != 1 {
		t.Fatalf("the end of the failure was said %d times: %v", n, *logs)
	}
	if len(*logs) > 8 {
		t.Fatalf("%d lines: %v", len(*logs), *logs)
	}
	// While it fails, status says so.
	f.gh.SetToken("another")
	f.runFor(3, nil, nil)
	var out bytes.Buffer
	healthy, _ := Status(context.Background(), f.e.Config, f.e.Store.Now(), &out)
	if healthy || !strings.Contains(out.String(), "PROBLEM: the last 3 poll(s) failed") || !strings.Contains(out.String(), "MAM-GIT-0001") {
		t.Fatalf("%v\n%s", healthy, out.String())
	}
}

// A8: a rotated token is picked up with no restart: on a 401 when the file holds another token, and
// at once on SIGHUP (the reload channel).
func TestRotatedTokenIsReloaded(t *testing.T) {
	f := newFixture(t)
	logs := f.logged()
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	f.e.Config.TokenFile = tokenFile
	f.e.GitHub.TokenFile = tokenFile
	os.WriteFile(tokenFile, []byte("token\n"), 0o600)

	// 1: the token is replaced at GitHub and in the file: the 401 is answered by reading the file.
	f.gh.SetToken("second")
	os.WriteFile(tokenFile, []byte("second\n"), 0o600)
	f.runFor(1, nil, nil)
	if f.e.GitHub.Token != "second" || count(*logs, "holds another one: using it") != 1 {
		t.Fatalf("token %q: %v", f.e.GitHub.Token, *logs)
	}
	hb, _, _ := f.e.Store.LastHeartbeat(context.Background())
	if hb.Result != "ok" {
		t.Fatalf("the poll after the reload: %+v", hb)
	}

	// 2: the token is replaced after a poll; SIGHUP makes the next poll use it with no 401 in between.
	f.runFor(2, func(i int) bool { return i == 1 }, func(i int) {
		if i == 1 {
			f.gh.SetToken("third")
			os.WriteFile(tokenFile, []byte("third\n"), 0o600)
		}
	})
	if f.e.GitHub.Token != "third" || count(*logs, "SIGHUP: the token file was read again: a new token is in use") != 1 || count(*logs, "holds another one") != 1 {
		t.Fatalf("token %q: %v", f.e.GitHub.Token, *logs)
	}
	if hb, _, _ := f.e.Store.LastHeartbeat(context.Background()); hb.Result != "ok" || hb.Failing != 0 {
		t.Fatalf("a poll failed between: %+v", hb)
	}

	// An empty or missing file leaves the token alone, and says why.
	os.WriteFile(tokenFile, []byte("\n"), 0o600)
	if changed, err := f.e.ReloadToken(); changed || err == nil || !strings.Contains(err.Error(), "empty") || f.e.GitHub.Token != "third" {
		t.Fatalf("empty file: %v %v", changed, err)
	}
	os.Remove(tokenFile)
	if changed, err := f.e.ReloadToken(); changed || err == nil || f.e.GitHub.Token != "third" {
		t.Fatalf("missing file: %v %v", changed, err)
	}
}

// A7c: the loop waits for GitHub's rate limit to be over.
func TestRunWaitsOutARateLimit(t *testing.T) {
	f := newFixture(t)
	logs := f.logged()
	cur := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	f.gh.SetRateLimit(3, cur.Add(20*time.Minute)) // the fake's own clock is elsewhere: only the header matters
	waits, _ := f.runFor(1, nil, nil)
	if len(waits) != 1 || waits[0] < 19*time.Minute {
		t.Fatalf("waited %v for a limit of 20 minutes: %v", waits, *logs)
	}
	if count(*logs, "MAM-GIT-0003") != 1 {
		t.Fatalf("%v", *logs)
	}
}

// B11: `mamelacd repin` makes the next poll pin the account the login has then.
func TestRepin(t *testing.T) {
	f := newFixture(t)
	f.once()
	var out bytes.Buffer
	if err := Repin(context.Background(), f.e.Config, "alice", &out); err != nil || !strings.Contains(out.String(), "forgot the account pinned for @alice") {
		t.Fatalf("%v %s", err, out.String())
	}
	if err := Repin(context.Background(), f.e.Config, "alice", &out); err == nil {
		t.Fatal("a pin that is not there was forgotten")
	}
	f.once()
	ps, _ := f.e.Store.Principals(context.Background())
	pinned := false
	for _, p := range ps {
		pinned = pinned || p.Login == "alice"
	}
	if !pinned {
		t.Fatalf("not pinned again: %+v", ps)
	}
}
