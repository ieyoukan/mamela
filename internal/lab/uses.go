package lab

import (
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
// can run (denied ones never do), with what the plan fixes. A standing grant appears only where
// the policy decided the operation automatically; the approved apt change set narrows the
// install.
func (s *Site) PlannedRequirements(p *plan.Plan, g *graph.Graph) []spec.Requirement {
	ps := s.Providers()
	desired := map[string]graph.Resource{}
	for _, r := range g.Resources {
		desired[r.ID()] = r
	}
	var out []spec.Requirement
	for _, c := range p.Changes {
		if c.Status != plan.StatusChange {
			continue
		}
		r, ok := desired[c.ID()]
		if !ok {
			continue
		}
		for _, op := range c.Operations {
			if op.Decision.Verdict == policy.Denied {
				continue
			}
			for _, q := range ps.OperationRequirements(r, op.Op, s) {
				if op.Decision.Verdict != policy.Auto {
					q.Standing = ""
				}
				if q.Capability == node.CapAptInstall {
					for _, d := range c.Diffs {
						if d.Field == "changeSet" {
							q.Limit = "only the approved apt change set " + d.To + ", simulated again just before; never a removal, only from allowed APT origins"
						}
					}
				}
				out = append(out, q)
			}
		}
	}
	return out
}
