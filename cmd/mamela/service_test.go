package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// testBackend is the API's backend over the database at db, the way `mamela api` makes it.
func testBackend(t *testing.T, db, providers string) *apiBackend {
	t.Helper()
	toolDir := toolchainDir(t)
	found, err := (toolFlags{dir: &toolDir, pomeCache: new(string)}).find()
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &apiBackend{st: st, tools: found, providers: providers}
}

// What the API returns is posted on GitHub: the Control Plane's local path of the declaration
// stays out of it, whether planning or evaluating fails.
func TestAPIBackendHidesThePath(t *testing.T) {
	b := testBackend(t, filepath.Join(t.TempDir(), "state.db"), "")
	dir := filepath.Join(t.TempDir(), "extracted-tree", "declaration")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := b.Plan(context.Background(), dir)
	if err == nil || strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "<declaration>") {
		t.Errorf("plan: %v", err)
	}
	_, err = b.Revision(context.Background(), dir)
	if err == nil || strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "<declaration>") {
		t.Errorf("revision: %v", err)
	}
}
