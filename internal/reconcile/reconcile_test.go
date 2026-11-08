package reconcile_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/fake"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
)

const homelab = `{"resources":[{"kind":"VirtualMachine","name":"app-server","spec":{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}},{"kind":"NodePackage","name":"app-server/nginx","spec":{"node":"app-server","package":"nginx","state":"Present"}},{"kind":"NodeService","name":"app-server/nginx","spec":{"enabled":true,"node":"app-server","running":true,"service":"nginx"}}],"schema":"mamela-graph/v1"}`

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

type env struct {
	t      *testing.T
	dbPath string
	ctx    context.Context
	clock  *clock
	store  *store.Store
	world  *fake.World
	g      *graph.Graph
	pol    policy.Policy
}

func newEnv(t *testing.T) *env {
	t.Helper()
	c := &clock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "mamela.db")
	s, err := store.Open(context.Background(), path, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	g, err := graph.Decode([]byte(homelab))
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, dbPath: path, ctx: context.Background(), clock: c, store: s,
		world: fake.New(c.Now, "app-server/nginx"), g: g, pol: policy.Default}
}

func (e *env) reconciler(owner string) *reconcile.Reconciler {
	return &reconcile.Reconciler{Store: e.store, Provider: e.world, Owner: owner, Lease: time.Minute}
}

