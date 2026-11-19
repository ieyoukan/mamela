package linux

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
)

// OpInstallArtifact installs an artifact (NodeArtifact).
const OpInstallArtifact policy.Op = "linux.install-artifact"

// CapInstallArtifact is the capability of the operation: what mamelet does with the grant.
const CapInstallArtifact = "linux.install-artifact"

// KeyArtifactSHA256 is the condition key of the sha256 of the download mamelet installs from, which
// it checks.
const KeyArtifactSHA256 = "mamela:linux.artifact.sha256"

var installArtifactOp = operation{
	op: OpInstallArtifact,
	decision: policy.Decision{Verdict: policy.ApprovalRequired, Code: "MAM-POL-0225",
		Message: "downloads a file over https and installs it on the node as root (or one member of it, a .tar.gz), only if its sha256 is the one of this plan, with the mode and owner of this plan, and restarts the services it names",
		Hint:    "check the URL and the sha256 against the publisher's release (its checksum file) before approving: what is installed runs, or is read, as this plan says"},
	grant: grantInstall,
}

// artifactManifest is NodeArtifact in the node provider's manifest.
var artifactManifest = spec.Kind{Kind: KindArtifact, Operations: []spec.Operation{{
	Op: OpInstallArtifact, Support: spec.Supported, Needs: []spec.Need{{
		Capability: CapInstallArtifact, Target: spec.Target{Type: node.TargetNode, Ref: "{node}"}, Object: "{path}", Always: true,
		Conditions: append(node.GrantConditions(), spec.Condition{Key: KeyArtifactSHA256, Test: spec.Equals, Values: []string{"{sha256}"}}),
		Note: "mamelet downloads the URL over https only (a redirect too), bounded in size and time, and installs nothing unless the download's sha256 is the approved one; " +
			"it takes only the named member (a regular file) out of a .tar.gz, installs it atomically and never through a symbolic link, creates missing directories (0755, root's), " +
			"reloads systemd for a unit, then restarts the running services the artifact names",
		Reasons: []string{"mamelet installs the artifact once"},
	}},
}}}

// publishArtifacts adds to the part the artifacts g declares on the node that it does not have yet
// (with no permission, not InForce), and adds to each the owner and group the declaration names (what
// mamelet reports the existence of: they allow nothing). It takes nothing away that the declaration
// in force has (publishFiles).
func publishArtifacts(g *graph.Graph, name string, p *Part) {
	declared := map[string]bool{}
	for _, r := range g.Sorted() {
		a := ArtifactOf(r)
		if a == nil || a.Node != name {
			continue
		}
		declared[a.Path] = true
		cur := p.artifact(a.Path)
		if cur == nil {
			p.Artifacts = append(p.Artifacts, Artifact{Path: a.Path})
			cur = &p.Artifacts[len(p.Artifacts)-1]
		}
		cur.Owners, cur.Groups = union(cur.Held.heldOwners(), cur.Owners, []string{a.Owner}), union(cur.Held.heldGroups(), cur.Groups, []string{a.Group})
	}
	p.Artifacts = bound(p.Artifacts, func(a Artifact) string { return a.Path }, func(a Artifact) bool { return a.InForce || a.Install != nil }, declared, MaxNodeArtifacts)
}

// pruneArtifacts makes the part the artifacts g, an approved declaration being applied, declares on
// the node (pruneFiles, all of them): an installation of another download, mode, owner or services is
// withdrawn. The files stay on the node.
func pruneArtifacts(g *graph.Graph, name string, p *Part) {
	declared := map[string]*ArtifactSpec{}
	for _, r := range g.Resources {
		if a := ArtifactOf(r); a != nil && a.Node == name {
			declared[a.Path] = a
		}
	}
	p.Artifacts = slices.DeleteFunc(p.Artifacts, func(a Artifact) bool { return declared[a.Path] == nil })
	for _, path := range slices.Sorted(maps.Keys(declared)) {
		if p.artifact(path) == nil {
			p.Artifacts = append(p.Artifacts, Artifact{Path: path})
		}
	}
	for i := range p.Artifacts {
		a, want := &p.Artifacts[i], declared[p.Artifacts[i].Path]
		a.InForce, a.Owners, a.Groups = true, []string{want.Owner}, []string{want.Group}
		a.Held = &OwnerTargets{Owners: []string{want.Owner}, Groups: []string{want.Group}}
		if in := a.Install; in != nil && !installs(*in, want) {
			a.Install = nil
		}
	}
}

// installs reports whether an approved installation installs exactly what the declaration a has.
func installs(in Install, a *ArtifactSpec) bool {
	return in.URL == a.URL && in.SHA256 == a.SHA256 && in.Extract == a.Extract && in.Mode == a.Mode && in.Owner == a.Owner && in.Group == a.Group &&
		slices.Equal(in.Restart, a.Restart)
}

// protectArtifacts takes every installation out of the part of a protected node.
func protectArtifacts(p *Part) {
	for i := range p.Artifacts {
		p.Artifacts[i].Install = nil
	}
}

// grantInstall grants the installation of exactly the artifact the approved plan showed, against what
// the plan's observation found at the path (Install.Before).
func grantInstall(_ context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := ArtifactOf(q.Step.Resource)
	if want == nil {
		return node.Grant{}, false, nil
	}
	a := *want
	o, err := observedAt(q.Observed, a.Node)
	if err != nil {
		return node.Grant{}, true, fmt.Errorf("install %s: %w (plan again)", a.Path, err)
	}
	got := o.artifact(a.Path)
	if got == nil {
		return node.Grant{}, true, fmt.Errorf("install %s: the approved plan was made without a report of it (plan again)", a.Path)
	}
	before := beforeOf(got.FileState)
	g := node.Grant{Node: a.Node, What: "install " + a.Path}
	g.Put = func(rev *noderev.Revision, once func(string) noderev.Once) error {
		// Exactly what the approved plan showed, or nothing happens.
		for field, want := range map[string]string{"url": a.URL, "sha256": a.SHA256, "extract": shownExtract(a.Extract), "mode": a.Mode, "owner": a.Owner, "group": a.Group} {
			if approved := q.Approved(field); approved != "" && approved != want {
				return fmt.Errorf("the %s of %s is not the one the approved plan showed (plan again)", field, a.Path)
			}
		}
		p := partOf(rev)
		in := &Install{URL: a.URL, SHA256: a.SHA256, Extract: a.Extract, Mode: a.Mode, Owner: a.Owner, Group: a.Group, Restart: slices.Clone(a.Restart),
			Before: before, Grant: once(ActionInstallArtifact)}
		cur := p.artifact(a.Path)
		if cur == nil {
			p.Artifacts = append(p.Artifacts, Artifact{Path: a.Path})
			cur = &p.Artifacts[len(p.Artifacts)-1]
		}
		cur.Owners, cur.Groups = union(cur.Held.heldOwners(), cur.Owners, []string{a.Owner}), union(cur.Held.heldGroups(), cur.Groups, []string{a.Group})
		cur.InForce, cur.Install = true, in
		setPart(rev, p)
		return nil
	}
	return g, true, nil
}
