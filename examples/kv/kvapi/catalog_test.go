package kvapi

import (
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// The catalog file the demo configures is this catalog.
func TestCatalogFileIsTheCatalog(t *testing.T) {
	data, err := os.ReadFile("../../kv-demo/kv-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var c protocol.Catalog
	if err := json.Unmarshal(data, &c, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(Catalog, json.Deterministic(true))
	got, _ := json.Marshal(c, json.Deterministic(true))
	if string(got) != string(want) {
		t.Errorf("examples/kv-demo/kv-catalog.json:\n%s\nwant\n%s", got, want)
	}
	if err := Catalog.Check(); err != nil {
		t.Error(err)
	}
}
