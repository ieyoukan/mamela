package store

import (
	"context"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/provider/protocol"
)

func TestExplicitAbsenceRevokesOnlyOriginalNode(t *testing.T) {
	for _, recreated := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "re-enrolled"}[recreated], func(t *testing.T) {
			s, _, _ := open(t)
			ctx := context.Background()
			for i, name := range []string{"web", "other"} {
				token, err := s.IssueEnrollToken(ctx, name, 7001+i, "seed", time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Enroll(ctx, token, name, 7001+i, "original"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.PublishNodeRevision(ctx, name, func(*noderev.Revision) (noderev.Revision, error) { return noderev.Revision{VMID: 7001 + i}, nil }); err != nil {
					t.Fatal(err)
				}
				if err := s.PutNodeReport(ctx, noderev.Report{Node: name, VMID: 7001 + i}); err != nil {
					t.Fatal(err)
				}
			}
			ids := map[string]string{"machine": "web-1"}
			if err := s.RecordCreation(ctx, "test", "Machine web", ids, "plan", "create"); err != nil {
				t.Fatal(err)
			}
			o := protocol.Observed{ID: "Machine web", Kind: "Machine", Name: "web", Exists: true, Owned: true, IDs: ids, Node: &protocol.ObservedNode{ID: 7001}}
			ch := protocol.Change{Resource: o.ID, Kind: o.Kind, Op: "delete", IDs: ids}
			if err := s.StartObservedExplicit(ctx, "act-1", "test", "delete", "alice", "contract", o, ch); err != nil {
				t.Fatal(err)
			}
			// The operation may have completed remotely before this process restarted. Its intent
			// retains the object and contract needed for recovery without sending it again.
			e, err := s.Explicit(ctx, "act-1")
			if err != nil {
				t.Fatal(err)
			}
			if e.Outcome != "started" || e.Context.Providers != "contract" {
				t.Fatalf("intent: %+v", e)
			}
			if recreated {
				token, _ := s.IssueEnrollToken(ctx, "web", 7001, "new-seed", time.Hour)
				if err := s.Enroll(ctx, token, "web", 7001, "new-cert"); err != nil {
					t.Fatal(err)
				}
				if err := s.CompleteExplicit(ctx, e, "done", "", true); err == nil {
					t.Fatal("revoked a new identity")
				}
				if a, ok, _ := s.Agent(ctx, "web"); !ok || a.CertSerial != "new-cert" {
					t.Fatal("lost new certificate")
				}
				return
			}
			// A data-losing operation whose object still exists keeps ownership and enrollment.
			if err := s.CompleteExplicit(ctx, e, "done", "object still exists", false); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := s.Agent(ctx, "web"); !ok {
				t.Fatal("revoked an existing object")
			}
			for range 2 {
				if err := s.CompleteExplicit(ctx, e, "done", "absence confirmed", true); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok, _ := s.Agent(ctx, "web"); ok {
				t.Fatal("old certificate remains accepted")
			}
			if err := s.PutAuthenticatedNodeReport(ctx, noderev.Report{Node: "web", VMID: 7001}, "original"); err != ErrAgentRevoked {
				t.Fatalf("late report: %v", err)
			}
			if rev, _ := s.NodeRevision(ctx, "web"); rev != nil {
				t.Fatal("old grants remain")
			}
			if _, ok, _ := s.NodeReportOf(ctx, "web"); ok {
				t.Fatal("old report remains")
			}
			if records, _ := s.Creations(ctx, "test", o.ID); len(records) > 0 {
				t.Fatal("ownership remains")
			}
			if _, ok, _ := s.Agent(ctx, "other"); !ok {
				t.Fatal("unrelated node was changed")
			}
			if e, err := s.Explicit(ctx, "act-1"); err != nil || e.Outcome != "done" {
				t.Fatal("audit was lost")
			}
		})
	}
}
