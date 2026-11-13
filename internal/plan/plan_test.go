package plan

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/provider/protocol"
)

const homelab = `{"resources":[{"kind":"TestVM","name":"app-server","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}},{"kind":"NodePackage","name":"app-server/nginx","spec":{"node":"app-server","package":"nginx","state":"Present"}},{"kind":"NodeService","name":"app-server/nginx","spec":{"enabled":true,"node":"app-server","running":true,"service":"nginx"}}],"schema":"mamela-graph/v2"}`

// vm is a provider's machine ("TestVM" of provider "test") with a network.
func vm(name, network string) string {
	return `{"kind":"TestVM","name":"` + name + `","provider":"test","spec":{"cpu":1,"network":` + network + `}}`
}

// machineObserved records what the provider observed of a declared machine of g, and its changes.
func machineObserved(s *state.Snapshot, g *graph.Graph, name string, exists, owned, running bool, uptime int64, changes ...state.ProvidedChange) {
	for _, r := range g.Resources {
		if r.Name != name || r.Provided == nil {
			continue
		}
		o := protocol.Observed{ID: r.ID(), Kind: r.Kind, Name: name, Exists: exists, Owned: owned}
		if exists {
			o.IDs = map[string]string{"name": name}
			o.Node = &protocol.ObservedNode{ID: 7000, Running: running, UptimeSeconds: uptime}
		}
		if s.Provided == nil {
			s.Provided = map[string]state.Provided{}
		}
		s.Provided[r.ID()] = state.Provided{Provider: r.Provided.Provider, Kind: r.Kind, Name: name, Declared: true, Observed: o,
			SpecDigest: state.SpecDigest(r.Provided.Spec), Changes: changes, ObservedAt: t0}
	}
	s.AddProvided(s.Provided)
}

