// Package reconcile carries out an approved plan through a Provider, safely:
//
//   - the plan is re-derived from its stored inputs (never trusted as stored text);
//   - the world is observed again right before acting, and a resource whose situation differs
//     from what the plan assumed is not touched (the plan must be made again);
//   - Denied operations never run; ApprovalRequired ones run only with a valid approval of that
//     exact plan digest;
//   - intent is written to the journal before every side effect, each operation is claimed under a
//     lease, and an operation whose outcome is unknown stops the run until the world is observed
//     again. Exactly-once execution is never assumed: completion is confirmed by observation.
package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
)

// ErrUnknownOutcome is returned by a Provider when it cannot tell whether an operation took
// effect (a timeout, a lost connection). The reconciler then stops and re-observes next time.
var ErrUnknownOutcome = errors.New("outcome unknown")

// ErrPartlyDone is returned by a Provider when an operation failed after some of its changes took
// effect (or may have): neither a clean failure nor unknown as a whole. The reconciler stops, and
// the next run observes before doing anything, so the operation continues from what is there.
var ErrPartlyDone = errors.New("partly done")

// Step is one operation handed to a provider.
type Step struct {
	OperationID string // stable idempotency key: the same plan, resource and operation give the same ID
	PlanDigest  string
	Op          policy.Op
	Resource    graph.Resource // the desired resource
	// Approved is the resource's change in the approved plan. A provider acts on what was approved
	// (for example the exact apt change set), never on a newer observation.
	Approved plan.Change
}

// Provider observes and changes the real world.
type Provider interface {
	Observe(ctx context.Context) (state.Snapshot, error)
	Apply(ctx context.Context, step Step) error
}

// Outcome is what happened to one operation in a run.
type Outcome string

const (
	OutcomeDone          Outcome = "done"           // executed and reported successful
	OutcomeAlreadyDone   Outcome = "already-done"   // observation shows the effect; not executed again
	OutcomeNeedsApproval Outcome = "needs-approval" // not executed: no valid approval of this plan
	OutcomeDenied        Outcome = "denied"         // never executed
	OutcomeBusy          Outcome = "busy"           // another worker holds the operation
	OutcomeFailed        Outcome = "failed"         // executed and known not to have taken effect
	OutcomeUnknown       Outcome = "unknown"        // executed, outcome unknown
	OutcomePartial       Outcome = "partly-done"    // failed after some of its changes took effect
)

// StepReport is one line of a run report.
type StepReport struct {
	Resource string
	Op       policy.Op
	Outcome  Outcome
	Detail   string
}

// Stop explains why a run stopped early. Code is a stable reason code.
type Stop struct {
	Code    string
	Message string
	Hint    string
}

// Report is the result of a run.
type Report struct {
	PlanDigest string
	Steps      []StepReport
	Stopped    *Stop
}

// Reconciler applies plans.
type Reconciler struct {
	Store    *store.Store
	Provider Provider
	Owner    string        // identity of this worker in the journal
	Lease    time.Duration // how long a claim lasts without being finished
	// Providers is the digest of the contracts of the providers this run uses
	// (spec.Providers.Digest). A plan made for other providers is not applied.
	Providers string
}

// OperationID is the idempotency key of one operation of one plan.
func OperationID(planDigest, resource string, op policy.Op) string {
	sum := sha256.Sum256([]byte(planDigest + "\x00" + resource + "\x00" + string(op)))
	return "op-" + hex.EncodeToString(sum[:12])
}

func orNone(s string) string {
	if s == "" {
		return "(none recorded)"
	}
	return s
}

