package store

import (
	"context"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// Each created object belongs to the declaration it was made for; records that disagree, or that
// name none, are trusted by nobody. Objects are numbered by when they were first created.
func TestCreationStackAndOrder(t *testing.T) {
	s, _, _ := open(t)
	ctx := context.Background()
	rec := func(stack, resource, id string) {
		t.Helper()
		if err := s.RecordCreation(ctx, stack, "test", resource, map[string]string{"id": id}, "plan", "op"); err != nil {
			t.Fatal(err)
		}
	}
	rec("infra", "Machine db", "1")
	rec("infra", "Machine web", "2")
	rec("", "Machine old", "3")
	rec("a", "Machine both", "4")
	rec("b", "Machine both", "5")
	for resource, want := range map[string]string{"Machine db": "infra", "Machine web": "infra", "Machine old": "", "Machine both": "", "Machine none": ""} {
		if got, err := s.CreationStack(ctx, "test", resource); err != nil || got != want {
			t.Errorf("CreationStack(%s) = %q, %v; want %q", resource, got, err, want)
		}
	}
	order, err := s.CreationOrder(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !(order["Machine db"] < order["Machine web"] && order["Machine web"] > 0) {
		t.Errorf("order %v: what was made later has the higher number", order)
	}
}

// An object that is gone after a deletion whose answer was lost completes the deletion the way it
// would have: its records end and its machine's registration with them. An object that vanished
// without Mamela deleting it only has its creation records forgotten. Repeating is safe.
func TestFinishRemoval(t *testing.T) {
	s, _, _ := open(t)
	ctx := context.Background()
	token, err := s.IssueEnrollToken(ctx, "web", 7001, "seed", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enroll(ctx, token, "web", 7001, "serial"); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{"machine": "web-1"}
	if err := s.RecordCreation(ctx, "infra", "test", "Machine web", ids, "plan", "create"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCreation(ctx, "infra", "test", "Machine gone", map[string]string{"machine": "g"}, "plan", "create"); err != nil {
		t.Fatal(err)
	}
	o := protocol.Observed{ID: "Machine web", Kind: "Machine", Name: "web", Exists: true, Owned: true, IDs: ids, Node: &protocol.ObservedNode{ID: 7001}}
	ch := protocol.Change{Resource: o.ID, Kind: o.Kind, Op: "delete", IDs: ids}
	if err := s.StartObservedExplicit(ctx, "rm-1", "test", "delete", "plan sha256:x", "contract", o, ch); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteExplicit(ctx, Explicit{ID: "rm-1", Provider: "test", Resource: o.ID, Op: "delete", Change: ch,
		Context: ExplicitContext{Observed: o, Providers: "contract", Creations: []map[string]string{ids}, AgentSerial: "serial"}}, "unknown", "answer lost", false); err != nil {
		t.Fatal(err)
	}
	// An earlier attempt of the same deletion, whose worker was killed, is still "started".
	if err := s.StartObservedExplicit(ctx, "rm-0", "test", "delete", "plan sha256:y", "contract", o, ch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE explicit_operations SET started_ms = started_ms - 1000 WHERE id = 'rm-0'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.FinishRemoval(ctx, "test", "Machine web", "delete"); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := s.Agent(ctx, "web"); ok {
		t.Error("the registration of a deleted machine remains")
	}
	for id, want := range map[string]string{"rm-1": "done", "rm-0": "superseded"} {
		if got, _ := s.ExplicitOutcome(ctx, id); got != want {
			t.Errorf("%s ended as %q, want %q", id, got, want)
		}
	}
	if c, _ := s.Creations(ctx, "test", "Machine web"); len(c) != 0 {
		t.Errorf("creation records remain: %v", c)
	}
	if err := s.FinishRemoval(ctx, "test", "Machine gone", "delete"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Creations(ctx, "test", "Machine gone"); len(c) != 0 {
		t.Errorf("an object that vanished keeps its creation records: %v", c)
	}
}
