package mamelet

import (
	"context"
	"fmt"
	"slices"

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
	// Programs are the programs it runs (absolute paths), added to what ExecRunner allows.
	Programs []string
	// Observe adds to the report what the extension observes of what the revision declares.
	Observe func(ctx context.Context, s *System, rev *noderev.Revision, rep *noderev.Report)
	// Act carries out the extension's grants of the phase, and says whether it changed the system.
	Act func(ctx context.Context, x *Act, phase Phase) bool
	// Results are the recorded outcomes of the extension's grants in the revision, in the report's
	// order for the phase (known: the outcome of an attempt, if there is one).
	Results func(rev *noderev.Revision, phase Phase, known func(op string, attempt int) (noderev.Result, bool)) []noderev.Result
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
	extensions = append(extensions, e)
}

// Act is what an extension carries out its grants with, in one round.
type Act struct {
	System   *System
	Revision *noderev.Revision
	Node     string // this node
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
