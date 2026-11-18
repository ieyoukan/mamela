package linux

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/state"
)

// What the part asks mamelet to observe (its observation targets) and what the Control Plane knows of
// the declaration in force, for every kind (wire.go explains why the targets are a union).
//
// Publish adds a declaration's targets to what the part has: two declarations planned in turn (a pull
// request's and the branch's, as mamelacd does) each find their own observation in the report, and the
// revision does not change from one plan to the next. Prune, when an approved plan is applied, sets
// them to that declaration's: the others' come back at their next plan. A list that would grow past
// maxTargets, or a kind whose entries no approved declaration has would grow past maxCandidates, is
// set to the planned declaration's instead: bounded, at the price of the other declaration's plan
// waiting for one report.

// maxTargets bounds each list of observation targets of an entry (owners, groups, shells, uids, the
// URIs and suites of a source).
const maxTargets = 16

// maxCandidates bounds the entries of one kind on one node that only plans of declarations nobody
// applied have added (entries without a grant that are not InForce).
const maxCandidates = 64

// union is a and b, sorted, each value once; a when it would hold more than maxTargets values, then b
// alone (the planned declaration's).
func union[T cmp.Ordered](a, b []T) []T {
	out := slices.Concat(a, b)
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > maxTargets {
		out = slices.Clone(b)
		slices.Sort(out)
		out = slices.Compact(out)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sorted is l sorted, each value once (nil when empty).
func sorted[T cmp.Ordered](l []T) []T {
	return union[T](nil, l)
}

// boundCandidates takes out of entries those of a kind only declarations nobody applied added, past
// maxCandidates: those that are not InForce, have no grant and the planned declaration does not have
// (keep).
func boundCandidates[E any](entries []E, candidate func(E) bool, keep func(E) bool) []E {
	n := 0
	for _, e := range entries {
		if candidate(e) {
			n++
		}
	}
	if n <= maxCandidates {
		return entries
	}
	return slices.DeleteFunc(entries, func(e E) bool { return candidate(e) && !keep(e) })
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
