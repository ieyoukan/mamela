package store

import (
	"context"
	"testing"
	"time"
)

func TestAgentsAndEnrollTokenCheck(t *testing.T) {
	s, _, _ := open(t)
	ctx := context.Background()
	if n, err := s.AgentCount(ctx); err != nil || n != 0 {
		t.Fatalf("empty: %d %v", n, err)
	}
	token, _ := s.IssueEnrollToken(ctx, "web", 7001, "", time.Hour)
	// Checking the token spends nothing: it can be checked again, and still enrolls.
	for range 2 {
		if ok, err := s.EnrollTokenUsable(ctx, token, "web", 7001); err != nil || !ok {
			t.Fatalf("a good token: %v %v", ok, err)
		}
	}
	for name, try := range map[string]func() (bool, error){
		"wrong node":  func() (bool, error) { return s.EnrollTokenUsable(ctx, token, "db", 7001) },
		"wrong vmid":  func() (bool, error) { return s.EnrollTokenUsable(ctx, token, "web", 7002) },
		"wrong token": func() (bool, error) { return s.EnrollTokenUsable(ctx, token+"x", "web", 7001) },
	} {
		if ok, err := try(); err != nil || ok {
			t.Errorf("%s: %v %v", name, ok, err)
		}
	}
	if err := s.Enroll(ctx, token, "web", 7001, "s1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.EnrollTokenUsable(ctx, token, "web", 7001); ok {
		t.Fatal("a spent token is usable")
	}
	late, _ := s.IssueEnrollToken(ctx, "db", 7002, "", time.Minute)
	s.Enroll(ctx, late, "db", 7002, "s2")
	agents, err := s.Agents(ctx)
	if err != nil || len(agents) != 2 || agents[0].Node != "db" || agents[1].Node != "web" || agents[1].CertSerial != "s1" {
		t.Fatalf("agents: %+v %v", agents, err)
	}
	if n, _ := s.AgentCount(ctx); n != 2 {
		t.Fatalf("count %d", n)
	}
}

// The end of a node's certificate is kept for the registration it was issued under only: after the
// node enrolls again, a note about the earlier registration says nothing about the new one.
func TestAgentCertNotes(t *testing.T) {
	s, _, _ := open(t)
	ctx := context.Background()
	enroll := func(serial string) {
		token, _ := s.IssueEnrollToken(ctx, "web", 7001, "", time.Hour)
		if err := s.Enroll(ctx, token, "web", 7001, serial); err != nil {
			t.Fatal(err)
		}
	}
	first := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	enroll("s1")
	if got, err := s.AgentCertNotAfter(ctx); err != nil || len(got) != 0 {
		t.Fatalf("no notes: %v %v", got, err)
	}
	s.NoteAgentCert(ctx, "web", "s1", first)
	s.NoteAgentCert(ctx, "web", "s1", first.AddDate(0, 8, 0)) // a renewal
	if got, _ := s.AgentCertNotAfter(ctx); !got["web"].Equal(first.AddDate(0, 8, 0)) {
		t.Fatalf("the latest note: %v", got)
	}
	enroll("s2")
	if got, _ := s.AgentCertNotAfter(ctx); len(got) != 0 {
		t.Fatalf("a note of the earlier registration is taken for the new one: %v", got)
	}
	s.NoteAgentCert(ctx, "web", "s2", first.AddDate(2, 0, 0))
	if got, _ := s.AgentCertNotAfter(ctx); !got["web"].Equal(first.AddDate(2, 0, 0)) {
		t.Fatalf("the new registration's note: %v", got)
	}
}
