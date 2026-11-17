package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func eventKinds(t *testing.T, s *Store, digest string) []string {
	t.Helper()
	evs, err := s.Events(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func countApprovals(t *testing.T, s *Store, digest string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM approvals WHERE plan_digest = ?`, digest).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Approving a plan that is approved already must not change who approved it or until when: the
// record is a record (B11). The request is kept as an event.
func TestReapprovalKeepsTheFirstApproval(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	p := savedPlan(t, s, clock.Now())
	first, err := s.Approve(ctx, p.Digest, "github:alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Minute)
	again, err := s.Approve(ctx, p.Digest, "github:bob", 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Kept || again.Approver != "github:alice" || !again.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("the second approval changed the record: %+v (first %+v)", again, first)
	}
	a, err := s.ValidApproval(ctx, p.Digest)
	if err != nil || a == nil || a.Approver != "github:alice" || !a.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("the approval now: %+v %v", a, err)
	}
	if n := countApprovals(t, s, p.Digest); n != 1 {
		t.Fatalf("%d approval records, want the one", n)
	}
	if got := eventKinds(t, s, p.Digest); !slices.Equal(got, []string{"approved", "approval-kept"}) {
		t.Fatalf("events %v", got)
	}
}

// An approval that has expired is not kept: approving again records a new one beside it.
func TestExpiredApprovalIsApprovedAnew(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	p := savedPlan(t, s, clock.Now())
	if _, err := s.Approve(ctx, p.Digest, "github:alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	a, err := s.Approve(ctx, p.Digest, "github:bob", time.Hour)
	if err != nil || a.Kept || a.Approver != "github:bob" {
		t.Fatalf("%+v %v", a, err)
	}
	if v, _ := s.ValidApproval(ctx, p.Digest); v == nil || v.Approver != "github:bob" {
		t.Fatalf("valid approval %+v", v)
	}
	if n := countApprovals(t, s, p.Digest); n != 2 {
		t.Fatalf("%d approval records, want both", n)
	}
}

// A revoked approval is not valid; the approval and its revocation stay as records, and the plan can
// be approved again (B3).
func TestRevokedApprovalIsNotValid(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	p := savedPlan(t, s, clock.Now())
	if _, err := s.Revoke(ctx, p.Digest, "carol", ""); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("revoking what is not approved: %v", err)
	}
	if _, err := s.Approve(ctx, p.Digest, "github:alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	was, err := s.Revoke(ctx, p.Digest, "carol", "the policy changed")
	if err != nil || was.Approver != "github:alice" {
		t.Fatalf("revoke: %+v %v", was, err)
	}
	if a, _ := s.ValidApproval(ctx, p.Digest); a != nil {
		t.Fatalf("a revoked approval is valid: %+v", a)
	}
	if _, err := s.Revoke(ctx, p.Digest, "carol", ""); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("revoking twice: %v", err)
	}
	if n := countApprovals(t, s, p.Digest); n != 1 {
		t.Fatalf("the approval record is gone (%d)", n)
	}
	var by, reason string
	if err := s.db.QueryRow(`SELECT revoked_by, reason FROM approval_revocations`).Scan(&by, &reason); err != nil || by != "carol" || reason != "the policy changed" {
		t.Fatalf("revocation record %q %q %v", by, reason, err)
	}
	// Approving anew is a new record, valid by itself.
	if a, err := s.Approve(ctx, p.Digest, "github:dave", time.Hour); err != nil || a.Kept || a.Approver != "github:dave" {
		t.Fatalf("approve again: %+v %v", a, err)
	}
	if v, _ := s.ValidApproval(ctx, p.Digest); v == nil || v.Approver != "github:dave" {
		t.Fatalf("valid approval %+v", v)
	}
	want := []string{"approved", "approval-revoked", "approved"}
	if got := eventKinds(t, s, p.Digest); !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// A database from before approvals were appended keeps its approvals through the migration.
func TestMigrationKeepsApprovals(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	from := len(migrations) - 1
	seedAt(t, path, from)
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, q := range []string{
		`INSERT INTO revisions VALUES('r', x'00', 1)`, `INSERT INTO snapshots VALUES('s', x'00', 1)`, `INSERT INTO policies VALUES('p', x'00')`,
		`INSERT INTO plans(digest, revision_digest, snapshot_digest, policy_digest, canonical, created_ms) VALUES('sha256:old', 'r', 's', 'p', x'00', 1)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO approvals VALUES('sha256:old', 'github:alice', ?, ?)`, now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	s, err := Open(ctx, path, func() time.Time { return now.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.ValidApproval(ctx, "sha256:old")
	if err != nil || a == nil || a.Approver != "github:alice" || !a.ApprovedAt.Equal(now) || !a.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("the approval after the migration: %+v %v", a, err)
	}
}
