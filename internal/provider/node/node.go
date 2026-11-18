// Package node is the provider for what lives inside VMs (APT packages, systemd services). It does
// not touch the VMs itself: it reads mamelet's reports as observations, and carries out an
// approved operation by adding a one-time grant to the node's revision and waiting until mamelet
// reports the result.
package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
)

// Provider implements reconcile.Provider for node resources.
type Provider struct {
	Store        *store.Store
	Clock        func() time.Time
	Sleep        func(context.Context, time.Duration) error // nil: a real sleep
	Timeout      time.Duration                              // waiting for mamelet's result (default 10m)
	PollInterval time.Duration                              // default 2s
}

var _ reconcile.Provider = (*Provider)(nil)

// Observe turns the latest report of every node into observations, timestamped with when the
// Control Plane received them (so staleness is measured on one clock).
func (p *Provider) Observe(ctx context.Context) (state.Snapshot, error) {
	snap := state.Empty(p.Clock())
	reports, err := p.Store.NodeReports(ctx)
	if err != nil {
		return state.Snapshot{}, err
	}
	for _, nr := range reports {
		r := nr.Report
		a, enrolled, err := p.Store.Agent(ctx, r.Node)
		if err != nil {
			return state.Snapshot{}, err
		}
		if enrolled {
			if !nr.Current(a) {
				continue
			}
			if snap.AgentRegistrations == nil {
				snap.AgentRegistrations = map[string]string{}
			}
			snap.AgentRegistrations[r.Node] = a.CertSerial
		}
		rev, err := p.Store.NodeRevision(ctx, r.Node)
		if err != nil {
			return state.Snapshot{}, err
		}
		if r.Refused != "" {
			// mamelet did not accept the revision on offer: no permission reaches this node.
			if snap.NodeRefusals == nil {
				snap.NodeRefusals = map[string]state.NodeRefusal{}
			}
			refusal := state.NodeRefusal{WorksFrom: r.Serial, Reason: printable(r.Refused, maxRefusal), ObservedAt: nr.Received}
			if rev != nil {
				refusal.Serial = rev.Serial
			}
			snap.NodeRefusals[r.Node] = refusal
		}
		for _, pk := range r.Packages {
			snap.Packages[r.Node+"/"+pk.Name] = state.Package{Installed: pk.Installed, Version: pk.Version,
				ChangeSet: pk.ChangeSet, Error: pk.Error, ObservedAt: nr.Received}
		}
		for _, sv := range r.Services {
			repair := false
			if rev != nil && rev.VMID == r.VMID {
				if active := findService(rev, sv.Name); active != nil {
					repair = active.Running && slices.Contains(active.Repair, noderev.ActionStart)
				}
			}
			snap.Services[r.Node+"/"+sv.Name] = state.Service{Exists: sv.Exists, Enabled: sv.Enabled, Running: sv.Running,
				RepairStart: repair, ObservedAt: nr.Received}
		}
		if r.MameletSHA256 != "" {
			if snap.Mamelets == nil {
				snap.Mamelets = map[string]state.Mamelet{}
			}
			snap.Mamelets[r.Node+"/mamelet"] = state.Mamelet{SHA256: r.MameletSHA256, Version: r.Version, ObservedAt: nr.Received}
		}
		for _, e := range extensions {
			if e.Observe != nil {
				e.Observe(r, nr.Received, &snap)
			}
		}
		observeExt(r, nr.Received, &snap)
	}
	return snap, nil
}

// maxRefusal bounds the reason a mamelet gives for refusing a revision, in characters.
const maxRefusal = 300

