package linux

import (
	"context"
	"fmt"
	"slices"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
)

// OpUpgradePackages upgrades the installed packages of a node to exactly the versions the plan showed
// (NodeAptUpgrades). Like an installation, it is held to the core's rules of an apt change set too
// (policy.Decide): a change set that removes a package is denied (MAM-POL-0304), and so is a package
// from an origin the policy does not allow (MAM-POL-0305).
const OpUpgradePackages policy.Op = "linux.upgrade-packages"

// CapUpgradePackages is the capability of the operation: what mamelet does with the grant.
const CapUpgradePackages = "linux.upgrade-packages"

var upgradePackagesOp = operation{
	op: OpUpgradePackages,
	decision: policy.Decision{Verdict: policy.ApprovalRequired, Code: "MAM-POL-0227",
		Message: "upgrades the installed packages of the node to exactly the versions shown, as root: their maintainer scripts run, " +
			"and a package restarts its own services (a library's users keep the old one until they restart)",
		Hint: "read the packages and versions above; mamelet simulates again just before and upgrades only this change set. " +
			"A kernel or libc takes effect at the next reboot, which Mamela does not do"},
	grant: grantUpgradePackages,
}

// aptUpgradesManifest is NodeAptUpgrades in the node provider's manifest: as an installation, the
// fresh simulation's change set must be the approved one, remove nothing, and come from the policy's
// origins.
var aptUpgradesManifest = spec.Kind{Kind: KindAptUpgrades, Operations: []spec.Operation{{
	Op: OpUpgradePackages, Support: spec.Supported, Needs: []spec.Need{{
		Capability: CapUpgradePackages, Target: spec.Target{Type: node.TargetNode, Ref: "{node}"}, Object: "apt-upgrades", Always: true,
		Conditions: append(node.GrantConditions(),
			spec.Condition{Key: node.KeyChangeSet, Test: spec.Equals, Values: []string{"{" + node.FactChangeSet + "}"}},
			spec.Condition{Key: node.KeyAptRemove, Test: spec.NotPresent},
			spec.Condition{Key: node.KeyAptOrigin, Test: spec.Like, Values: []string{"{" + node.FactOrigins + "}"}}),
		Note: "mamelet simulates apt-get install --only-upgrade of the approved package versions again just before, and upgrades them only if it is " +
			"exactly the approved change set, removes nothing and comes from the origins its configuration allows",
		Reasons: []string{"mamelet runs apt-get install --only-upgrade once"},
	}},
}}}

// addOriginSet adds origins to the sets of an entry (sorted, each once). Past maxAptUpgradeSets, it
// keeps held (the declaration in force's), then origins (the planned declaration's), then the others,
// as many as fit (union).
func addOriginSet(held []string, cur [][]string, origins []string) [][]string {
	same := func(s []string) bool { return slices.Equal(s, origins) }
	if slices.ContainsFunc(cur, same) {
		return cur
	}
	out := append(slices.Clone(cur), slices.Clone(origins))
	if len(out) > maxAptUpgradeSets {
		others := slices.Clone(cur)
		slices.SortFunc(others, slices.Compare)
		var first [][]string
		if held != nil {
			first = append(first, held)
		}
		out = nil
		for _, x := range slices.Concat(first, [][]string{origins}, others) {
			if len(out) < maxAptUpgradeSets && !slices.ContainsFunc(out, func(s []string) bool { return slices.Equal(s, x) }) {
				out = append(out, x)
			}
		}
	}
	slices.SortFunc(out, slices.Compare)
	return out
}

// aptUpgradesOn is the upgrades g declares on node (nil: none).
func aptUpgradesOn(g *graph.Graph, node string) *AptUpgradesSpec {
	for _, r := range g.Resources {
		if u := AptUpgradesOf(r); u != nil && u.Node == node {
			return u
		}
	}
	return nil
}

