package reconcile_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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

const homelab = `{"resources":[{"kind":"FakeVM","name":"app-server","provider":"fake","spec":{"cpu":1}},{"kind":"NodePackage","name":"app-server/nginx","spec":{"node":"app-server","package":"nginx","state":"Present"}},{"kind":"NodeService","name":"app-server/nginx","spec":{"enabled":true,"node":"app-server","running":true,"service":"nginx"}}],"schema":"mamela-graph/v2"}`

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
	w := fake.New(c.Now, "app-server/nginx")
	w.Declare(g)
	return &env{t: t, dbPath: path, ctx: context.Background(), clock: c, store: s, world: w, g: g, pol: policy.Default}
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
	if got := outcomes(rep)[fake.OpCreate]; got != reconcile.OutcomeNeedsApproval {
		t.Fatalf("unapproved create: %s", got)
	}
	if e.world.Calls(fake.OpCreate) != 0 {
		t.Fatal("created a VM without approval")
	}
	e.approve(p1)
	rep = e.apply(p1)
	if rep.Stopped != nil || outcomes(rep)[fake.OpCreate] != reconcile.OutcomeDone {
		t.Fatalf("approved create: %+v", rep)
	}

	// Round 2: the agent now reports; installing needs approval. The service waits for its unit.
	p2 := e.plan()
	e.approve(p2)
	rep = e.apply(p2)
	if got := outcomes(rep); got[policy.OpInstallPackage] != reconcile.OutcomeDone || len(got) != 1 {
		t.Fatalf("round 2: %+v", rep)
	}

	// Round 3: the unit exists; enable and start need approval.
	p3 := e.plan()
	e.approve(p3)
	rep = e.apply(p3)
	got := outcomes(rep)
	for _, op := range []policy.Op{policy.OpEnableService, policy.OpStartService} {
		if got[op] != reconcile.OutcomeDone {
			t.Errorf("%s: %s (%+v)", op, got[op], rep)
		}
	}

	// Round 4: converged.
	for _, c := range e.plan().Changes {
		if c.Status != plan.StatusInSync {
			t.Errorf("not converged: %s %s", c.ID(), c.Status)
		}
	}
	// Applying an already applied plan again does nothing new.
	rep = e.apply(p1)
	if outcomes(rep)[fake.OpCreate] != reconcile.OutcomeAlreadyDone || e.world.Calls(fake.OpCreate) != 1 {
		t.Fatalf("re-apply: %+v, calls %d", rep, e.world.Calls(fake.OpCreate))
	}
	evs, _ := e.store.Events(e.ctx, p1.Digest)
	if len(evs) == 0 {
		t.Error("no events recorded")
	}
}

func TestDriftRepairIsAutomaticOnlyWhenAllowed(t *testing.T) {
	e := newEnv(t)
	e.world.SetMachine("app-server", fake.Machine{Owned: true, Running: true, CPU: 1})
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
	e.world.SetMachine("app-server", fake.Machine{Owned: true, Running: true, CPU: 4}) // declared: 1
	p := e.plan()
	e.approve(p)
	rep := e.apply(p)
	if outcomes(rep)[fake.OpShrink] != reconcile.OutcomeDenied || e.world.Calls(fake.OpShrink) != 0 {
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
	if outcomes(e.apply(p2))[fake.OpCreate] != reconcile.OutcomeNeedsApproval {
		t.Fatal("an approval of another plan authorized this one")
	}
	e.clock.Add(time.Hour)
	if outcomes(e.apply(p1))[fake.OpCreate] != reconcile.OutcomeNeedsApproval {
		t.Fatal("an expired approval authorized the plan")
	}
	if e.world.Calls(fake.OpCreate) != 0 {
		t.Fatal("created a VM")
	}
}

func TestLostResponseIsConfirmedNotRepeated(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	e.world.Inject(fake.OpCreate, fake.LoseResponse)
	rep := e.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0003" {
		t.Fatalf("lost response: %+v", rep)
	}
	op, _ := e.store.Operation(e.ctx, reconcile.OperationID(p.Digest, "FakeVM app-server", fake.OpCreate))
	if op.Stage != store.StageUnknown {
		t.Fatalf("journal: %+v", op)
	}
	rep = e.apply(p)
	if outcomes(rep)[fake.OpCreate] != reconcile.OutcomeAlreadyDone {
		t.Fatalf("resume: %+v", rep)
	}
	if n := e.world.Calls(fake.OpCreate); n != 1 || e.world.Duplicates != 0 {
		t.Fatalf("create called %d times, %d duplicates", n, e.world.Duplicates)
	}
}

