package linux

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// The whole way of a file: the Control Plane publishes the node's revision, mamelet observes, the
// plan asks for an approval, the apply grants the write, mamelet writes it; and before that, a node
// whose mamelet does not take the extension gets nothing of it.

// storeCP is the Control Plane as mamelet sees it: revisions signed from the store as it hands them
// out, reports into the store.
type storeCP struct {
	st  *store.Store
	key ed25519.PrivateKey
	now func() time.Time
}

func (c storeCP) Revision(ctx context.Context) (noderev.Signed, error) {
	r, err := c.st.NodeRevision(ctx, "web")
	if err != nil || r == nil {
		return noderev.Signed{}, fmt.Errorf("no revision: %v", err)
	}
	r.IssuedAt, r.ExpiresAt = c.now(), c.now().Add(time.Hour)
	return noderev.Sign(c.key, r.ForMamelet(c.now()))
}

func (c storeCP) Report(ctx context.Context, r noderev.Report) error {
	return c.st.PutNodeReport(ctx, r)
}

// machineWorld: the machine web exists, is Mamela's and runs (7001); the rest is mamelet's.
type machineWorld struct{ f *flow }

func (w machineWorld) Observe(ctx context.Context) (state.Snapshot, error) {
	s, err := w.f.nodes.Observe(ctx)
	if err != nil {
		return s, err
	}
	for _, r := range w.f.g.Resources {
		if r.Provided != nil {
			s.AddProvided(map[string]state.Provided{r.ID(): {Provider: r.Provided.Provider, Kind: r.Kind, Name: r.Name, Declared: true,
				SpecDigest: state.SpecDigest(r.Provided.Spec), ObservedAt: s.AsOf,
				Observed: protocol.Observed{ID: r.ID(), Kind: r.Kind, Name: r.Name, Exists: true, Owned: true, IDs: map[string]string{"vmid": "7001"},
					Node: &protocol.ObservedNode{ID: 7001, Running: true, UptimeSeconds: 60}}}})
		}
	}
	return s, nil
}

func (w machineWorld) Apply(ctx context.Context, step reconcile.Step) error {
	return w.f.nodes.Apply(ctx, step)
}

type flow struct {
	t     *testing.T
	ctx   context.Context
	now   time.Time
	st    *store.Store
	g     *graph.Graph
	node  *mnode
	nodes *node.Provider
	rec   *reconcile.Reconciler
}

