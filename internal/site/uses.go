package site

import (
	"slices"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
)

// Providers are Mamela's built-in providers: mamelet, for what runs on the machines. Machines
// come from providers' node kinds (ADR 0004, 0006).
func (s *Site) Providers() spec.Providers {
	return spec.Providers{node.Manifest()}
}

// ResolveTarget names no target: a provider's targets are templates over its resources' fields
// (ADR 0006 moved every infrastructure's settings out of the site).
func (s *Site) ResolveTarget(provider string, t spec.Target) (string, bool) { return "", false }

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
