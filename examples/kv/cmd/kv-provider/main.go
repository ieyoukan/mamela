// Command kv-provider is the example provider as a program. Build it for WASI:
//
//	GOOS=wasip1 GOARCH=wasm go build -o kv-provider.wasm ./examples/kv/cmd/kv-provider
package main

import (
	"github.com/ieyoukan/mamela/examples/kv/kvprovider"
	"github.com/ieyoukan/mamela/provider/sdk"
)

func main() { sdk.Serve(kvprovider.Provider{}) }
