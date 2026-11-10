package lab

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
)

const l2 = `{"schema":"mamela-graph/v1","resources":[
{"kind":"VirtualMachine","name":"web","spec":{"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04"}},
{"kind":"VirtualMachine","name":"db","spec":{"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04"}},
{"kind":"NodePackage","name":"db/nginx","spec":{"node":"db","package":"nginx","state":"Present"}},
{"kind":"NodeService","name":"db/nginx","spec":{"node":"db","service":"nginx","enabled":true,"running":true}}]}`

func TestPlanUsesAreConcreteAndGranted(t *testing.T) {
	s := exampleSite(t)
	g, err := graph.Decode([]byte(l2))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	snap := state.Empty(t0)
	snap.VMs["db"] = state.VM{Managed: true, CPU: 1, MemoryMiB: 1024, DiskGiB: 8, Image: "ubuntu-24.04", ObservedAt: t0}
	cs := noderev.NewChangeSet([]noderev.Change{{Action: "install", Package: "nginx", Version: "1", Origins: []string{"Ubuntu:24.04/noble"}}})
	snap.Packages["db/nginx"] = state.Package{ChangeSet: &cs, ObservedAt: t0}
	snap.Services["db/nginx"] = state.Service{Exists: true, ObservedAt: t0}
	pol := policy.Default
	pol.AutoStartServices = []string{"nginx"}
	p := plan.Build(g, snap, pol)

	byOp := map[string]OpUses{}
	for _, cu := range s.PlanUses(p, g) {
		for _, u := range cu.Ops {
			byOp[cu.Change.ID()+" "+string(u.Op)] = u
		}
	}
	create := byOp["VirtualMachine web create-vm"]
	has := func(u OpUses, path, priv string, maybe bool) bool {
		return slices.ContainsFunc(u.Proxmox, func(x Use) bool { return x.Path == path && x.Priv == priv && x.Maybe == maybe })
	}
	for _, want := range []struct {
		path, priv string
		maybe      bool
	}{
		{"/pool/mamela-lab", "VM.Allocate", false},
		{"/storage/local-lvm", "Datastore.AllocateSpace", false},
		{"/storage/local", "Datastore.Audit", false},
		{"/storage/local", "Datastore.AllocateTemplate", true}, // only if not uploaded yet
		{"/pool/mamela-lab", "VM.PowerMgmt", true},
		{"/sdn/zones/localnetwork/vmbr0", "SDN.Use", false},
	} {
		if !has(create, want.path, want.priv, want.maybe) {
			t.Errorf("create-vm lacks %+v: %+v", want, create.Proxmox)
		}
	}
	if grow := byOp["VirtualMachine db grow-vm-disk"]; !has(grow, "/pool/mamela-lab", "VM.Config.Disk", false) || has(grow, "/pool/mamela-lab", "VM.Allocate", false) {
		t.Errorf("grow-vm-disk: %+v", grow.Proxmox)
	}
	if u := byOp["NodePackage db/nginx install-package"]; len(u.Proxmox) != 0 || !strings.Contains(u.Mamelet, cs.Digest) {
		t.Errorf("install: %+v", u)
	}
	if u := byOp["NodeService db/nginx start-service"]; !strings.Contains(u.Mamelet, "whenever it finds it stopped") {
		t.Errorf("auto start: %+v", u)
	}
	if u := byOp["NodeService db/nginx enable-service"]; !strings.Contains(u.Mamelet, "systemctl enable nginx once") {
		t.Errorf("enable: %+v", u)
	}

	// Everything a plan uses is granted, and nothing granted goes unused by the site's calls.
	granted := map[string]bool{}
	for _, gr := range s.Grants() {
		for _, pr := range gr.Privs {
			granted[gr.Path+" "+pr] = true
		}
	}
	for _, u := range append(s.ObserveUses(), append(create.Proxmox, byOp["VirtualMachine db grow-vm-disk"].Proxmox...)...) {
		if !granted[u.Path+" "+u.Priv] {
			t.Errorf("the plan uses %s on %s, which is not granted", u.Priv, u.Path)
		}
	}
}

func TestMayNeedCoversEveryKind(t *testing.T) {
	g, _ := graph.Decode([]byte(l2))
	var s *Site // no site: placeholder paths
	kinds := map[graph.Kind][]OpUses{}
	for _, ku := range s.MayNeed(g) {
		kinds[ku.Kind] = ku.Ops
	}
	if len(kinds) != 3 {
		t.Fatalf("kinds %v", kinds)
	}
	vm := kinds[graph.KindVirtualMachine]
	if vm[0].Op != policy.OpCreateVM || vm[0].Proxmox[0].Path != "/pool/<lab pool>" {
		t.Errorf("create-vm: %+v", vm[0])
	}
	if !slices.ContainsFunc(vm, func(u OpUses) bool { return u.Op == policy.OpShrinkVMDisk && u.Unsupported != "" }) {
		t.Error("shrink-vm-disk is not shown as never carried out")
	}
	if !slices.ContainsFunc(vm, func(u OpUses) bool { return u.Op == policy.OpResizeVMCPU && len(u.Proxmox) > 0 }) {
		t.Error("resize-vm-cpu does not say what it uses")
	}
	if kinds[graph.KindNodePackage][0].Mamelet == "" {
		t.Error("install-package does not say what mamelet does")
	}
}