func providedChange(id string, op protocol.Op, effect protocol.Effect) state.ProvidedChange {
	return state.ProvidedChange{Change: protocol.Change{Resource: id, Kind: "TestVM", Op: op, Reasons: []string{string(op)}},
		Verified: protocol.Verified{Effects: []protocol.Effect{effect}}}
}

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func desired(t *testing.T) *graph.Graph {
	t.Helper()
	g, err := graph.Decode([]byte(homelab))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// converged is the snapshot of a world that matches the declaration.
func converged() state.Snapshot {
	g, err := graph.Decode([]byte(homelab))
	if err != nil {
		panic(err)
	}
	s := state.Empty(t0)
	machineObserved(&s, g, "app-server", true, true, true, 600)
	s.Packages["app-server/nginx"] = state.Package{Installed: true, Version: "1.24.0", ObservedAt: t0}
	s.Services["app-server/nginx"] = state.Service{Exists: true, Enabled: true, Running: true, ObservedAt: t0}
	return s
}

func find(t *testing.T, p *Plan, id string) Change {
	t.Helper()
	for _, c := range p.Changes {
		if c.ID() == id {
			return c
		}
	}
	t.Fatalf("no change for %s", id)
	return Change{}
}

func ops(c Change) []policy.Op {
	out := make([]policy.Op, len(c.Operations))
	for i, o := range c.Operations {
		out[i] = o.Op
	}
	return out
}

func TestEmptyWorld(t *testing.T) {
	g := desired(t)
	s := state.Empty(t0)
	machineObserved(&s, g, "app-server", false, false, false, 0,
		providedChange("TestVM app-server", "create-vm", protocol.EffectCreate))
	p := Build(g, s, policy.Default)
	m := find(t, p, "TestVM app-server")
	if m.Status != StatusChange || m.Verdict != policy.ApprovalRequired || ops(m)[0] != "create-vm" || !m.Creates {
		t.Errorf("machine: %+v", m)
	}
	for _, id := range []string{"NodePackage app-server/nginx", "NodeService app-server/nginx"} {
		c := find(t, p, id)
		if c.Status != StatusWaiting || c.Note == nil || c.Note.Code != "MAM-PLAN-0101" {
			t.Errorf("%s: %+v", id, c)
		}
	}
}

func TestConvergedWorldIsInSync(t *testing.T) {
	p := Build(desired(t), converged(), policy.Default)
	for _, c := range p.Changes {
		if c.Status != StatusInSync || len(c.Operations) != 0 {
			t.Errorf("%s: %+v", c.ID(), c)
		}
	}
}

func TestDeterministic(t *testing.T) {
	a := Build(desired(t), converged(), policy.Default)
	for range 20 {
		b := Build(desired(t), converged(), policy.Default)
		if a.Digest != b.Digest || !bytes.Equal(a.Canonical(), b.Canonical()) {
			t.Fatalf("plan not deterministic: %s vs %s", a.Digest, b.Digest)
		}
	}
}

func TestDigestCoversInputs(t *testing.T) {
	base := Build(desired(t), converged(), policy.Default)
	later := converged()
	later.AsOf = t0.Add(time.Minute)
	if Build(desired(t), later, policy.Default).Digest == base.Digest {
		t.Error("a different snapshot time gave the same plan digest")
	}
	pol := policy.Default
	pol.AutoStartServices = []string{"nginx"}
	if Build(desired(t), converged(), pol).Digest == base.Digest {
		t.Error("a different policy gave the same plan digest")
	}
}

func TestPackageAndServiceDrift(t *testing.T) {
	s := converged()
	cs := nginxChangeSet()
	s.Packages["app-server/nginx"] = state.Package{Installed: false, ChangeSet: &cs, ObservedAt: t0}
	s.Services["app-server/nginx"] = state.Service{Exists: true, Enabled: true, Running: false, ObservedAt: t0}

	p := Build(desired(t), s, policy.Default)
	pkg := find(t, p, "NodePackage app-server/nginx")
	if pkg.Verdict != policy.ApprovalRequired || ops(pkg)[0] != policy.OpInstallPackage {
		t.Errorf("package: %+v", pkg)
	}
	svc := find(t, p, "NodeService app-server/nginx")
	if svc.Verdict != policy.ApprovalRequired || ops(svc)[0] != policy.OpStartService {
		t.Errorf("service without autostart: %+v", svc)
	}

	pol := policy.Default
	pol.AutoStartServices = []string{"nginx"}
	svc = find(t, Build(desired(t), s, pol), "NodeService app-server/nginx")
	if svc.Verdict != policy.Auto {
		t.Errorf("service with autostart: %+v", svc)
	}
}

func TestForeignMachineIsNeverChanged(t *testing.T) {
	s := converged()
	machineObserved(&s, desired(t), "app-server", true, false, true, 600)
	p := Build(desired(t), s, policy.Default)
	c := find(t, p, "TestVM app-server")
	if c.Verdict != policy.Denied || ops(c)[0] != policy.OpChangeForeign {
		t.Errorf("machine: %+v", c)
	}
	pkg := find(t, p, "NodePackage app-server/nginx")
	if pkg.Status != StatusWaiting || pkg.Note.Code != "MAM-PLAN-0102" {
		t.Errorf("package on a foreign machine: %+v", pkg)
	}
}

func TestStaleObservationBlocks(t *testing.T) {
	s := converged()
	s.AsOf = t0.Add(time.Hour)
	p := Build(desired(t), s, policy.Default)
	for _, c := range p.Changes {
		if c.Status != StatusBlocked || c.Note == nil || c.Note.Code != "MAM-PLAN-0201" {
			t.Errorf("%s: %+v", c.ID(), c)
		}
	}
}

func TestMissingAgentReportWaits(t *testing.T) {
	s := converged()
	delete(s.Packages, "app-server/nginx")
	c := find(t, Build(desired(t), s, policy.Default), "NodePackage app-server/nginx")
	if c.Status != StatusWaiting || c.Note.Code != "MAM-PLAN-0103" {
		t.Errorf("%+v", c)
	}
}

// undeclared is a machine a provider reports that is no longer declared.
func undeclared(s *state.Snapshot, name string, owned bool) {
	s.Provided["TestVM "+name] = state.Provided{Provider: "test", Kind: "TestVM", Name: name, ObservedAt: t0,
		Observed: protocol.Observed{ID: "TestVM " + name, Kind: "TestVM", Name: name, Exists: true, Owned: owned}}
}

func TestRemovedMachineIsRetained(t *testing.T) {
	s := converged()
	undeclared(&s, "old-db", true)
	undeclared(&s, "someone-elses", false)
	p := Build(desired(t), s, policy.Default)
	c := find(t, p, "TestVM old-db")
	if c.Status != StatusRetained || len(c.Operations) != 0 || c.Note.Code != "MAM-PLAN-0301" {
		t.Errorf("%+v", c)
	}
	for _, ch := range p.Changes {
		if ch.Name == "someone-elses" {
			t.Errorf("a foreign machine that is not declared must not appear in the plan: %+v", ch)
		}
	}
}

func TestRender(t *testing.T) {
	s := converged()
	s.Services["app-server/nginx"] = state.Service{Exists: true, Enabled: true, Running: false, ObservedAt: t0}
	undeclared(&s, "old-db", true)
	var buf bytes.Buffer
	Render(&buf, Build(desired(t), s, policy.Default))
	out := buf.String()
	for _, want := range []string{
		"= TestVM app-server  [in sync]  via test",
		"~ NodeService app-server/nginx  [needs approval]",
		"running: false -> true",
		"MAM-POL-0206",
		"hint: add it to autoStartServices",
		"= TestVM old-db  [retained]  via test",
		"Summary: 1 needs approval, 1 retained, 2 in sync.",
		"Nothing has been changed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}

func nginxChangeSet() noderev.ChangeSet {
	return noderev.NewChangeSet([]noderev.Change{
		{Action: noderev.ChangeInstall, Package: "nginx", Version: "1.24.0-2ubuntu7.19", Origins: []string{"Ubuntu:24.04/noble-updates"}},
		{Action: noderev.ChangeInstall, Package: "nginx-common", Version: "1.24.0-2ubuntu7.19", Origins: []string{"Ubuntu:24.04/noble-updates"}},
	})
}

// Approving an installation approves the exact apt change set: it is part of the plan (and its
// digest). Removals and unknown origins are denied; without a simulation the plan waits.
func TestInstallPlansTheChangeSet(t *testing.T) {
	plan := func(p state.Package) Change {
		s := converged()
		p.ObservedAt = t0
		s.Packages["app-server/nginx"] = p
		return find(t, Build(desired(t), s, policy.Default), "NodePackage app-server/nginx")
	}
	cs := nginxChangeSet()
	c := plan(state.Package{ChangeSet: &cs})
	if c.Verdict != policy.ApprovalRequired || len(c.Diffs) != 4 || c.Diffs[1].To != cs.Digest ||
		c.Diffs[2].To != "install nginx 1.24.0-2ubuntu7.19 (Ubuntu:24.04/noble-updates)" {
		t.Fatalf("install: %+v", c)
	}
	other := noderev.NewChangeSet(append(cs.Changes, noderev.Change{Action: noderev.ChangeInstall, Package: "libfoo", Version: "1", Origins: []string{"Ubuntu:24.04/noble"}}))
	s1, s2 := converged(), converged()
	s1.Packages["app-server/nginx"] = state.Package{ChangeSet: &cs, ObservedAt: t0}
	s2.Packages["app-server/nginx"] = state.Package{ChangeSet: &other, ObservedAt: t0}
	if Build(desired(t), s1, policy.Default).Digest == Build(desired(t), s2, policy.Default).Digest {
		t.Fatal("a different change set gave the same plan digest")
	}

	removes := noderev.NewChangeSet(append(cs.Changes, noderev.Change{Action: noderev.ChangeRemove, Package: "apache2", Version: "2.4"}))
	if c := plan(state.Package{ChangeSet: &removes}); c.Verdict != policy.Denied || c.Operations[0].Decision.Code != "MAM-POL-0304" {
		t.Fatalf("removal: %+v", c)
	}
	ppa := noderev.NewChangeSet([]noderev.Change{{Action: noderev.ChangeInstall, Package: "nginx", Version: "1.27", Origins: []string{"LP-PPA-someone:noble"}}})
	if c := plan(state.Package{ChangeSet: &ppa}); c.Verdict != policy.Denied || c.Operations[0].Decision.Code != "MAM-POL-0305" {
		t.Fatalf("origin: %+v", c)
	}
	if c := plan(state.Package{}); c.Status != StatusWaiting || c.Note.Code != "MAM-PLAN-0105" {
		t.Fatalf("no simulation: %+v", c)
	}
	if c := plan(state.Package{Error: "E: Unable to locate package nginx"}); c.Status != StatusBlocked || c.Note.Code != "MAM-PLAN-0202" {
		t.Fatalf("error: %+v", c)
	}
}

func TestServiceWaitsForItsUnit(t *testing.T) {
	s := converged()
	s.Services["app-server/nginx"] = state.Service{Exists: false, ObservedAt: t0}
	c := find(t, Build(desired(t), s, policy.Default), "NodeService app-server/nginx")
	if c.Status != StatusWaiting || c.Note.Code != "MAM-PLAN-0106" || len(c.Operations) != 0 {
		t.Fatalf("%+v", c)
	}
}

// An agent of a server with a declared static address joins that address at once: it neither
// waits for the server's report nor follows it.
func TestK3sAgentOfAStaticServer(t *testing.T) {
	const sha = "265b3ee6a3198ef14033c63ef46cbb32370d83795199fb6d1b01f21575eddbda"
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
		vm("srv", `{"mode":"static","address":"192.0.2.40/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}`) + `,` + vm("ag", `{"mode":"dhcp"}`) + `,` +
		`{"kind":"NodeK3s","name":"srv/k3s","spec":{"node":"srv","role":"server","sha256":"` + sha + `","version":"v1.37.2+k3s1"}},` +
		`{"kind":"NodeK3s","name":"ag/k3s","spec":{"node":"ag","role":"agent","server":"srv","sha256":"` + sha + `","version":"v1.37.2+k3s1"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s := state.Empty(t0)
	for _, n := range []string{"srv", "ag"} {
		machineObserved(&s, g, n, true, true, true, 600)
	}
	s.K3s = map[string]state.K3s{
		"srv/k3s": {Installed: true, SHA256: sha, Addresses: []string{"10.42.0.0", "192.0.2.77"}, ObservedAt: t0},
		"ag/k3s":  {ObservedAt: t0},
	}
	c := find(t, Build(g, s, policy.Default), "NodeK3s ag/k3s")
	if !slices.ContainsFunc(c.Diffs, func(d Diff) bool { return d.Field == "server" && d.To == "https://192.0.2.40:6443" }) {
		t.Fatalf("agent of a static server: %+v %+v", c.Diffs, c.Note)
	}
}

// A stopped machine's node resources wait. A machine that has been running long past a first
// boot without its agent ever reporting is called out, with what to do.
func TestStoppedMachineAndSilentAgent(t *testing.T) {
	g := desired(t)
	s := state.Empty(t0)
	machineObserved(&s, g, "app-server", true, true, false, 0, providedChange("TestVM app-server", "start-vm", protocol.EffectRestart))
	p := Build(g, s, policy.Default)
	if m := find(t, p, "TestVM app-server"); len(m.Operations) != 1 || m.Operations[0].Op != "start-vm" || m.Verdict != policy.ApprovalRequired {
		t.Fatalf("stopped machine: %+v", m)
	}
	if c := find(t, p, "NodePackage app-server/nginx"); c.Note == nil || c.Note.Code != "MAM-PLAN-0111" {
		t.Fatalf("package of a stopped machine: %+v", c.Note)
	}
	machineObserved(&s, g, "app-server", true, true, true, int64((5 * time.Minute).Seconds()))
	if c := find(t, Build(g, s, policy.Default), "NodePackage app-server/nginx"); c.Status != StatusWaiting || c.Note.Code != "MAM-PLAN-0103" {
		t.Fatalf("young machine: %+v", c)
	}
	machineObserved(&s, g, "app-server", true, true, true, int64((40 * time.Minute).Seconds()))
	c := find(t, Build(g, s, policy.Default), "NodePackage app-server/nginx")
	if c.Status != StatusBlocked || c.Note.Code != "MAM-PLAN-0110" || !strings.Contains(c.Note.Message, "40m") ||
		!strings.Contains(c.Note.Hint, "mamela act") {
		t.Fatalf("silent agent: %+v", c.Note)
	}
}

// A k3s agent's service starts only once its server's runs (its start would wait for the server).
func TestK3sAgentStartsAfterItsServer(t *testing.T) {
	const sha = "265b3ee6a3198ef14033c63ef46cbb32370d83795199fb6d1b01f21575eddbda"
	svc := func(node string) string {
		return `{"kind":"NodeService","name":"` + node + `/k3s","spec":{"enabled":true,"node":"` + node + `","running":true,"service":"k3s"}}`
	}
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + vm("srv", `{"mode":"dhcp"}`) + `,` + vm("ag", `{"mode":"dhcp"}`) + `,` +
		`{"kind":"NodeK3s","name":"srv/k3s","spec":{"node":"srv","role":"server","sha256":"` + sha + `","version":"v1.37.2+k3s1"}},` +
		`{"kind":"NodeK3s","name":"ag/k3s","spec":{"node":"ag","role":"agent","server":"srv","sha256":"` + sha + `","version":"v1.37.2+k3s1"}},` +
		svc("srv") + `,` + svc("ag") + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	s := state.Empty(t0)
	for _, n := range []string{"srv", "ag"} {
		machineObserved(&s, g, n, true, true, true, 600)
		s.Services[n+"/k3s"] = state.Service{Exists: true, ObservedAt: t0}
	}
	s.K3s = map[string]state.K3s{"srv/k3s": {Installed: true, SHA256: sha, ObservedAt: t0}, "ag/k3s": {Installed: true, SHA256: sha, ObservedAt: t0}}
	p := Build(g, s, policy.Default)
	if c := find(t, p, "NodeService ag/k3s"); c.Status != StatusWaiting || c.Note.Code != "MAM-PLAN-0112" {
		t.Fatalf("agent before its server runs: %+v", c)
	}
	if c := find(t, p, "NodeService srv/k3s"); c.Status != StatusChange {
		t.Fatalf("server: %+v", c)
	}
	s.Services["srv/k3s"] = state.Service{Exists: true, Enabled: true, Running: true, ObservedAt: t0}
	if c := find(t, Build(g, s, policy.Default), "NodeService ag/k3s"); c.Status != StatusChange {
		t.Fatalf("agent once its server runs: %+v", c)
	}
}

// A provider's machine is a node: created, Mamela's, running, and how long.
func TestNodeResourcesOnAProvidersMachine(t *testing.T) {
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
		`{"kind":"ProxmoxVM","name":"web","provider":"proxmox","spec":{"cpu":1}},` +
		`{"kind":"NodePackage","name":"web/nginx","spec":{"node":"web","package":"nginx","state":"Present"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	pkg := func(s state.Snapshot) Change { return find(t, Build(g, s, policy.Default), "NodePackage web/nginx") }
	s := state.Empty(t0)
	if c := pkg(s); c.Note == nil || c.Note.Code != "MAM-PLAN-0101" {
		t.Fatalf("no machine yet: %+v", c.Note)
	}
	s.Nodes = map[string]state.Node{"web": {Provider: "proxmox", Managed: true, ID: 7001, Running: false, ObservedAt: t0}}
	if c := pkg(s); c.Note == nil || c.Note.Code != "MAM-PLAN-0111" {
		t.Fatalf("stopped machine: %+v", c.Note)
	}
	s.Nodes["web"] = state.Node{Provider: "proxmox", Managed: true, ID: 7001, Running: true, UptimeSeconds: 3600, ObservedAt: t0}
	if c := pkg(s); c.Note == nil || c.Note.Code != "MAM-PLAN-0110" {
		t.Fatalf("silent agent on a provider's machine: %+v", c.Note)
	}
	s.Nodes["web"] = state.Node{Provider: "proxmox", Managed: false, ID: 7001, Running: true, ObservedAt: t0}
	if c := pkg(s); c.Note == nil || c.Note.Code != "MAM-PLAN-0102" {
		t.Fatalf("someone else's machine: %+v", c.Note)
	}
}

// A provider's machine that is the Control Plane's host is protected (ADR 0003): its change, and
// every change of what runs on it, is shown and denied, never approvable.
func TestProvidersMachineAsControlPlaneHostIsProtected(t *testing.T) {
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[{"kind":"ProxmoxVM","name":"cp","provider":"proxmox","spec":{"cpu":4}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s := state.Empty(t0)
	s.Provided = map[string]state.Provided{"ProxmoxVM cp": {Provider: "proxmox", Kind: "ProxmoxVM", Name: "cp", Declared: true,
		SpecDigest: state.SpecDigest(g.Resources[0].Provided.Spec), ObservedAt: t0,
		Observed: protocol.Observed{ID: "ProxmoxVM cp", Kind: "ProxmoxVM", Name: "cp", Exists: true, Owned: true},
		Changes:  []state.ProvidedChange{{Change: protocol.Change{Resource: "ProxmoxVM cp", Kind: "ProxmoxVM", Op: "resize-vm"}}}}}
	pol := policy.Default
	pol.ProtectedNodes = []string{"cp"}
	c := find(t, Build(g, s, pol), "ProxmoxVM cp")
	if c.Verdict != policy.Denied || len(c.Operations) != 1 || c.Operations[0].Decision.Code != "MAM-POL-0308" {
		t.Fatalf("%+v", c)
	}
}

// A difference a provider never acts on is shown and denied, with its reason; it is not "in sync".
func TestUnsupportedProviderChangeIsDenied(t *testing.T) {
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[{"kind":"ProxmoxVM","name":"web","provider":"proxmox","spec":{"diskGiB":5}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s := state.Empty(t0)
	s.Provided = map[string]state.Provided{"ProxmoxVM web": {Provider: "proxmox", Kind: "ProxmoxVM", Name: "web", Declared: true,
		SpecDigest: state.SpecDigest(g.Resources[0].Provided.Spec), ObservedAt: t0,
		Observed: protocol.Observed{ID: "ProxmoxVM web", Kind: "ProxmoxVM", Name: "web", Exists: true, Owned: true},
		Changes: []state.ProvidedChange{{Change: protocol.Change{Resource: "ProxmoxVM web", Kind: "ProxmoxVM", Op: "shrink-vm-disk",
			Reasons: []string{"disk 10 -> 5 GiB"}}, Unsupported: "shrinking a disk can destroy data"}}}}
	c := find(t, Build(g, s, policy.Default), "ProxmoxVM web")
	if c.Status != StatusChange || c.Verdict != policy.Denied || c.Operations[0].Decision.Code != "MAM-POL-0309" ||
		!strings.Contains(c.Operations[0].Decision.Message, "shrinking a disk") {
		t.Fatalf("%+v", c)
	}
}

// What runs on the Control Plane's host is protected too, and the protection is part of the plan's
// inputs.
func TestWhatRunsOnTheControlPlaneHostIsProtected(t *testing.T) {
	pol := policy.Default
	pol.ProtectedNodes = []string{"app-server"}
	s := converged()
	s.Services["app-server/nginx"] = state.Service{Exists: true, Enabled: true, Running: false, ObservedAt: t0}
	c := find(t, Build(desired(t), s, pol), "NodeService app-server/nginx")
	if c.Verdict != policy.Denied || len(c.Operations) == 0 || c.Operations[0].Decision.Code != "MAM-POL-0308" {
		t.Fatalf("%+v", c)
	}
	if Build(desired(t), s, pol).Digest == Build(desired(t), s, policy.Default).Digest {
		t.Fatal("the protection is not part of the plan's inputs")
	}
}

// A resource that waits for another is shown waiting, with why, and nothing runs for it.
func TestWaitingForADependency(t *testing.T) {
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
		`{"kind":"Thing","name":"a","provider":"test","spec":{}},` +
		`{"kind":"Thing","name":"b","provider":"test","after":["Thing a"],"spec":{}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s := state.Empty(t0)
	s.Provided = map[string]state.Provided{}
	for _, r := range g.Resources {
		p := state.Provided{Provider: "test", Kind: r.Kind, Name: r.Name, Declared: true, SpecDigest: state.SpecDigest(r.Provided.Spec), ObservedAt: t0,
			Observed: protocol.Observed{ID: r.ID(), Kind: r.Kind, Name: r.Name}}
		if r.Name == "b" {
			p.Waiting = "waits for Thing a to be created"
		}
		s.Provided[r.ID()] = p
	}
	c := find(t, Build(g, s, policy.Default), "Thing b")
	if c.Status != StatusWaiting || c.Note == nil || c.Note.Code != "MAM-PLAN-0113" || len(c.Operations) != 0 {
		t.Fatalf("%+v", c)
	}
}
