package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// One apply at a time, across connections to the database (the programs of the Control Plane), and
// the lock is free again once its holder lets go (B2).
func TestApplyLockIsExclusiveAcrossConnections(t *testing.T) {
	s, _, path := open(t)
	other, err := Open(context.Background(), path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	a, _, err := s.LockApply("host/1#boot:2", "sha256:aaaa")
	if err != nil {
		t.Fatal(err)
	}
	b, holder, err := other.LockApply("host/2", "sha256:bbbb")
	if !errors.Is(err, ErrApplyBusy) || b != nil {
		t.Fatalf("a second lock was granted: %v", err)
	}
	if !strings.Contains(holder, "host/1#boot:2") || !strings.Contains(holder, "sha256:aaaa") {
		t.Errorf("the holder is %q", holder)
	}
	// Not even the same connection takes it twice.
	if _, _, err := s.LockApply("host/1", "sha256:cccc"); !errors.Is(err, ErrApplyBusy) {
		t.Errorf("the same program took it twice: %v", err)
	}
	a.Release()
	a.Release() // releasing twice is harmless
	b, _, err = other.LockApply("host/2", "sha256:bbbb")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	b.Release()
}

// ReleaseOwner gives up a stopping worker's unfinished operations as unknown, and only those.
func TestReleaseOwnerLeavesTheRest(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := open(t)
	p := savedPlan(t, s, clock.Now())
	for id, owner := range map[string]string{"op-mine": "me", "op-other": "someone", "op-done": "me"} {
		if _, err := s.Intend(ctx, id, p.Digest, "r "+id, "create"); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Claim(ctx, id, owner, 3*time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if err := s.Finish(ctx, "op-done", "me", StageSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReleaseOwner(ctx, "me", "stopped")
	if err != nil || n != 1 {
		t.Fatalf("released %d (%v)", n, err)
	}
	for id, want := range map[string]Stage{"op-mine": StageUnknown, "op-other": StageStarted, "op-done": StageSucceeded} {
		if o, _ := s.Operation(ctx, id); o.Stage != want {
			t.Errorf("%s is %s, want %s", id, o.Stage, want)
		}
	}
	// The next apply may claim it at once.
	if ok, _ := s.Claim(ctx, "op-mine", "next", time.Hour); !ok {
		t.Error("a released operation cannot be claimed")
	}
}