func newFlow(t *testing.T, resources ...string) *flow {
	f := &flow{t: t, ctx: context.Background(), now: mt0}
	clock := func() time.Time { return f.now }
	st, err := store.Open(f.ctx, filepath.Join(t.TempDir(), "state.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.st = st
	f.declare(resources...)
	f.node = newNode(t)
	f.node.a.CP = storeCP{st: st, key: f.node.key, now: clock}
	f.node.a.Now = clock
	f.nodes = &node.Provider{Store: st, Clock: clock, Timeout: time.Minute, Sleep: func(ctx context.Context, d time.Duration) error {
		f.now = f.now.Add(d)
		f.node.a.Round(ctx)
		return nil
	}}
	f.rec = &reconcile.Reconciler{Store: st, Provider: machineWorld{f}, Owner: "test", Lease: time.Hour,
		Prune: func(ctx context.Context, g *graph.Graph) ([]string, error) { return node.Prune(ctx, st, g) }}
	return f
}

func (f *flow) declare(resources ...string) {
	f.t.Helper()
	g, err := decodeGraph(resources...)
	if err != nil {
		f.t.Fatal(err)
	}
	f.g = g
}

func (f *flow) publish() {
	f.t.Helper()
	if err := node.Publish(f.ctx, f.st, f.g, policy.Default, "", map[string]int64{"web": 7001}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *flow) revision() *noderev.Revision {
	f.t.Helper()
	r, err := f.st.NodeRevision(f.ctx, "web")
	if err != nil || r == nil {
		f.t.Fatalf("no revision: %v", err)
	}
	return r
}

func (f *flow) plan() (*plan.Plan, plan.Change) {
	f.t.Helper()
	f.publish()
	snap, err := f.rec.Provider.Observe(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	p := plan.Build(f.g, snap, policy.Default)
	if err := f.st.SavePlan(f.ctx, f.g, snap, policy.Default, p); err != nil {
		f.t.Fatal(err)
	}
	for _, c := range p.Changes {
		if c.Kind == KindFile {
			return p, c
		}
	}
	return p, plan.Change{}
}

func (f *flow) apply(p *plan.Plan) *reconcile.Report {
	f.t.Helper()
	if _, err := f.st.Approve(f.ctx, p.Digest, "test", time.Hour); err != nil {
		f.t.Fatal(err)
	}
	rep, err := f.rec.Apply(f.ctx, p.Digest)
	if err != nil {
		f.t.Fatal(err)
	}
	return rep
}

var flowFile = fileRes("web/file/etc/app/app.conf", `{"node":"web","path":"/etc/app/app.conf","content":"listen 8080\n","mode":"0640"}`)

func TestAFileFromDeclarationToNode(t *testing.T) {
	f := newFlow(t, flowFile)
	// The node's mamelet has not reported yet: its revision has nothing of linux.
	f.publish()
	if r := f.revision(); len(r.Ext) != 0 {
		t.Fatalf("a part before the mamelet said it takes it: %s", r.Content())
	}
	if _, c := f.plan(); c.Status != plan.StatusWaiting || c.Note.Code != codeFileAwait {
		t.Fatalf("before any report: %+v", c)
	}
	f.node.round() // it says it takes linux 1
	if _, c := f.plan(); c.Status != plan.StatusWaiting || c.Note.Code != codeFileAwait {
		t.Fatalf("before it observed the file: %+v", c)
	}
	if p := partOf(f.revision()); len(p.Files) != 1 || p.Files[0].Write != nil {
		t.Fatalf("the part %+v", p)
	}
	f.node.round() // it observes the file
	p, c := f.plan()
	if c.Status != plan.StatusChange || c.Verdict != policy.ApprovalRequired {
		t.Fatalf("the plan %+v", c)
	}
	rep := f.apply(p)
	if len(rep.Steps) != 1 || rep.Steps[0].Outcome != reconcile.OutcomeDone {
		t.Fatalf("apply %+v", rep)
	}
	if got := f.node.read("/etc/app/app.conf"); got != "listen 8080\n" {
		t.Fatalf("on the node: %q", got)
	}
	// The grant has its deadline in the store, never in what mamelet gets.
	for _, g := range f.revision().Grants() {
		if g.Action == ActionWrite && g.Expires.IsZero() {
			t.Error("a grant without a deadline")
		}
	}
	f.node.round()
	if _, c := f.plan(); c.Status != plan.StatusInSync {
		t.Fatalf("after the write: %+v", c)
	}

	// Taken out of the declaration: the plan says the file stays; applying it stops the watching,
	// and the file stays on the node.
	f.declare()
	p, _ = f.plan()
	kept := p.Changes[len(p.Changes)-1]
	if kept.ID() != "NodeFile web/file/etc/app/app.conf" || kept.Status != plan.StatusRetained || kept.Note.Code != codeFileKept {
		t.Fatalf("kept %+v", p.Changes)
	}
	f.apply(p)
	if r := f.revision(); len(r.Ext) != 0 {
		t.Errorf("still watched: %s", r.Content())
	}
	if got := f.node.read("/etc/app/app.conf"); got != "listen 8080\n" {
		t.Errorf("the file was touched: %q", got)
	}
}

// A mamelet older than the extension (it lists none) gets no part, its revision stays readable to
// it, the plan says to update it, and no grant is made for it.
func TestAMameletOlderThanTheExtensionGetsNothingOfIt(t *testing.T) {
	f := newFlow(t, flowFile)
	f.publish()
	old := noderev.Report{Node: "web", VMID: 7001, Version: "0.9.2", MameletSHA256: strings.Repeat("ab", 32),
		Packages: []noderev.PackageState{}, Services: []noderev.ServiceState{}, Results: []noderev.Result{}}
	if err := f.st.PutNodeReport(f.ctx, old); err != nil {
		t.Fatal(err)
	}
	p, c := f.plan()
	if c.Status != plan.StatusBlocked || c.Note.Code != "MAM-PLAN-0122" || !strings.Contains(c.Note.Message, "runs 0.9.2") {
		t.Fatalf("the plan %+v", c)
	}
	if r := f.revision(); len(r.Ext) != 0 || strings.Contains(string(r.Content()), `"ext"`) {
		t.Fatalf("a part for an old mamelet: %s", r.Content())
	}
	// Even an apply that reached it would grant nothing to that node.
	step := reconcile.Step{OperationID: "op-aaaaaaaaaaaaaaaaaaaaaaaa", PlanDigest: p.Digest, Op: OpWriteFile, Resource: f.g.Resources[1]}
	if _, err := f.st.Intend(f.ctx, step.OperationID, p.Digest, step.Resource.ID(), string(OpWriteFile)); err != nil {
		t.Fatal(err)
	}
	if err := f.nodes.Apply(f.ctx, step); err == nil || !strings.Contains(err.Error(), "MAM-NODE-0104") {
		t.Errorf("granted: %v", err)
	}

	// A part that was given before the node went back to an older mamelet (its update failed) is taken
	// out at the next publish.
	f.node.round()
	f.publish()
	if r := f.revision(); len(r.Ext) == 0 {
		t.Fatal("no part for a mamelet that takes it")
	}
	if err := f.st.PutNodeReport(f.ctx, old); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if r := f.revision(); len(r.Ext) != 0 {
		t.Errorf("the part stays for a mamelet that went back: %s", r.Content())
	}
}

// The way to the extension on a node whose mamelet is older (docs/mamelet-update.md): the declaration
// updates mamelet and declares a file in one go. The file waits for the update (it is not blocked), so
// the plan has nothing blocked and mamelacd's sync = "push" approves it; once the new mamelet reports,
// the file is planned.
func TestAFileWaitsForTheMameletUpdateOfItsDeclaration(t *testing.T) {
	const old, next = "1111111111111111111111111111111111111111111111111111111111111111", "2222222222222222222222222222222222222222222222222222222222222222"
	update := `{"kind":"NodeMamelet","name":"web/mamelet","spec":{"node":"web","version":"0.10.0","sha256":"` + next + `"}}`
	f := newFlow(t, update, flowFile)
	f.publish()
	if err := f.st.PutNodeReport(f.ctx, noderev.Report{Node: "web", VMID: 7001, Version: "0.9.2", MameletSHA256: old,
		Packages: []noderev.PackageState{}, Services: []noderev.ServiceState{}, Results: []noderev.Result{}}); err != nil {
		t.Fatal(err)
	}
	p, c := f.plan()
	if c.Status != plan.StatusWaiting || c.Note.Code != "MAM-PLAN-0123" || !strings.Contains(c.Note.Message, "0.10.0") {
		t.Fatalf("the file: %+v", c)
	}
	blocked := 0
	for _, ch := range p.Changes {
		if ch.Status == plan.StatusBlocked {
			blocked++
		}
		if ch.Kind == graph.KindNodeMamelet && ch.Status != plan.StatusChange {
			t.Errorf("the update: %+v", ch)
		}
	}
	if blocked != 0 {
		t.Errorf("%d blocked: %+v", blocked, p.Changes)
	}
	// Without the update in the declaration, nothing brings the extension: blocked.
	f.declare(flowFile)
	if _, c := f.plan(); c.Status != plan.StatusBlocked || c.Note.Code != "MAM-PLAN-0122" {
		t.Errorf("without the update: %+v", c)
	}
	// The update is done (the new mamelet reports): the file is planned.
	f.declare(update, flowFile)
	f.node.a.Version = "0.10.0"
	f.node.round() // it says it takes linux
	f.publish()
	f.node.round() // it observes the file
	if _, c := f.plan(); c.Status != plan.StatusChange {
		t.Errorf("after the update: %+v", c)
	}
}

// A protected node (the Control Plane's host) gets no write of linux, even one granted before.
func TestAProtectedNodeGetsNoWrite(t *testing.T) {
	f := newFlow(t, flowFile)
	f.publish()
	f.node.round()
	f.publish()
	if _, err := f.st.PublishNodeRevision(f.ctx, "web", func(prev *noderev.Revision) (noderev.Revision, error) {
		r := *prev
		p := partOf(&r)
		p.Files[0].Write = &Write{Content: "x", SHA256: ContentSHA256("x"), Mode: "0644", Owner: "root", Group: "root",
			Grant: noderev.Once{Operation: opA, Attempt: 1, Action: ActionWrite, Expires: f.now.Add(time.Hour)}}
		setPart(&r, p)
		return r, nil
	}); err != nil {
		t.Fatal(err)
	}
	protected := policy.Default
	protected.ProtectedNodes = []string{"web"}
	if err := node.Publish(f.ctx, f.st, f.g, protected, "", map[string]int64{"web": 7001}); err != nil {
		t.Fatal(err)
	}
	if p := partOf(f.revision()); len(p.Files) != 1 || p.Files[0].Write != nil {
		t.Errorf("the part %+v", p)
	}
}