// printable is what a node says of itself, made safe to show in a plan: only printable characters
// (a node is trusted to report, not to lay out a person's screen or a PR comment), at most limit.
func printable(s string, limit int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == limit {
			b.WriteString("…")
			break
		}
		if !unicode.IsPrint(r) {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// Publish requests observations without activating a candidate declaration. Existing desired
// values and grants survive unchanged, whatever the candidate declares or no longer declares: a
// plan is made of declarations that nobody approved (a pull request, a branch), and must not take
// away what the declaration in force allows. New targets have no permission to change the machine.
// What a declaration no longer has loses its permissions only when a plan made from it is approved
// and applied (Prune).
//
// A protected node is the exception: local safety revokes all grants, including undeclared ones.
// Control Plane relocation is not an observation and is never published by plan.
func Publish(ctx context.Context, st *store.Store, g *graph.Graph, pol policy.Policy, _ string, nodeIDs map[string]int64) error {
	for _, machine := range g.Sorted() {
		vmid := int(nodeIDs[machine.Name])
		if machine.Provided == nil || vmid <= 0 {
			continue
		}
		name := machine.Name
		var pkgs []noderev.Package
		var svcs []noderev.Service
		for _, r := range g.Sorted() {
			switch {
			case r.NodePackage != nil && r.NodePackage.Node == name:
				pkgs = append(pkgs, noderev.Package{Name: r.NodePackage.Package})
			case r.NodeService != nil && r.NodeService.Node == name:
				svcs = append(svcs, noderev.Service{Name: r.NodeService.Service})
			}
		}
		// What the extensions add to the node's revision, worked out before it is written.
		var adds []func(r *noderev.Revision)
		for _, e := range extensions {
			if e.Publish == nil {
				continue
			}
			add, err := e.Publish(ctx, st, g, name)
			if err != nil {
				return err
			}
			adds = append(adds, add)
		}
		report, err := taken(ctx, st, name)
		if err != nil {
			return fmt.Errorf("observe node %s: %w", name, err)
		}
		if _, err := st.PublishNodeRevision(ctx, name, func(prev *noderev.Revision) (noderev.Revision, error) {
			r := noderev.Revision{VMID: vmid}
			if prev != nil {
				if prev.VMID != vmid {
					return r, fmt.Errorf("MAM-NODE-0101: node %s changed identity; revoke its old registration before reusing its name", name)
				}
				r = *prev
				r.Packages = slices.Clone(prev.Packages)
				r.Services = slices.Clone(prev.Services)
			}
			for _, p := range pkgs {
				if findPackage(&r, p.Name) == nil {
					r.Packages = append(r.Packages, p)
				}
			}
			for _, s := range svcs {
				if findService(&r, s.Name) == nil {
					r.Services = append(r.Services, s)
				}
			}
			for _, add := range adds {
				add(&r)
			}
			// An extension's part goes only to a mamelet that takes it (noderev's ext.go): an older
			// mamelet would refuse the whole revision.
			r.KeepTaken(report)
			if pol.Protected(name) {
				for i := range r.Packages {
					r.Packages[i].Install = nil
				}
				for i := range r.Services {
					r.Services[i].Repair, r.Services[i].Once = nil, nil
				}
				r.Mamelet = nil
				for _, e := range extensions {
					if e.Protect != nil {
						e.Protect(&r)
					}
				}
			}
			return r, nil
		}); err != nil {
			return fmt.Errorf("observe node %s: %w", name, err)
		}
	}
	return nil
}

// errNoRevision ends a Prune of a node that has no revision (there is nothing to take away).
var errNoRevision = errors.New("no revision")

// Prune takes away, from the revision of each machine g declares, what g no longer has: the
// package, service and mamelet items, and the node extensions' (manifest, k3s), with every
// permission they carry (a one-time grant, a standing repair, a manifest's write). A permission is
// for something the declaration in force wants. It returns the nodes whose revision changed.
//
// It is for the declaration of a plan that has been approved, when that plan is applied
// (reconcile.Reconciler.Prune), and never for planning: a plan is made of declarations that nobody
// has approved (a pull request, a branch), and what they leave out must not take away what the
// declaration in force allows (see Publish). It takes away permissions and what mamelet watches,
// and does nothing to the machine: the package stays installed, the service stays as it is, the
// manifest stays in the directory (ADR 0013 section 6). A machine g does not declare, and a node
// that has no revision, are left alone.
func Prune(ctx context.Context, st *store.Store, g *graph.Graph) ([]string, error) {
	var changed []string
	for _, machine := range g.Sorted() {
		if machine.Provided == nil {
			continue
		}
		name := machine.Name
		declared := declaredOn(g, name)
		before, err := st.NodeRevision(ctx, name)
		if err != nil {
			return changed, fmt.Errorf("prune node %s: %w", name, err)
		}
		if before == nil {
			continue
		}
		report, err := taken(ctx, st, name)
		if err != nil {
			return changed, fmt.Errorf("prune node %s: %w", name, err)
		}
		after, err := st.PublishNodeRevision(ctx, name, func(prev *noderev.Revision) (noderev.Revision, error) {
			if prev == nil {
				return noderev.Revision{}, errNoRevision
			}
			r := *prev
			r.Packages = slices.DeleteFunc(slices.Clone(prev.Packages), func(p noderev.Package) bool { return !declared.packages[p.Name] })
			r.Services = slices.DeleteFunc(slices.Clone(prev.Services), func(s noderev.Service) bool { return !declared.services[s.Name] })
			if !declared.mamelet {
				r.Mamelet = nil
			}
			for _, e := range extensions {
				if e.Prune != nil {
					e.Prune(g, name, &r)
				}
			}
			r.KeepTaken(report)
			return r, nil
		})
		switch {
		case errors.Is(err, errNoRevision):
		case err != nil:
			return changed, fmt.Errorf("prune node %s: %w", name, err)
		case after.Serial != before.Serial:
			changed = append(changed, name)
		}
	}
	return changed, nil
}

// declared is what a declaration has on one node.
type declared struct {
	packages, services map[string]bool
	mamelet            bool
}

func declaredOn(g *graph.Graph, node string) declared {
	d := declared{packages: map[string]bool{}, services: map[string]bool{}}
	for _, r := range g.Resources {
		switch {
		case r.NodePackage != nil && r.NodePackage.Node == node:
			d.packages[r.NodePackage.Package] = true
		case r.NodeService != nil && r.NodeService.Node == node:
			d.services[r.NodeService.Service] = true
		case r.NodeMamelet != nil && r.NodeMamelet.Node == node:
			d.mamelet = true
		}
	}
	return d
}

func findPackage(r *noderev.Revision, name string) *noderev.Package {
	for i := range r.Packages {
		if r.Packages[i].Name == name {
			return &r.Packages[i]
		}
	}
	return nil
}

func findService(r *noderev.Revision, name string) *noderev.Service {
	for i := range r.Services {
		if r.Services[i].Name == name {
			return &r.Services[i]
		}
	}
	return nil
}

// Apply grants one approved operation to the node's mamelet and waits for its result.
func (p *Provider) Apply(ctx context.Context, step reconcile.Step) error {
	op, err := p.Store.Operation(ctx, step.OperationID)
	if err != nil {
		return err
	}
	attempt := op.Attempt
	if attempt < 1 {
		attempt = 1
	}
	_, inputs, err := p.Store.LoadPlan(ctx, step.PlanDigest)
	if err != nil {
		return err
	}
	var node, what string
	configure := step.Op == policy.OpConfigureService
	var exp time.Time // when the grant below stops being handed out (set before it is issued)
	var grant func(r *noderev.Revision) error
	switch r := step.Resource; {
	case step.Op == policy.OpInstallPackage && r.NodePackage != nil:
		node, what = r.NodePackage.Node, "install "+r.NodePackage.Package
		digest := approvedChangeSet(step.Approved)
		if digest == "" {
			return fmt.Errorf("%s: the approved plan has no change set", what)
		}
		grant = func(rev *noderev.Revision) error {
			pk := findPackage(rev, r.NodePackage.Package)
			if pk == nil {
				return fmt.Errorf("package %s is not in the revision of %s (plan again)", r.NodePackage.Package, node)
			}
			pk.Install = &noderev.Install{Operation: step.OperationID, Attempt: attempt, ChangeSet: digest, Expires: exp}
			return nil
		}
	case step.Op == policy.OpUpdateMamelet && r.NodeMamelet != nil:
		// Exactly the release the approved plan showed, which is the declared one, or nothing happens.
		m := *r.NodeMamelet
		if want := approvedDiff(step.Approved, "sha256"); want != m.SHA256 {
			return fmt.Errorf("the approved plan updates mamelet to %q, the declaration pins %q (plan again)", want, m.SHA256)
		}
		node, what = m.Node, "update mamelet to "+m.SHA256[:12]
		grant = func(rev *noderev.Revision) error {
			rev.Mamelet = &noderev.MameletUpdate{SHA256: m.SHA256, Once: noderev.Once{Operation: step.OperationID, Attempt: attempt, Action: noderev.ActionUpdate, Expires: exp}}
			return nil
		}
	case (serviceActions[step.Op] != "" || configure) && r.NodeService != nil:
		action := serviceActions[step.Op]
		node, what = r.NodeService.Node, action+" "+r.NodeService.Service
		grant = func(rev *noderev.Revision) error {
			sv := findService(rev, r.NodeService.Service)
			if sv == nil {
				return fmt.Errorf("service %s is not in the revision of %s (plan again)", r.NodeService.Service, node)
			}
			if configure {
				sv.Repair = nil
				if r.NodeService.Running && inputs.Policy.Decide(policy.Subject{Op: policy.OpStartService, Service: sv.Name}).Verdict == policy.Auto {
					sv.Running = true
					sv.Repair = []string{noderev.ActionStart}
				}
				return nil
			}
			if action == noderev.ActionStart || action == noderev.ActionStop {
				sv.Running = action == noderev.ActionStart
				if !sv.Running {
					sv.Repair = nil
				}
			} else {
				sv.Enabled = action == noderev.ActionEnable
			}
			sv.Once = slices.DeleteFunc(slices.Clone(sv.Once), func(o noderev.Once) bool {
				return (o.Action == noderev.ActionStart || o.Action == noderev.ActionStop) == (action == noderev.ActionStart || action == noderev.ActionStop)
			})
			sv.Once = append(sv.Once, noderev.Once{Operation: step.OperationID, Attempt: attempt, Action: action, Expires: exp})
			return nil
		}
	default:
		gf, ok := grants[step.Op]
		if !ok {
			return fmt.Errorf("mamelet does not carry out %s", step.Op)
		}
		// An extension's operation: it says on which node, what, and what the grant puts in the
		// revision; the core gives the grant its operation, attempt and deadline, and waits.
		g, ok, err := gf(ctx, GrantRequest{Store: p.Store, Step: step, Declaration: inputs.Revision})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("mamelet does not carry out %s", step.Op)
		}
		once := func(action string) noderev.Once {
			return noderev.Once{Operation: step.OperationID, Attempt: attempt, Action: action, Expires: exp}
		}
		node, what = g.Node, g.What
		grant = func(rev *noderev.Revision) error { return g.Put(rev, once) }
		if ext := wireOps[step.Op]; ext != "" {
			// The grant goes into the extension's part: a mamelet that does not take the part would never
			// see it (the plan blocks such a change: MAM-PLAN-0122).
			report, err := taken(ctx, p.Store, node)
			if err != nil {
				return err
			}
			if !noderev.Takes(report, ext) {
				return fmt.Errorf("MAM-NODE-0104: %s on %s: the mamelet there does not take the %s extension (it is older than it); update mamelet first (docs/mamelet-update.md), then plan again",
					what, node, ext)
			}
		}
	}

	if !configure {
		if exp, err = p.deadline(ctx, step); err != nil {
			return fmt.Errorf("%s on %s: %w", what, node, err)
		}
	}
	if _, err := p.Store.PublishNodeRevisionForAgent(ctx, node, inputs.Snapshot.AgentRegistrations[node], func(prev *noderev.Revision) (noderev.Revision, error) {
		if prev == nil {
			return noderev.Revision{}, fmt.Errorf("node %s has no revision yet (plan with --site first)", node)
		}
		if expected := inputs.Snapshot.Nodes[node].ID; expected > 0 && int64(prev.VMID) != expected {
			return noderev.Revision{}, fmt.Errorf("MAM-NODE-0101: node %s is a different machine; plan again", node)
		}
		r := *prev
		r.Packages = slices.Clone(prev.Packages)
		r.Services = slices.Clone(prev.Services)
		return r, grant(&r)
	}); err != nil {
		return fmt.Errorf("%s on %s: %w", what, node, err)
	}
	if configure {
		return nil
	}
	return p.await(ctx, node, what, step.OperationID, attempt)
}

// GrantGrace is how long a grant stays good after the apply that issued it has stopped waiting for
// mamelet's answer (the outcome is then unknown): a mamelet that was only slow, or restarting, still
// does it, and the next apply confirms it by observation. A grant never outlives the approval it
// comes from, and never the declaration that has the item (Publish takes it away with the item).
const GrantGrace = 15 * time.Minute

// deadline is when a grant issued now for an operation of the approved plan stops being handed
// out: while the apply waits for the result and the grace after it, or until the approval ends if
// that is sooner. An operation that needs approval is not granted once the approval has ended.
func (p *Provider) deadline(ctx context.Context, step reconcile.Step) (time.Time, error) {
	end := p.Clock().Add(p.timeout() + GrantGrace)
	approval, err := p.Store.ValidApproval(ctx, step.PlanDigest)
	switch {
	case err != nil:
		return time.Time{}, err
	case approval == nil && step.Approved.Verdict == policy.ApprovalRequired:
		return time.Time{}, fmt.Errorf("MAM-NODE-0120: the approval of plan %s has ended, so no permission is given to the node; "+
			"approve the plan again and apply it", step.PlanDigest)
	case approval != nil && approval.ExpiresAt.Before(end):
		end = approval.ExpiresAt
	}
	return end.UTC(), nil
}

// serviceActions are the service operations mamelet carries out, as one-time grants.
var serviceActions = map[policy.Op]string{
	policy.OpEnableService:  noderev.ActionEnable,
	policy.OpStartService:   noderev.ActionStart,
	policy.OpDisableService: noderev.ActionDisable,
	policy.OpStopService:    noderev.ActionStop,
}

// approvedChangeSet is the change set digest in the approved change's diffs.
func approvedChangeSet(c plan.Change) string { return approvedDiff(c, "changeSet") }

// approvedDiff is the value a field gets in the approved change ("" when it has no such diff).
func approvedDiff(c plan.Change, field string) string {
	for _, d := range c.Diffs {
		if d.Field == field {
			return d.To
		}
	}
	return ""
}

// timeout is how long Apply waits for mamelet's result.
func (p *Provider) timeout() time.Duration {
	if p.Timeout == 0 {
		return 10 * time.Minute
	}
	return p.Timeout
}

func (p *Provider) await(ctx context.Context, node, what, opID string, attempt int) error {
	timeout, interval := p.timeout(), p.PollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	start := p.Clock()
	for {
		nr, ok, err := p.Store.NodeReportOf(ctx, node)
		if err != nil {
			return err
		}
		if ok {
			for _, res := range nr.Report.Results {
				if res.Operation != opID || res.Attempt != attempt {
					continue
				}
				switch res.Outcome {
				case noderev.OutcomeDone:
					return nil
				case noderev.OutcomeRefused:
					return fmt.Errorf("mamelet on %s refused to %s: %s", node, what, res.Detail)
				default:
					return fmt.Errorf("mamelet on %s failed to %s: %s", node, what, res.Detail)
				}
			}
		}
		if p.Clock().Sub(start) > timeout {
			return fmt.Errorf("mamelet on %s has not reported whether it could %s within %s (is it running and connected?); "+
				"it may still do it: %w", node, what, timeout, reconcile.ErrUnknownOutcome)
		}
		if err := p.sleep(ctx, interval); err != nil {
			return fmt.Errorf("wait for mamelet on %s: %v: %w", node, err, reconcile.ErrUnknownOutcome)
		}
	}
}

func (p *Provider) sleep(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
