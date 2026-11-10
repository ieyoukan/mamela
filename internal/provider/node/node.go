// Package node is the provider for what lives inside VMs (APT packages, systemd services). It does
// not touch the VMs itself: it reads mamelet's reports as observations, and carries out an
// approved operation by adding a one-time grant to the node's revision and waiting until mamelet
// reports the result.
package node

import (
	"context"
	"fmt"
	"slices"
	"time"

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
		for _, pk := range r.Packages {
			snap.Packages[r.Node+"/"+pk.Name] = state.Package{Installed: pk.Installed, Version: pk.Version,
				ChangeSet: pk.ChangeSet, Error: pk.Error, ObservedAt: nr.Received}
		}
		for _, sv := range r.Services {
			snap.Services[r.Node+"/"+sv.Name] = state.Service{Exists: sv.Exists, Enabled: sv.Enabled, Running: sv.Running,
				ObservedAt: nr.Received}
		}
	}
	return snap, nil
}

// Publish writes each declared node's revision: which packages and services mamelet observes,
// and the standing repairs the policy allows without approval (starting a service listed in
// autoStartServices). One-time grants already in a revision are kept while their package or
// service is still declared. Nodes without a reserved VMID (not created yet) are skipped.
func Publish(ctx context.Context, st *store.Store, g *graph.Graph, pol policy.Policy) error {
	type nodeRes struct {
		pkgs []graph.NodePackageSpec
		svcs []graph.NodeServiceSpec
	}
	nodes := map[string]*nodeRes{}
	var names []string
	for _, r := range g.Sorted() {
		switch r.Kind {
		case graph.KindVirtualMachine:
			nodes[r.Name] = &nodeRes{}
			names = append(names, r.Name)
		case graph.KindNodePackage:
			if n := nodes[r.NodePackage.Node]; n != nil {
				n.pkgs = append(n.pkgs, *r.NodePackage)
			}
		case graph.KindNodeService:
			if n := nodes[r.NodeService.Node]; n != nil {
				n.svcs = append(n.svcs, *r.NodeService)
			}
		}
	}
	for _, name := range names {
		vmid, ok, err := st.VMID(ctx, name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		n := nodes[name]
		if _, err := st.PublishNodeRevision(ctx, name, func(prev *noderev.Revision) (noderev.Revision, error) {
			r := noderev.Revision{VMID: vmid, Packages: []noderev.Package{}, Services: []noderev.Service{}}
			for _, pk := range n.pkgs {
				np := noderev.Package{Name: pk.Package}
				if prev != nil && prev.VMID == vmid {
					if old := findPackage(prev, pk.Package); old != nil {
						np.Install = old.Install
					}
				}
				r.Packages = append(r.Packages, np)
			}
			for _, sv := range n.svcs {
				ns := noderev.Service{Name: sv.Service, Enabled: sv.Enabled, Running: sv.Running, Repair: []string{}, Once: []noderev.Once{}}
				if sv.Running && pol.Decide(policy.Subject{Op: policy.OpStartService, Service: sv.Service}).Verdict == policy.Auto {
					ns.Repair = []string{noderev.ActionStart}
				}
				if prev != nil && prev.VMID == vmid {
					if old := findService(prev, sv.Service); old != nil {
						ns.Once = old.Once
					}
				}
				r.Services = append(r.Services, ns)
			}
			return r, nil
		}); err != nil {
			return fmt.Errorf("publish the revision of %s: %w", name, err)
		}
	}
	return nil
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
	var node, what string
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
			pk.Install = &noderev.Install{Operation: step.OperationID, Attempt: attempt, ChangeSet: digest}
			return nil
		}
	case (step.Op == policy.OpEnableService || step.Op == policy.OpStartService) && r.NodeService != nil:
		action := noderev.ActionEnable
		if step.Op == policy.OpStartService {
			action = noderev.ActionStart
		}
		node, what = r.NodeService.Node, action+" "+r.NodeService.Service
		grant = func(rev *noderev.Revision) error {
			sv := findService(rev, r.NodeService.Service)
			if sv == nil {
				return fmt.Errorf("service %s is not in the revision of %s (plan again)", r.NodeService.Service, node)
			}
			sv.Once = slices.DeleteFunc(slices.Clone(sv.Once), func(o noderev.Once) bool { return o.Action == action })
			sv.Once = append(sv.Once, noderev.Once{Operation: step.OperationID, Attempt: attempt, Action: action})
			return nil
		}
	default:
		return fmt.Errorf("mamelet does not carry out %s", step.Op)
	}

	if _, err := p.Store.PublishNodeRevision(ctx, node, func(prev *noderev.Revision) (noderev.Revision, error) {
		if prev == nil {
			return noderev.Revision{}, fmt.Errorf("node %s has no revision yet (plan with --site first)", node)
		}
		r := *prev
		r.Packages = slices.Clone(prev.Packages)
		r.Services = slices.Clone(prev.Services)
		return r, grant(&r)
	}); err != nil {
		return fmt.Errorf("%s on %s: %w", what, node, err)
	}
	return p.await(ctx, node, what, step.OperationID, attempt)
}

// approvedChangeSet is the change set digest in the approved change's diffs.
func approvedChangeSet(c plan.Change) string {
	for _, d := range c.Diffs {
		if d.Field == "changeSet" {
			return d.To
		}
	}
	return ""
}

func (p *Provider) await(ctx context.Context, node, what, opID string, attempt int) error {
	timeout, interval := p.Timeout, p.PollInterval
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
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

// Combined observes and changes both the VMs (vms) and what lives inside them (nodes).
type Combined struct {
	VMs   reconcile.Provider
	Nodes reconcile.Provider
}

// Observe merges the VM snapshot with the node snapshot.
func (c Combined) Observe(ctx context.Context) (state.Snapshot, error) {
	vms, err := c.VMs.Observe(ctx)
	if err != nil {
		return state.Snapshot{}, err
	}
	nodes, err := c.Nodes.Observe(ctx)
	if err != nil {
		return state.Snapshot{}, err
	}
	vms.Packages, vms.Services = nodes.Packages, nodes.Services
	return vms, nil
}

// Apply sends VM operations to the VM provider and the rest to the node provider.
func (c Combined) Apply(ctx context.Context, step reconcile.Step) error {
	if step.Resource.Kind == graph.KindVirtualMachine {
		return c.VMs.Apply(ctx, step)
	}
	return c.Nodes.Apply(ctx, step)
}
