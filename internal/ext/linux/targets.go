package linux

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/state"
)

// What the part asks mamelet to observe (its observation targets) and what the Control Plane knows of
// the declaration in force, for every kind (wire.go explains why the targets are a union).
//
// Publish adds a declaration's targets to what the part has: two declarations planned in turn (a pull
// request's and the branch's, as mamelacd does) each find their own observation in the report, and the
// revision does not change from one plan to the next. Prune, when an approved plan is applied, sets
// them to that declaration's: the others' come back at their next plan.
//
// The union is bounded, and the bound decides by a fixed order, never by chance: a list of an entry
// that would grow past maxTargets keeps the values of the declaration in force (Held, which the
// Control Plane keeps for itself), then the planned declaration's, then those others added, as many as
// fit (an unapproved declaration never takes out what the one in force observes); the entries of a kind on a node
// are never more than mamelet observes (MaxNodeFiles and the like, room), kept in this order, each
// group by key: those the declaration in force has or a grant is for, then the planned declaration's,
// then those plans of other declarations added. What does not fit is left out of the part: the planned
// declaration's own entries that do not fit wait in its plan (MAM-PLAN-0219, roomNote), and the plan
// of a declaration whose entries another plan left out waits for them again. Two declarations whose
// new entries do not fit together therefore take the room in turn; once one of them is applied, its
// entries are in force, no plan of another declaration takes them out, and its plans converge.

// maxTargets bounds each list of observation targets of an entry (owners, groups, shells, uids, the
// URIs and suites of a source). A declaration never has more of one entry (a user has at most
// maxTargets groups).
const maxTargets = 16

// union is a and b, sorted, each value once, while that is at most maxTargets values. Past it, it
// keeps held (the values of the declaration in force, which it never takes out), then b's (the
// planned declaration's), then a's others (other declarations'), each in order, as many as fit: an
// unapproved declaration (a pull request) never takes out what the declaration in force observes,
// whatever it declares, and its own come before the others'.
func union[T cmp.Ordered](held, a, b []T) []T {
	out := sorted(slices.Concat(a, b))
	if len(out) <= maxTargets {
		return out
	}
	var keep []T
	for _, l := range [][]T{held, b, a} {
		for _, v := range sorted(l) {
			if len(keep) < maxTargets && !slices.Contains(keep, v) {
				keep = append(keep, v)
			}
		}
	}
	return sorted(keep)
}

