package linux

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
)

// planAsTheControlPlane plans the declaration as the Control Plane does (mamela plan, mamela api): it
// observes, plans and saves, then adds the declaration's observation targets to the node's revision.
func (f *flow) planAsTheControlPlane() *plan.Plan {
	f.t.Helper()
	snap, err := f.rec.Provider.Observe(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	p := plan.Build(f.g, snap, policy.Default)
	if err := f.st.SavePlan(f.ctx, f.g, snap, policy.Default, p); err != nil {
		f.t.Fatal(err)
	}
	f.publish()
	return p
}

// files are n files /srv/<prefix>NNN on web, each with content "<prefix>\n".
func files(prefix string, from, n int) []string {
	var out []string
	for i := from; i < from+n; i++ {
		path := fmt.Sprintf("/srv/%s%03d", prefix, i)
		out = append(out, fileRes("web/file"+path, fmt.Sprintf(`{"node":"web","path":%q,"content":"%s\n"}`, path, prefix)))
	}
	return out
}

func statusOf(t *testing.T, p *plan.Plan, id string) plan.Change {
	t.Helper()
	for _, c := range p.Changes {
		if c.ID() == id {
			return c
		}
	}
	t.Fatalf("no change of %s", id)
	return plan.Change{}
}

// mainInForce makes a flow whose node has the files of main in place and in force (an approved plan of
// main applied), but /srv/m000, whose write main's apply hands out while mamelet does not answer.
func mainInForce(t *testing.T, main []string) (*flow, string) {
	f := newFlow(t, main...)
	for i := range len(main) {
		write(t, filepath.Join(f.node.root, fmt.Sprintf("srv/m%03d", i)), "m\n")
	}
	write(t, filepath.Join(f.node.root, "srv/m000"), "old\n")
	f.publish()
	f.node.round() // it takes linux
	f.publish()
	f.node.round() // it observes main's files
	p := f.planAsTheControlPlane()
	if c := statusOf(t, p, "NodeFile web/file/srv/m000"); c.Status != plan.StatusChange {
		t.Fatalf("main's write: %+v %+v", c, c.Note)
	}
	f.applyUnknown(p)
	part := partOf(f.revision())
	w := part.file("/srv/m000").Write
	if w == nil {
		t.Fatal("no write handed out")
	}
	return f, w.Grant.Operation
}

// The plan of a declaration nobody approved (a pull request) whose new files, with those of the
// declaration in force, are more than mamelet observes on the node: the part keeps every file in
// force and granted, and of the pull request's new ones only those that fit (by name); mamelet does
// not refuse the part, main's write is carried out, and the pull request's plan says why the others
// wait (MAM-PLAN-0219). Once it is applied, they are its own.
func TestAPlanPastTheRoomLeavesMainsGrantsAlone(t *testing.T) {
	main := files("m", 0, 250)
	f, op := mainInForce(t, main)

	// The pull request replaces 10 of main's files with 10 of its own: 250 files, 260 with main's.
	pr := slices.Concat(main[10:], files("p", 0, 10))
	f.declare(pr...)
	p := f.planAsTheControlPlane()
	part := partOf(f.revision())
	if len(part.Files) != MaxNodeFiles {
		t.Fatalf("the part has %d files", len(part.Files))
	}
	if err := checkPart(f.revision().Ext[Name]); err != nil {
		t.Fatalf("mamelet refuses the part: %v", err)
	}
	for i := range 10 {
		id := fmt.Sprintf("NodeFile web/file/srv/p%03d", i)
		c := statusOf(t, p, id)
		in := part.file(fmt.Sprintf("/srv/p%03d", i)) != nil
		switch {
		case i < 6 && (c.Status != plan.StatusWaiting || c.Note.Code != codeFileAwait || !in):
			t.Errorf("%s fits: %+v %+v (in the part: %v)", id, c, c.Note, in)
		case i >= 6 && (c.Status != plan.StatusWaiting || c.Note.Code != codeNoRoom || in ||
			!strings.Contains(c.Note.Message, "at most 256 files, and the declaration in force has 250 of them there: 6 of the 10 new ones")):
			t.Errorf("%s does not fit: %+v %+v (in the part: %v)", id, c, c.Note, in)
		}
	}

	rep := f.node.round()
	if len(rep.ExtRefused) != 0 {
		t.Fatalf("mamelet refused the part: %v", rep.ExtRefused)
	}
	if r := result(t, rep, op); r.Outcome != noderev.OutcomeDone || f.node.read("/srv/m000") != "m\n" {
		t.Fatalf("main's write: %+v", r)
	}

	// The pull request's plan again: the files that fit are planned, the others still wait.
	p = f.planAsTheControlPlane()
	if c := statusOf(t, p, "NodeFile web/file/srv/p000"); c.Status != plan.StatusChange {
		t.Errorf("p000: %+v %+v", c, c.Note)
	}
	if c := statusOf(t, p, "NodeFile web/file/srv/p009"); c.Status != plan.StatusWaiting || c.Note.Code != codeNoRoom {
		t.Errorf("p009: %+v %+v", c, c.Note)
	}

	// Applied (it is approved, merged): the part is the pull request's, and its next plan has all.
	f.apply(p)
	f.node.round()
	p = f.planAsTheControlPlane()
	if c := statusOf(t, p, "NodeFile web/file/srv/p009"); c.Status != plan.StatusChange {
		t.Errorf("p009 after the apply: %+v %+v", c, c.Note)
	}
	if n := len(partOf(f.revision()).Files); n != 250 {
		t.Errorf("the part has %d files", n)
	}
}

// Two declarations nobody applied whose new files do not fit together, planned in turn (mamelacd
// plans each while it waits): each plan puts its own files in the part, by a fixed order, and says
// that the room is shared. Once one of them is applied (on the host: its plan only waits), its files
// are in force: no plan of the other takes them out, its next plan converges, the other's wait with
// the reason, and the revision no longer changes from one plan to the next.
func TestTwoDeclarationsTakeTheRoomInTurnUntilOneIsApplied(t *testing.T) {
	main := files("m", 0, 250)
	f, _ := mainInForce(t, main)
	f.node.round() // main's write
	a := slices.Concat(main, files("a", 0, 6))
	b := slices.Concat(main, files("b", 0, 6))
	planOf := func(decl []string) *plan.Plan {
		f.declare(decl...)
		p := f.planAsTheControlPlane()
		f.node.round()
		return p
	}
	planOf(b)
	var last *plan.Plan
	for i := range 5 {
		decl, mine := a, "a"
		if i%2 == 1 {
			decl, mine = b, "b"
		}
		last = planOf(decl)
		c := statusOf(t, last, "NodeFile web/file/srv/"+mine+"000")
		if c.Status != plan.StatusWaiting || c.Note.Code != codeNoRoom || !strings.Contains(c.Note.Message, "take the room in turn") {
			t.Errorf("plan %d of %s: %+v %+v", i, mine, c, c.Note)
		}
		if n := len(partOf(f.revision()).Files); n != MaxNodeFiles {
			t.Errorf("plan %d: the part has %d files", i, n)
		}
	}

	// a's last plan only waits; approved and applied, the room is a's.
	f.declare(a...)
	f.apply(last)
	f.node.round()
	f.declare(a...)
	if c := statusOf(t, f.planAsTheControlPlane(), "NodeFile web/file/srv/a000"); c.Status != plan.StatusChange {
		t.Fatalf("a after its apply: %+v %+v", c, c.Note)
	}
	f.node.round()
	serial := f.serial()
	for i := range 4 {
		decl, mine := a, "a"
		if i%2 == 1 {
			decl, mine = b, "b"
		}
		p := planOf(decl)
		c := statusOf(t, p, "NodeFile web/file/srv/"+mine+"000")
		switch {
		case mine == "a" && c.Status != plan.StatusChange:
			t.Errorf("plan %d of a, in force: %+v %+v", i, c, c.Note)
		case mine == "b" && (c.Status != plan.StatusWaiting || c.Note.Code != codeNoRoom || !strings.Contains(c.Note.Message, "has 256 of them there: 0 of the 6")):
			t.Errorf("plan %d of b: %+v %+v", i, c, c.Note)
		}
	}
	if s := f.serial(); s != serial {
		t.Errorf("the revision changed from %d to %d with plans alone", serial, s)
	}
}
