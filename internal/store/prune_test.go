package store

import (
	"context"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/state"
)

func count(t *testing.T, s *Store, table, where string, args ...any) int {
	t.Helper()
	var n int
	q := `SELECT COUNT(*) FROM ` + table
	if where != "" {
		q += ` WHERE ` + where
	}
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Plans nobody approved or applied, and the inputs only they used, go after the retention; what was
// approved or applied, and what is recent, stays (B6: nothing used to be deleted at all).
func TestPruneDeletesOldUnapprovedPlansOnly(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	day0 := clock.Now()
	unapproved := savedPlan(t, s, day0)
	approved := savedPlan(t, s, day0.Add(time.Second))
	if _, err := s.Approve(ctx, approved.Digest, "github:alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	applied := savedPlan(t, s, day0.Add(2*time.Second))
	if _, err := s.Intend(ctx, "op-1", applied.Digest, "FakeVM web", "create-vm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSnapshot(ctx, state.Empty(day0.Add(time.Hour))); err != nil { // apply's own observation
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, unapproved.Digest, "", "observed", "an event of the plan to go"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, "", "", "note", "an event of no plan"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(40 * 24 * time.Hour)
	recent := savedPlan(t, s, clock.Now())
	if _, err := s.PutSnapshot(ctx, state.Empty(clock.Now().Add(time.Second))); err != nil { // a recent observation of apply
		t.Fatal(err)
	}

	n, err := s.Prune(ctx, clock.Now().Add(-Retention))
	if err != nil {
		t.Fatal(err)
	}
	if n.Plans != 1 || n.Events != 1 {
		t.Fatalf("pruned %+v", n)
	}
	for name, p := range map[string]bool{unapproved.Digest: false, approved.Digest: true, applied.Digest: true, recent.Digest: true} {
		if got := count(t, s, "plans", "digest = ?", name) == 1; got != p {
			t.Errorf("plan %.20s kept = %v, want %v", name, got, p)
		}
	}
	// The kept plans still recompute: their inputs were not touched.
	for _, d := range []string{approved.Digest, applied.Digest, recent.Digest} {
		if _, _, err := s.LoadPlan(ctx, d); err != nil {
			t.Errorf("a kept plan no longer loads: %v", err)
		}
	}
	if a, _ := s.ValidApproval(ctx, approved.Digest); a != nil {
		t.Error("expired anyway") // the approval is long expired: only its record must remain
	}
	if count(t, s, "approvals", "") != 1 {
		t.Error("an approval record was deleted")
	}
	// Observations: the unapproved plan's and the old orphan went; the other plans' and the recent orphan stay.
	if got := count(t, s, "snapshots", ""); got != 3+1 { // approved, applied, recent; and the recent orphan
		t.Errorf("%d snapshots left", got)
	}
	if count(t, s, "revisions", "") != 1 || count(t, s, "policies", "") != 1 {
		t.Error("an input that kept plans use was deleted")
	}
	if count(t, s, "events", "kind = 'note'") != 1 {
		t.Error("an event of no plan was deleted")
	}

	// Nothing is left to delete the second time.
	if n, err := s.Prune(ctx, clock.Now().Add(-Retention)); err != nil || n != (Pruned{}) {
		t.Errorf("second prune: %+v %v", n, err)
	}
}

// What only a pruned plan used goes with it (a provider program, an observation, a revision); what
// another plan uses stays until that plan goes too.
func TestPruneDeletesInputsNoPlanUsesAnyMore(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	old := savedPlan(t, s, clock.Now())
	shared := Program{Provider: "p", Version: "1", Source: "x", Data: []byte("program bytes")}
	shared.Digest = programDigest(shared.Data)
	own := Program{Provider: "q", Version: "1", Source: "y", Data: []byte("another program")}
	own.Digest = programDigest(own.Data)
	if err := s.SavePlanPrograms(ctx, old.Digest, []Program{shared, own}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(40 * 24 * time.Hour)
	recent := savedPlan(t, s, clock.Now())
	if err := s.SavePlanPrograms(ctx, recent.Digest, []Program{shared}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Prune(ctx, clock.Now().Add(-Retention)); err != nil {
		t.Fatal(err)
	}
	if count(t, s, "provider_programs", "digest = ?", own.Digest) != 0 || count(t, s, "provider_programs", "digest = ?", shared.Digest) != 1 {
		t.Errorf("programs: %d left, want the shared one", count(t, s, "provider_programs", ""))
	}
	if count(t, s, "plans", "") != 1 || count(t, s, "revisions", "") != 1 || count(t, s, "snapshots", "") != 1 {
		t.Error("what the recent plan uses was deleted, or what only the old one used was kept")
	}
	if _, _, err := s.LoadPlan(ctx, recent.Digest); err != nil {
		t.Fatal(err)
	}

	clock.Advance(Retention + time.Hour) // now the recent plan is old too
	if _, err := s.Prune(ctx, clock.Now().Add(-Retention)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"plans", "revisions", "snapshots", "policies", "plan_programs", "provider_programs"} {
		if n := count(t, s, table, ""); n != 0 {
			t.Errorf("%s: %d rows left", table, n)
		}
	}
}

// It runs at most once an hour, whichever program asks.
func TestPruneIfDueIsThrottled(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	savedPlan(t, s, clock.Now())
	clock.Advance(40 * 24 * time.Hour)
	if _, ran, err := s.PruneIfDue(ctx, Retention); err != nil || !ran {
		t.Fatalf("first: %v %v", ran, err)
	}
	if n, ran, err := s.PruneIfDue(ctx, Retention); err != nil || ran {
		t.Fatalf("second, at once: %v %v %+v", ran, err, n)
	}
	clock.Advance(pruneEvery)
	if _, ran, err := s.PruneIfDue(ctx, Retention); err != nil || !ran {
		t.Fatalf("an hour later: %v %v", ran, err)
	}
}
