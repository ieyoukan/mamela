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
		a, enrolled, err := p.Store.Agent(ctx, r.Node)
		if err != nil {
			return state.Snapshot{}, err
		}
		if enrolled {
			if a.VMID != r.VMID || nr.Received.Before(a.Enrolled) {
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
		for _, m := range r.Manifests {
			if snap.K3sManifests == nil {
				snap.K3sManifests = map[string]state.K3sManifest{}
			}
			snap.K3sManifests[r.Node+"/manifest/"+m.Name] = state.K3sManifest{SHA256: m.SHA256, Error: m.Error, ObservedAt: nr.Received}
		}
		if k := r.K3s; k != nil {
			if snap.K3s == nil {
				snap.K3s = map[string]state.K3s{}
			}
			var nodes []state.K3sNode
			for _, n := range k.Nodes {
				nodes = append(nodes, state.K3sNode{Name: n.Name, Version: n.Version, Ready: n.Ready})
			}
			snap.K3s[r.Node+"/k3s"] = state.K3s{Installed: k.Installed, SHA256: k.SHA256, Error: k.Error, Server: k.Server,
				Running: k.Running, Stale: k.Stale, Nodes: nodes,
				Addresses: slices.Sorted(slices.Values(k.Addresses)), ObservedAt: nr.Received}
		}
	}
	return snap, nil
}

// Publish requests observations without activating a candidate declaration. Existing desired
// values and grants survive unchanged; new targets have no permission to change the machine.
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
		var k3s *noderev.K3s
		var manifests []noderev.Manifest
		for _, r := range g.Sorted() {
			switch {
			case r.NodeK3sManifest != nil && r.NodeK3sManifest.Node == name:
				manifests = append(manifests, noderev.Manifest{Name: r.NodeK3sManifest.Name, Content: r.NodeK3sManifest.Content})
			case r.NodePackage != nil && r.NodePackage.Node == name:
				pkgs = append(pkgs, noderev.Package{Name: r.NodePackage.Package})
			case r.NodeService != nil && r.NodeService.Node == name:
				svcs = append(svcs, noderev.Service{Name: r.NodeService.Service})
			case r.NodeK3s != nil && r.NodeK3s.Node == name:
				kr, ok, err := k3sRevision(ctx, st, *r.NodeK3s, g.StaticIP(r.NodeK3s.Server))
				if err != nil {
					return err
				}
				if ok {
					k3s = &kr
				}
			}
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
				r.Manifests = slices.Clone(prev.Manifests)
			}
			for _, m := range manifests {
				if findManifest(&r, m.Name) == nil {
					r.Manifests = append(r.Manifests, m)
				}
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
			if r.K3s == nil {
				r.K3s = k3s
			}
			if rm := r.K3s; rm != nil && rm.Remove != nil && declaresNode(g, name, rm.Remove.Node) {
				// A node that is declared again is never removed by an old grant.
				k := *rm
				k.Remove = nil
				r.K3s = &k
			}
			if pol.Protected(name) {
				for i := range r.Packages {
					r.Packages[i].Install = nil
				}
				for i := range r.Services {
					r.Services[i].Repair, r.Services[i].Once = nil, nil
				}
				if r.K3s != nil {
					k := *r.K3s
					k.Install = nil
					r.K3s = &k
				}
				for i := range r.Manifests {
					r.Manifests[i].Write = nil
				}
			}
			return r, nil
		}); err != nil {
			return fmt.Errorf("observe node %s: %w", name, err)
		}
	}
	return nil
}

// k3sRevision is what a node's revision says about k3s: the release, the role, the cluster's
// token (made once per cluster, keyed by its server) and, for an agent, its server's address as
// the server's mamelet last reported it, unless the server has a declared static address
// (serverIP). ok is false while an agent's server has no address yet.
func k3sRevision(ctx context.Context, st *store.Store, k graph.NodeK3sSpec, serverIP string) (noderev.K3s, bool, error) {
	kr := noderev.K3s{Version: k.Version, SHA256: k.SHA256, Role: string(k.Role)}
	cluster := k.Node
	if k.Role == graph.K3sAgent && serverIP != "" {
		cluster, kr.Server = k.Server, graph.K3sServerURL(serverIP)
	} else if k.Role == graph.K3sAgent {
		cluster = k.Server
		url, ok, err := K3sServerURL(ctx, st, k.Server)
		if err != nil || !ok {
			return kr, false, err
		}
		kr.Server = url
	}
	token, err := st.K3sToken(ctx, cluster)
	if err != nil {
		return kr, false, err
	}
	kr.Token = token
	return kr, true, nil
}