func TestFailureIsRetried(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	e.world.Inject(fake.OpCreate, fake.FailBefore)
	rep := e.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0004" {
		t.Fatalf("failure: %+v", rep)
	}
	rep = e.apply(p)
	if outcomes(rep)[fake.OpCreate] != reconcile.OutcomeDone {
		t.Fatalf("retry: %+v", rep)
	}
	op, _ := e.store.Operation(e.ctx, reconcile.OperationID(p.Digest, "FakeVM app-server", fake.OpCreate))
	if op.Stage != store.StageSucceeded || op.Attempt != 2 {
		t.Fatalf("journal: %+v", op)
	}
}

func TestCrashAfterTheChangeIsConfirmed(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	// A worker claimed the operation, created the VM, and died before recording the result.
	id := reconcile.OperationID(p.Digest, "FakeVM app-server", fake.OpCreate)
	e.store.Intend(e.ctx, id, p.Digest, "FakeVM app-server", string(fake.OpCreate))
	e.store.Claim(e.ctx, id, "worker-dead", time.Minute)
	e.world.SetMachine("app-server", fake.Machine{Owned: true, Running: true, CPU: 1})

	// While the dead worker's lease is live, nobody touches it.
	if rep := e.apply(p); rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0005" {
		t.Fatalf("live lease: %+v", rep)
	}
	e.clock.Add(time.Minute)
	if rep := e.apply(p); outcomes(rep)[fake.OpCreate] != reconcile.OutcomeAlreadyDone {
		t.Fatalf("after lease expiry: %+v", rep)
	}
	if e.world.Calls(fake.OpCreate) != 0 {
		t.Fatal("re-ran an operation whose effect was already visible")
	}
}

