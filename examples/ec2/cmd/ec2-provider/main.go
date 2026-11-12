// Command ec2-provider is the example EC2 provider as a program. examples/ec2/build.sh builds it
// into its Pome (examples/ec2/pome/provider.wasm).
package main

import (
	"github.com/ieyoukan/mamela/examples/ec2/ec2provider"
	"github.com/ieyoukan/mamela/provider/sdk"
)

func main() { sdk.Serve(ec2provider.Provider{}) }