// publishAptUpgrades adds the origins g declares on the node to what mamelet works out (observation
// targets), and lets mamelet read the package lists again when they are old (Refresh: Protect takes
// it away on a node the Control Plane must not change). It takes nothing away that the declaration in
// force has.
func publishAptUpgrades(g *graph.Graph, name string, p *Part) {
	u := aptUpgradesOn(g, name)
	if u == nil {
		return
	}
	if p.AptUpgrades == nil {
		p.AptUpgrades = &AptUpgrades{}
	}
	e := p.AptUpgrades
	e.Sets = addOriginSet(e.Held, e.Sets, u.Origins)
	e.Refresh = true
}

// pruneAptUpgrades makes the entry the one of g, an approved declaration being applied: its origins
// only, in force, and an upgrade granted for other origins withdrawn. Without upgrades declared on the
// node, mamelet stops working them out (and reading the lists again). What was upgraded stays.
func pruneAptUpgrades(g *graph.Graph, name string, p *Part) {
	u := aptUpgradesOn(g, name)
	if u == nil {
		p.AptUpgrades = nil
		return
	}
	if p.AptUpgrades == nil {
		p.AptUpgrades = &AptUpgrades{}
	}
	e := p.AptUpgrades
	e.Sets, e.Held, e.InForce = [][]string{slices.Clone(u.Origins)}, slices.Clone(u.Origins), true
	if e.Upgrade != nil && !slices.Equal(e.Upgrade.Origins, u.Origins) {
		e.Upgrade = nil
	}
}

// protectAptUpgrades takes the upgrade, and the reading of the lists, out of the part of a protected
// (or observe-only) node: mamelet still works out what is pending, and changes nothing.
func protectAptUpgrades(p *Part) {
	if p.AptUpgrades != nil {
		p.AptUpgrades.Upgrade, p.AptUpgrades.Refresh = nil, false
	}
}

// grantUpgradePackages grants the upgrade of exactly the change set the approved plan showed: its
// packages and versions are those the plan's observation found for the declaration's origins.
func grantUpgradePackages(_ context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := AptUpgradesOf(q.Step.Resource)
	if want == nil {
		return node.Grant{}, false, nil
	}
	s := *want
	digest := q.Approved("changeSet")
	if digest == "" {
		return node.Grant{}, true, fmt.Errorf("the upgrades of %s: the approved plan has no change set (plan again)", s.Node)
	}
	o, err := observedAt(q.Observed, s.Node)
	if err != nil {
		return node.Grant{}, true, fmt.Errorf("the upgrades of %s: %w (plan again)", s.Node, err)
	}
	var set *AptUpgradeSet
	if o.AptUpgrades != nil {
		set = o.AptUpgrades.set(s.Origins)
	}
	switch {
	case set == nil:
		return node.Grant{}, true, fmt.Errorf("the upgrades of %s: the approved plan was made without a report of them (plan again)", s.Node)
	case set.Digest != digest:
		return node.Grant{}, true, fmt.Errorf("the upgrades of %s: the change set the approved plan showed (%s) is not the one its observation has (%s) (plan again)",
			s.Node, digest, set.Digest)
	}
	up := &AptUpgrade{Origins: slices.Clone(s.Origins), Packages: slices.Clone(set.Packages), ChangeSet: digest}
	if err := checkUpgradeOf(*up); err != nil {
		return node.Grant{}, true, fmt.Errorf("the upgrades of %s: %w (plan again)", s.Node, err)
	}
	g := node.Grant{Node: s.Node, What: fmt.Sprintf("upgrade %d packages", len(up.Packages))}
	g.Put = func(rev *noderev.Revision, once func(string) noderev.Once) error {
		p := partOf(rev)
		if p.AptUpgrades == nil {
			p.AptUpgrades = &AptUpgrades{}
		}
		e := p.AptUpgrades
		e.Sets = addOriginSet(e.Held, e.Sets, s.Origins)
		u := *up
		u.Grant = once(ActionUpgradePackages)
		e.InForce, e.Upgrade = true, &u
		setPart(rev, p)
		return nil
	}
	return g, true, nil
}
