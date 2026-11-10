package pve

import (
	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/proxmox"
)

// The API calls of each thing the provider does, for the permission catalog. They must list every
// call the code makes: TestCallsCoverWhatTheProviderSends checks them against a recording of the
// requests the provider actually sends.

// The parameters of calls that send some besides create and config.
var (
	uploadParams   = []string{"content", "checksum-algorithm", "checksum", "filename"}
	taskListParams = []string{"vmid", "typefilter", "source", "limit"}
)

// ObserveCalls are made by every observation (planning and before applying).
var ObserveCalls = []proxmox.Call{
	{Method: "GET", Path: "/cluster/resources"},
	{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/config", When: "for each VM in the lab pool"},
}

// OperationCalls are the calls of one operation, beyond the observation. ok is false for
// operations the provider does not carry out.
func OperationCalls(op policy.Op, withSeed bool) (calls []proxmox.Call, ok bool) {
	finish := []proxmox.Call{
		{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/status/current"},
		{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/resize", Params: []string{"disk", "size"}, When: "if the disk is smaller than declared", Maybe: true},
		{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/start", When: "if the VM is not running", Maybe: true},
		{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Params: []string{"description", "tags", "digest"}, When: "to mark it ready"},
		{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status"},
	}
	switch op {
	case policy.OpCreateVM:
		calls = []proxmox.Call{
			{Method: "GET", Path: "/cluster/nextid", Params: []string{"vmid"}},
			{Method: "GET", Path: "/nodes/{node}/storage/{storage}/content", Params: []string{"content"}},
			{Method: "POST", Path: "/nodes/{node}/storage/{storage}/upload", Params: uploadParams, When: "only if the image is not uploaded yet", Maybe: true},
		}
		if withSeed {
			calls = append(calls, proxmox.Call{Method: "POST", Path: "/nodes/{node}/storage/{storage}/upload", Params: uploadParams,
				When: "the VM's cloud-init seed, unless a still valid one is there", Maybe: true})
		}
		calls = append(calls,
			proxmox.Call{Method: "POST", Path: "/nodes/{node}/qemu", Params: CreateParamNames(withSeed)},
			proxmox.Call{Method: "GET", Path: "/nodes/{node}/tasks", Params: taskListParams, When: "to find the create task after a lost answer", Maybe: true},
		)
		return append(calls, finish...), true
	case policy.OpResizeVMCPU, policy.OpResizeVMMemory:
		return []proxmox.Call{
			{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Params: []string{"cores", "memory", "digest"}},
			{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/status/current"},
			{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/reboot", When: "if the VM is running (applies the change)", Maybe: true},
			{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status", Maybe: true},
		}, true
	case policy.OpGrowVMDisk:
		return []proxmox.Call{
			{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/config"},
			{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/resize", Params: []string{"disk", "size"}},
			{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status"},
		}, true
	}
	return nil, false
}

// Version is the version of the Proxmox provider's contract (its manifest).
const Version = "0.1.0"

// Manifest is the Proxmox provider's declaration of itself, derived from the calls above and the
// permission catalog. withSeed: VMs get a mamelet seed (the site runs a Control Plane).
func Manifest(withSeed bool) spec.Manifest {
	m := spec.Manifest{Schema: spec.Schema, Provider: "proxmox", Version: Version, Observe: needs(ObserveCalls)}
	k := spec.Kind{Kind: graph.KindVirtualMachine}
	for _, op := range []policy.Op{policy.OpCreateVM, policy.OpGrowVMDisk, policy.OpResizeVMCPU, policy.OpResizeVMMemory} {
		calls, _ := OperationCalls(op, withSeed)
		k.Operations = append(k.Operations, spec.Operation{Op: op, Support: spec.Supported, Needs: needs(calls)})
	}
	for _, op := range []policy.Op{policy.OpShrinkVMDisk, policy.OpReplaceVMImage} {
		k.Operations = append(k.Operations, spec.Operation{Op: op, Support: spec.Unsupported,
			Why: "it can destroy data (the policy denies it)"})
	}
	m.Kinds = []spec.Kind{k}
	return m
}

// needs turns calls into capabilities: Proxmox privileges on ACL scopes ("pool", "disk-storage";
// a site resolves them to paths).
func needs(calls []proxmox.Call) []spec.Need {
	var out []spec.Need
	for _, r := range proxmox.NeedsFor(calls) {
		out = append(out, spec.Need{Capability: r.Priv, Target: spec.Target{Type: TargetACL, Ref: string(r.Scope)},
			Always: r.Always, Reasons: r.Reasons})
	}
	return out
}

// TargetACL is the target type of Proxmox privileges: an ACL scope.
const TargetACL = "proxmox-acl"
