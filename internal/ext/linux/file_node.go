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

// OpWriteFile writes a file (NodeFile). The linux operations are named linux.<verb>-<object>.
const OpWriteFile policy.Op = "linux.write-file"

// CapWriteFile is the capability of the operation: what mamelet does with the grant.
const CapWriteFile = "linux.write-file"

// KeyFileSHA256 is the condition key of the sha256 of the content mamelet writes, which it checks.
const KeyFileSHA256 = "mamela:linux.file.sha256"

var writeFileOp = operation{
	op: OpWriteFile,
	decision: policy.Decision{Verdict: policy.ApprovalRequired, Code: "MAM-POL-0220",
		Message: "writes a file on the node as root, with exactly the content, mode and owner of this plan, and restarts the services it names",
		Hint:    "read the whole content in this plan: a file can change how the node runs (a unit, a cron job, a service's configuration)"},
	grant: grantWrite,
}

// fileManifest is NodeFile in the node provider's manifest.
var fileManifest = spec.Kind{Kind: KindFile, Operations: []spec.Operation{{
	Op: OpWriteFile, Support: spec.Supported, Needs: []spec.Need{{
		Capability: CapWriteFile, Target: spec.Target{Type: node.TargetNode, Ref: "{node}"}, Object: "{path}", Always: true,
		Conditions: append(node.GrantConditions(), spec.Condition{Key: KeyFileSHA256, Test: spec.Equals, Values: []string{"{sha256}"}}),
		Note: "mamelet writes exactly the approved content (it checks the sha256), mode and owner, atomically and never through a symbolic link, " +
			"creates missing directories (0755, root's), reloads systemd for a unit, then restarts the running services the file names",
		Reasons: []string{"mamelet writes the file once"},
	}},
}}}

// publishFiles adds to the part the files g declares on the node that it does not have yet (with no
// permission), and names in each the owner and group the declaration has now (what mamelet reports
// the existence of: they allow nothing). An approved write stays as it is.
func publishFiles(g *graph.Graph, name string, p *Part) {
	for _, r := range g.Sorted() {
		f := FileOf(r)
		if f == nil || f.Node != name {
			continue
		}
		if cur := p.file(f.Path); cur != nil {
			cur.Owner, cur.Group = f.Owner, f.Group
		} else {
			p.Files = append(p.Files, File{Path: f.Path, Owner: f.Owner, Group: f.Group})
		}
	}
}

// pruneFiles takes out of the part the files g no longer declares on the node, with their writes.
// The files stay on the node.
func pruneFiles(g *graph.Graph, name string, p *Part) {
	declared := map[string]bool{}
	for _, r := range g.Resources {
		if f := FileOf(r); f != nil && f.Node == name {
			declared[f.Path] = true
		}
	}
	p.Files = slices.DeleteFunc(p.Files, func(f File) bool { return !declared[f.Path] })
}

// protectFiles takes every write out of the part of a protected node.
func protectFiles(p *Part) {
	for i := range p.Files {
		p.Files[i].Write = nil
	}
}

// grantWrite grants the write of exactly the file the approved plan showed.
func grantWrite(_ context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := FileOf(q.Step.Resource)
	if want == nil {
		return node.Grant{}, false, nil
	}
	f := *want
	sum := ContentSHA256(f.Content)
	g := node.Grant{Node: f.Node, What: "write " + f.Path}
	g.Put = func(rev *noderev.Revision, once func(string) noderev.Once) error {
		// Exactly what the approved plan showed, or nothing happens.
		if approved := q.Approved("sha256"); approved != "" && approved != sum {
			return fmt.Errorf("the content of %s is not the one the approved plan showed (plan again)", f.Path)
		}
		for field, want := range map[string]string{"mode": f.Mode, "owner": f.Owner, "group": f.Group} {
			if approved := q.Approved(field); approved != "" && approved != want {
				return fmt.Errorf("the %s of %s is not the one the approved plan showed (plan again)", field, f.Path)
			}
		}
		p := partOf(rev)
		w := &Write{Content: f.Content, SHA256: sum, Mode: f.Mode, Owner: f.Owner, Group: f.Group, Restart: slices.Clone(f.Restart), Grant: once(ActionWrite)}
		if cur := p.file(f.Path); cur != nil {
			cur.Owner, cur.Group, cur.Write = f.Owner, f.Group, w
		} else {
			p.Files = append(p.Files, File{Path: f.Path, Owner: f.Owner, Group: f.Group, Write: w})
		}
		setPart(rev, p)
		return nil
	}
	return g, true, nil
}
