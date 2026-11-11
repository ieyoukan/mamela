package external

import (
	"github.com/ieyoukan/mamela/provider/catalogs"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Catalogs are the API catalogs Mamela ships (catalog = "builtin:<name>"): what each call does,
// as Mamela knows it, never as a provider says.
var Catalogs = map[string]protocol.Catalog{
	"proxmox": ProxmoxCatalog,
}

// ProxmoxCatalog is Mamela's Proxmox VE catalog (provider/catalogs).
var ProxmoxCatalog = catalogs.Proxmox
