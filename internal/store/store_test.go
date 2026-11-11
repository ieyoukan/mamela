package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
)

const homelab = `{"resources":[{"kind":"TestVM","name":"app-server","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}},{"kind":"NodePackage","name":"app-server/nginx","spec":{"node":"app-server","package":"nginx","state":"Present"}},{"kind":"NodeService","name":"app-server/nginx","spec":{"enabled":true,"node":"app-server","running":true,"service":"nginx"}}],"schema":"mamela-graph/v2"}`

// fakeClock is a settable clock safe for concurrent use.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func open(t *testing.T) (*Store, *fakeClock, string) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "mamela.db")
	s, err := Open(context.Background(), path, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clock, path
}

func graphFixture(t *testing.T) *graph.Graph {
	t.Helper()
	g, err := graph.Decode([]byte(homelab))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestReopenKeepsDataAndSchema(t *testing.T) {
	ctx := context.Background()
	s, clock, path := open(t)
	d, err := s.PutRevision(ctx, graphFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(ctx, path, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	g, err := s2.Revision(ctx, d)
	if err != nil || g.Digest() != d {
		t.Fatalf("Revision after reopen: %v", err)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	s, clock, path := open(t)
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, path, clock.Now); err == nil {
		t.Fatal("opened a database from a newer Mamela")
	}
}

func TestRevisionIsIdempotentAndTamperEvident(t *testing.T) {
	ctx := context.Background()
	s, _, _ := open(t)
	g := graphFixture(t)
	d1, _ := s.PutRevision(ctx, g)
	d2, err := s.PutRevision(ctx, g)
	if err != nil || d1 != d2 {
		t.Fatalf("second put: %v %s %s", err, d1, d2)
	}
	tampered := []byte(`{"resources":[],"schema":"mamela-graph/v1"}`)
	if _, err := s.db.ExecContext(ctx, `UPDATE revisions SET canonical = ? WHERE digest = ?`, tampered, d1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revision(ctx, d1); err == nil {
		t.Fatal("a revision whose content changed was accepted")
	}
	if _, err := s.Revision(ctx, "sha256:missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing revision: %v", err)
	}
}

func TestPlanIsRecomputedOnLoad(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	g := graphFixture(t)
	snap := state.Empty(clock.Now())
	p := plan.Build(g, snap, policy.Default)
	if err := s.SavePlan(ctx, g, snap, policy.Default, p); err != nil {
		t.Fatal(err)
	}
	got, inputs, err := s.LoadPlan(ctx, p.Digest)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if got.Digest != p.Digest || inputs.Revision.Digest() != g.Digest() {
		t.Fatalf("loaded plan differs")
	}
	// Changing the stored plan text is detected: the plan is re-derived, not trusted.
	if _, err := s.db.ExecContext(ctx, `UPDATE plans SET canonical = '{}' WHERE digest = ?`, p.Digest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoadPlan(ctx, p.Digest); !errors.Is(err, ErrPlanMismatch) || !strings.Contains(err.Error(), "were changed") {
		t.Fatalf("tampered plan: %v", err)
	}
	// Made by another Mamela: the error says so, since a different planner gives a different plan.
	if _, err := s.db.ExecContext(ctx, `UPDATE plans SET made_by = 'abc123' WHERE digest = ?`, p.Digest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoadPlan(ctx, p.Digest); !errors.Is(err, ErrPlanMismatch) || !strings.Contains(err.Error(), "made by Mamela abc123") {
		t.Fatalf("plan of another version: %v", err)
	}
}

func TestApprovalExpires(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	g := graphFixture(t)
	snap := state.Empty(clock.Now())
	p := plan.Build(g, snap, policy.Default)
	if err := s.SavePlan(ctx, g, snap, policy.Default, p); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.ValidApproval(ctx, p.Digest); a != nil {
		t.Fatal("approved before approval")
	}
	if _, err := s.Approve(ctx, p.Digest, "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if a, err := s.ValidApproval(ctx, p.Digest); err != nil || a == nil || a.Approver != "alice" {
		t.Fatalf("approval: %v %+v", err, a)
	}
	clock.Advance(time.Hour)
	if a, _ := s.ValidApproval(ctx, p.Digest); a != nil {
		t.Fatal("expired approval still valid")
	}
	if _, err := s.Approve(ctx, "sha256:no-such-plan", "alice", time.Hour); err == nil {
		t.Fatal("approved a plan that does not exist")
	}
}

func savedPlan(t *testing.T, s *Store, now time.Time) *plan.Plan {
	t.Helper()
	g := graphFixture(t)
	snap := state.Empty(now)
	p := plan.Build(g, snap, policy.Default)
	if err := s.SavePlan(context.Background(), g, snap, policy.Default, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestJournalClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	p := savedPlan(t, s, clock.Now())
	o, err := s.Intend(ctx, "op1", p.Digest, "TestVM app-server", "create-vm")
	if err != nil || o.Stage != StageIntended {
		t.Fatalf("Intend: %v %+v", err, o)
	}
	// Intend again keeps the existing entry.
	if o, _ := s.Intend(ctx, "op1", p.Digest, "TestVM app-server", "create-vm"); o.Stage != StageIntended {
		t.Fatalf("second Intend changed the entry: %+v", o)
	}
	if ok, _ := s.Claim(ctx, "op1", "worker-a", time.Minute); !ok {
		t.Fatal("first claim failed")
	}
	if ok, _ := s.Claim(ctx, "op1", "worker-b", time.Minute); ok {
		t.Fatal("second claim succeeded while the lease is held")
	}
	if err := s.Finish(ctx, "op1", "worker-b", StageSucceeded, ""); err == nil {
		t.Fatal("a non-owner finished the operation")
	}
	// The lease expires (worker-a died): worker-b may take over, and worker-a may not finish.
	clock.Advance(time.Minute)
	if ok, _ := s.Claim(ctx, "op1", "worker-b", time.Minute); !ok {
		t.Fatal("could not take over an expired lease")
	}
	if err := s.Finish(ctx, "op1", "worker-a", StageSucceeded, ""); err == nil {
		t.Fatal("the old owner overwrote the new owner's claim")
	}
	if err := s.Finish(ctx, "op1", "worker-b", StageSucceeded, "created"); err != nil {
		t.Fatal(err)
	}
	o, _ = s.Operation(ctx, "op1")
	if o.Stage != StageSucceeded || o.Attempt != 2 || o.Owner != "worker-b" {
		t.Fatalf("final entry: %+v", o)
	}
	if ok, _ := s.Claim(ctx, "op1", "worker-c", time.Minute); ok {
		t.Fatal("claimed a succeeded operation")
	}
}

func TestJournalConcurrentClaims(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	p := savedPlan(t, s, clock.Now())
	if _, err := s.Intend(ctx, "op1", p.Digest, "r", "create-vm"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.Claim(ctx, "op1", "w"+string(rune('a'+i)), time.Minute)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d workers claimed the same operation", wins)
	}
}

func TestConfirmDoesNotStealALiveClaim(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	p := savedPlan(t, s, clock.Now())
	s.Intend(ctx, "op1", p.Digest, "r", "create-vm")
	s.Claim(ctx, "op1", "worker-a", time.Minute)
	if err := s.Confirm(ctx, "op1", "observed"); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.Operation(ctx, "op1"); o.Stage != StageStarted {
		t.Fatalf("Confirm overrode a live claim: %+v", o)
	}
	clock.Advance(time.Minute)
	s.Confirm(ctx, "op1", "observed")
	if o, _ := s.Operation(ctx, "op1"); o.Stage != StageSucceeded {
		t.Fatalf("Confirm after the lease expired: %+v", o)
	}
}

func TestEventsAreOrdered(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	p := savedPlan(t, s, clock.Now())
	for _, m := range []string{"one", "two", "three"} {
		if err := s.AppendEvent(ctx, p.Digest, "r", "note", m); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := s.Events(ctx, p.Digest)
	if err != nil || len(evs) != 3 || evs[0].Message != "one" || evs[2].Message != "three" {
		t.Fatalf("events: %v %+v", err, evs)
	}
}

func TestPlanProgramsAndPermits(t *testing.T) {
	ctx := context.Background()
	s, _, _ := open(t)
	g := graphFixture(t)
	snap := state.Empty(time.Unix(0, 0))
	p := plan.Build(g, snap, policy.Default)
	if err := s.SavePlan(ctx, g, snap, policy.Default, p); err != nil {
		t.Fatal(err)
	}
	data := []byte("\x00asm program")
	prog := Program{Provider: "kv", Version: "0.1.0", Source: "example.com/kv", Digest: programDigest(data), Data: data}
	if err := s.SavePlanPrograms(ctx, p.Digest, []Program{{Provider: "kv", Digest: "sha256:00", Data: data}}); err == nil {
		t.Error("a program with the wrong digest was stored")
	}
	if err := s.SavePlanPrograms(ctx, p.Digest, []Program{prog}); err != nil {
		t.Fatal(err)
	}
	got, err := s.PlanPrograms(ctx, p.Digest)
	if err != nil || len(got) != 1 || string(got[0].Data) != string(data) || got[0].Source != prog.Source {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.UsePermit(ctx, "op-1@1", "op-1", p.Digest); err != nil {
		t.Fatal(err)
	}
	if err := s.UsePermit(ctx, "op-1@1", "op-1", p.Digest); !errors.Is(err, ErrPermitUsed) {
		t.Errorf("a permit issued twice: %v", err)
	}
}

// A node keeps its number; numbers are above every Proxmox VMID and never shared.
func TestNodeNumbers(t *testing.T) {
	st, _, _ := open(t)
	ctx := context.Background()
	a, err := st.NodeNumber(ctx, "web")
	if err != nil || a != FirstNodeNumber {
		t.Fatalf("first %d %v", a, err)
	}
	b, _ := st.NodeNumber(ctx, "db")
	again, _ := st.NodeNumber(ctx, "web")
	if b != a+1 || again != a {
		t.Fatalf("db %d, web again %d", b, again)
	}
}
