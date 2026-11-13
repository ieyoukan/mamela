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
}

func (c storeCP) Revision(ctx context.Context) (noderev.Signed, error) {
	r, err := c.st.NodeRevision(ctx, "app")
	if err != nil || r == nil {
		return noderev.Signed{}, fmt.Errorf("no revision: %v", err)
	}
	r.IssuedAt, r.ExpiresAt = c.now(), c.now().Add(time.Hour)
	return noderev.Sign(c.key, *r)
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
	e.rec = &reconcile.Reconciler{Store: st, Provider: machineWorld{e: e, nodes: e.nodes}, Owner: "test", Lease: time.Hour}
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

	p = e.plan()
	if svc := change(t, p, "NodeService app/nginx"); svc.Verdict != policy.ApprovalRequired || len(svc.Operations) != 2 {
		t.Fatalf("service: %+v", svc)
	}
	if rep := e.applyApproved(p); rep.Stopped != nil || !e.os.enabled || !e.os.running {
		t.Fatalf("service: %+v", rep)
	}
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
	// Removing a declaration from a candidate must not revoke active permissions either.
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
	// mamelet comes back, picks up the grant and installs; the next apply confirms by observation.
	e.round()
	if e.os.installs != 1 {
		t.Fatalf("installs %d", e.os.installs)
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
