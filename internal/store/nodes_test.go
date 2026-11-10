package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

func TestEnrollTokenIsOneTimeAndBound(t *testing.T) {
	s, clock, _ := open(t)
	ctx := context.Background()
	token, err := s.IssueEnrollToken(ctx, "web", 7001, "seed-a.iso", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for name, try := range map[string]func() error{
		"wrong node":  func() error { return s.Enroll(ctx, token, "db", 7001, "s1") },
		"wrong vmid":  func() error { return s.Enroll(ctx, token, "web", 7002, "s1") },
		"wrong token": func() error { return s.Enroll(ctx, token+"x", "web", 7001, "s1") },
	} {
		if err := try(); !errors.Is(err, ErrEnrollRefused) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if seed, ok, _ := s.ReusableSeed(ctx, "web", 7001, 10*time.Minute); !ok || seed != "seed-a.iso" {
		t.Fatalf("reusable seed %q %v", seed, ok)
	}
	if err := s.Enroll(ctx, token, "web", 7001, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Enroll(ctx, token, "web", 7001, "s2"); !errors.Is(err, ErrEnrollRefused) {
		t.Fatalf("the token worked twice: %v", err)
	}
	if a, ok, _ := s.Agent(ctx, "web"); !ok || a.CertSerial != "s1" || a.VMID != 7001 {
		t.Fatalf("agent %+v", a)
	}
	if _, ok, _ := s.ReusableSeed(ctx, "web", 7001, 0); ok {
		t.Fatal("a used token's seed is reusable")
	}

	late, _ := s.IssueEnrollToken(ctx, "web", 7001, "seed-b.iso", time.Hour)
	if _, ok, _ := s.ReusableSeed(ctx, "web", 7001, 2*time.Hour); ok {
		t.Fatal("a seed whose token expires too soon is reusable")
	}
	clock.now = clock.now.Add(2 * time.Hour)
	if err := s.Enroll(ctx, late, "web", 7001, "s3"); !errors.Is(err, ErrEnrollRefused) {
		t.Fatalf("an expired token worked: %v", err)
	}
}

func TestPublishNodeRevisionBumpsSerialOnlyOnChange(t *testing.T) {
	s, _, _ := open(t)
	ctx := context.Background()
	content := func(running bool) func(*noderev.Revision) (noderev.Revision, error) {
		return func(prev *noderev.Revision) (noderev.Revision, error) {
			return noderev.Revision{VMID: 7001, Services: []noderev.Service{{Name: "nginx", Running: running}}}, nil
		}
	}
	r1, err := s.PublishNodeRevision(ctx, "web", content(true))
	if err != nil || r1.Serial != 1 {
		t.Fatalf("%v %+v", err, r1)
	}
	r2, _ := s.PublishNodeRevision(ctx, "web", content(true))
	if r2.Serial != 1 {
		t.Fatalf("same content got serial %d", r2.Serial)
	}
	r3, _ := s.PublishNodeRevision(ctx, "web", content(false))
	if r3.Serial != 2 {
		t.Fatalf("new content got serial %d", r3.Serial)
	}
	got, err := s.NodeRevision(ctx, "web")
	if err != nil || got == nil || got.Serial != 2 || got.Services[0].Running {
		t.Fatalf("%v %+v", err, got)
	}
	if other, _ := s.NodeRevision(ctx, "db"); other != nil {
		t.Fatal("a node without revisions has one")
	}
}

func TestNodeReports(t *testing.T) {
	s, clock, _ := open(t)
	ctx := context.Background()
	if err := s.PutNodeReport(ctx, noderev.Report{Node: "web", VMID: 7001, Serial: 1}); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Minute)
	if err := s.PutNodeReport(ctx, noderev.Report{Node: "web", VMID: 7001, Serial: 2}); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.NodeReportOf(ctx, "web")
	if err != nil || !ok || r.Report.Serial != 2 || !r.Received.Equal(clock.now) {
		t.Fatalf("%v %v %+v", err, ok, r)
	}
	all, _ := s.NodeReports(ctx)
	if len(all) != 1 {
		t.Fatalf("%d reports", len(all))
	}
}
