package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/lab"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/proxmox"
)

// The requirement listings are rendered here, from spec.Requirement values; nothing below the CLI
// builds sentences about permissions.

// writeRequirements prints requirements grouped by resource and operation, and within one
// operation by provider and target. It returns whether any is used only in some cases ("?").
func writeRequirements(w io.Writer, indent string, reqs []spec.Requirement) bool {
	maybe := false
	type group struct {
		resource, op string
		reqs         []spec.Requirement
	}
	var groups []group
	for _, q := range reqs {
		if n := len(groups); n > 0 && groups[n-1].resource == q.Resource && groups[n-1].op == string(q.Op) {
			groups[n-1].reqs = append(groups[n-1].reqs, q)
			continue
		}
		groups = append(groups, group{resource: q.Resource, op: string(q.Op), reqs: []spec.Requirement{q}})
	}
	lastResource := "\x00"
	for _, g := range groups {
		in := indent
		if g.resource != "" {
			if g.resource != lastResource {
				fmt.Fprintf(w, "%s%s\n", indent, g.resource)
			}
			lastResource = g.resource
			in += "  "
		}
		label := g.op
		if label == "" {
			label = "observing"
		}
		if writeOp(w, in, label, g.reqs) {
			maybe = true
		}
	}
	return maybe
}

// writeOp prints one operation's requirements: "op  provider  caps on target".
func writeOp(w io.Writer, indent, label string, reqs []spec.Requirement) bool {
	const width = 17
	pad := func(first bool) string {
		if first {
			return fmt.Sprintf("%s%-*s", indent, width, label)
		}
		return indent + strings.Repeat(" ", width)
	}
	if q := reqs[0]; q.Support != spec.Supported {
		fmt.Fprintf(w, "%snot carried out: %s\n", pad(true), q.Why)
		return false
	}
	maybe := false
	type key struct{ provider, targetType, target, object string }
	var keys []key
	caps := map[key][]string{}
	notes := map[key][]string{}
	for _, q := range reqs {
		k := key{q.Provider, q.TargetType, q.Target, q.Object}
		if _, ok := caps[k]; !ok {
			keys = append(keys, k)
		}
		c := q.Capability
		if !q.Always {
			c += "?"
			maybe = true
		}
		if !slices.Contains(caps[k], c) {
			caps[k] = append(caps[k], c)
		}
		for _, n := range grantNotes(q) {
			if !slices.Contains(notes[k], n) {
				notes[k] = append(notes[k], n)
			}
		}
	}
	for i, k := range keys {
		what := strings.Join(caps[k], ", ")
		if k.object != "" {
			what += " " + k.object
		}
		on := k.target
		if t := k.targetType; t != "" && !strings.HasPrefix(on, "/") {
			on = t + " " + on // "node web"; Proxmox ACL paths speak for themselves
		}
		provider := k.provider
		if i > 0 && keys[i-1].provider == k.provider {
			provider = ""
		}
		fmt.Fprintf(w, "%s%-8s %s on %s\n", pad(i == 0), provider, what, on)
		for _, n := range notes[k] {
			fmt.Fprintf(w, "%s         %s\n", pad(false), n)
		}
	}
	return maybe
}

// grantNotes renders a requirement's grant and conditions for people, from their fields.
func grantNotes(q spec.Requirement) []string {
	var out []string
	if q.Grant == spec.Standing {
		out = append(out, "standing grant (not tied to one operation)")
	}
	if len(q.StandingIf) > 0 {
		var cs []string
		for _, c := range q.StandingIf {
			cs = append(cs, condition(c))
		}
		out = append(out, "may be standing if "+strings.Join(cs, " and "))
	}
	var cs []string
	for _, c := range q.Conditions {
		cs = append(cs, condition(c))
	}
	if len(cs) > 0 {
		out = append(out, "only if "+strings.Join(cs, ", "))
	}
	if q.Note != "" {
		out = append(out, q.Note)
	}
	return out
}

func condition(c spec.Condition) string {
	vals := strings.Join(c.Values, " | ")
	switch c.Test {
	case spec.Equals:
		return c.Key + " = " + vals
	case spec.In, spec.Like:
		return c.Key + " " + string(c.Test) + " [" + vals + "]"
	case spec.Before:
		return c.Key + " < " + vals
	case spec.Contains:
		return c.Key + " ∋ " + vals
	case spec.NotPresent:
		return "no " + c.Key
	}
	return c.Key + " " + string(c.Test) + " " + vals
}

// renderPlanUses says, for each change, what applying the plan would use, concretely for the site.
func renderPlanUses(w io.Writer, site *lab.Site, p *plan.Plan, g *graph.Graph, pol policy.Policy) {
	fmt.Fprintln(w, "\nWhat applying this plan uses (Proxmox privileges of the operating token, `mamela lab permissions` explains them;")
	fmt.Fprintln(w, "mamelet actions granted in the node's signed revision):")
	reqs := append(site.Providers().Observation(site), site.PlannedRequirements(p, g, pol)...)
	if writeRequirements(w, "  ", reqs) {
		fmt.Fprintln(w, "  ? used only in some cases (for example an upload that is already done)")
	}
}

// renderMayNeed says what each resource of the declaration may need, whatever the world is.
func renderMayNeed(w io.Writer, site *lab.Site, ps spec.Providers, g *graph.Graph) {
	fmt.Fprintln(w, "\nMay need when applied (every operation each resource can lead to; `mamela plan --site` says what a plan uses):")
	reqs := append(ps.Observation(site), ps.Analyze(g, site)...)
	if writeRequirements(w, "  ", reqs) {
		fmt.Fprintln(w, "  ? used only in some cases")
	}
}

// renderProxmoxUses prints what a fixed list of Proxmox calls needs on the site.
func renderProxmoxUses(w io.Writer, site *lab.Site, label string, calls []proxmox.Call) {
	writeOp(w, "", label, site.Requirements(proxmox.NeedsFor(calls)))
}