// sorted is l sorted, each value once (nil when empty).
func sorted[T cmp.Ordered](l []T) []T {
	out := slices.Clone(l)
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// room is how many entries of a kind mamelet observes on one node, and how Publish and the plan find
// them: of a kind, the node and key (path or name) of a resource (ok false: another kind), the keys the
// declaration in force has (InForce), and those mamelet reported.
type room struct {
	what     string // "files", for a person
	most     int
	of       func(r graph.Resource) (node, key string, ok bool)
	inForce  func(*InForce) []string
	reported func(Observed) []string
}

var rooms = map[graph.Kind]room{
	KindFile: {"files", MaxNodeFiles, func(r graph.Resource) (string, string, bool) {
		if f := FileOf(r); f != nil {
			return f.Node, f.Path, true
		}
		return "", "", false
	}, func(i *InForce) []string { return i.Files }, func(o Observed) []string {
		var out []string
		for _, f := range o.Files {
			out = append(out, f.Path)
		}
		return out
	}},
	KindUser: {"users", MaxNodeUsers, func(r graph.Resource) (string, string, bool) {
		if u := UserOf(r); u != nil {
			return u.Node, u.Name, true
		}
		return "", "", false
	}, func(i *InForce) []string { return i.Users }, func(o Observed) []string {
		var out []string
		for _, u := range o.UserStates {
			out = append(out, u.Name)
		}
		return out
	}},
	KindAptRepo: {"APT sources", MaxNodeAptRepos, func(r graph.Resource) (string, string, bool) {
		if a := AptRepoOf(r); a != nil {
			return a.Node, a.Name, true
		}
		return "", "", false
	}, func(i *InForce) []string { return i.AptRepos }, func(o Observed) []string {
		var out []string
		for _, r := range o.AptRepos {
			out = append(out, r.Name)
		}
		return out
	}},
	KindArtifact: {"artifacts", MaxNodeArtifacts, func(r graph.Resource) (string, string, bool) {
		if a := ArtifactOf(r); a != nil {
			return a.Node, a.Path, true
		}
		return "", "", false
	}, func(i *InForce) []string { return i.Artifacts }, func(o Observed) []string {
		var out []string
		for _, a := range o.Artifacts {
			out = append(out, a.Path)
		}
		return out
	}},
}

// declaredKeys are the keys of the resources of a kind that g declares on node.
func (rm room) declaredKeys(rs []graph.Resource, node string) map[string]bool {
	out := map[string]bool{}
	for _, r := range rs {
		if n, key, ok := rm.of(r); ok && n == node {
			out[key] = true
		}
	}
	return out
}

// bound keeps the entries of one kind of a node's part within what mamelet observes of the kind
// (most), in this order, each group by key: every entry the declaration in force has or a grant is for
// (held: all of them, a declaration in force never has more than most), then those the planned
// declaration has (declared), then those plans of other declarations added, while there is room. What
// it leaves out is no approved plan's: the planned declaration's entries left out wait in its plan
// (roomNote computes the same), another declaration's come back at its next plan.
func bound[E any](entries []E, key func(E) string, held func(E) bool, declared map[string]bool, most int) []E {
	var keep, planned, others []E
	for _, e := range entries {
		switch {
		case held(e):
			keep = append(keep, e)
		case declared[key(e)]:
			planned = append(planned, e)
		default:
			others = append(others, e)
		}
	}
	byKey := func(a, b E) int { return strings.Compare(key(a), key(b)) }
	slices.SortFunc(planned, byKey)
	slices.SortFunc(others, byKey)
	free := max(most-len(keep), 0)
	planned = planned[:min(len(planned), free)]
	free -= len(planned)
	others = others[:min(len(others), free)]
	return slices.Concat(keep, planned, others)
}

// codeNoRoom: mamelet does not observe a resource of the declaration, because the node's part has no
// room for it, or shares it with plans of other declarations: it waits.
const codeNoRoom = "MAM-PLAN-0219"

// roomNote says why the resource r of the declaration being planned is not observed, when the room of
// its kind on its node is why (nil otherwise): the part has room for most of the kind, the declaration
// in force has some of it, and this declaration's other entries that are not in force take the rest,
// in order of key (as Publish does: bound). An entry past that waits until this declaration is applied
// (Prune makes the part this declaration's). An entry that fits but that mamelet has not reported, while
// what other declarations' plans added fills the room, waits for mamelet's report after this plan: the
// plans of declarations that do not fit together take the room in turn, until one is applied.
func roomNote(k *plan.Kit, r graph.Resource) *plan.Note {
	rm, ok := rooms[r.Kind]
	if !ok {
		return nil
	}
	node, key, _ := rm.of(r)
	o, readable := observed(k, node)
	if !readable {
		return nil // what the plan waits for is the report
	}
	in := map[string]bool{}
	if o.InForce != nil {
		for _, n := range rm.inForce(o.InForce) {
			in[n] = true
		}
	}
	if in[key] {
		return nil
	}
	declared := rm.declaredKeys(k.Resources(), node)
	var mine []string
	for _, n := range sortedKeys(declared) {
		if !in[n] {
			mine = append(mine, n)
		}
	}
	free := max(rm.most-len(in), 0)
	if i := slices.Index(mine, key); i >= free {
		return &plan.Note{Code: codeNoRoom,
			Message: fmt.Sprintf("mamelet on %s observes at most %d %s, and the declaration in force has %d of them there: %d of the %d new ones of this declaration fit (in order of name), and %s is not one of them, so mamelet does not report it",
				node, rm.most, rm.what, len(in), free, len(mine), key),
			Hint: fmt.Sprintf("approve and apply this plan: what mamelet observes on %s then becomes this declaration's (what it no longer has stays on the node, unobserved), and the next plan has %s. "+
				"With sync = \"push\", a plan that has nothing else to change is not approved by a push: approve it on the Control Plane's host (mamela approve <digest>, then mamela apply). Or declare fewer %s on %s",
				node, key, rm.what, node)}
	}
	reported := rm.reported(o)
	if slices.Contains(reported, key) {
		return nil
	}
	others := 0
	for _, n := range reported {
		if !in[n] && !declared[n] {
			others++
		}
	}
	if others == 0 || min(len(mine), free)+others <= free {
		return nil // newly declared: the report after this plan has it
	}
	return &plan.Note{Code: codeNoRoom,
		Message: fmt.Sprintf("waits for the agent on %s to report %s: mamelet observes at most %d %s there, and the declaration in force has %d, this declaration %d new ones, and plans of other declarations %d more; "+
			"this plan puts %s in what mamelet observes, and a plan of another declaration made before mamelet reports takes it out again (the declarations take the room in turn)",
			node, key, rm.most, rm.what, len(in), len(mine), others, key),
		Hint: fmt.Sprintf("it is reported when mamelet reports before another declaration is planned; or apply the declaration that is to be in force: the room on %s is then its own, and the plans of the others wait (with this code) until they are applied",
			node)}
}

// annotate is the linux observation of a node as the plan reads it: the report's, with what the
// Control Plane's own record of the node says (rev, its current revision; nil: none for the machine
// that reported). InForce is set from the part, whatever the report says.
func annotate(rev *noderev.Revision, o *state.ExtObservation) {
	if len(o.Payload) == 0 {
		return
	}
	var obs Observed
	if decodeStrict(o.Payload, &obs) != nil {
		return // the plan says that it cannot be read
	}
	obs.InForce = nil
	if rev != nil {
		p := partOf(rev)
		in := InForce{}
		for _, f := range p.Files {
			if f.InForce {
				in.Files = append(in.Files, f.Path)
			}
		}
		for _, u := range p.Users {
			if u.InForce {
				in.Users = append(in.Users, u.Name)
			}
		}
		for _, a := range p.Artifacts {
			if a.InForce {
				in.Artifacts = append(in.Artifacts, a.Path)
			}
		}
		for _, r := range p.AptRepos {
			if r.InForce {
				in.AptRepos = append(in.AptRepos, r.Name)
			}
		}
		if len(in.Files)+len(in.Users)+len(in.Artifacts)+len(in.AptRepos) > 0 {
			obs.InForce = &in
		}
	}
	c := encode(obs)
	if c.Canonicalize() == nil {
		o.Payload = c
	}
}

// inForce reports whether the declaration in force has the item of a kind (InForce of a node's
// observation).
func (o Observed) inForce(list func(*InForce) []string, key string) bool {
	return o.InForce != nil && slices.Contains(list(o.InForce), key)
}

// observedAt is the linux observation of node in the snapshot an approved plan was made from (a
// grant's before-state comes from it).
func observedAt(s state.Snapshot, node string) (Observed, error) {
	e, ok := s.Ext[node][Name]
	if !ok || len(e.Payload) == 0 {
		return Observed{}, fmt.Errorf("the approved plan was made without a report of linux from %s", node)
	}
	var o Observed
	if err := decodeStrict(e.Payload, &o); err != nil {
		return Observed{}, fmt.Errorf("the report of linux from %s the approved plan was made of cannot be read: %w", node, err)
	}
	return o, nil
}
