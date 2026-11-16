package mamelet

import (
	"context"
	"time"
)

// Waiter waits for a revision newer than a serial: it returns the newest serial and whether it is
// newer than the one given. Client is one.
type Waiter interface {
	WaitChange(ctx context.Context, serial int64, d time.Duration) (int64, bool, error)
}

// minGap is the shortest pause between two rounds: a revision that keeps being newer (one mamelet
// refuses, for example) does not make it spin.
const minGap = time.Second

// Pause waits for the next round: until the Control Plane has a revision newer than after, or the
// interval has passed, whichever is first. A Control Plane that does not know the waiting request
// (an older one), or an error, make it an ordinary pause of the interval. It returns the newest
// serial seen: pass it as after next time, so that a revision that was seen and refused does not
// end every pause at once. sleep is time.Sleep that stops with the context (a parameter for tests).
func Pause(ctx context.Context, w Waiter, after int64, interval time.Duration, now func() time.Time, sleep func(context.Context, time.Duration) error) (int64, error) {
	start := now()
	rest := func(total time.Duration) error {
		if left := total - now().Sub(start); left > 0 {
			return sleep(ctx, left)
		}
		return ctx.Err()
	}
	if w == nil {
		return after, rest(interval)
	}
	serial, changed, err := w.WaitChange(ctx, after, interval)
	switch {
	case ctx.Err() != nil:
		return after, ctx.Err()
	case err != nil:
		return after, rest(interval)
	case changed:
		return max(after, serial), rest(minGap)
	}
	// Nothing new: the wait was the pause, unless the Control Plane answered at once.
	if now().Sub(start) < interval*3/4 {
		return max(after, serial), rest(interval)
	}
	return max(after, serial), nil
}
