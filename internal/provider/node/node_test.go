package node_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/reconcile"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

const desiredJSON = `{"schema":"mamela-graph/v2","resources":[
{"kind":"TestVM","name":"app","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}},
{"kind":"NodePackage","name":"app/nginx","spec":{"node":"app","package":"nginx","state":"Present"}},
{"kind":"NodeService","name":"app/nginx","spec":{"node":"app","service":"nginx","enabled":true,"running":true}}]}`

const sim = `Inst nginx-common (1.24.0-2ubuntu7.19 Ubuntu:24.04/noble-updates [all])
Inst nginx (1.24.0-2ubuntu7.19 Ubuntu:24.04/noble-updates [amd64])
`

// tinyOS is the inside of the VM: dpkg-query, apt-get and systemctl over a little state.
type tinyOS struct {
	sim                       string
	installed                 bool
	exists, enabled, running  bool
	installs, enables, starts int
	restarts                  int
	kubectl                   string   // what `k3s kubectl get nodes` prints
	kubectlCalls              []string // the changing kubectl commands mamelet ran
	beforeInstall             func()
}

func (o *tinyOS) Run(_ context.Context, program string, args []string, _ []string) ([]byte, []byte, int, error) {
	switch {
	case strings.HasSuffix(program, "dpkg-query"):
		if o.installed {
			return []byte("installed\t1.24.0-2ubuntu7.19\n"), nil, 0, nil
		}
		return nil, []byte("dpkg-query: no packages found matching nginx"), 1, nil
	case strings.HasSuffix(program, "apt-get") && slices.Contains(args, "-s"):
		return []byte(o.sim), nil, 0, nil
	case strings.HasSuffix(program, "apt-get"):
		o.installs++
		o.installed, o.exists = true, true // installed, but (here) neither enabled nor started
		return nil, nil, 0, nil
	case args[0] == "show":
		f := func(b bool, y, n string) string {
			if b {
				return y
			}
			return n
		}
		return []byte(fmt.Sprintf("LoadState=%s\nUnitFileState=%s\nActiveState=%s\n", f(o.exists, "loaded", "not-found"),
			f(o.enabled, "enabled", "disabled"), f(o.running, "active", "inactive"))), nil, 0, nil
	case args[0] == "enable":
		o.enables++
		o.enabled = true
	case args[0] == "start":
		o.starts++
		o.running = true
	}
	return nil, nil, 0, nil
}

// storeCP is the Control Plane as mamelet sees it, without TLS: revisions signed from the store,
// reports into the store.
type storeCP struct {
	st  *store.Store
	key ed25519.PrivateKey
	now func() time.Time
	// binaries is what the Control Plane serves as mamelet, by sha256.
	binaries map[string][]byte
}

func (c storeCP) Mamelet(_ context.Context, sha string) ([]byte, error) {
	if b, ok := c.binaries[sha]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("the Control Plane does not serve a mamelet with this sha256")
}

func (c storeCP) Revision(ctx context.Context) (noderev.Signed, error) {
	r, err := c.st.NodeRevision(ctx, "app")
	if err != nil || r == nil {
		return noderev.Signed{}, fmt.Errorf("no revision: %v", err)
	}
	r.IssuedAt, r.ExpiresAt = c.now(), c.now().Add(time.Hour)
	return noderev.Sign(c.key, r.ForMamelet(c.now())) // as the Control Plane hands it out
}

func (c storeCP) Report(ctx context.Context, r noderev.Report) error {
	return c.st.PutNodeReport(ctx, r)
}

// machineWorld is mamelet's world on machine app, which its provider reports existing, Mamela's
// and running (number 7001).
type machineWorld struct {
	e     *env
	nodes *node.Provider
}

func (w machineWorld) Observe(ctx context.Context) (state.Snapshot, error) {
	s, err := w.nodes.Observe(ctx)
	if err != nil {
		return s, err
	}
	for _, r := range w.e.g.Resources {
		if r.Provided == nil {
			continue
		}
		s.AddProvided(map[string]state.Provided{r.ID(): {Provider: r.Provided.Provider, Kind: r.Kind, Name: r.Name, Declared: true,
			SpecDigest: state.SpecDigest(r.Provided.Spec), ObservedAt: s.AsOf,
			Observed: protocol.Observed{ID: r.ID(), Kind: r.Kind, Name: r.Name, Exists: true, Owned: true, IDs: map[string]string{"vmid": "7001"},
				Node: &protocol.ObservedNode{ID: 7001, Running: true, UptimeSeconds: 60}}}})
	}
	return s, nil
}

