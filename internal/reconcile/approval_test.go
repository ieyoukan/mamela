package reconcile_test

import (
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/fake"
	"github.com/ieyoukan/mamela/internal/reconcile"
)

// An approval was a judgment under the policy of the plan. When the policy in force has changed since
// (a node protected, removal retained), the plan is not applied: it is made again under the new one
// (B3). The same policy applies it.
func TestPlanMadeUnderAnotherPolicyIsNotApplied(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	retaining := policy.Default
	retaining.Removal = "retain"
	if retaining.Digest() == policy.Default.Digest() {
		t.Fatal("setup: the policies should differ")
	}

	r := e.reconciler("worker-a")
	r.PolicyDigest = retaining.Digest()
	rep, err := r.Apply(e.ctx, p.Digest)
	if err != nil || rep.Stopped == nil || rep.Stopped.Code != reconcile.CodePolicyChanged || len(rep.Steps) != 0 {
		t.Fatalf("under another policy: %+v %v", rep, err)
	}
	if e.world.Calls(fake.OpCreate) != 0 {
		t.Fatal("create-vm ran under a policy the plan was not made for")
	}
	for _, want := range []string{retaining.Digest(), policy.Default.Digest()} {
		if !strings.Contains(rep.Stopped.Message, want) {
			t.Errorf("the stop does not name %s: %s", want, rep.Stopped.Message)
		}
	}

	r.PolicyDigest = policy.Default.Digest()
	if rep, err := r.Apply(e.ctx, p.Digest); err != nil || rep.Stopped != nil || outcomes(rep)[fake.OpCreate] != reconcile.OutcomeDone {
		t.Fatalf("under the same policy: %+v %v", rep, err)
	}
}

// A revoked approval authorizes nothing.
func TestRevokedApprovalRunsNothing(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	if _, err := e.store.Revoke(e.ctx, p.Digest, "carol", "changed my mind"); err != nil {
		t.Fatal(err)
	}
	rep := e.apply(p)
	if outcomes(rep)[fake.OpCreate] != reconcile.OutcomeNeedsApproval || e.world.Calls(fake.OpCreate) != 0 {
		t.Fatalf("a revoked approval authorized the plan: %+v", rep)
	}
	// Approving again is a new approval, and it works.
	e.approve(p)
	if rep := e.apply(p); outcomes(rep)[fake.OpCreate] != reconcile.OutcomeDone {
		t.Fatalf("after approving again: %+v", rep)
	}
}

// Revoking while an apply is under way stops what has not begun: the operations to come that need
// the approval do not run (the create did; the start after it does not).
func TestRevokingStopsARunInProgress(t *testing.T) {
	e := newEnv(t)
	p := e.plan()
	e.approve(p)
	r := e.reconciler("worker-a")
	r.Provider = hooked{World: e.world, before: func(step reconcile.Step) {
		if step.Op == fake.OpCreate {
			if _, err := e.store.Revoke(e.ctx, p.Digest, "carol", "stop"); err != nil {
				t.Error(err)
			}
		}
	}}
	rep, err := r.Apply(e.ctx, p.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stopped == nil || rep.Stopped.Code != reconcile.CodeRevoked {
		t.Fatalf("stopped %+v", rep.Stopped)
	}
	if e.world.Calls(fake.OpCreate) != 1 || e.world.Calls(fake.OpStart) != 0 {
		t.Fatalf("create %d, start %d: what had not begun ran", e.world.Calls(fake.OpCreate), e.world.Calls(fake.OpStart))
	}
}
