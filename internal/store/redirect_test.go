package store

import (
	"context"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

func TestRedirectPreservesGrantsAndIsIdempotent(t *testing.T) {
	s, _, _ := open(t)
	ctx := context.Background()
	token, _ := s.IssueEnrollToken(ctx, "web", 7001, "seed", time.Hour)
	if err := s.Enroll(ctx, token, "web", 7001, "cert"); err != nil {
		t.Fatal(err)
	}
	before, err := s.PublishNodeRevision(ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
		return noderev.Revision{VMID: 7001, Services: []noderev.Service{{Name: "nginx", Running: true, Repair: []string{"start"}}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.RedirectAgents(ctx, "https://next.example:8443"); err != nil || n != 1 {
		t.Fatalf("redirect %d %v", n, err)
	}
	after, err := s.NodeRevision(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if after.Serial != before.Serial+1 || after.ControlPlane != "https://next.example:8443" || len(after.Services[0].Repair) != 1 {
		t.Fatalf("redirect changed grants: %+v", after)
	}
	if n, err := s.RedirectAgents(ctx, "https://next.example:8443"); err != nil || n != 0 {
		t.Fatalf("repeated redirect %d %v", n, err)
	}
	if _, err := s.RedirectAgents(ctx, "http://untrusted"); err == nil {
		t.Fatal("accepted an invalid endpoint")
	}
}
