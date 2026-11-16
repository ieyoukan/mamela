package host

import (
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// A3: observe lets through what the catalog calls a read. A catalog that calls a DELETE a read does
// not start a session, however it reached the host: the host checks every catalog it is given.
func TestHostRefusesACatalogWhoseReadDeletes(t *testing.T) {
	entries := slices.Clone(kvapi.Catalog.Entries)
	for i := range entries {
		if entries[i].Method == "DELETE" {
			entries[i].Effect = protocol.EffectRead
		}
	}
	exe, err := Inspect(rogue(t, "observe-delete"))
	if err != nil {
		t.Fatal(err)
	}
	api := kvapi.New()
	api.Put(kvapi.Item{Name: "victim", Value: "precious", Version: 1})
	_, err = Start(t.Context(), exe, Options{Wasmtime: wasmtime(t), Catalog: protocol.Catalog{API: kvapi.API, Entries: entries}, Upstream: Handler{Handler: api}})
	if err == nil || !strings.Contains(err.Error(), "MAM-PRV-0011") {
		t.Fatalf("a catalog whose read is a DELETE: %v", err)
	}
	if len(api.Items()) != 1 {
		t.Errorf("the API changed: %v", api.Items())
	}
	// And with the honest catalog, the same program's delete during observe is refused.
	w := must(t, rogue(t, "observe-delete"))
	w.api.Put(kvapi.Item{Name: "victim", Value: "precious", Version: 1})
	if _, _, err := w.s.Observe(t.Context(), []protocol.Resource{web}); code(err) != CodeRefused {
		t.Errorf("observe: %v", err)
	}
	if len(w.api.Items()) != 1 {
		t.Errorf("the API changed: %v", w.api.Items())
	}
}
