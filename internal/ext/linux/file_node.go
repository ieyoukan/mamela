package linux

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
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
		cur.Owners, cur.Groups = union(cur.Held.heldOwners(), cur.Owners, []string{f.Owner}), union(cur.Held.heldGroups(), cur.Groups, []string{f.Group})
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
		f.Held = &OwnerTargets{Owners: []string{want.Owner}, Groups: []string{want.Group}}
		if w := f.Write; w != nil && !writes(*w, want) {
			f.Write = nil
		}
	}
}

// writes reports whether an approved write writes exactly what the declaration f has.
func writes(w Write, f *FileSpec) bool {
	return w.SHA256 == ContentSHA256(f.Content) && w.Mode == f.Mode && w.Owner == f.Owner && w.Group == f.Group && slices.Equal(w.Restart, f.Restart) &&
		w.Check == f.Check && slices.Equal(w.Reload, f.Reload)
}

// protectFiles takes every write out of the part of a protected node.
func protectFiles(p *Part) {
	for i := range p.Files {
		p.Files[i].Write = nil
	}
}

// grantWrite grants the write of exactly the file the approved plan showed, against what the plan's
// observation found at the path (Write.Before). A file with a check is granted with the other files
// of the approved plan that write on its node with the same check (checkedWith), under the one grant
// of this operation: the reconciler applies one operation at a time, and waits for its outcome before
// the next, so that the check, which mamelet runs once the writes of a check in its part are made,
// would otherwise see the first file of two that are valid only together. The operations of the
// others, which follow, find their files written (their grants then write nothing, and the check
// runs again).
func grantWrite(ctx context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := FileOf(q.Step.Resource)
	if want == nil {
		return node.Grant{}, false, nil
	}
	f := *want
	o, err := observedAt(q.Observed, f.Node)
	if err != nil {
		return node.Grant{}, true, fmt.Errorf("write %s: %w (plan again)", f.Path, err)
	}
	w, err := writeOf(o, &f, q.Step.Approved)
	if err != nil {
		return node.Grant{}, true, err
	}
	writes := []pendingWrite{{f.Path, w, &f}}
	others, err := checkedWith(ctx, q, o, &f)
	if err != nil {
		return node.Grant{}, true, err
	}
	writes = append(writes, others...)
	what := "write " + f.Path
	if len(others) > 0 {
		var paths []string
		for _, o := range others {
			paths = append(paths, o.path)
		}
		v, _ := validatorOf(f.Check)
		what += fmt.Sprintf(" with %s (%s runs once they are written)", strings.Join(paths, ", "), v.Shown)
	}
	g := node.Grant{Node: f.Node, What: what}
	g.Put = func(rev *noderev.Revision, once func(string) noderev.Once) error {
		p := partOf(rev)
		grant := once(ActionWrite)
		for _, pw := range writes {
			w := pw.write
			w.Grant = grant
			cur := p.file(pw.path)
			if cur == nil {
				p.Files = append(p.Files, File{Path: pw.path})
				cur = &p.Files[len(p.Files)-1]
			}
			cur.Owners, cur.Groups = union(cur.Held.heldOwners(), cur.Owners, []string{w.Owner}), union(cur.Held.heldGroups(), cur.Groups, []string{w.Group})
			cur.InForce, cur.Write = true, &w
		}
		setPart(rev, p)
		return nil
	}
	return g, true, nil
}

// pendingWrite is a write a grant puts into the part: its path, the write (without its grant), and
// the file declared.
type pendingWrite struct {
	path  string
	write Write
	spec  *FileSpec
}

// writeOf is the write of the declared file f, against what the observation o (the approved plan's)
// found at its path, if it is exactly what the change approved (c) showed: an error otherwise (nothing
// is then granted).
func writeOf(o Observed, f *FileSpec, c plan.Change) (Write, error) {
	got := o.file(f.Path)
	if got == nil {
		return Write{}, fmt.Errorf("write %s: the approved plan was made without a report of it (plan again)", f.Path)
	}
	approved := func(field string) string {
		for _, d := range c.Diffs {
			if d.Field == field {
				return d.To
			}
		}
		return ""
	}
	sum := ContentSHA256(f.Content)
	// Exactly what the approved plan showed, or nothing happens.
	if a := approved("sha256"); a != "" && a != sum {
		return Write{}, fmt.Errorf("the content of %s is not the one the approved plan showed (plan again)", f.Path)
	}
	for _, d := range []struct{ field, want string }{{"mode", f.Mode}, {"owner", f.Owner}, {"group", f.Group}} {
		if a := approved(d.field); a != "" && a != d.want {
			return Write{}, fmt.Errorf("the %s of %s is not the one the approved plan showed (plan again)", d.field, f.Path)
		}
	}
	// The check and the services reloaded: the plan shows them with every write of the file.
	for _, d := range []struct {
		field, want string
		declared    bool
	}{{"check", checkText(f.Check), f.Check != ""}, {"reload", reloadText(f.Reload), len(f.Reload) > 0}} {
		if a := approved(d.field); (a != "" || d.declared) && a != d.want {
			return Write{}, fmt.Errorf("the %s of %s is not the one the approved plan showed (plan again)", d.field, f.Path)
		}
	}
	return Write{Content: f.Content, SHA256: sum, Mode: f.Mode, Owner: f.Owner, Group: f.Group, Restart: slices.Clone(f.Restart),
		Check: f.Check, Reload: slices.Clone(f.Reload), Before: beforeOf(*got)}, nil
}

// checkedWith are the writes of the other files of the approved plan on f's node with f's check (none
// for a file without one): each a change of the plan that writes its file with an approval (not one
// the policy denies, nor one that waits), as the plan showed it.
func checkedWith(ctx context.Context, q node.GrantRequest, o Observed, f *FileSpec) ([]pendingWrite, error) {
	if f.Check == "" || q.Store == nil || q.Declaration == nil {
		return nil, nil
	}
	p, _, err := q.Store.LoadPlan(ctx, q.Step.PlanDigest)
	if err != nil {
		return nil, fmt.Errorf("write %s: read the approved plan for the files checked with it: %w", f.Path, err)
	}
	declared := map[string]*FileSpec{}
	for _, r := range q.Declaration.Resources {
		if s := FileOf(r); s != nil {
			declared[r.ID()] = s
		}
	}
	self := FileName(f.Node, f.Path)
	approvedWrite := func(op plan.Operation) bool {
		return op.Op == OpWriteFile && op.Decision.Verdict == policy.ApprovalRequired
	}
	var out []pendingWrite
	for _, c := range p.Changes {
		s := declared[c.ID()]
		if c.Kind != KindFile || c.Name == self || s == nil || s.Node != f.Node || s.Check != f.Check || c.Status != plan.StatusChange ||
			!slices.ContainsFunc(c.Operations, approvedWrite) {
			continue
		}
		w, err := writeOf(o, s, c)
		if err != nil {
			return nil, err
		}
		out = append(out, pendingWrite{s.Path, w, s})
	}
	return out, nil
}
