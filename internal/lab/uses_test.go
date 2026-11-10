package lab

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/pve"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/proxmox"
	"github.com/ieyoukan/mamela/internal/state"
)

const l2 = `{"schema":"mamela-graph/v1","resources":[
{"kind":"VirtualMachine","name":"web","spec":{"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}}},
{"kind":"VirtualMachine","name":"db","spec":{"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}}},
{"kind":"NodePackage","name":"db/nginx","spec":{"node":"db","package":"nginx","state":"Present"}},
{"kind":"NodeService","name":"db/nginx","spec":{"node":"db","service":"nginx","enabled":true,"running":true}}]}`

func TestPlannedRequirementsAreConcreteAndGranted(t *testing.T) {
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

	byOp := map[string][]spec.Requirement{}
	for _, q := range s.PlannedRequirements(p, g, pol) {
		k := q.Resource + " " + string(q.Op)
		byOp[k] = append(byOp[k], q)
	}
	has := func(reqs []spec.Requirement, target, capability string, always bool) bool {
		return slices.ContainsFunc(reqs, func(q spec.Requirement) bool {
			return q.Provider == "proxmox" && q.Target == target && q.Resolved && q.Capability == capability && q.Always == always
		})
	}
	create := byOp["VirtualMachine web create-vm"]
	for _, want := range []struct {
		target, capability string
		always             bool
	}{
		{"/pool/mamela-lab", "VM.Allocate", true},
		{"/storage/local-lvm", "Datastore.AllocateSpace", true},
		{"/storage/local", "Datastore.Audit", true},
		{"/storage/local", "Datastore.AllocateTemplate", false}, // only if not uploaded yet
		{"/pool/mamela-lab", "VM.PowerMgmt", false},
		{"/sdn/zones/localnetwork/vmbr0", "SDN.Use", true},
	} {
		if !has(create, want.target, want.capability, want.always) {
			t.Errorf("create-vm lacks %+v: %+v", want, create)
		}
	}
	grow := byOp["VirtualMachine db grow-vm-disk"]
	if !has(grow, "/pool/mamela-lab", "VM.Config.Disk", true) || has(grow, "/pool/mamela-lab", "VM.Allocate", true) {
		t.Errorf("grow-vm-disk: %+v", grow)
	}
	mamelet := func(k, capability string) spec.Requirement {
		reqs := byOp[k]
		if len(reqs) != 1 || reqs[0].Provider != "mamelet" || reqs[0].Capability != capability || reqs[0].Target != "db" ||
			reqs[0].TargetType != "node" || reqs[0].Object != "nginx" {
			t.Errorf("%s: %+v", k, reqs)
			return spec.Requirement{}
		}
		return reqs[0]
	}
	// The install's conditions are data that hold exactly for the approved simulation.
	install := mamelet("NodePackage db/nginx install-package", "apt.install")
	approved := map[string][]string{
		node.KeyNode: {"db"}, node.KeyOperation: {"op-1"}, node.KeyNow: {"2026-11-10T01:00:00Z"},
		node.KeyChangeSet: {cs.Digest}, node.KeyAptOrigin: {"Ubuntu:24.04/noble"},
	}
	install.Fill(map[string][]string{node.FactOperationID: {"op-1"}, node.FactExpiresAt: {"2026-11-11T00:00:00Z"}})
	if !holdAll(install.Conditions, approved) {
		t.Errorf("the approved install does not satisfy its conditions: %+v", install.Conditions)
	}
	for name, edit := range map[string]func(map[string][]string){
		"another change set": func(f map[string][]string) { f[node.KeyChangeSet] = []string{"sha256:other"} },
		"a removal":          func(f map[string][]string) { f[node.KeyAptRemove] = []string{"libfoo"} },
		"another origin":     func(f map[string][]string) { f[node.KeyAptOrigin] = []string{"Ubuntu:24.04/noble", "ppa:evil"} },
		"another node":       func(f map[string][]string) { f[node.KeyNode] = []string{"web"} },
		"another operation":  func(f map[string][]string) { f[node.KeyOperation] = []string{"op-2"} },
		"expired":            func(f map[string][]string) { f[node.KeyNow] = []string{"2026-11-12T00:00:00Z"} },
	} {
		facts := maps.Clone(approved)
		edit(facts)
		if holdAll(install.Conditions, facts) {
			t.Errorf("%s satisfies the install's conditions", name)
		}
	}
	// Unfilled facts never hold.
	if raw := mamelet("NodePackage db/nginx install-package", "apt.install"); holdAll(raw.Conditions, approved) {
		t.Error("conditions with an unfilled operation and expiry hold")
	}
	if q := mamelet("NodeService db/nginx start-service", "systemd.start"); q.Grant != spec.Standing ||
		slices.ContainsFunc(q.Conditions, func(c spec.Condition) bool { return c.Key == node.KeyOperation }) {
		t.Errorf("auto start is not a standing grant: %+v", q)
	}
	if q := mamelet("NodeService db/nginx enable-service", "systemd.enable"); q.Grant != spec.Once || len(q.StandingIf) != 0 {
		t.Errorf("enable is standing: %+v", q)
	}

	// Everything a plan uses is granted.
	granted := map[string]bool{}
	for _, gr := range s.Grants() {
		for _, pr := range gr.Privs {
			granted[gr.Path+" "+pr] = true
		}
	}
	for _, q := range append(s.Providers().Observation(s), s.PlannedRequirements(p, g, pol)...) {
		if q.Provider == "proxmox" && !granted[q.Target+" "+q.Capability] {
			t.Errorf("the plan uses %s on %s, which is not granted", q.Capability, q.Target)
		}
	}
}

