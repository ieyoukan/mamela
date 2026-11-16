package mamelet

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeWaiter struct {
	serial  int64
	changed bool
	err     error
	took    time.Duration // how long the wait lasted
	after   []int64
	clock   *time.Time
}

func (f *fakeWaiter) WaitChange(_ context.Context, after int64, _ time.Duration) (int64, bool, error) {
	f.after = append(f.after, after)
	*f.clock = f.clock.Add(f.took)
	return f.serial, f.changed, f.err
}

func TestPause(t *testing.T) {
	const interval = 30 * time.Second
	for _, c := range []struct {
		name      string
		w         func(clock *time.Time) Waiter
		wantSleep time.Duration // what is slept after the wait
		wantSeen  int64
	}{
		{"no waiter: the interval", func(*time.Time) Waiter { return nil }, interval, 5},
		{"a newer revision: after the minimum gap", func(c *time.Time) Waiter {
			return &fakeWaiter{serial: 9, changed: true, took: 2 * time.Second, clock: c}
		}, 0, 9}, // 2s have passed: more than the gap
		{"a newer revision at once", func(c *time.Time) Waiter { return &fakeWaiter{serial: 9, changed: true, clock: c} }, minGap, 9},
		{"nothing new, the whole wait", func(c *time.Time) Waiter { return &fakeWaiter{serial: 5, took: interval, clock: c} }, 0, 5},
		{"nothing new, answered at once: the rest of the interval", func(c *time.Time) Waiter { return &fakeWaiter{serial: 5, took: time.Second, clock: c} }, interval - time.Second, 5},
		{"an older Control Plane: the interval", func(c *time.Time) Waiter {
			return &fakeWaiter{err: errors.New("404 Not Found"), took: 100 * time.Millisecond, clock: c}
		}, interval - 100*time.Millisecond, 5},
	} {
		clock := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
		var slept time.Duration
		sleep := func(_ context.Context, d time.Duration) error { slept += d; return nil }
		seen, err := Pause(context.Background(), c.w(&clock), 5, interval, func() time.Time { return clock }, sleep)
		if err != nil || seen != c.wantSeen || slept != c.wantSleep {
			t.Errorf("%s: seen %d slept %v err %v, want seen %d slept %v", c.name, seen, slept, err, c.wantSeen, c.wantSleep)
		}
	}
}

// A stopped mamelet leaves the pause at once.
func TestPauseStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clock := time.Now()
	_, err := Pause(ctx, &fakeWaiter{err: ctx.Err(), clock: &clock}, 1, time.Second, time.Now, func(ctx context.Context, d time.Duration) error { return ctx.Err() })
	if err == nil {
		t.Fatal("a cancelled context did not stop the pause")
	}
}
