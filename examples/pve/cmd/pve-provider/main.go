// Command pve-provider is the Proxmox VM provider as a program. examples/pve/build.sh builds it
// into its Pome (examples/pve/pome/provider.wasm).
package main

import (
	"github.com/ieyoukan/mamela/examples/pve/pveprovider"
	"github.com/ieyoukan/mamela/provider/sdk"
)

func main() { sdk.Serve(pveprovider.Provider{}) }
