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
// (with no permission), and names in each the owner and group the declaration has now (what mamelet
// reports the existence of: they allow nothing). An approved installation stays as it is.
func publishArtifacts(g *graph.Graph, name string, p *Part) {
	for _, r := range g.Sorted() {
		a := ArtifactOf(r)
		if a == nil || a.Node != name {
			continue
		}
		if cur := p.artifact(a.Path); cur != nil {
			cur.Owner, cur.Group = a.Owner, a.Group
		} else {
			p.Artifacts = append(p.Artifacts, Artifact{Path: a.Path, Owner: a.Owner, Group: a.Group})
		}
	}
}

// pruneArtifacts takes out of the part the artifacts g no longer declares on the node, with their
// installations. The files stay on the node.
func pruneArtifacts(g *graph.Graph, name string, p *Part) {
	declared := map[string]bool{}
	for _, r := range g.Resources {
		if a := ArtifactOf(r); a != nil && a.Node == name {
			declared[a.Path] = true
		}
	}
	p.Artifacts = slices.DeleteFunc(p.Artifacts, func(a Artifact) bool { return !declared[a.Path] })
}

// protectArtifacts takes every installation out of the part of a protected node.
func protectArtifacts(p *Part) {
	for i := range p.Artifacts {
		p.Artifacts[i].Install = nil
	}
}

// grantInstall grants the installation of exactly the artifact the approved plan showed.
func grantInstall(_ context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := ArtifactOf(q.Step.Resource)
	if want == nil {
		return node.Grant{}, false, nil
	}
	a := *want
	g := node.Grant{Node: a.Node, What: "install " + a.Path}
	g.Put = func(rev *noderev.Revision, once func(string) noderev.Once) error {
		// Exactly what the approved plan showed, or nothing happens.
		for field, want := range map[string]string{"url": a.URL, "sha256": a.SHA256, "extract": shownExtract(a.Extract), "mode": a.Mode, "owner": a.Owner, "group": a.Group} {
			if approved := q.Approved(field); approved != "" && approved != want {
				return fmt.Errorf("the %s of %s is not the one the approved plan showed (plan again)", field, a.Path)
			}
		}
		p := partOf(rev)
		in := &Install{URL: a.URL, SHA256: a.SHA256, Extract: a.Extract, Mode: a.Mode, Owner: a.Owner, Group: a.Group, Restart: slices.Clone(a.Restart), Grant: once(ActionInstallArtifact)}
		if cur := p.artifact(a.Path); cur != nil {
			cur.Owner, cur.Group, cur.Install = a.Owner, a.Group, in
		} else {
			p.Artifacts = append(p.Artifacts, Artifact{Path: a.Path, Owner: a.Owner, Group: a.Group, Install: in})
		}
		setPart(rev, p)
		return nil
	}
	return g, true, nil
}
