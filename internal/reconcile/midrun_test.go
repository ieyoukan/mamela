package reconcile_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/provider/fake"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/store"
)

// hooked runs a function in the middle of an apply: just before a step is handed to the provider.
type hooked struct {
	*fake.World
	before func(step reconcile.Step)
}

func (h hooked) Apply(ctx context.Context, step reconcile.Step) error {
	if h.before != nil {
		h.before(step)
	}
	return h.World.Apply(ctx, step)
}

// A deletion is decided on what the world is when it is about to run, not on what it was when the
// apply began: an earlier step can take hours (an image import), and the object may change meanwhile
// (B1). Here "db" is resized by hand while another machine is being created; the approved deletion
// was of the old version, so nothing is deleted and the plan is to be made again.
func TestDeletionIsCheckedAgainJustBeforeItRuns(t *testing.T) {
	e := made(t)
	e.declare("web", "extra") // db removed; extra is new
	p := e.plan()
	e.approve(p)
	r := e.reconciler("worker-a")
	r.Provider = hooked{World: e.world, before: func(step reconcile.Step) {
		if step.Op == fake.OpCreate {
			e.world.SetMachine("db", fake.Machine{Owned: true, Stack: "infra", CPU: 9, Running: true, ID: 7001})
		}
	}}
	rep, err := r.Apply(e.ctx, p.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if e.world.Calls(fake.OpDelete) != 0 || !has(machinesOf(e), "db") {
		t.Fatalf("db was deleted although it changed after the apply began: %+v", rep)
	}
	if rep.Stopped == nil || rep.Stopped.Code != reconcile.CodeStale {
		t.Fatalf("stopped %+v, want %s", rep.Stopped, reconcile.CodeStale)
	}
	if !has(machinesOf(e), "extra") {
		t.Error("the create before it did not run")
	}
}

// The object may also be another declaration's by then (it was made again, there, under the same name).
func TestDeletionStopsWhenTheObjectBelongsToAnotherDeclarationNow(t *testing.T) {
	e := made(t)
	e.declare("web", "extra")
	p := e.plan()
	e.approve(p)
	r := e.reconciler("worker-a")
	r.Provider = hooked{World: e.world, before: func(step reconcile.Step) {
		if step.Op == fake.OpCreate {
			e.world.SetMachine("db", fake.Machine{Owned: true, Stack: "other", CPU: 1, ID: 7001})
		}
	}}
	rep, err := r.Apply(e.ctx, p.Digest)
	if err != nil || e.world.Calls(fake.OpDelete) != 0 || !has(machinesOf(e), "db") || rep.Stopped == nil || rep.Stopped.Code != reconcile.CodeStale {
		t.Fatalf("deleted a machine of another declaration: %+v %v", rep, err)
	}
}

// When the world cannot be observed right before a deletion, nothing is deleted: the apply stops
// with a code, and the operations before it stand.
func TestDeletionStopsWhenItCannotBeObservedAgain(t *testing.T) {
	e := made(t)
	e.declare("web", "extra")
	p := e.plan()
	e.approve(p)
	r := e.reconciler("worker-a")
	r.Provider = &failingObserver{World: e.world, n: 1} // the observation before the operations only
	rep, err := r.Apply(e.ctx, p.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if e.world.Calls(fake.OpDelete) != 0 || !has(machinesOf(e), "db") {
		t.Fatalf("deleted without observing again: %+v", rep)
	}
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0008" || outcomes(rep)[fake.OpCreate] != reconcile.OutcomeDone {
		t.Fatalf("report %+v (stopped %+v)", rep, rep.Stopped)
	}
	// The next apply observes, and carries on.
	rep = e.apply(p)
	if rep.Stopped != nil || has(machinesOf(e), "db") {
		t.Fatalf("the next apply: %+v", rep)
	}
}

// Two plans that were each approved, applied at the same time by two programs (the CLI and mamela
// api), both create the machine that both observed missing: the create is sent twice (B2). One apply
// at a time per database: the second finds the first's machine there, or waits.
func TestTwoPlansAppliedAtOnceCreateOnce(t *testing.T) {
	e := newEnv(t)
	p1 := e.plan()
	e.clock.Add(time.Second)
	p2 := e.plan() // the same change, another observation: another plan digest
	if p1.Digest == p2.Digest {
		t.Fatal("setup: the plans should differ")
	}
	e.approve(p1)
	e.approve(p2)

	// The other program, with its own connection to the same database.
	other, err := store.Open(e.ctx, e.dbPath, e.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	// Whoever reaches the create first waits a moment for the other to arrive at it: both do, unless
	// the applies exclude each other.
	var mu sync.Mutex
	arrived := 0
	slow := func(step reconcile.Step) {
		if step.Op != fake.OpCreate {
			return
		}
		mu.Lock()
		arrived++
		mu.Unlock()
		for range 40 {
			mu.Lock()
			n := arrived
			mu.Unlock()
			if n >= 2 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	var wg sync.WaitGroup
	reports := make([]*reconcile.Report, 2)
	for i, c := range []struct {
		st *store.Store
		p  string
	}{{e.store, p1.Digest}, {other, p2.Digest}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.reconciler("worker-" + string(rune('a'+i)))
			r.Store = c.st
			r.Provider = hooked{World: e.world, before: slow}
			rep, err := r.Apply(e.ctx, c.p)
			if err != nil {
				t.Error(err)
			}
			reports[i] = rep
		}()
	}
	wg.Wait()
	if n := e.world.Calls(fake.OpCreate); n != 1 || e.world.Duplicates != 0 {
		t.Fatalf("create-vm was sent %d times (%d duplicates)", n, e.world.Duplicates)
	}
	// The one that did not run says why, with a code.
	busy := 0
	for _, rep := range reports {
		if rep != nil && rep.Stopped != nil && rep.Stopped.Code == "MAM-REC-0009" {
			busy++
		}
	}
	if busy > 1 {
		t.Errorf("%d applies were refused", busy)
	}
}

// After it ends, the lock is free: the same program, or another, applies the next plan.
func TestApplyLockIsReleased(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	if rep := e.apply(p); rep.Stopped != nil {
		t.Fatalf("%+v", rep.Stopped)
	}
	if rep := e.apply(p); rep.Stopped != nil {
		t.Fatalf("the second apply of the same plan: %+v", rep.Stopped)
	}
}