func (w machineWorld) Apply(ctx context.Context, step reconcile.Step) error {
	if step.Resource.Provided != nil {
		return fmt.Errorf("no machine changes here")
	}
	return w.nodes.Apply(ctx, step)
}

type env struct {
	t     *testing.T
	ctx   context.Context
	now   time.Time
	st    *store.Store
	g     *graph.Graph
	pol   policy.Policy
	os    *tinyOS
	agent *mamelet.Agent
	rec   *reconcile.Reconciler
	nodes *node.Provider
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, ctx: context.Background(), now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), pol: policy.Default,
		os: &tinyOS{sim: sim}}
	clock := func() time.Time { return e.now }
	st, err := store.Open(e.ctx, filepath.Join(t.TempDir(), "state.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	if e.g, err = graph.Decode([]byte(desiredJSON)); err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	e.agent = &mamelet.Agent{
		Config: mamelet.Config{Node: "app", VMID: 7001, RevisionKey: key.Public().(ed25519.PublicKey), AllowedOrigins: []string{"Ubuntu:"}},
		CP:     storeCP{st: st, key: key, now: clock}, System: &mamelet.System{Runner: e.os, AllowedOrigins: []string{"Ubuntu:"}},
		State: mamelet.FileState{Path: filepath.Join(t.TempDir(), "state.json")}, Now: clock,
	}
	// While the provider waits for a result, time passes and mamelet runs its rounds.
	e.nodes = &node.Provider{Store: st, Clock: clock, Timeout: time.Minute, Sleep: func(ctx context.Context, d time.Duration) error {
		e.now = e.now.Add(d)
		e.agent.Round(ctx)
		return nil
	}}
	e.rec = &reconcile.Reconciler{Store: st, Provider: machineWorld{e: e, nodes: e.nodes}, Owner: "test", Lease: time.Hour,
		Prune: func(ctx context.Context, declaration *graph.Graph) ([]string, error) {
			return node.Prune(ctx, st, declaration)
		}} // as cmd/mamela applies a plan
	return e
}

