package pve

import (
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/proxmox"
)

// The API calls of each thing the provider does, for the permission catalog. They must list every
// call the code makes: TestCallsCoverWhatTheProviderSends checks them against a recording of the
// requests the provider actually sends.

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
		{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/resize", When: "if the disk is smaller than declared", Maybe: true},
		{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/start", When: "if the VM is not running", Maybe: true},
		{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Params: []string{"description", "tags", "digest"}, When: "to mark it ready"},
		{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status"},
	}
	switch op {
	case policy.OpCreateVM:
		calls = []proxmox.Call{
			{Method: "GET", Path: "/cluster/nextid"},
			{Method: "GET", Path: "/nodes/{node}/storage/{storage}/content"},
			{Method: "POST", Path: "/nodes/{node}/storage/{storage}/upload", When: "only if the image is not uploaded yet", Maybe: true},
		}
		if withSeed {
			calls = append(calls, proxmox.Call{Method: "POST", Path: "/nodes/{node}/storage/{storage}/upload",
				When: "the VM's cloud-init seed, unless a still valid one is there", Maybe: true})
		}
		calls = append(calls,
			proxmox.Call{Method: "POST", Path: "/nodes/{node}/qemu", Params: CreateParamNames(withSeed)},
			proxmox.Call{Method: "GET", Path: "/nodes/{node}/tasks", When: "to find the create task after a lost answer", Maybe: true},
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
			{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/resize"},
			{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status"},
		}, true
	}
	return nil, false
}