// plan observes, plans and saves, like `mamela plan` would.
func (e *env) plan() *plan.Plan {
	e.t.Helper()
	snap, err := e.world.Observe(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	p := plan.Build(e.g, snap, e.pol)
	if err := e.store.SavePlan(e.ctx, e.g, snap, e.pol, p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) approve(p *plan.Plan) {
	e.t.Helper()
	if _, err := e.store.Approve(e.ctx, p.Digest, "alice", time.Hour); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) apply(p *plan.Plan) *reconcile.Report {
	e.t.Helper()
	rep, err := e.reconciler("worker-a").Apply(e.ctx, p.Digest)
	if err != nil {
		e.t.Fatalf("Apply: %v", err)
	}
	return rep
}

func outcomes(rep *reconcile.Report) map[policy.Op]reconcile.Outcome {
	out := map[policy.Op]reconcile.Outcome{}
	for _, s := range rep.Steps {
		out[s.Op] = s.Outcome
	}
	return out
}

func TestConvergesWithApprovals(t *testing.T) {
	e := newEnv(t)

	// Round 1: only the VM can be planned; nothing runs without approval.
	p1 := e.plan()
	rep := e.apply(p1)
	if got := outcomes(rep)[policy.OpCreateVM]; got != reconcile.OutcomeNeedsApproval {
		t.Fatalf("unapproved create: %s", got)
	}
	if e.world.Calls(policy.OpCreateVM) != 0 {
		t.Fatal("created a VM without approval")
	}
	e.approve(p1)
	rep = e.apply(p1)
	if rep.Stopped != nil || outcomes(rep)[policy.OpCreateVM] != reconcile.OutcomeDone {
		t.Fatalf("approved create: %+v", rep)
	}

	// Round 2: the agent now reports; install, enable and start need approval.
	p2 := e.plan()
	e.approve(p2)
	rep = e.apply(p2)
	got := outcomes(rep)
	for _, op := range []policy.Op{policy.OpInstallPackage, policy.OpEnableService, policy.OpStartService} {
		if got[op] != reconcile.OutcomeDone {
			t.Errorf("%s: %s (%+v)", op, got[op], rep)
		}
	}

	// Round 3: converged.
	for _, c := range e.plan().Changes {
		if c.Status != plan.StatusInSync {
			t.Errorf("not converged: %s %s", c.ID(), c.Status)
		}
	}
	// Applying an already applied plan again does nothing new.
	rep = e.apply(p1)
	if outcomes(rep)[policy.OpCreateVM] != reconcile.OutcomeAlreadyDone || e.world.Calls(policy.OpCreateVM) != 1 {
		t.Fatalf("re-apply: %+v, calls %d", rep, e.world.Calls(policy.OpCreateVM))
	}
	evs, _ := e.store.Events(e.ctx, p1.Digest)
	if len(evs) == 0 {
		t.Error("no events recorded")
	}
}

func TestDriftRepairIsAutomaticOnlyWhenAllowed(t *testing.T) {
	e := newEnv(t)
	e.world.SetVM("app-server", state.VM{Managed: true, CPU: 1, MemoryMiB: 1024, DiskGiB: 10, Image: "ubuntu-24.04"})
	p := e.plan()
	e.approve(p)
	e.apply(p)                                                                           // install + enable + start
	e.world.SetService("app-server/nginx", state.Service{Enabled: true, Running: false}) // someone stops nginx

	p = e.plan()
	if rep := e.apply(p); outcomes(rep)[policy.OpStartService] != reconcile.OutcomeNeedsApproval {
		t.Fatalf("default policy restarted without approval: %+v", rep)
	}
	e.pol.AutoStartServices = []string{"nginx"}
	p = e.plan()
	if rep := e.apply(p); outcomes(rep)[policy.OpStartService] != reconcile.OutcomeDone {
		t.Fatalf("autostart policy did not restart: %+v", rep)
	}
}

func TestDeniedNeverRunsEvenIfApproved(t *testing.T) {
	e := newEnv(t)
	e.world.SetVM("app-server", state.VM{Managed: true, CPU: 1, MemoryMiB: 1024, DiskGiB: 50, Image: "ubuntu-24.04"})
	p := e.plan()
	e.approve(p)
	rep := e.apply(p)
	if outcomes(rep)[policy.OpShrinkVMDisk] != reconcile.OutcomeDenied || e.world.Calls(policy.OpShrinkVMDisk) != 0 {
		t.Fatalf("shrink: %+v", rep)
	}
}

func TestApprovalIsBoundToThePlanAndExpires(t *testing.T) {
	e := newEnv(t)
	p1 := e.plan()
	e.approve(p1)
	e.clock.Add(time.Second)
	p2 := e.plan() // same world, later observation: a different plan digest
	if p1.Digest == p2.Digest {
		t.Fatal("plans should differ")
	}
	if outcomes(e.apply(p2))[policy.OpCreateVM] != reconcile.OutcomeNeedsApproval {
		t.Fatal("an approval of another plan authorized this one")
	}
	e.clock.Add(time.Hour)
	if outcomes(e.apply(p1))[policy.OpCreateVM] != reconcile.OutcomeNeedsApproval {
		t.Fatal("an expired approval authorized the plan")
	}
	if e.world.Calls(policy.OpCreateVM) != 0 {
		t.Fatal("created a VM")
	}
}

func TestLostResponseIsConfirmedNotRepeated(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	e.world.Inject(policy.OpCreateVM, fake.LoseResponse)
	rep := e.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0003" {
		t.Fatalf("lost response: %+v", rep)
	}
	op, _ := e.store.Operation(e.ctx, reconcile.OperationID(p.Digest, "VirtualMachine app-server", policy.OpCreateVM))
	if op.Stage != store.StageUnknown {
		t.Fatalf("journal: %+v", op)
	}
	rep = e.apply(p)
	if outcomes(rep)[policy.OpCreateVM] != reconcile.OutcomeAlreadyDone {
		t.Fatalf("resume: %+v", rep)
	}
	if n := e.world.Calls(policy.OpCreateVM); n != 1 || e.world.Duplicates != 0 {
		t.Fatalf("create called %d times, %d duplicates", n, e.world.Duplicates)
	}
}

func TestFailureIsRetried(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	e.world.Inject(policy.OpCreateVM, fake.FailBefore)
	rep := e.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0004" {
		t.Fatalf("failure: %+v", rep)
	}
	rep = e.apply(p)
	if outcomes(rep)[policy.OpCreateVM] != reconcile.OutcomeDone {
		t.Fatalf("retry: %+v", rep)
	}
	op, _ := e.store.Operation(e.ctx, reconcile.OperationID(p.Digest, "VirtualMachine app-server", policy.OpCreateVM))
	if op.Stage != store.StageSucceeded || op.Attempt != 2 {
		t.Fatalf("journal: %+v", op)
	}
}

func TestCrashAfterTheChangeIsConfirmed(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	// A worker claimed the operation, created the VM, and died before recording the result.
	id := reconcile.OperationID(p.Digest, "VirtualMachine app-server", policy.OpCreateVM)
	e.store.Intend(e.ctx, id, p.Digest, "VirtualMachine app-server", string(policy.OpCreateVM))
	e.store.Claim(e.ctx, id, "worker-dead", time.Minute)
	e.world.SetVM("app-server", state.VM{Managed: true, CPU: 1, MemoryMiB: 1024, DiskGiB: 10, Image: "ubuntu-24.04"})

	// While the dead worker's lease is live, nobody touches it.
	if rep := e.apply(p); rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0005" {
		t.Fatalf("live lease: %+v", rep)
	}
	e.clock.Add(time.Minute)
	if rep := e.apply(p); outcomes(rep)[policy.OpCreateVM] != reconcile.OutcomeAlreadyDone {
		t.Fatalf("after lease expiry: %+v", rep)
	}
	if e.world.Calls(policy.OpCreateVM) != 0 {
		t.Fatal("re-ran an operation whose effect was already visible")
	}
}

