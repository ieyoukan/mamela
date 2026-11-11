package external

import "github.com/ieyoukan/mamela/provider/protocol"

// Catalogs are the API catalogs Mamela ships (catalog = "builtin:<name>"): what each call does,
// as Mamela knows it, never as a provider says.
var Catalogs = map[string]protocol.Catalog{
	"proxmox": ProxmoxCatalog,
}

// ProxmoxCatalog is the part of the Proxmox VE API that providers of Proxmox VMs use. Effects
// follow the API documentation: creating allocates a VM and its disks; a configuration change
// with digest refuses a configuration that changed since it was read; starting and stopping
// interrupt the guest; deleting destroys its disks.
var ProxmoxCatalog = protocol.Catalog{API: "proxmox", Entries: []protocol.Entry{
	{Method: "GET", Path: "/cluster/resources", Effect: protocol.EffectRead, Query: []string{"type"}},
	{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/config", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/status/current", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status", Effect: protocol.EffectRead},
	{Method: "POST", Path: "/nodes/{node}/qemu", Effect: protocol.EffectCreate,
		// No tags: Proxmox checks them before the VM is in its pool.
		Form: []string{"vmid", "name", "cores", "memory", "pool", "scsi0", "scsihw", "ostype", "description"}},
	{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Effect: protocol.EffectChange,
		Form: []string{"cores", "memory", "tags", "description", "digest"}, Precondition: "digest"},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/start", Effect: protocol.EffectRestart},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/shutdown", Effect: protocol.EffectRestart},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/stop", Effect: protocol.EffectRestart},
	{Method: "DELETE", Path: "/nodes/{node}/qemu/{vmid}", Effect: protocol.EffectLoseData, Query: []string{"purge", "destroy-unreferenced-disks"}},
}}