func TestCrashBeforeTheChangeIsRetriedOnce(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	id := reconcile.OperationID(p.Digest, "FakeVM app-server", fake.OpCreate)
	e.store.Intend(e.ctx, id, p.Digest, "FakeVM app-server", string(fake.OpCreate))
	e.store.Claim(e.ctx, id, "worker-dead", time.Minute)
	e.clock.Add(time.Minute)
	if rep := e.apply(p); outcomes(rep)[fake.OpCreate] != reconcile.OutcomeDone {
		t.Fatalf("%+v", rep)
	}
	if e.world.Calls(fake.OpCreate) != 1 {
		t.Fatalf("create called %d times", e.world.Calls(fake.OpCreate))
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
	if n := e.world.Calls(fake.OpCreate); n != 1 || e.world.Duplicates != 0 {
		t.Fatalf("create called %d times, %d duplicates", n, e.world.Duplicates)
	}
}

func TestWorldChangedSincePlanStops(t *testing.T) {
	e := newEnv(t)
	g, err := graph.Decode([]byte(strings.Replace(homelab, `"spec":{"cpu":1}`, `"spec":{"cpu":4}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	e.g = g
	e.world.Declare(g)
	e.world.SetMachine("app-server", fake.Machine{Owned: true, Running: true, CPU: 2})
	p := e.plan() // plans resize-vm 2 -> 4, based on the observed version
	e.approve(p)
	e.world.SetMachine("app-server", fake.Machine{Owned: true, Running: true, CPU: 3})
	rep := e.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0002" {
		t.Fatalf("%+v", rep)
	}
	if e.world.Calls(fake.OpResize) != 0 {
		t.Fatal("acted on a resource that changed after planning")
	}
}

func TestForeignMachineIsNeverTouched(t *testing.T) {
	e := newEnv(t)
	e.world.SetMachine("app-server", fake.Machine{Owned: false, Running: true, CPU: 8})
	p := e.plan()
	e.approve(p)
	rep := e.apply(p)
	if outcomes(rep)[policy.OpChangeForeign] != reconcile.OutcomeDenied {
		t.Fatalf("%+v", rep)
	}
	for _, op := range []policy.Op{fake.OpCreate, fake.OpResize, fake.OpStart, fake.OpShrink} {
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
	if e.world.Calls(fake.OpCreate) != 0 {
		t.Fatal("acted on a tampered plan")
	}
}

func TestLiveClaimByAnotherWorkerIsRespected(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	// Another worker has claimed the create and is still working on it (lease live, nothing visible yet).
	id := reconcile.OperationID(p.Digest, "FakeVM app-server", fake.OpCreate)
	e.store.Intend(e.ctx, id, p.Digest, "FakeVM app-server", string(fake.OpCreate))
	if ok, _ := e.store.Claim(e.ctx, id, "worker-b", time.Minute); !ok {
		t.Fatal("setup claim failed")
	}
	rep := e.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0005" || outcomes(rep)[fake.OpCreate] != reconcile.OutcomeBusy {
		t.Fatalf("%+v", rep)
	}
	if e.world.Calls(fake.OpCreate) != 0 {
		t.Fatal("ran an operation another worker holds")
	}
}

// Part of an approved change happened already (an earlier run, a lost answer, an agent that acted
// late): the rest is still within the approval and is carried out; nothing is done twice.
func TestPartlyDoneChangeIsFinished(t *testing.T) {
	e := newEnv(t)
	p1 := e.plan()
	e.approve(p1)
	e.apply(p1) // the VM
	p2 := e.plan()
	e.approve(p2)
	e.apply(p2) // the package; the service's unit now exists, disabled and stopped
	p3 := e.plan()
	e.approve(p3)
	e.world.SetService("app-server/nginx", state.Service{Exists: true, Enabled: true, Running: false}) // enabled meanwhile
	rep := e.apply(p3)
	got := outcomes(rep)
	if rep.Stopped != nil || got[policy.OpEnableService] != reconcile.OutcomeAlreadyDone || got[policy.OpStartService] != reconcile.OutcomeDone {
		t.Fatalf("report %+v", rep)
	}
	if e.world.Calls(policy.OpEnableService) != 0 {
		t.Fatal("enabled again")
	}

	// An operation the approval did not cover still stops the run: approved "start" only, then
	// the service is also disabled by hand, so enabling it would be needed as well.
	e.world.SetService("app-server/nginx", state.Service{Exists: true, Enabled: true, Running: false})
	p4 := e.plan()
	e.approve(p4)
	e.world.SetService("app-server/nginx", state.Service{Exists: true, Enabled: false, Running: false})
	rep = e.apply(p4)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0002" || e.world.Calls(policy.OpEnableService) != 0 {
		t.Fatalf("report %+v", rep)
	}
}

// A plan is bound to the providers' contracts it was made for: after a provider changes, its
// approval does not cover the new one, and nothing runs.
func TestPlanForOtherProvidersIsNotApplied(t *testing.T) {
	e := newEnv(t)
	snap, err := e.world.Observe(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := plan.BuildFor(e.g, snap, e.pol, "sha256:providers-v1")
	if err := e.store.SavePlan(e.ctx, e.g, snap, e.pol, p); err != nil {
		t.Fatal(err)
	}
	e.approve(p)
	if again, _, err := e.store.LoadPlan(e.ctx, p.Digest); err != nil || again.Inputs.ProvidersDigest != "sha256:providers-v1" {
		t.Fatalf("the providers digest is not part of the stored plan: %v", err)
	}
	if other := plan.BuildFor(e.g, snap, e.pol, "sha256:providers-v2"); other.Digest == p.Digest {
		t.Fatal("the providers digest does not change the plan digest")
	}
	for _, current := range []string{"sha256:providers-v2", ""} {
		r := e.reconciler("worker-a")
		r.Providers = current
		rep, err := r.Apply(e.ctx, p.Digest)
		if err != nil || rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0007" || len(rep.Steps) != 0 {
			t.Fatalf("providers %q: %+v %v", current, rep, err)
		}
	}
	if n := e.world.Calls(fake.OpCreate); n != 0 {
		t.Errorf("create-vm ran %d times", n)
	}
	r := e.reconciler("worker-a")
	r.Providers = "sha256:providers-v1"
	if rep, err := r.Apply(e.ctx, p.Digest); err != nil || rep.Stopped != nil {
		t.Fatalf("the same providers: %+v %v", rep, err)
	}
}

// Every machine of a run is created before any is started (its provider marks the starts late): a
// first boot must not compete with another machine's disk import. A create that fails starts nothing; the next run starts what was created.
func TestMachinesAreStartedAfterEveryCreate(t *testing.T) {
	e := newEnv(t)
	vm := func(name string) string {
		return `{"kind":"FakeVM","name":"` + name + `","provider":"fake","spec":{"cpu":1}}`
	}
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + vm("a") + `,` + vm("b") + `,` + vm("c") + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	e.g = g
	e.world.Declare(g)
	p := e.plan()
	e.approve(p)
	e.world.Inject(fake.OpCreate, fake.Fault(0))   // a succeeds
	e.world.Inject(fake.OpCreate, fake.FailBefore) // b fails
	rep := e.apply(p)
	if rep.Stopped == nil || strings.Join(e.world.Applied, ",") != "create-vm a,create-vm b" {
		t.Fatalf("applied %v, stopped %+v", e.world.Applied, rep.Stopped)
	}
	snap, _ := e.world.Observe(e.ctx)
	if snap.Nodes["a"].Running {
		t.Fatal("a VM was started although a create of the run failed")
	}
	// The next plan creates b and c and starts all three, creates first.
	e.world.Applied = nil
	p = e.plan()
	e.approve(p)
	if rep := e.apply(p); rep.Stopped != nil {
		t.Fatalf("%+v", rep.Stopped)
	}
	if got := strings.Join(e.world.Applied, ","); got != "create-vm b,create-vm c,start-vm a,start-vm b,start-vm c" {
		t.Fatalf("order %s", got)
	}
}
