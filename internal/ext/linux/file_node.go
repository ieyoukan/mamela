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
// permission, not InForce), and adds to each the owner and group the declaration names (what mamelet
// reports the existence of: they allow nothing). It takes nothing away that the declaration in force
// has: an approved write stays as it is, whatever g declares (g may be a declaration nobody approved).
// Only what no approved plan needs is left out, past what mamelet observes on a node (bound).
func publishFiles(g *graph.Graph, name string, p *Part) {
	declared := map[string]bool{}
	for _, r := range g.Sorted() {
		f := FileOf(r)
		if f == nil || f.Node != name {
			continue
		}
		declared[f.Path] = true
		cur := p.file(f.Path)
		if cur == nil {
			p.Files = append(p.Files, File{Path: f.Path})
			cur = &p.Files[len(p.Files)-1]
		}
		cur.Owners, cur.Groups = union(cur.Owners, []string{f.Owner}), union(cur.Groups, []string{f.Group})
	}
	p.Files = bound(p.Files, func(f File) string { return f.Path }, func(f File) bool { return f.InForce || f.Write != nil }, declared, MaxNodeFiles)
}

// pruneFiles makes the part the files g, an approved declaration being applied, declares on the node:
// the others go, with their writes (the files stay on the node); those it declares are InForce (those
// a plan left out for want of room too: bound), with its owner and group as targets, and keep a write
// only if it writes what g declares (a write approved for another content is withdrawn: the plan of g
// said nothing of it).
func pruneFiles(g *graph.Graph, name string, p *Part) {
	declared := map[string]*FileSpec{}
	for _, r := range g.Resources {
		if f := FileOf(r); f != nil && f.Node == name {
			declared[f.Path] = f
		}
	}
	p.Files = slices.DeleteFunc(p.Files, func(f File) bool { return declared[f.Path] == nil })
	for _, path := range slices.Sorted(maps.Keys(declared)) {
		if p.file(path) == nil {
			p.Files = append(p.Files, File{Path: path})
		}
	}
	for i := range p.Files {
		f, want := &p.Files[i], declared[p.Files[i].Path]
		f.InForce, f.Owners, f.Groups = true, []string{want.Owner}, []string{want.Group}
		if w := f.Write; w != nil && !writes(*w, want) {
			f.Write = nil
		}
	}
}

// writes reports whether an approved write writes exactly what the declaration f has.
func writes(w Write, f *FileSpec) bool {
	return w.SHA256 == ContentSHA256(f.Content) && w.Mode == f.Mode && w.Owner == f.Owner && w.Group == f.Group && slices.Equal(w.Restart, f.Restart)
}

// protectFiles takes every write out of the part of a protected node.
func protectFiles(p *Part) {
	for i := range p.Files {
		p.Files[i].Write = nil
	}
}

// grantWrite grants the write of exactly the file the approved plan showed, against what the plan's
// observation found at the path (Write.Before).
func grantWrite(_ context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := FileOf(q.Step.Resource)
	if want == nil {
		return node.Grant{}, false, nil
	}
	f := *want
	sum := ContentSHA256(f.Content)
	o, err := observedAt(q.Observed, f.Node)
	if err != nil {
		return node.Grant{}, true, fmt.Errorf("write %s: %w (plan again)", f.Path, err)
	}
	got := o.file(f.Path)
	if got == nil {
		return node.Grant{}, true, fmt.Errorf("write %s: the approved plan was made without a report of it (plan again)", f.Path)
	}
	before := beforeOf(*got)
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
		w := &Write{Content: f.Content, SHA256: sum, Mode: f.Mode, Owner: f.Owner, Group: f.Group, Restart: slices.Clone(f.Restart),
			Before: before, Grant: once(ActionWrite)}
		cur := p.file(f.Path)
		if cur == nil {
			p.Files = append(p.Files, File{Path: f.Path})
			cur = &p.Files[len(p.Files)-1]
		}
		cur.Owners, cur.Groups = union(cur.Owners, []string{f.Owner}), union(cur.Groups, []string{f.Group})
		cur.InForce, cur.Write = true, w
		setPart(rev, p)
		return nil
	}
	return g, true, nil
}
