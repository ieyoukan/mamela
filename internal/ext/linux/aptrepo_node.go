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

// OpAddAptRepo adds (or changes) an APT source and its key (NodeAptRepo), and runs apt-get update.
const OpAddAptRepo policy.Op = "linux.add-apt-repo"

// CapAddAptRepo is the capability of the operation: what mamelet does with the grant.
const CapAddAptRepo = "linux.add-apt-repo"

// KeyAptKeySHA256 is the condition key of the sha256 of the key mamelet downloads, which it checks.
const KeyAptKeySHA256 = "mamela:linux.apt-repo.key-sha256"

var addAptRepoOp = operation{
	op: OpAddAptRepo,
	decision: policy.Decision{Verdict: policy.ApprovalRequired, Code: "MAM-POL-0223",
		Message: "adds an APT source and the key that signs it on the node as root, and runs apt-get update: apt then installs packages of this source too, " +
			"a package of it with a higher version replaces the system's own (apt installs the highest version), and its maintainer scripts run as root",
		Hint: "check the URIs and the key's sha256 against the vendor's instructions; a package of this source is installed only from an origin " +
			"the policy allows (allowedAptOrigins) and the node's mamelet allows (allowedOrigins in /etc/mamelet/config.json)"},
	grant: grantAddAptRepo,
}

// aptRepoManifest is NodeAptRepo in the node provider's manifest.
var aptRepoManifest = spec.Kind{Kind: KindAptRepo, Operations: []spec.Operation{{
	Op: OpAddAptRepo, Support: spec.Supported, Needs: []spec.Need{{
		Capability: CapAddAptRepo, Target: spec.Target{Type: node.TargetNode, Ref: "{node}"}, Object: "{path}", Always: true,
		Conditions: append(node.GrantConditions(), spec.Condition{Key: KeyAptKeySHA256, Test: spec.Equals, Values: []string{"{keySha256}"}}),
		Note: "mamelet downloads the key over https (at most 64 KiB), keeps it only if its sha256 is the approved one and it is an OpenPGP public key, " +
			"writes it and the approved source's file (0644, root's) atomically and never through a symbolic link, then runs apt-get update",
		Reasons: []string{"mamelet adds the APT source once"},
	}},
}}}

// publishAptRepos adds to the part the sources g declares on the node that it does not have yet
// (with no permission), and names in each the URIs and suites the declaration has now (what mamelet
// reports of apt's lists and of other source files: they allow nothing). An approved addition stays
// as it is.
func publishAptRepos(g *graph.Graph, name string, p *Part) {
	for _, r := range g.Sorted() {
		a := AptRepoOf(r)
		if a == nil || a.Node != name {
			continue
		}
		if cur := p.aptRepo(a.Name); cur != nil {
			cur.URIs, cur.Suites = slices.Clone(a.URIs), slices.Clone(a.Suites)
		} else {
			p.AptRepos = append(p.AptRepos, AptRepo{Name: a.Name, URIs: slices.Clone(a.URIs), Suites: slices.Clone(a.Suites)})
		}
	}
}

// pruneAptRepos takes out of the part the sources g no longer declares on the node, with their
// additions. The sources stay on the node.
func pruneAptRepos(g *graph.Graph, name string, p *Part) {
	declared := map[string]bool{}
	for _, r := range g.Resources {
		if a := AptRepoOf(r); a != nil && a.Node == name {
			declared[a.Name] = true
		}
	}
	p.AptRepos = slices.DeleteFunc(p.AptRepos, func(r AptRepo) bool { return !declared[r.Name] })
}

// protectAptRepos takes every addition out of the part of a protected node.
func protectAptRepos(p *Part) {
	for i := range p.AptRepos {
		p.AptRepos[i].Add = nil
	}
}

// grantAddAptRepo grants the addition of exactly the source the approved plan showed.
func grantAddAptRepo(_ context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := AptRepoOf(q.Step.Resource)
	if want == nil {
		return node.Grant{}, false, nil
	}
	s := *want
	armored, binary := aptSourcesSHA256(s)
	// Exactly what the approved plan showed, or nothing happens.
	sum := q.Approved("sha256")
	switch {
	case sum != "" && sum != armored && sum != binary:
		return node.Grant{}, true, fmt.Errorf("the file of APT source %s is not the one the approved plan showed (plan again)", s.Name)
	case sum == "":
		sum = armored // the plan only ran apt-get update again: the file is in place
	}
	if k := q.Approved("key sha256"); k != "" && k != s.KeySHA256 {
		return node.Grant{}, true, fmt.Errorf("the key of APT source %s is not the one the approved plan showed (plan again)", s.Name)
	}
	g := node.Grant{Node: s.Node, What: "add APT source " + s.Name}
	g.Put = func(rev *noderev.Revision, once func(string) noderev.Once) error {
		p := partOf(rev)
		add := &AptRepoAdd{URIs: slices.Clone(s.URIs), Suites: slices.Clone(s.Suites), Components: nonNil(slices.Clone(s.Components)),
			Architectures: slices.Clone(s.Architectures), KeyURL: s.KeyURL, KeySHA256: s.KeySHA256, SourcesSHA256: sum, Grant: once(ActionAddAptRepo)}
		if cur := p.aptRepo(s.Name); cur != nil {
			cur.URIs, cur.Suites, cur.Add = slices.Clone(s.URIs), slices.Clone(s.Suites), add
		} else {
			p.AptRepos = append(p.AptRepos, AptRepo{Name: s.Name, URIs: slices.Clone(s.URIs), Suites: slices.Clone(s.Suites), Add: add})
		}
		setPart(rev, p)
		return nil
	}
	return g, true, nil
}
