package mamelet

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"slices"

	"github.com/ieyoukan/mamela/internal/names"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// A node extension (k3s, docs/design/node-extensions.md) adds to mamelet what is its: what it
// observes for the report, the one-time grants it carries out, and the programs it runs. It is
// compiled in: mamelet runs no code it fetched, and only fixed programs (Programs). The core keeps
// what makes a grant safe: the revision is verified first, a one-time grant is carried out only
// from a revision fetched in this round (Act.Online) and once per attempt (Act.Done, Act.Record).

// Phase is where in a round an extension acts, around the update of mamelet itself: the order of
// a round is fixed (packages, the extensions before, the update, the extensions after, services).
type Phase int

const (
	BeforeUpdate Phase = iota
	AfterUpdate
)

// Extension is what a node extension adds to mamelet.
type Extension struct {
	Name string
	// Wire is the version of the extension's part of a revision (Revision.Ext[Name]) this mamelet
	// reads; 0 for an extension that has members of its own in the revision (k3s). An extension with
	// a Wire is listed in every report (Report.Extensions), and the Control Plane gives the node its
	// part only then (noderev's ext.go). Its observations go into Report.Ext[Name].
	Wire int
	// Check checks the extension's part of a verified revision before anything else sees it: a part
	// it refuses is taken out of the revision for this round (Observe and Act do not see it) and
	// reported (Report.ExtRefused), and the rest of the revision goes on.
	Check func(part jsontext.Value) error
	// Programs are the programs it runs (absolute paths), added to what ExecRunner allows.
	Programs []string
	// Observe adds to the report what the extension observes of what the revision declares. The
	// report has the recorded outcomes of the revision's grants already (Report.Results).
	Observe func(ctx context.Context, s *System, rev *noderev.Revision, rep *noderev.Report)
	// Act carries out the extension's grants of the phase, and says whether it changed the system.
	Act func(ctx context.Context, x *Act, phase Phase) bool
	// Results are the recorded outcomes of the extension's grants in the revision, in the report's
	// order for the phase (known: the outcome of an attempt, if there is one).
	Results func(rev *noderev.Revision, phase Phase, known func(op string, attempt int) (noderev.Result, bool)) []noderev.Result
	// Recover finishes, from the extension's own record in the state directory, what a change of it
	// that mamelet stopped in the middle of left half done (mamelet was killed, the node lost power).
	// It runs at the start of every round, before the revision is fetched and whatever it says (none,
	// or one that expired while mamelet was down): it only finishes or undoes what the extension began
	// under a grant, and starts nothing.
	Recover func(ctx context.Context, s *System)
}

// recoverExtensions runs each extension's Recover.
func recoverExtensions(ctx context.Context, s *System) {
	for _, e := range extensions {
		if e.Recover != nil {
			e.Recover(ctx, s)
		}
	}
}

var extensions []Extension

// Register adds a node extension. It panics when its name is taken or a program is not an
// absolute path: programming errors, found when the program starts.
func Register(e Extension) {
	if e.Name == "" || slices.ContainsFunc(extensions, func(o Extension) bool { return o.Name == e.Name }) {
		panic(fmt.Sprintf("mamelet: extension %q: a name of its own is needed", e.Name))
	}
	for _, p := range e.Programs {
		if len(p) == 0 || p[0] != '/' {
			panic(fmt.Sprintf("mamelet: extension %s: program %q is not an absolute path", e.Name, p))
		}
	}
	if e.Wire < 0 || (e.Wire > 0 && (!names.Extension(e.Name) || e.Check == nil)) {
		panic(fmt.Sprintf("mamelet: extension %s: an extension of the generic wire needs the name of an extension and Check", e.Name))
	}
	extensions = append(extensions, e)
}

// takes are the extensions whose part of a revision this mamelet takes, with the version it reads
// (nil when there is none: a report then is what it was before the generic wire).
func takes() map[string]int {
	var out map[string]int
	for _, e := range extensions {
		if e.Wire > 0 {
			if out == nil {
				out = map[string]int{}
			}
			out[e.Name] = e.Wire
		}
	}
	return out
}

// takeParts is the revision with only the extensions' parts this mamelet takes: a part of an
// extension it does not know, or that its extension's Check refuses, is left out, and the report
// says why (Report.ExtRefused). Nothing else of the revision changes: the rest goes on.
func takeParts(rev *noderev.Revision, rep *noderev.Report) *noderev.Revision {
	if len(rev.Ext) == 0 {
		return rev
	}
	r := *rev
	r.Ext = map[string]jsontext.Value{}
	refuse := func(name, why string) {
		if rep.ExtRefused == nil {
			rep.ExtRefused = map[string]string{}
		}
		rep.ExtRefused[name] = why
	}
	for _, name := range slices.Sorted(maps.Keys(rev.Ext)) {
		i := slices.IndexFunc(extensions, func(e Extension) bool { return e.Name == name && e.Wire > 0 })
		if i < 0 {
			refuse(name, "this mamelet does not know the extension "+name+"; it went on with the rest of the revision")
			continue
		}
		if err := extensions[i].Check(rev.Ext[name]); err != nil {
			refuse(name, err.Error())
			continue
		}
		r.Ext[name] = rev.Ext[name]
	}
	return &r
}

// Act is what an extension carries out its grants with, in one round.
type Act struct {
	System   *System
	Revision *noderev.Revision
	Node     string // this node
	// StateDir is mamelet's own directory (Config.StateDir): no extension writes there.
	StateDir string
	// Online: the revision was fetched and verified in this round. One-time grants need it.
	Online bool
	st     *State
	record func(op string, attempt int, outcome, detail string)
}

// Done is whether the attempt of the operation was made already (it is never made twice).
func (x *Act) Done(op string, attempt int) bool {
	_, done := x.st.Attempts[attemptKey(op, attempt)]
	return done
}

// Record keeps the outcome of an attempt: it is reported, and the attempt is not made again.
func (x *Act) Record(op string, attempt int, outcome, detail string) {
	x.record(op, attempt, outcome, detail)
}

func (a *Agent) actExtensions(ctx context.Context, x *Act, phase Phase) bool {
	changed := false
	for _, e := range extensions {
		if e.Act != nil && e.Act(ctx, x, phase) {
			changed = true
		}
	}
	return changed
}

func extResults(rev *noderev.Revision, st State, phase Phase) []noderev.Result {
	known := func(op string, attempt int) (noderev.Result, bool) {
		r, ok := st.Attempts[attemptKey(op, attempt)]
		return r, ok
	}
	var out []noderev.Result
	for _, e := range extensions {
		if e.Results != nil {
			out = append(out, e.Results(rev, phase, known)...)
		}
	}
	return out
}
