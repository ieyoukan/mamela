// Package catalogs holds the API catalogs Mamela ships: what each call of an API does (its
// effect, where the API reads each parameter, its precondition), as Mamela knows it. A provider
// never supplies its own: the host classifies a provider's requests with these (ADR 0002, 0004).
// Providers import them only to test against the catalog Mamela will use.
package catalogs

import "github.com/ieyoukan/mamela/provider/protocol"

// Proxmox is the part of the Proxmox VE API that providers of Proxmox VMs use. Effects
// follow the API documentation: creating allocates a VM and its disks; a configuration change
// with digest refuses a configuration that changed since it was read; starting and stopping
// interrupt the guest; deleting destroys its disks.
var Proxmox = protocol.Catalog{API: "proxmox", Entries: []protocol.Entry{
	{Method: "GET", Path: "/cluster/resources", Effect: protocol.EffectRead, Query: []string{"type"}},
	{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/config", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/status/current", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/tasks", Effect: protocol.EffectRead, Query: []string{"vmid", "typefilter", "source", "limit"}},
	{Method: "GET", Path: "/nodes/{node}/storage/{storage}/content", Effect: protocol.EffectRead, Query: []string{"content"}},
	// Uploading adds a file to a storage (an image to import, a cloud-init seed). Proxmox checks
	// the checksum before it keeps the file. The file itself is the multipart parameter "filename".
	{Method: "POST", Path: "/nodes/{node}/storage/{storage}/upload", Effect: protocol.EffectCreate,
		Form: []string{"content", "checksum-algorithm", "checksum"}, File: "filename"},
	{Method: "POST", Path: "/nodes/{node}/qemu", Effect: protocol.EffectCreate,
		// No tags: Proxmox checks them before the VM is in its pool.
		Form: []string{"vmid", "name", "cores", "memory", "pool", "scsi0", "scsihw", "ostype", "description",
			"boot", "serial0", "vga", "net0", "onboot", "ide2", "agent"}},
	{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Effect: protocol.EffectChange,
		Form: []string{"cores", "memory", "tags", "description", "digest"}, Precondition: "digest"},
	// Growing a disk keeps its data (Proxmox never shrinks one).
	{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/resize", Effect: protocol.EffectChange,
		Form: []string{"disk", "size", "digest"}, Precondition: "digest"},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/start", Effect: protocol.EffectRestart},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/reboot", Effect: protocol.EffectRestart},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/shutdown", Effect: protocol.EffectRestart},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/stop", Effect: protocol.EffectRestart},
	{Method: "DELETE", Path: "/nodes/{node}/qemu/{vmid}", Effect: protocol.EffectLoseData, Query: []string{"purge", "destroy-unreferenced-disks"}},
}}