// K3sServerURL is the address an agent joins a server node at, from the server's last report.
func K3sServerURL(ctx context.Context, st *store.Store, server string) (string, bool, error) {
	nr, ok, err := st.NodeReportOf(ctx, server)
	if err != nil || !ok || nr.Report.K3s == nil || len(nr.Report.K3s.Addresses) == 0 {
		return "", false, err
	}
	return "https://" + slices.Sorted(slices.Values(nr.Report.K3s.Addresses))[0] + ":6443", true, nil
}

// declaresNode is whether the declaration has a k3s node called node that joins server (or is it).
func declaresNode(g *graph.Graph, server, node string) bool {
	for _, r := range g.Sorted() {
		if k := r.NodeK3s; k != nil && k.Node == node && (k.Server == server || k.Node == server) {
			return true
		}
	}
	return false
}

func findPackage(r *noderev.Revision, name string) *noderev.Package {
	for i := range r.Packages {
		if r.Packages[i].Name == name {
			return &r.Packages[i]
		}
	}
	return nil
}

func findManifest(r *noderev.Revision, name string) *noderev.Manifest {
	for i := range r.Manifests {
		if r.Manifests[i].Name == name {
			return &r.Manifests[i]
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
	case (step.Op == policy.OpInstallK3s || step.Op == policy.OpUpgradeK3s) && r.NodeK3s != nil:
		kr, ok, err := k3sRevision(ctx, p.Store, *r.NodeK3s, inputs.Revision.StaticIP(r.NodeK3s.Server))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("k3s server address is not observed; plan again")
		}
		node, what = r.NodeK3s.Node, "install k3s "+r.NodeK3s.Version
		if step.Op == policy.OpUpgradeK3s {
			what = "upgrade to k3s " + r.NodeK3s.Version
		}
		grant = func(rev *noderev.Revision) error {
			// An agent joins the server the approved plan named, or nothing happens.
			if want := approvedDiff(step.Approved, "server"); kr.Server != want {
				return fmt.Errorf("the revision of %s names server %q, the approved plan %q (plan again)", node, kr.Server, want)
			}
			k := kr
			k.Install = &noderev.Once{Operation: step.OperationID, Attempt: attempt, Action: noderev.ActionInstall}
			rev.K3s = &k
			return nil
		}
	case step.Op == policy.OpRemoveK3sNode && r.NodeK3s != nil && r.NodeK3s.Role == graph.K3sServer:
		// The node to remove is the one the approved plan named, and no other.
		target := approvedDiff(step.Approved, "remove-node")
		if !noderev.ValidNode(target) {
			return fmt.Errorf("the approved plan names no Kubernetes node to remove (plan again)")
		}
		node, what = r.NodeK3s.Node, "remove Kubernetes node "+target
		grant = func(rev *noderev.Revision) error {
			if rev.K3s == nil || rev.K3s.Role != noderev.K3sServer {
				return fmt.Errorf("%s has no k3s server in its revision (plan again)", node)
			}
			k := *rev.K3s
			k.Remove = &noderev.NodeRemoval{Node: target, Once: noderev.Once{Operation: step.OperationID, Attempt: attempt, Action: noderev.ActionRemove}}
			rev.K3s = &k
			return nil
		}
	case step.Op == policy.OpWriteK3sManifest && r.NodeK3sManifest != nil:
		m := *r.NodeK3sManifest
		node, what = m.Node, "write k3s manifest "+m.Name
		grant = func(rev *noderev.Revision) error {
			// Exactly the content the approved plan showed, or nothing happens.
			if want := approvedDiff(step.Approved, "sha256"); want != noderev.ManifestSHA256(m.Content) {
				return fmt.Errorf("the manifest %s is not the one the approved plan showed (plan again)", m.Name)
			}
			w := &noderev.Once{Operation: step.OperationID, Attempt: attempt, Action: noderev.ActionWrite}
			if cur := findManifest(rev, m.Name); cur != nil {
				cur.Content, cur.Write = m.Content, w
			} else {
				rev.Manifests = append(rev.Manifests, noderev.Manifest{Name: m.Name, Content: m.Content, Write: w})
			}
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
			sv.Once = append(sv.Once, noderev.Once{Operation: step.OperationID, Attempt: attempt, Action: action})
			return nil
		}
	default:
		return fmt.Errorf("mamelet does not carry out %s", step.Op)
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
		r.Manifests = slices.Clone(prev.Manifests)
		return r, grant(&r)
	}); err != nil {
		return fmt.Errorf("%s on %s: %w", what, node, err)
	}
	if configure {
		return nil
	}
	return p.await(ctx, node, what, step.OperationID, attempt)
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