// Without approval by policy, a start is a one-time grant, not a standing one.
func TestStandingOnlyWhenThePolicyDecides(t *testing.T) {
	g, _ := graph.Decode([]byte(l2))
	t0 := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	snap := state.Empty(t0)
	snap.VMs["db"] = state.VM{Managed: true, CPU: 1, MemoryMiB: 1024, DiskGiB: 10, Image: "ubuntu-24.04", ObservedAt: t0}
	snap.Packages["db/nginx"] = state.Package{Installed: true, Version: "1", ObservedAt: t0}
	snap.Services["db/nginx"] = state.Service{Exists: true, Enabled: true, ObservedAt: t0}
	pol := policy.Default
	p := plan.Build(g, snap, pol)
	var s *Site
	starts := 0
	for _, q := range s.PlannedRequirements(p, g, pol) {
		if q.Op == policy.OpStartService {
			starts++
			if q.Grant != spec.Once {
				t.Errorf("standing without the policy: %+v", q)
			}
		}
	}
	if starts != 1 {
		t.Errorf("%d start requirements, want 1", starts)
	}
}

// Every resource is analysed, not one per kind: two VMs give two sets of requirements.
func TestAnalyzeCoversEveryResource(t *testing.T) {
	g, _ := graph.Decode([]byte(l2))
	var s *Site // no site: placeholder paths
	reqs := s.Providers().Analyze(g, s)
	ops := map[string][]policy.Op{}
	for _, q := range reqs {
		if !slices.Contains(ops[q.Resource], q.Op) {
			ops[q.Resource] = append(ops[q.Resource], q.Op)
		}
	}
	for _, id := range []string{"VirtualMachine web", "VirtualMachine db", "NodePackage db/nginx", "NodeService db/nginx"} {
		if len(ops[id]) == 0 {
			t.Errorf("%s is not analysed: %v", id, ops)
		}
	}
	vm := ops["VirtualMachine web"]
	for _, op := range []policy.Op{policy.OpCreateVM, policy.OpGrowVMDisk, policy.OpResizeVMCPU, policy.OpResizeVMMemory,
		policy.OpShrinkVMDisk, policy.OpReplaceVMImage} {
		if !slices.Contains(vm, op) {
			t.Errorf("VM lacks %s: %v", op, vm)
		}
	}
	for _, q := range reqs {
		switch {
		case q.Op == policy.OpShrinkVMDisk && (q.Support != spec.Unsupported || q.Why == ""):
			t.Errorf("shrink is not shown as never carried out: %+v", q)
		case q.Provider == "proxmox" && q.Op == policy.OpCreateVM && q.Capability == "VM.Allocate" && (q.Target != "/pool/<lab pool>" || q.Resolved):
			t.Errorf("placeholder: %+v", q)
		}
	}
}

// Every operation the planner can produce for a kind is declared by the kind's provider.
func TestManifestsDeclareEveryPlannedOperation(t *testing.T) {
	var s *Site
	ps := s.Providers()
	if err := ps.Check(); err != nil {
		t.Fatal(err)
	}
	for kind, ops := range map[graph.Kind][]policy.Op{
		graph.KindVirtualMachine: {policy.OpCreateVM, policy.OpGrowVMDisk, policy.OpResizeVMCPU, policy.OpResizeVMMemory,
			policy.OpShrinkVMDisk, policy.OpReplaceVMImage},
		graph.KindNodePackage: {policy.OpInstallPackage},
		graph.KindNodeService: {policy.OpEnableService, policy.OpStartService, policy.OpDisableService, policy.OpStopService},
	} {
		m, ok := ps.For(kind)
		if !ok {
			t.Errorf("no provider for %s", kind)
			continue
		}
		for _, op := range ops {
			if _, ok := m.Operation(kind, op); !ok {
				t.Errorf("%s does not declare %s %s", m.Provider, kind, op)
			}
		}
	}
}

// Observation does not depend on the declaration: with nothing declared, Mamela still observes
// the lab (to find VMs it keeps), and needs exactly what the provider's observation calls need.
func TestObservationIsWhatTheProvidersObserve(t *testing.T) {
	s := exampleSite(t)
	empty, err := graph.Decode([]byte(`{"schema":"mamela-graph/v1","resources":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, q := range s.Providers().Observation(s) {
		got[q.Target+" "+q.Capability] = true
	}
	want := map[string]bool{}
	for _, q := range s.Requirements(proxmox.NeedsFor(pve.ObserveCalls)) {
		want[q.Target+" "+q.Capability] = true
	}
	if len(want) == 0 || !maps.Equal(got, want) {
		t.Errorf("observation needs %v, the observation calls need %v", got, want)
	}
	if reqs := s.Providers().Analyze(empty, s); len(reqs) != 0 {
		t.Errorf("an empty declaration has operation requirements: %+v", reqs)
	}
}
