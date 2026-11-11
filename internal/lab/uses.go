package lab

import (
	"slices"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/pve"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/proxmox"
)

// Providers are the providers of the lab: Proxmox for VMs, mamelet for what runs in them. A nil
// site (no site given) is a lab without a Control Plane.
func (s *Site) Providers() spec.Providers {
	return spec.Providers{pve.Manifest(s.withSeed()), node.Manifest()}
}

func (s *Site) withSeed() bool { return s != nil && s.ControlPlane.Enabled() }

// placeholders name the Proxmox ACL scopes when there is no site.
var placeholders = map[proxmox.Scope]string{
	proxmox.ScopeVM: "/pool/<lab pool>", proxmox.ScopePool: "/pool/<lab pool>",
	proxmox.ScopeDiskStorage: "/storage/<disk storage>", proxmox.ScopeImageStorage: "/storage/<image storage>",
	proxmox.ScopeBridge: "/sdn/zones/<zone>/<bridge>", proxmox.ScopeNode: "/nodes/<node>", proxmox.ScopeRoot: "/",
}

// ResolveTarget names a Proxmox ACL scope as the site's ACL path (where the grant is placed). A
// VM's privileges are held through the lab pool. Without a site, the path is a placeholder.
func (s *Site) ResolveTarget(provider string, t spec.Target) (string, bool) {
	if provider != "proxmox" || t.Type != pve.TargetACL {
		return "", false
	}
	if s == nil {
		return placeholders[proxmox.Scope(t.Ref)], false
	}
	p, ok := s.scopePaths()[proxmox.Scope(t.Ref)]
	return p[0], ok
}

// Requirements resolves Proxmox requirements (from the permission catalog) on the site's paths.
func (s *Site) Requirements(reqs []proxmox.Requirement) []spec.Requirement {
	var out []spec.Requirement
	for _, r := range reqs {
		path, ok := s.ResolveTarget("proxmox", spec.Target{Type: pve.TargetACL, Ref: string(r.Scope)})
		out = append(out, spec.Requirement{Provider: "proxmox", Support: spec.Supported, Capability: r.Priv,
			TargetType: pve.TargetACL, Target: path, Resolved: ok, Always: r.Always, Reasons: r.Reasons})
	}
	return out
}

// PlannedRequirements is what applying the plan would use: for each change, each operation that
// can run (denied ones never do), with the facts the plan and the policy fix filled into the
// conditions (the approved apt change set, the allowed origins). A grant is standing only where
// the policy decided the operation automatically and the need's StandingIf conditions hold; a
// standing grant is tied to the declaration, not to one operation.
func (s *Site) PlannedRequirements(p *plan.Plan, g *graph.Graph, pol policy.Policy, external ...spec.Manifest) []spec.Requirement {
	ps := append(s.Providers(), external...)
	desired := map[string]graph.Resource{}
	for _, r := range g.Resources {
		desired[r.ID()] = r
	}
	var origins []string
	for _, o := range pol.AllowedAptOrigins {
		origins = append(origins, o+"*") // the policy's origins are prefixes
	}
	policyFacts := map[string][]string{node.KeyAutoStart: pol.AutoStartServices}
	var out []spec.Requirement
	for _, c := range p.Changes {
		if c.Status != plan.StatusChange {
			continue
		}
		r, ok := desired[c.ID()]
		if !ok {
			continue
		}
		facts := map[string][]string{node.FactOrigins: origins}
		for _, d := range c.Diffs {
			if d.Field == "changeSet" {
				facts[node.FactChangeSet] = []string{d.To}
			}
		}
		for _, op := range c.Operations {
			if op.Decision.Verdict == policy.Denied {
				continue
			}
			for _, q := range ps.OperationRequirements(r, op.Op, s) {
				q.Fill(facts)
				if op.Decision.Verdict == policy.Auto && len(q.StandingIf) > 0 && holdAll(q.StandingIf, policyFacts) {
					q.Grant = spec.Standing
					q.Conditions = slices.DeleteFunc(slices.Clone(q.Conditions), func(c spec.Condition) bool { return c.Key == node.KeyOperation })
				} else {
					q.StandingIf = nil
				}
				out = append(out, q)
			}
		}
	}
	return out
}

func holdAll(cs []spec.Condition, facts map[string][]string) bool {
	for _, c := range cs {
		if !c.Holds(facts) {
			return false
		}
	}
	return true
}
