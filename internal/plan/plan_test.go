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
)

const homelab = `{"resources":[{"kind":"VirtualMachine","name":"app-server","spec":{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024,"network":{"mode":"dhcp"}}},{"kind":"NodePackage","name":"app-server/nginx","spec":{"node":"app-server","package":"nginx","state":"Present"}},{"kind":"NodeService","name":"app-server/nginx","spec":{"enabled":true,"node":"app-server","running":true,"service":"nginx"}}],"schema":"mamela-graph/v1"}`

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
	s := state.Empty(t0)
	s.VMs["app-server"] = state.VM{Managed: true, CPU: 1, MemoryMiB: 1024, DiskGiB: 10, Image: "ubuntu-24.04", ObservedAt: t0}
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
	p := Build(desired(t), state.Empty(t0), policy.Default)
	vm := find(t, p, "VirtualMachine app-server")
	if vm.Status != StatusChange || vm.Verdict != policy.ApprovalRequired || ops(vm)[0] != policy.OpCreateVM {
		t.Errorf("VM: %+v", vm)
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

func TestDestructiveVMChangesAreDenied(t *testing.T) {
	s := converged()
	vm := s.VMs["app-server"]
	vm.DiskGiB = 20 // declared 10: shrinking
	vm.Image = "debian-12"
	s.VMs["app-server"] = vm
	c := find(t, Build(desired(t), s, policy.Default), "VirtualMachine app-server")
	if c.Verdict != policy.Denied {
		t.Fatalf("verdict %s, want Denied: %+v", c.Verdict, c)
	}
	got := ops(c)
	if len(got) != 2 || got[0] != policy.OpShrinkVMDisk || got[1] != policy.OpReplaceVMImage {
		t.Errorf("ops %v", got)
	}
}

func TestResizeNeedsApproval(t *testing.T) {
	s := converged()
	vm := s.VMs["app-server"]
	vm.CPU, vm.MemoryMiB, vm.DiskGiB = 2, 2048, 5 // declared 1 / 1024 / 10
	s.VMs["app-server"] = vm
	c := find(t, Build(desired(t), s, policy.Default), "VirtualMachine app-server")
	if c.Verdict != policy.ApprovalRequired {
		t.Fatalf("verdict %s", c.Verdict)
	}
	want := []policy.Op{policy.OpGrowVMDisk, policy.OpResizeVMCPU, policy.OpResizeVMMemory}
	if got := ops(c); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("ops %v, want %v", got, want)
	}
}