func TestCrashBeforeTheChangeIsRetriedOnce(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	id := reconcile.OperationID(p.Digest, "VirtualMachine app-server", policy.OpCreateVM)
	e.store.Intend(e.ctx, id, p.Digest, "VirtualMachine app-server", string(policy.OpCreateVM))
	e.store.Claim(e.ctx, id, "worker-dead", time.Minute)
	e.clock.Add(time.Minute)
	if rep := e.apply(p); outcomes(rep)[policy.OpCreateVM] != reconcile.OutcomeDone {
		t.Fatalf("%+v", rep)
	}
	if e.world.Calls(policy.OpCreateVM) != 1 {
		t.Fatalf("create called %d times", e.world.Calls(policy.OpCreateVM))
	}
}

func TestConcurrentAppliesCreateOnce(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.reconciler("worker-"+string(rune('a'+i))).Apply(e.ctx, p.Digest); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := e.world.Calls(policy.OpCreateVM); n != 1 || e.world.Duplicates != 0 {
		t.Fatalf("create called %d times, %d duplicates", n, e.world.Duplicates)
	}
}

func TestWorldChangedSincePlanStops(t *testing.T) {
	e := newEnv(t)
	e.world.SetVM("app-server", state.VM{Managed: true, CPU: 2, MemoryMiB: 1024, DiskGiB: 10, Image: "ubuntu-24.04"})
	p := e.plan() // plans resize-vm-cpu 2 -> 1
	e.approve(p)
	e.world.SetVM("app-server", state.VM{Managed: true, CPU: 4, MemoryMiB: 1024, DiskGiB: 10, Image: "ubuntu-24.04"})
	rep := e.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0002" {
		t.Fatalf("%+v", rep)
	}
	if e.world.Calls(policy.OpResizeVMCPU) != 0 {
		t.Fatal("acted on a resource that changed after planning")
	}
}

func TestForeignVMIsNeverTouched(t *testing.T) {
	e := newEnv(t)
	e.world.SetVM("app-server", state.VM{Managed: false, CPU: 8, MemoryMiB: 16384, DiskGiB: 64, Image: "something"})
	p := e.plan()
	e.approve(p)
	rep := e.apply(p)
	if outcomes(rep)[policy.OpChangeForeign] != reconcile.OutcomeDenied {
		t.Fatalf("%+v", rep)
	}
	for _, op := range []policy.Op{policy.OpCreateVM, policy.OpResizeVMCPU, policy.OpResizeVMMemory, policy.OpReplaceVMImage} {
		if e.world.Calls(op) != 0 {
			t.Errorf("%s was called on a foreign VM", op)
		}
	}
}

func TestTamperedPlanIsRefused(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	// Swap the stored snapshot for a different one: the plan no longer recomputes to its digest.
	other := state.Empty(e.clock.Now().Add(time.Hour))
	d, _ := e.store.PutSnapshot(e.ctx, other)
	// Tamper with the database file directly, as an attacker or a broken tool would.
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE plans SET snapshot_digest = ? WHERE digest = ?`, d, p.Digest); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = e.reconciler("worker-a").Apply(e.ctx, p.Digest)
	if !errors.Is(err, store.ErrPlanMismatch) {
		t.Fatalf("got %v", err)
	}
	if e.world.Calls(policy.OpCreateVM) != 0 {
		t.Fatal("acted on a tampered plan")
	}
}

func TestLiveClaimByAnotherWorkerIsRespected(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	// Another worker has claimed the create and is still working on it (lease live, nothing visible yet).
	id := reconcile.OperationID(p.Digest, "VirtualMachine app-server", policy.OpCreateVM)
	e.store.Intend(e.ctx, id, p.Digest, "VirtualMachine app-server", string(policy.OpCreateVM))
	if ok, _ := e.store.Claim(e.ctx, id, "worker-b", time.Minute); !ok {
		t.Fatal("setup claim failed")
	}
	rep := e.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0005" || outcomes(rep)[policy.OpCreateVM] != reconcile.OutcomeBusy {
		t.Fatalf("%+v", rep)
	}
	if e.world.Calls(policy.OpCreateVM) != 0 {
		t.Fatal("ran an operation another worker holds")
	}
}