// Apply runs the plan identified by digest as far as it safely can.
func (r *Reconciler) Apply(ctx context.Context, planDigest string) (*Report, error) {
	p, in, err := r.Store.LoadPlan(ctx, planDigest)
	if err != nil {
		return nil, err
	}
	rep := &Report{PlanDigest: planDigest}
	if in.Providers != r.Providers {
		rep.Stopped = &Stop{Code: "MAM-REC-0007",
			Message: fmt.Sprintf("the plan was made for providers %s, but this Mamela has %s; its approval does not cover them",
				orNone(in.Providers), orNone(r.Providers)),
			Hint: "make the plan again (mamela plan) and review and approve the new one"}
		return rep, nil
	}
	approval, err := r.Store.ValidApproval(ctx, planDigest)
	if err != nil {
		return nil, err
	}

	fresh, err := r.Provider.Observe(ctx)
	if err != nil {
		return nil, fmt.Errorf("observe before applying: %w", err)
	}
	if _, err := r.Store.PutSnapshot(ctx, fresh); err != nil {
		return nil, err
	}
	r.event(ctx, planDigest, "", "observed", "observed the world before applying (snapshot "+fresh.Digest()+")")
	now := plan.BuildFor(in.Revision, fresh, in.Policy, in.Providers)
	desired := map[string]graph.Resource{}
	for _, res := range in.Revision.Resources {
		desired[res.ID()] = res
	}

	// Operations run in phases: every VM of the run is created before any is started, so that no
	// first boot competes with another VM's disk import (a boot that waits too long for its disk
	// ends in emergency mode).
	for phase := range 2 {
		for _, full := range p.Changes {
			if full.Status != plan.StatusChange {
				continue
			}
			c := full
			c.Operations = slices.DeleteFunc(slices.Clone(full.Operations), func(o plan.Operation) bool { return opPhase(o.Op) != phase })
			if len(c.Operations) == 0 {
				continue
			}
			id := c.ID()
			switch {
			case c.Verdict == policy.Denied:
				r.addAll(rep, c, OutcomeDenied, "denied by policy; Mamela never runs it")
				continue
			case c.Verdict == policy.ApprovalRequired && approval == nil:
				r.addAll(rep, c, OutcomeNeedsApproval, "approve plan "+planDigest+" to run it")
				continue
			}

			current, found := findChange(now, id)
			if found && current.Status == plan.StatusInSync {
				// The effect is already visible (a previous run finished, or its response was lost).
				for _, op := range c.Operations {
					opID := OperationID(planDigest, id, op.Op)
					if _, err := r.Store.Intend(ctx, opID, planDigest, id, string(op.Op)); err != nil {
						return nil, err
					}
					if err := r.Store.Confirm(ctx, opID, "confirmed by observation"); err != nil {
						return nil, err
					}
					o, err := r.Store.Operation(ctx, opID)
					if err != nil {
						return nil, err
					}
					if o.Stage != store.StageSucceeded {
						rep.add(id, op.Op, OutcomeBusy, "another worker holds this operation")
						rep.Stopped = &Stop{Code: "MAM-REC-0005", Message: "another worker is applying this plan",
							Hint: "wait for it to finish, then run apply again"}
						return rep, nil
					}
					rep.add(id, op.Op, OutcomeAlreadyDone, "observation shows it is done")
				}
				continue
			}
			if !found || !partOf(current, full) {
				rep.Stopped = &Stop{Code: "MAM-REC-0002",
					Message: fmt.Sprintf("%s is no longer as the plan assumed; nothing more was changed", id),
					Hint:    "make a new plan from a fresh observation and review it"}
				r.event(ctx, planDigest, id, "conflict", rep.Stopped.Message)
				return rep, nil
			}

			for _, op := range c.Operations {
				opID := OperationID(planDigest, id, op.Op)
				if !slices.ContainsFunc(current.Operations, func(o plan.Operation) bool { return o.Op == op.Op }) {
					// Part of the approved change is already visible (an earlier run did it, or its
					// answer was lost): confirm it by observation instead of doing it again.
					if _, err := r.Store.Intend(ctx, opID, planDigest, id, string(op.Op)); err != nil {
						return nil, err
					}
					if err := r.Store.Confirm(ctx, opID, "confirmed by observation"); err != nil {
						return nil, err
					}
					rep.add(id, op.Op, OutcomeAlreadyDone, "observation shows it is done")
					continue
				}
				o, err := r.Store.Intend(ctx, opID, planDigest, id, string(op.Op))
				if err != nil {
					return nil, err
				}
				if o.Stage == store.StageSucceeded {
					rep.add(id, op.Op, OutcomeAlreadyDone, "the journal shows it succeeded")
					continue
				}
				claimed, err := r.Store.Claim(ctx, opID, r.Owner, r.Lease)
				if err != nil {
					return nil, err
				}
				if !claimed {
					rep.add(id, op.Op, OutcomeBusy, "another worker holds this operation")
					rep.Stopped = &Stop{Code: "MAM-REC-0005", Message: "another worker is applying this plan",
						Hint: "wait for it to finish, then run apply again"}
					return rep, nil
				}
				r.event(ctx, planDigest, id, "started", fmt.Sprintf("%s started (operation %s)", op.Op, opID))
				applyErr := r.Provider.Apply(ctx, Step{OperationID: opID, PlanDigest: planDigest, Op: op.Op, Resource: desired[id], Approved: full})
				switch {
				case applyErr == nil:
					if err := r.Store.Finish(ctx, opID, r.Owner, store.StageSucceeded, ""); err != nil {
						return nil, err
					}
					rep.add(id, op.Op, OutcomeDone, "")
					r.event(ctx, planDigest, id, "succeeded", string(op.Op)+" succeeded")
				case errors.Is(applyErr, ErrPartlyDone):
					// Recorded like an unknown outcome: it must be observed before it is retried.
					if err := r.Store.Finish(ctx, opID, r.Owner, store.StageUnknown, applyErr.Error()); err != nil {
						return nil, err
					}
					rep.add(id, op.Op, OutcomePartial, applyErr.Error())
					rep.Stopped = &Stop{Code: "MAM-REC-0006",
						Message: fmt.Sprintf("%s on %s failed after some of its changes took effect: %v", op.Op, id, applyErr),
						Hint:    "fix the cause, then run apply again: it observes first and continues from what is there"}
					r.event(ctx, planDigest, id, "partly-done", rep.Stopped.Message)
					return rep, nil
				case errors.Is(applyErr, ErrUnknownOutcome):
					if err := r.Store.Finish(ctx, opID, r.Owner, store.StageUnknown, applyErr.Error()); err != nil {
						return nil, err
					}
					rep.add(id, op.Op, OutcomeUnknown, applyErr.Error())
					rep.Stopped = &Stop{Code: "MAM-REC-0003",
						Message: fmt.Sprintf("the outcome of %s on %s is unknown; Mamela does not assume it succeeded", op.Op, id),
						Hint:    "run apply again: it observes first and either confirms the result or retries"}
					r.event(ctx, planDigest, id, "unknown", rep.Stopped.Message)
					return rep, nil
				default:
					if err := r.Store.Finish(ctx, opID, r.Owner, store.StageFailed, applyErr.Error()); err != nil {
						return nil, err
					}
					rep.add(id, op.Op, OutcomeFailed, applyErr.Error())
					rep.Stopped = &Stop{Code: "MAM-REC-0004",
						Message: fmt.Sprintf("%s on %s failed: %v", op.Op, id, applyErr),
						Hint:    "fix the cause, then run apply again; the failed operation is retried"}
					r.event(ctx, planDigest, id, "failed", rep.Stopped.Message)
					return rep, nil
				}
			}
		}
	}
	return rep, nil
}

