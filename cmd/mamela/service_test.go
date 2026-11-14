package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/reconcile"
)

// The API reads how an apply ended from the result, not from the text it printed.
func TestApplyResult(t *testing.T) {
	stale := &reconcile.Stop{Code: reasonStale}
	other := &reconcile.Stop{Code: "MAM-REC-0004"}
	for _, c := range []struct {
		name        string
		res         applyResult
		ok, isStale bool
	}{
		{"clean", applyResult{}, true, false},
		{"stopped because the world changed", applyResult{Stopped: stale}, false, true},
		{"stopped for another reason", applyResult{Stopped: other}, false, false},
		{"a guard blocked before", applyResult{BlockedBefore: true}, false, false},
		{"a guard blocked after", applyResult{BlockedAfter: true}, false, false},
		{"blocked after a stale stop", applyResult{BlockedAfter: true, Stopped: stale}, false, true},
	} {
		if c.res.OK() != c.ok || c.res.Stale() != c.isStale {
			t.Errorf("%s: OK=%v Stale=%v, want %v %v", c.name, c.res.OK(), c.res.Stale(), c.ok, c.isStale)
		}
	}
}

// An error that already says what to do is shown as it is; others get the CLI's prefix.
func TestRendered(t *testing.T) {
	if got := rendered(told{"cannot approve x: nope"}); got != "cannot approve x: nope" {
		t.Errorf("told: %q", got)
	}
	if got := rendered(fmt.Errorf("wrapped: %w", errors.New("boom"))); got != "mamela: wrapped: boom" {
		t.Errorf("plain: %q", got)
	}
	if got := rendered(&noPlanError{"sha256:ab"}); !strings.HasPrefix(got, "mamela: no plan sha256:ab") {
		t.Errorf("no plan: %q", got)
	}
}
