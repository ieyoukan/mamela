package plan

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
)

const homelab = `{"resources":[{"kind":"VirtualMachine","name":"app-server","spec":{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}},{"kind":"NodePackage","name":"app-server/nginx","spec":{"node":"app-server","package":"nginx","state":"Present"}},{"kind":"NodeService","name":"app-server/nginx","spec":{"enabled":true,"node":"app-server","running":true,"service":"nginx"}}],"schema":"mamela-graph/v1"}`

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
	s.Services["app-server/nginx"] = state.Service{Enabled: true, Running: true, ObservedAt: t0}
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
	s.Packages["app-server/nginx"] = state.Package{Installed: false, ObservedAt: t0}
	s.Services["app-server/nginx"] = state.Service{Enabled: true, Running: false, ObservedAt: t0}

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
	want := []policy.Op{policy.OpResizeVMCPU, policy.OpResizeVMMemory, policy.OpGrowVMDisk}
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
	s.Services["app-server/nginx"] = state.Service{Enabled: true, Running: false, ObservedAt: t0}
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