func TestForeignVMIsNeverChanged(t *testing.T) {
	s := converged()
	vm := s.VMs["app-server"]
	vm.Managed = false
	s.VMs["app-server"] = vm
	p := Build(desired(t), s, policy.Default)
	c := find(t, p, "VirtualMachine app-server")
	if c.Verdict != policy.Denied || ops(c)[0] != policy.OpChangeForeign {
		t.Errorf("VM: %+v", c)
	}
	pkg := find(t, p, "NodePackage app-server/nginx")
	if pkg.Status != StatusWaiting || pkg.Note.Code != "MAM-PLAN-0102" {
		t.Errorf("package on a foreign VM: %+v", pkg)
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

func TestRemovedVMIsRetained(t *testing.T) {
	s := converged()
	s.VMs["old-db"] = state.VM{Managed: true, CPU: 2, MemoryMiB: 2048, DiskGiB: 20, Image: "ubuntu-24.04", ObservedAt: t0}
	s.VMs["someone-elses"] = state.VM{Managed: false, CPU: 4, MemoryMiB: 8192, DiskGiB: 32, Image: "x", ObservedAt: t0}
	p := Build(desired(t), s, policy.Default)
	c := find(t, p, "VirtualMachine old-db")
	if c.Status != StatusRetained || len(c.Operations) != 0 || c.Note.Code != "MAM-PLAN-0301" {
		t.Errorf("%+v", c)
	}
	for _, ch := range p.Changes {
		if ch.Name == "someone-elses" {
			t.Errorf("a foreign VM that is not declared must not appear in the plan: %+v", ch)
		}
	}
}

func TestRender(t *testing.T) {
	s := converged()
	s.Services["app-server/nginx"] = state.Service{Exists: true, Enabled: true, Running: false, ObservedAt: t0}
	s.VMs["old-db"] = state.VM{Managed: true, CPU: 2, MemoryMiB: 2048, DiskGiB: 20, Image: "ubuntu-24.04", ObservedAt: t0}
	var buf bytes.Buffer
	Render(&buf, Build(desired(t), s, policy.Default))
	out := buf.String()
	for _, want := range []string{
		"= VirtualMachine app-server  [in sync]",
		"~ NodeService app-server/nginx  [needs approval]",
		"running: false -> true",
		"MAM-POL-0206",
		"hint: add it to autoStartServices",
		"- VirtualMachine old-db  [retained]",
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

// A VM keeps the network it was created with: a declared change is shown and denied. VMs made
// before networks were recorded observe as "dhcp".
func TestNetworkChangeIsDenied(t *testing.T) {
	s := converged()
	vm := s.VMs["app-server"]
	vm.Network = "static 192.0.2.10/24 via 192.0.2.1 dns 192.0.2.1"
	s.VMs["app-server"] = vm
	c := find(t, Build(desired(t), s, policy.Default), "VirtualMachine app-server")
	if c.Verdict != policy.Denied || len(c.Operations) != 1 || c.Operations[0].Op != policy.OpChangeVMNetwork ||
		c.Operations[0].Decision.Code != "MAM-POL-0307" || len(c.Diffs) != 1 || c.Diffs[0].To != "dhcp" {
		t.Fatalf("%+v", c)
	}
	vm.Network = "dhcp"
	s.VMs["app-server"] = vm
	if c := find(t, Build(desired(t), s, policy.Default), "VirtualMachine app-server"); c.Status != StatusInSync {
		t.Fatalf("same network: %+v", c)
	}
	created := find(t, Build(desired(t), state.Empty(t0), policy.Default), "VirtualMachine app-server")
	if !slices.ContainsFunc(created.Diffs, func(d Diff) bool { return d.Field == "network" && d.To == "dhcp" }) {
		t.Fatalf("create does not show the network: %+v", created.Diffs)
	}
}

// An agent of a server with a declared static address joins that address at once: it neither
// waits for the server's report nor follows it.
func TestK3sAgentOfAStaticServer(t *testing.T) {
	const sha = "265b3ee6a3198ef14033c63ef46cbb32370d83795199fb6d1b01f21575eddbda"
	vm := func(name, network string) string {
		return `{"kind":"VirtualMachine","name":"` + name + `","spec":{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024,"network":` + network + `}}`
	}
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v1","resources":[` +
		vm("srv", `{"mode":"static","address":"192.0.2.40/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}`) + `,` + vm("ag", `{"mode":"dhcp"}`) + `,` +
		`{"kind":"NodeK3s","name":"srv/k3s","spec":{"node":"srv","role":"server","sha256":"` + sha + `","version":"v1.37.2+k3s1"}},` +
		`{"kind":"NodeK3s","name":"ag/k3s","spec":{"node":"ag","role":"agent","server":"srv","sha256":"` + sha + `","version":"v1.37.2+k3s1"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s := state.Empty(t0)
	for _, n := range []string{"srv", "ag"} {
		s.VMs[n] = state.VM{Managed: true, CPU: 1, MemoryMiB: 1024, DiskGiB: 10, Image: "ubuntu-24.04", ObservedAt: t0}
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

// A stopped VM is started (with approval) and its node resources wait. A VM that has been running
// long past a first boot without its agent ever reporting is called out, with what to do.
func TestStoppedVMAndSilentAgent(t *testing.T) {
	s := state.Empty(t0)
	s.VMs["app-server"] = state.VM{Managed: true, CPU: 1, MemoryMiB: 1024, DiskGiB: 10, Image: "ubuntu-24.04", Network: "dhcp",
		Stopped: true, ObservedAt: t0}
	p := Build(desired(t), s, policy.Default)
	vm := find(t, p, "VirtualMachine app-server")
	if len(vm.Operations) != 1 || vm.Operations[0].Op != policy.OpStartVM || vm.Verdict != policy.ApprovalRequired {
		t.Fatalf("stopped VM: %+v", vm)
	}
	if c := find(t, p, "NodePackage app-server/nginx"); c.Note == nil || c.Note.Code != "MAM-PLAN-0111" {
		t.Fatalf("package of a stopped VM: %+v", c.Note)
	}

	young := s.VMs["app-server"]
	young.Stopped, young.UptimeSeconds = false, int64((5 * time.Minute).Seconds())
	s.VMs["app-server"] = young
	if c := find(t, Build(desired(t), s, policy.Default), "NodePackage app-server/nginx"); c.Status != StatusWaiting || c.Note.Code != "MAM-PLAN-0103" {
		t.Fatalf("young VM: %+v", c)
	}
	old := young
	old.UptimeSeconds = int64((40 * time.Minute).Seconds())
	s.VMs["app-server"] = old
	c := find(t, Build(desired(t), s, policy.Default), "NodePackage app-server/nginx")
	if c.Status != StatusBlocked || c.Note.Code != "MAM-PLAN-0110" || !strings.Contains(c.Note.Message, "40m") ||
		!strings.Contains(c.Note.Hint, "mamela reboot app-server") {
		t.Fatalf("silent agent: %+v", c.Note)
	}
}

// A Control Plane never changes its own host (ADR 0003): every change to it, or to what runs on
// it, is shown and denied. Other VMs are planned as usual.
func TestControlPlaneHostIsProtected(t *testing.T) {
	pol := policy.Default
	pol.ProtectedNodes = []string{"app-server"}
	s := converged()
	vm := s.VMs["app-server"]
	vm.CPU = 2
	s.VMs["app-server"] = vm
	s.Services["app-server/nginx"] = state.Service{Exists: true, Enabled: true, Running: false, ObservedAt: t0}
	p := Build(desired(t), s, pol)
	for _, id := range []string{"VirtualMachine app-server", "NodeService app-server/nginx"} {
		c := find(t, p, id)
		if c.Verdict != policy.Denied || len(c.Operations) == 0 || c.Operations[0].Decision.Code != "MAM-POL-0308" {
			t.Fatalf("%s: %+v", id, c)
		}
	}
	if Build(desired(t), s, pol).Digest == Build(desired(t), s, policy.Default).Digest {
		t.Fatal("the protection is not part of the plan's inputs")
	}
}
