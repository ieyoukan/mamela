package kv_test

import (
	"net/http"
	"testing"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
	"github.com/ieyoukan/mamela/examples/kv/kvprovider"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/providertest"
)

func item(name, spec string) protocol.Resource {
	return protocol.Resource{ID: string(kvprovider.Kind) + "/" + name, Kind: kvprovider.Kind, Name: name, Spec: []byte(spec)}
}

// The example provider passes the conformance suite, as a WASI program.
func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Suite{
		Provider: providertest.BuildWASI(t, "./cmd/kv-provider"),
		Catalog:  kvapi.Catalog,
		NewAPI:   func() http.Handler { return kvapi.New() },
		Cases: []providertest.Case{
			{Name: "one-item", Desired: []protocol.Resource{item("web", `{"value":"v1"}`)}},
			{Name: "items-with-labels", Desired: []protocol.Resource{
				item("db", `{"value":"v1","labels":["prod","eu"]}`),
				item("web", `{"value":"v2"}`),
			}},
		},
	})
}
