package lab

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/pve"
	"github.com/ieyoukan/mamela/internal/proxmox"
)

// Use is a privilege on an ACL path, and why it is used.
type Use struct {
	Path    string
	Priv    string
	Reasons []string
	Maybe   bool // used only in some cases (an upload that may already be done, a start of a stopped VM)
}

// OpUses is what one operation uses: Proxmox privileges of the operating token, or what mamelet
// is allowed to do in the VM. Unsupported operations are carried out by nobody.
type OpUses struct {
	Op          policy.Op
	Proxmox     []Use
	Mamelet     string
	Unsupported string
}

// PathOf renders a scope as the site's ACL path; without a site, as a placeholder.
func (s *Site) pathOf(scope proxmox.Scope) string {
	if s == nil {
		return map[proxmox.Scope]string{
			proxmox.ScopeVM: "/pool/<lab pool>", proxmox.ScopePool: "/pool/<lab pool>",
			proxmox.ScopeDiskStorage: "/storage/<disk storage>", proxmox.ScopeImageStorage: "/storage/<image storage>",
			proxmox.ScopeBridge: "/sdn/zones/<zone>/<bridge>", proxmox.ScopeNode: "/nodes/<node>", proxmox.ScopeRoot: "/",
		}[scope]
	}
	return s.scopePaths()[scope][0]
}

func (s *Site) uses(reqs []proxmox.Requirement) []Use {
	var out []Use
	for _, r := range reqs {
		out = append(out, Use{Path: s.pathOf(r.Scope), Priv: r.Priv, Reasons: r.Reasons, Maybe: !r.Always})
	}
	slices.SortStableFunc(out, func(a, b Use) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Priv, b.Priv)
	})
	return out
}

func (s *Site) withSeed() bool { return s != nil && s.ControlPlane.Enabled() }

// ObserveUses is what observing the lab uses (every plan and every apply observes).
func (s *Site) ObserveUses() []Use { return s.uses(proxmox.NeedsFor(pve.ObserveCalls)) }

// UsesOf is what one operation on a resource uses. c, when given, is the planned change (for the
// approved apt change set and whether a start is a standing repair).
func (s *Site) UsesOf(op policy.Op, r graph.Resource, c *plan.Change) OpUses {
	u := OpUses{Op: op}
	if calls, ok := pve.OperationCalls(op, s.withSeed()); ok {
		u.Proxmox = s.uses(proxmox.NeedsFor(calls))
		return u
	}
	switch {
	case op == policy.OpInstallPackage && r.NodePackage != nil:
		set := "the approved apt change set"
		if c != nil {
			for _, d := range c.Diffs {
				if d.Field == "changeSet" {
					set += " " + d.To
				}
			}
		}
		u.Mamelet = fmt.Sprintf("no Proxmox permission; mamelet on %s runs apt-get install %s once, only if a fresh simulation is exactly %s",
			r.NodePackage.Node, r.NodePackage.Package, set)
	case op == policy.OpEnableService && r.NodeService != nil:
		u.Mamelet = fmt.Sprintf("no Proxmox permission; mamelet on %s runs systemctl enable %s once", r.NodeService.Node, r.NodeService.Service)
	case op == policy.OpStartService && r.NodeService != nil:
		auto := c != nil && slices.ContainsFunc(c.Operations, func(o plan.Operation) bool {
			return o.Op == policy.OpStartService && o.Decision.Verdict == policy.Auto
		})
		if auto {
			u.Mamelet = fmt.Sprintf("no Proxmox permission; mamelet on %s may run systemctl start %s whenever it finds it stopped (autoStartServices)",
				r.NodeService.Node, r.NodeService.Service)
		} else {
			u.Mamelet = fmt.Sprintf("no Proxmox permission; mamelet on %s runs systemctl start %s once", r.NodeService.Node, r.NodeService.Service)
		}
	default:
		u.Unsupported = "neither the Proxmox provider nor mamelet carries out " + string(op)
	}
	return u
}

// ChangeUses is a planned change and what each of its operations uses.
type ChangeUses struct {
	Change plan.Change
	Ops    []OpUses
}

// PlanUses is what applying the plan would use, per change with operations.
func (s *Site) PlanUses(p *plan.Plan, g *graph.Graph) []ChangeUses {
	desired := map[string]graph.Resource{}
	for _, r := range g.Resources {
		desired[r.ID()] = r
	}
	var out []ChangeUses
	for _, c := range p.Changes {
		if c.Status != plan.StatusChange {
			continue
		}
		cu := ChangeUses{Change: c}
		for _, op := range c.Operations {
			if op.Decision.Verdict == policy.Denied {
				continue // never runs
			}
			cu.Ops = append(cu.Ops, s.UsesOf(op.Op, desired[c.ID()], &c))
		}
		out = append(out, cu)
	}
	return out
}

// possibleOps are the operations a resource kind can lead to (including ones that are denied or
// not carried out, so the reader sees them).
var possibleOps = map[graph.Kind][]policy.Op{
	graph.KindVirtualMachine: {policy.OpCreateVM, policy.OpGrowVMDisk, policy.OpResizeVMCPU, policy.OpResizeVMMemory},
	graph.KindNodePackage:    {policy.OpInstallPackage},
	graph.KindNodeService:    {policy.OpEnableService, policy.OpStartService},
}

// KindUses is what declaring a kind of resource may lead to.
type KindUses struct {
	Kind graph.Kind
	Ops  []OpUses
}

// MayNeed is what the resources in a declaration may need, by kind, whatever the world is like.
// Without a site (s == nil), paths are placeholders.
func (s *Site) MayNeed(g *graph.Graph) []KindUses {
	var out []KindUses
	for _, k := range []graph.Kind{graph.KindVirtualMachine, graph.KindNodePackage, graph.KindNodeService} {
		var r *graph.Resource
		for i := range g.Resources {
			if g.Resources[i].Kind == k {
				r = &g.Resources[i]
				break
			}
		}
		if r == nil {
			continue
		}
		ku := KindUses{Kind: k}
		for _, op := range possibleOps[k] {
			ku.Ops = append(ku.Ops, s.UsesOf(op, *r, nil))
		}
		out = append(out, ku)
	}
	return out
}
