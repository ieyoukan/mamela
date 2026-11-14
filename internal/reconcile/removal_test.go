package reconcile_test

import (
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/provider/fake"
	"github.com/ieyoukan/mamela/internal/reconcile"
)

func vms(names ...string) string {
	out := `{"schema":"mamela-graph/v2","resources":[`
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += `{"kind":"FakeVM","name":"` + n + `","provider":"fake","spec":{"cpu":1}}`
	}
	return out + `]}`
}

// declare changes what the declaration has.
func (e *env) declare(names ...string) {
	e.t.Helper()
	g, err := graph.Decode([]byte(vms(names...)))
	if err != nil {
		e.t.Fatal(err)
	}
	e.g = g
	e.world.Declare(g)
}

func machinesOf(e *env) map[string]fake.Machine { return e.world.Machines() }

func has(m map[string]fake.Machine, name string) bool { _, ok := m[name]; return ok }

// made creates two machines for the declaration "infra", then removes "db" from it.
func made(t *testing.T) *env {
	e := newEnv(t)
	e.world.SetStack("infra")
	e.declare("web", "db")
	for range 2 { // create, then start
		p := e.plan()
		e.approve(p)
		e.apply(p)
	}
	if m := machinesOf(e); !has(m, "web") || !has(m, "db") {
		t.Fatalf("setup: %v", m)
	}
	return e
}

// Removing a machine from the declaration that made it deletes it: planned, shown, carried out
// only with an approval, after everything else, and once.
func TestRemovedMachineIsDeletedWithApproval(t *testing.T) {
	e := made(t)
	e.declare("web", "extra") // db removed; extra is new
	p := e.plan()
	if p.Removals() != 1 {
		t.Fatalf("the plan has %d deletions", p.Removals())
	}
	rep := e.apply(p)
	if got := outcomes(rep)[fake.OpDelete]; got != reconcile.OutcomeNeedsApproval || e.world.Calls(fake.OpDelete) != 0 {
		t.Fatalf("a deletion without approval: %s (%d calls)", got, e.world.Calls(fake.OpDelete))
	}
	e.approve(p)
	rep = e.apply(p)
	if rep.Stopped != nil || outcomes(rep)[fake.OpDelete] != reconcile.OutcomeDone {
		t.Fatalf("approved deletion: %+v", rep)
	}
	var order []string
	for _, a := range e.world.Applied {
		order = append(order, a)
	}
	last := order[len(order)-1]
	if last != "delete-vm db" {
		t.Errorf("the deletion is not last: %v", order)
	}
	if m := machinesOf(e); has(m, "db") || !has(m, "web") || !has(m, "extra") {
		t.Errorf("after: %v", m)
	}
	// Applying the same plan again deletes nothing more: the world is as it asked.
	e.apply(p)
	if n := e.world.Calls(fake.OpDelete); n > 2 {
		t.Errorf("%d delete calls", n)
	}
}

// A machine another declaration made, one from before stacks were recorded, and a Foreign one are
// never deleted by removing them from this declaration.
func TestOnlyWhatThisDeclarationMadeIsDeleted(t *testing.T) {
	e := newEnv(t)
	e.world.SetMachine("theirs", fake.Machine{Owned: true, Stack: "other"})
	e.world.SetMachine("older", fake.Machine{Owned: true})
	e.world.SetMachine("foreign", fake.Machine{Owned: false, Stack: "infra"})
	e.world.SetMachine("mine", fake.Machine{Owned: true, Stack: "infra"})
	e.world.SetStack("infra")
	e.declare()
	p := e.plan()
	if p.Removals() != 1 {
		t.Fatalf("%d deletions, want only mine", p.Removals())
	}
	e.approve(p)
	e.apply(p)
	m := machinesOf(e)
	if has(m, "mine") || !has(m, "theirs") || !has(m, "older") || !has(m, "foreign") {
		t.Errorf("after: %v", m)
	}
}

// A deletion whose answer was lost: the next apply finds the machine gone and does not send it again.
func TestLostDeletionIsConfirmedNotRepeated(t *testing.T) {
	e := made(t)
	e.declare("web")
	p := e.plan()
	e.approve(p)
	e.world.Inject(fake.OpDelete, fake.LoseResponse)
	rep := e.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0003" {
		t.Fatalf("an unknown outcome stops the run: %+v", rep.Stopped)
	}
	if has(machinesOf(e), "db") {
		t.Fatal("the fake did not delete before losing the response")
	}
	rep = e.apply(p)
	if rep.Stopped != nil {
		t.Fatalf("the second apply: %+v", rep.Stopped)
	}
}

// A failed deletion deletes nothing and is retried; a machine changed after the plan is not deleted.
func TestDeletionFailureAndStalePlan(t *testing.T) {
	e := made(t)
	e.declare("web")
	p := e.plan()
	e.approve(p)
	e.world.Inject(fake.OpDelete, fake.FailBefore)
	if rep := e.apply(p); rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0004" || !has(machinesOf(e), "db") {
		t.Fatalf("failure: %+v", rep)
	}
	// Somebody resizes db by hand after the plan: the approved deletion was of another version.
	e.world.SetMachine("db", fake.Machine{Owned: true, Stack: "infra", CPU: 9, Running: true, ID: 7001})
	rep := e.apply(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0002" || !has(machinesOf(e), "db") {
		t.Fatalf("a changed machine was deleted by an old plan: %+v", rep)
	}
}

// Under a policy that retains, nothing is deleted.
func TestRetainPolicyKeepsRemovedMachines(t *testing.T) {
	e := made(t)
	e.pol.Removal = "retain"
	e.declare("web")
	p := e.plan()
	if p.Removals() != 0 {
		t.Fatal("planned a deletion under a retaining policy")
	}
}