// opPhase is the phase an operation runs in: starting a VM comes after everything else.
func opPhase(op policy.Op) int {
	if op == policy.OpStartVM {
		return 1
	}
	return 0
}

func (r *Reconciler) addAll(rep *Report, c plan.Change, outcome Outcome, detail string) {
	for _, op := range c.Operations {
		rep.add(c.ID(), op.Op, outcome, detail)
	}
}

func (rep *Report) add(resource string, op policy.Op, outcome Outcome, detail string) {
	rep.Steps = append(rep.Steps, StepReport{Resource: resource, Op: op, Outcome: outcome, Detail: detail})
}

func (r *Reconciler) event(ctx context.Context, planDigest, resource, kind, message string) {
	// The event log is for people; failing to write it must not hide the real outcome.
	_ = r.Store.AppendEvent(ctx, planDigest, resource, kind, message)
}

func findChange(p *plan.Plan, id string) (plan.Change, bool) {
	for _, c := range p.Changes {
		if c.ID() == id {
			return c, true
		}
	}
	return plan.Change{}, false
}

// partOf reports whether the fresh change asks for what the approved one did, or for part of it:
// the same status, and every remaining diff and operation (with its verdict) was in the approved
// change. Anything new or different means the plan must be made again.
func partOf(fresh, approved plan.Change) bool {
	if fresh.Status != approved.Status || len(fresh.Operations) == 0 {
		return false
	}
	// A provider's change must still be exactly an approved one: the same requests, objects and
	// precondition. Anything else (a new version of the object, other values) needs a new plan.
	if fresh.Provider != approved.Provider || !plan.SameProviderChanges(fresh.ProviderChanges, approved.ProviderChanges) {
		return false
	}
	for _, d := range fresh.Diffs {
		if !slices.Contains(approved.Diffs, d) {
			return false
		}
	}
	for _, o := range fresh.Operations {
		if !slices.ContainsFunc(approved.Operations, func(x plan.Operation) bool {
			return x.Op == o.Op && x.Decision.Verdict == o.Decision.Verdict
		}) {
			return false
		}
	}
	return true
}