func (e *env) publish() {
	e.t.Helper()
	if err := node.Publish(e.ctx, e.st, e.g, e.pol, "", map[string]int64{"app": 7001}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) round() noderev.Report {
	e.t.Helper()
	rep, err := e.agent.Round(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return rep
}

func (e *env) plan() *plan.Plan {
	e.t.Helper()
	snap, err := e.rec.Provider.Observe(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	p := plan.Build(e.g, snap, e.pol)
	if err := e.st.SavePlan(e.ctx, e.g, snap, e.pol, p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) applyApproved(p *plan.Plan) *reconcile.Report {
	e.t.Helper()
	if _, err := e.st.Approve(e.ctx, p.Digest, "test", time.Hour); err != nil {
		e.t.Fatal(err)
	}
	rep, err := e.rec.Apply(e.ctx, p.Digest)
	if err != nil {
		e.t.Fatal(err)
	}
	return rep
}

// granted checks that the node's revision holds a one-time grant of the action, and that the grant
// has the deadline the Control Plane gave it (a grant without one is never handed out).
func (e *env) granted(action string) {
	e.t.Helper()
	rev, err := e.st.NodeRevision(e.ctx, "app")
	if err != nil || rev == nil {
		e.t.Fatalf("no revision: %v", err)
	}
	for _, g := range rev.Grants() {
		if g.Action != action {
			continue
		}
		if g.Expires.IsZero() || !g.Expires.After(e.now.Add(-time.Hour)) || g.Expires.After(e.now.Add(time.Hour+node.GrantGrace)) {
			e.t.Errorf("the %s grant's deadline %s is not near now (%s) and within the approval", action, g.Expires, e.now)
		}
		return
	}
	e.t.Fatalf("no %s grant in %s", action, rev.Content())
}

func change(t *testing.T, p *plan.Plan, id string) plan.Change {
	for _, c := range p.Changes {
		if c.ID() == id {
			return c
		}
	}
	t.Fatalf("no change %s", id)
	return plan.Change{}
}

func TestInstallEnableStartThroughMamelet(t *testing.T) {
	e := newEnv(t)
	e.publish()
	e.round() // mamelet reports what installing nginx would change

	p := e.plan()
	pkg := change(t, p, "NodePackage app/nginx")
	if pkg.Status != plan.StatusChange || pkg.Verdict != policy.ApprovalRequired || len(pkg.Diffs) != 4 {
		t.Fatalf("package: %+v", pkg)
	}
	if svc := change(t, p, "NodeService app/nginx"); svc.Status != plan.StatusWaiting {
		t.Fatalf("service: %+v", svc)
	}
	if e.os.installs != 0 {
		t.Fatal("installed before approval")
	}
	if rep := e.applyApproved(p); rep.Stopped != nil || rep.Steps[0].Outcome != reconcile.OutcomeDone || e.os.installs != 1 {
		t.Fatalf("install: %+v (installs %d)", rep, e.os.installs)
	}
	e.granted(noderev.ActionInstall)

	p = e.plan()
	if svc := change(t, p, "NodeService app/nginx"); svc.Verdict != policy.ApprovalRequired || len(svc.Operations) != 2 {
		t.Fatalf("service: %+v", svc)
	}
	if rep := e.applyApproved(p); rep.Stopped != nil || !e.os.enabled || !e.os.running {
		t.Fatalf("service: %+v", rep)
	}
	e.granted(noderev.ActionEnable)
	e.granted(noderev.ActionStart)
	for _, c := range e.plan().Changes {
		if c.Status != plan.StatusInSync {
			t.Errorf("not converged: %s %s", c.ID(), c.Status)
		}
	}

	// The one-time start does not become a standing permission: a manual stop is drift that
	// needs a new approval...
	e.os.running = false
	e.round()
	if e.os.running || e.os.starts != 1 {
		t.Fatal("restarted without permission")
	}
	// A policy change is a candidate until apply activates it.
	e.pol.AutoStartServices = []string{"nginx"}
	e.publish()
	e.round()
	if e.os.running || e.os.starts != 1 {
		t.Fatal("plan activated a new repair permission")
	}
	if rep := e.applyApproved(e.plan()); rep.Stopped != nil {
		t.Fatalf("activate repairs: %+v", rep)
	}
	e.os.running = false
	e.round()
	if !e.os.running || e.os.starts != 3 {
		t.Fatal("did not repair with autoStartServices")
	}
}

func TestPlanPreservesActiveServiceAndPendingGrants(t *testing.T) {
	e := newEnv(t)
	e.publish()
	_, err := e.st.PublishNodeRevision(e.ctx, "app", func(prev *noderev.Revision) (noderev.Revision, error) {
		r := *prev
		r.Services = slices.Clone(prev.Services)
		r.Services[0].Running = false
		r.Services[0].Once = []noderev.Once{{Operation: "op-000000000000000000000000", Attempt: 1, Action: noderev.ActionStop}}
		r.ControlPlane = "https://active.example:8443"
		return r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.st.NodeRevision(e.ctx, "app")
	e.pol.AutoStartServices = []string{"nginx"}
	e.publish()
	after, _ := e.st.NodeRevision(e.ctx, "app")
	if string(before.Content()) != string(after.Content()) {
		t.Fatal("planning changed the active desired state or its grants")
	}
	// Removing a declaration from a candidate must not revoke active permissions either: a plan is
	// made of declarations nobody approved (a pull request), and closing it would not bring them back.
	e.g, err = graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[{"kind":"TestVM","name":"app","provider":"test","spec":{"cpu":1}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e.publish()
	after, _ = e.st.NodeRevision(e.ctx, "app")
	if string(before.Content()) != string(after.Content()) {
		t.Fatal("candidate removal changed active revision")
	}
}

// What the declaration no longer has loses its permissions, and is no longer watched, when a plan
// made from that declaration is approved and applied (audit B16: the standing repair of a service
// that was removed from the declaration stayed for ever). Planning it, which anybody can do with a
// candidate, takes nothing away; everything else of the revision stays; the machine is not touched.
func TestWhatTheDeclarationDropsLosesItsPermissionsWhenItsPlanIsApplied(t *testing.T) {
	e := newEnv(t)
	e.os.installed, e.os.exists, e.os.enabled, e.os.running = true, true, true, true
	e.pol.AutoStartServices = []string{"nginx"}
	e.publish()
	e.round()
	e.applyApproved(e.plan()) // the standing repair is activated by apply
	soon := e.now.Add(time.Minute)
	_, err := e.st.PublishNodeRevision(e.ctx, "app", func(prev *noderev.Revision) (noderev.Revision, error) {
		r := *prev
		r.Packages = slices.Clone(prev.Packages)
		r.Packages[0].Install = &noderev.Install{Operation: "op-000000000000000000000001", Attempt: 1, ChangeSet: "sha256:" + strings.Repeat("0", 64), Expires: soon}
		r.Services = slices.Clone(prev.Services)
		r.Services[0].Once = []noderev.Once{{Operation: "op-000000000000000000000002", Attempt: 1, Action: noderev.ActionStop, Expires: soon}}
		r.Manifests = []noderev.Manifest{{Name: "argocd", Content: "kind: Namespace\n"}}
		r.ControlPlane = "https://active.example:8443"
		return r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.st.NodeRevision(e.ctx, "app")
	if len(before.Services) != 1 || !slices.Contains(before.Services[0].Repair, noderev.ActionStart) || len(before.Grants()) != 2 {
		t.Fatalf("setup: %s", before.Content())
	}

	gone, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[{"kind":"TestVM","name":"app","provider":"test","spec":{"cpu":1}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e.g = gone
	// Planning it, as a candidate, takes nothing away.
	e.publish()
	p := e.plan()
	if rev, _ := e.st.NodeRevision(e.ctx, "app"); string(rev.Content()) != string(before.Content()) {
		t.Fatalf("planning a declaration took something away: %s", rev.Content())
	}
	// Neither does applying a plan nobody approved.
	if _, err := e.rec.Apply(e.ctx, p.Digest); err != nil {
		t.Fatal(err)
	}
	if rev, _ := e.st.NodeRevision(e.ctx, "app"); string(rev.Content()) != string(before.Content()) {
		t.Fatalf("a plan nobody approved took something away: %s", rev.Content())
	}
	// Applying the approved plan does.
	if rep := e.applyApproved(p); rep.Stopped != nil {
		t.Fatalf("apply: %+v", rep)
	}
	rev, _ := e.st.NodeRevision(e.ctx, "app")
	if len(rev.Packages) != 0 || len(rev.Services) != 0 || len(rev.Manifests) != 0 || rev.K3s != nil || rev.Mamelet != nil || len(rev.Grants()) != 0 {
		t.Fatalf("the revision still has what the declaration dropped: %s", rev.Content())
	}
	if rev.ControlPlane != "https://active.example:8443" {
		t.Fatalf("the Control Plane's address is not the declaration's to take away: %+v", rev)
	}
	if !slices.Contains(eventKinds(e, p.Digest), "pruned") {
		t.Fatal("what was taken away is not in the event log")
	}
	// Nothing was done to the machine: it keeps running, and mamelet does nothing about it.
	e.os.running = false
	e.round()
	if e.os.running || e.os.starts != 0 || e.os.installs != 0 || !e.os.installed || !e.os.enabled {
		t.Fatalf("mamelet acted on what is no longer declared (starts %d, installs %d)", e.os.starts, e.os.installs)
	}
}

// eventKinds are the kinds of the events the store has logged for a plan.
func eventKinds(e *env, digest string) []string {
	events, err := e.st.Events(e.ctx, digest)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, ev := range events {
		out = append(out, ev.Kind)
	}
	return out
}

func TestRepairPolicyChangeIsPlannedEvenWithoutServiceDrift(t *testing.T) {
	e := newEnv(t)
	e.os.installed, e.os.exists, e.os.enabled, e.os.running = true, true, true, true
	e.pol.AutoStartServices = []string{"nginx"}
	e.publish()
	e.round()
	p := e.plan()
	if c := change(t, p, "NodeService app/nginx"); c.Status != plan.StatusChange {
		t.Fatalf("repair permission missing from plan: %+v", c)
	}
	e.applyApproved(p)
	e.os.running = false
	e.round()
	if !e.os.running {
		t.Fatal("apply did not activate repair permission")
	}
	e.pol.AutoStartServices = nil
	e.publish()
	e.applyApproved(e.plan())
	e.os.running = false
	e.round()
	if e.os.running {
		t.Fatal("apply did not revoke repair permission")
	}
}

// The approval covers the change set as planned. If apt would now do something else, mamelet
// refuses, even though the operation itself was approved.
func TestChangeSetThatChangesAfterApprovalIsRefused(t *testing.T) {
	e := newEnv(t)
	e.publish()
	e.round()
	p := e.plan()
	e.os.sim = strings.ReplaceAll(sim, "7.19", "7.20") // a new version arrives before mamelet installs
	rep := e.applyApproved(p)
	if rep.Stopped == nil || rep.Steps[0].Outcome != reconcile.OutcomeFailed || !strings.Contains(rep.Steps[0].Detail, "refused") ||
		e.os.installs != 0 {
		t.Fatalf("report %+v, installs %d", rep, e.os.installs)
	}
	// Planning again shows the new set, which needs its own approval.
	p2 := e.plan()
	if got := change(t, p2, "NodePackage app/nginx"); !strings.Contains(got.Diffs[2].To, "7.20") {
		t.Fatalf("new plan: %+v", got)
	}
	if rep := e.applyApproved(p2); rep.Stopped != nil || e.os.installs != 1 {
		t.Fatalf("second approval: %+v", rep)
	}
}

// If mamelet never answers (not running, tunnel down), the outcome is unknown, not success.
func TestSilentMameletIsAnUnknownOutcome(t *testing.T) {
	e := newEnv(t)
	e.publish()
	e.round()
	p := e.plan()
	e.nodes.Sleep = func(_ context.Context, d time.Duration) error { e.now = e.now.Add(d); return nil }
	rep := e.applyApproved(p)
	if rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0003" {
		t.Fatalf("report %+v", rep)
	}
	// mamelet comes back soon, picks up the grant (it stays good for a while after the apply gave up
	// waiting, and never beyond the approval) and installs; the next apply confirms by observation.
	e.round()
	if e.os.installs != 1 {
		t.Fatalf("installs %d", e.os.installs)
	}
}

// An approval that has ended gives nothing, even to an apply that began while it was good: a long
// apply (an image import, hours) may reach a node's operation after it.
func TestNoGrantFromAnApprovalThatHasEnded(t *testing.T) {
	e := newEnv(t)
	e.publish()
	e.round()
	p := e.plan()
	if _, err := e.st.Approve(e.ctx, p.Digest, "test", time.Minute); err != nil {
		t.Fatal(err)
	}
	c := change(t, p, "NodePackage app/nginx")
	step := reconcile.Step{OperationID: reconcile.OperationID(p.Digest, c.ID(), policy.OpInstallPackage), PlanDigest: p.Digest,
		Op: policy.OpInstallPackage, Resource: e.g.Resources[slices.IndexFunc(e.g.Resources, func(r graph.Resource) bool { return r.NodePackage != nil })], Approved: c}
	if _, err := e.st.Intend(e.ctx, step.OperationID, p.Digest, c.ID(), string(step.Op)); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.st.Claim(e.ctx, step.OperationID, "test", time.Hour); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	e.now = e.now.Add(2 * time.Minute)
	err := e.nodes.Apply(e.ctx, step)
	if err == nil || !strings.Contains(err.Error(), "MAM-NODE-0120") {
		t.Fatalf("an approval that ended gave a grant: %v", err)
	}
	if rev, _ := e.st.NodeRevision(e.ctx, "app"); len(rev.Grants()) != 0 {
		t.Fatalf("a grant is in the revision: %s", rev.Content())
	}
}

// A one-time grant belongs to the apply that asked for it. When mamelet never answers, the grant
// does not wait for it for ever: after the approval has ended, a mamelet that comes back does nothing
// (audit A2: it used to install, 30 days later, what nobody had approved for that long).
func TestGrantEndsWithItsApproval(t *testing.T) {
	e := newEnv(t)
	e.publish()
	e.round()
	p := e.plan()
	e.nodes.Sleep = func(_ context.Context, d time.Duration) error { e.now = e.now.Add(d); return nil }
	if rep := e.applyApproved(p); rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0003" {
		t.Fatalf("report %+v", rep)
	}
	e.now = e.now.Add(2 * time.Hour) // the approval was for an hour
	e.round()
	if e.os.installs != 0 {
		t.Fatalf("a grant ran %s after its approval ended (installs %d)", "2h", e.os.installs)
	}
}

// A grant ends sooner than its approval when the apply that asked for it has long stopped waiting:
// the approval is up to a day, the wait is minutes.
func TestGrantEndsShortlyAfterTheApplyStoppedWaiting(t *testing.T) {
	e := newEnv(t)
	e.publish()
	e.round()
	p := e.plan()
	e.nodes.Sleep = func(_ context.Context, d time.Duration) error { e.now = e.now.Add(d); return nil }
	if _, err := e.st.Approve(e.ctx, p.Digest, "test", 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if rep, err := e.rec.Apply(e.ctx, p.Digest); err != nil || rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0003" {
		t.Fatalf("report %+v %v", rep, err)
	}
	e.now = e.now.Add(3 * time.Hour)
	e.round()
	if e.os.installs != 0 {
		t.Fatalf("a grant ran 3h after the apply gave up (installs %d)", e.os.installs)
	}
}

// What the declaration no longer has is no longer granted: the audit's reproduction (A2) approved an
// installation of nginx, timed out, dropped nginx from the declaration, and 30 days later mamelet
// installed it. Dropping it from a candidate does not take the grant away (a plan nobody approved
// changes nothing); applying the approved plan of the declaration that dropped it does, and so does
// the grant's own deadline, whatever happens to the declaration.
func TestGrantDoesNotOutliveItsDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name string
		wait time.Duration
	}{{"the plan of the declaration is applied", 0}, {"30 days later", 30 * 24 * time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.publish()
			e.round()
			p := e.plan()
			e.nodes.Sleep = func(_ context.Context, d time.Duration) error { e.now = e.now.Add(d); return nil }
			if rep := e.applyApproved(p); rep.Stopped == nil || rep.Stopped.Code != "MAM-REC-0003" {
				t.Fatalf("report %+v", rep)
			}
			g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[{"kind":"TestVM","name":"app","provider":"test","spec":{"cpu":1}}]}`))
			if err != nil {
				t.Fatal(err)
			}
			e.g = g
			e.publish() // as a candidate
			if rev, _ := e.st.NodeRevision(e.ctx, "app"); len(rev.Grants()) != 1 {
				t.Fatalf("a candidate took the grant away: %s", rev.Content())
			}
			if tc.wait == 0 {
				if rep := e.applyApproved(e.plan()); rep.Stopped != nil {
					t.Fatalf("apply: %+v", rep)
				}
			}
			e.now = e.now.Add(tc.wait)
			e.round()
			if e.os.installs != 0 {
				t.Fatalf("installed %d times after nginx left the declaration", e.os.installs)
			}
		})
	}
}

// The Control Plane's own host is never changed (ADR 0003): publishing takes every grant away from
// it, of every kind, even of what the declaration has (audit B17: the removal of a Kubernetes node
// was left).
func TestAProtectedNodeHasNoGrantOfAnyKind(t *testing.T) {
	e := newEnv(t)
	sha := strings.Repeat("a", 64)
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
		`{"kind":"TestVM","name":"app","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}},` +
		`{"kind":"NodePackage","name":"app/nginx","spec":{"node":"app","package":"nginx","state":"Present"}},` +
		`{"kind":"NodeService","name":"app/nginx","spec":{"node":"app","service":"nginx","enabled":true,"running":true}},` +
		`{"kind":"NodeK3s","name":"app/k3s","spec":{"node":"app","role":"server","sha256":"` + sha + `","version":"v1.37.2+k3s1"}},` +
		`{"kind":"NodeK3sManifest","name":"app/manifest/argocd","spec":{"node":"app","name":"argocd","content":"kind: Namespace\n"}},` +
		`{"kind":"NodeMamelet","name":"app/mamelet","spec":{"node":"app","version":"0.9.1","sha256":"` + sha + `"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e.g = g
	e.publish()
	op := func(n string) string { return "op-0123456789abcdef012345" + n }
	soon := e.now.Add(time.Minute)
	_, err = e.st.PublishNodeRevision(e.ctx, "app", func(prev *noderev.Revision) (noderev.Revision, error) {
		r := *prev
		r.Packages = slices.Clone(prev.Packages)
		r.Packages[0].Install = &noderev.Install{Operation: op("00"), Attempt: 1, ChangeSet: "sha256:" + strings.Repeat("0", 64), Expires: soon}
		r.Services = slices.Clone(prev.Services)
		r.Services[0].Repair = []string{noderev.ActionStart}
		r.Services[0].Once = []noderev.Once{{Operation: op("01"), Attempt: 1, Action: noderev.ActionStop, Expires: soon}}
		r.Manifests = slices.Clone(prev.Manifests)
		r.Manifests[0].Write = &noderev.Once{Operation: op("02"), Attempt: 1, Action: noderev.ActionWrite, Expires: soon}
		r.K3s = &noderev.K3s{Version: "v1.37.2+k3s1", SHA256: sha, Role: noderev.K3sServer, Token: sha,
			Install: &noderev.Once{Operation: op("03"), Attempt: 1, Action: noderev.ActionInstall, Expires: soon},
			Remove:  &noderev.NodeRemoval{Node: "gone", Once: noderev.Once{Operation: op("04"), Attempt: 1, Action: noderev.ActionRemove, Expires: soon}}}
		r.Mamelet = &noderev.MameletUpdate{SHA256: sha, Once: noderev.Once{Operation: op("05"), Attempt: 1, Action: noderev.ActionUpdate, Expires: soon}}
		return r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rev, _ := e.st.NodeRevision(e.ctx, "app"); len(rev.Grants()) != 6 {
		t.Fatalf("setup: %d grants, want one of each kind: %s", len(rev.Grants()), rev.Content())
	}
	e.pol.ProtectedNodes = []string{"app"}
	e.publish()
	rev, _ := e.st.NodeRevision(e.ctx, "app")
	if len(rev.Grants()) != 0 || (len(rev.Services) == 1 && len(rev.Services[0].Repair) != 0) {
		t.Fatalf("a protected node still has permissions: %s", rev.Content())
	}
	if len(rev.Packages) != 1 || len(rev.Services) != 1 || rev.K3s == nil || len(rev.Manifests) != 1 {
		t.Fatalf("it is still watched: %s", rev.Content())
	}
}

// A mamelet that refuses the Control Plane's revision is named in the plan, with why and what to do
// (audit A6: the reason was in the report, and nothing read it, so a node that could not be given a
// permission looked like one that was merely slow). The change cannot be carried out, so it does not
// wait for an approval to time out.
func TestAMameletThatRefusesTheRevisionIsNamedInThePlan(t *testing.T) {
	e := newEnv(t)
	e.publish()
	e.round()
	if c := change(t, e.plan(), "NodePackage app/nginx"); c.Status != plan.StatusChange {
		t.Fatalf("before: %+v", c)
	}
	// The node's clock is behind: the revision the Control Plane offers was issued in its future.
	e.now = e.now.Add(20 * time.Minute)
	e.agent.Now = func() time.Time { return e.now.Add(-10 * time.Minute) }
	if rep := e.round(); !strings.Contains(rep.Refused, "issued in the future") {
		t.Fatalf("the node did not refuse: %q", rep.Refused)
	}
	p := e.plan()
	c := change(t, p, "NodePackage app/nginx")
	if c.Status != plan.StatusBlocked || c.Note == nil || c.Note.Code != "MAM-PLAN-0130" ||
		!strings.Contains(c.Note.Message, "app") || !strings.Contains(c.Note.Message, "revision 1") || !strings.Contains(c.Note.Message, "issued in the future") ||
		!strings.Contains(c.Note.Hint, "clock") {
		t.Fatalf("the plan does not say why: %+v", c)
	}
	// Nothing is approved to wait on: no grant is given, and nothing is installed.
	if rep := e.applyApproved(p); rep.Stopped != nil || e.os.installs != 0 {
		t.Fatalf("apply: %+v (installs %d)", rep, e.os.installs)
	}
	if rev, _ := e.st.NodeRevision(e.ctx, "app"); len(rev.Grants()) != 0 {
		t.Fatalf("a grant was given to a node that refuses: %s", rev.Content())
	}
	// Once the clock is right the node accepts, and the plan is as before.
	e.agent.Now = func() time.Time { return e.now }
	if rep := e.round(); rep.Refused != "" {
		t.Fatalf("still refusing: %q", rep.Refused)
	}
	if c := change(t, e.plan(), "NodePackage app/nginx"); c.Status != plan.StatusChange || c.Note != nil {
		t.Fatalf("after: %+v", c)
	}
}

// A node that has accepted no revision at all, because it refuses each, is not "silent": the plan says
// it refuses, in the place where it would otherwise wait for a report that does not come.
func TestANodeThatRefusesEveryRevisionIsNotWaitedFor(t *testing.T) {
	e := newEnv(t)
	e.publish()
	st, _ := e.agent.State.Load()
	st.LastSerial = 99 // it accepted a higher serial than the Control Plane has (a database restored from an older copy)
	if err := e.agent.State.Save(st); err != nil {
		t.Fatal(err)
	}
	if rep := e.round(); !strings.Contains(rep.Refused, "older than one already accepted") {
		t.Fatalf("the node did not refuse: %q", rep.Refused)
	}
	c := change(t, e.plan(), "NodePackage app/nginx")
	if c.Status != plan.StatusBlocked || c.Note == nil || c.Note.Code != "MAM-PLAN-0130" || !strings.Contains(c.Note.Message, "serial 1 < 99") ||
		!strings.Contains(c.Note.Hint, "database") {
		t.Fatalf("the plan waits for a report: %+v", c)
	}
}

func TestOldApprovalCannotGrantToReenrolledNode(t *testing.T) {
	e := newEnv(t)
	enroll := func(serial string) {
		token, err := e.st.IssueEnrollToken(e.ctx, "app", 7001, "seed", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.st.Enroll(e.ctx, token, "app", 7001, serial); err != nil {
			t.Fatal(err)
		}
	}
	enroll("original")
	e.publish()
	e.round()
	p := e.plan()
	enroll("replacement") // same name and VMID, another machine identity
	if rep := e.applyApproved(p); rep.Stopped == nil || e.os.installs != 0 {
		t.Fatalf("old approval reached new agent: %+v", rep)
	}
}

// A provider's machine gets a revision with what is declared on it, by its observed ID, though its
// kind sorts after the node resources.
func TestPublishOnAProvidersMachine(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"), func() time.Time { return time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
		`{"kind":"ProxmoxVM","name":"ext","provider":"proxmox","spec":{"cpu":1}},` +
		`{"kind":"NodePackage","name":"ext/nginx","spec":{"node":"ext","package":"nginx","state":"Present"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Publish(ctx, st, g, policy.Default, "", map[string]int64{"ext": 7007}); err != nil {
		t.Fatal(err)
	}
	rev, err := st.NodeRevision(ctx, "ext")
	if err != nil || rev == nil || rev.VMID != 7007 || len(rev.Packages) != 1 || rev.Packages[0].Name != "nginx" {
		t.Fatalf("revision %+v %v", rev, err)
	}
}

// Publishing observation targets during plan must not bypass the denial on the Control Plane's
// own host, even when the service policy otherwise allows automatic repairs.
func TestPublishProtectedNodeCannotRepair(t *testing.T) {
	e := newEnv(t)
	e.os.installed, e.os.exists, e.os.enabled = true, true, true
	e.pol.AutoStartServices = []string{"nginx"}
	e.pol.ProtectedNodes = []string{"app"}
	e.publish()
	e.round()
	if e.os.starts != 0 || e.os.running {
		t.Fatal("publishing a plan started a service on a protected node")
	}
	if got := change(t, e.plan(), "NodeService app/nginx"); got.Verdict != policy.Denied {
		t.Fatalf("the protected service should remain denied: %+v", got)
	}
}

// Moving a Control Plane onto a previously managed node must revoke pending grants too.
func TestPublishProtectedNodeRevokesExistingGrants(t *testing.T) {
	e := newEnv(t)
	e.pol.AutoStartServices = []string{"nginx"}
	e.publish()
	_, err := e.st.PublishNodeRevision(e.ctx, "app", func(prev *noderev.Revision) (noderev.Revision, error) {
		r := *prev
		r.Packages = slices.Clone(prev.Packages)
		r.Services = slices.Clone(prev.Services)
		r.Packages[0].Install = &noderev.Install{Operation: "pending-install", Attempt: 1, ChangeSet: "pending"}
		r.Services[0].Once = []noderev.Once{{Operation: "pending-start", Attempt: 1, Action: noderev.ActionStart}}
		return r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.pol.ProtectedNodes = []string{"app"}
	e.publish()
	r, err := e.st.NodeRevision(e.ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if r.Packages[0].Install != nil || len(r.Services[0].Once) != 0 || len(r.Services[0].Repair) != 0 {
		t.Fatalf("protected node kept execution grants: %+v %+v", r.Packages, r.Services)
	}
}
