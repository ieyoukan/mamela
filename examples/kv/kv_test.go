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
	return protocol.Resource{ID: string(kvprovider.Kind) + " " + name, Kind: kvprovider.Kind, Name: name, Spec: []byte(spec)}
}

// The example provider passes the conformance suite, as a WASI program.
func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Suite{
		Provider:      providertest.BuildWASI(t, "./cmd/kv-provider"),
		Catalog:       kvapi.Catalog,
		NewAPI:        func() http.Handler { return kvapi.New() },
		GuardSettings: []byte(`{"max_items":5}`),
		Cases: []providertest.Case{
			{Name: "one-item", Desired: []protocol.Resource{item("web", `{"value":"v1"}`)},
				Explicit: []providertest.Explicit{{Op: kvprovider.OpDelete, Resource: "KVItem web"}}},
			// Imported from Terraform (mamela import terraform): adoptable, nothing planned for it,
			// until adopt-item makes it Mamela's; then it is as declared.
			{Name: "imported", Desired: []protocol.Resource{item("legacy", `{"value":"old","labels":["a"]}`)},
				World: func(api http.Handler) {
					api.(*kvapi.Server).Put(kvapi.Item{Name: "legacy", Value: "old", Owner: "terraform", Labels: []string{"a"}, Version: 4})
				},
				Adopting: []protocol.Adopting{{Resource: "KVItem legacy", IDs: map[string]string{"item": "legacy"}}},
				Explicit: []providertest.Explicit{{Op: kvprovider.OpAdopt, Resource: "KVItem legacy"}}},
			{Name: "items-with-labels", Desired: []protocol.Resource{
				item("db", `{"value":"v1","labels":["prod","eu"]}`),
				item("web", `{"value":"v2"}`),
			}},
		},
	})
}
