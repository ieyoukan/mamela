package external

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
)

// deleteAsRead is a catalog a malicious provider's author ships: the kv API's, with the delete
// called a read.
const deleteAsRead = `{"schema":"mamela-catalog/v1","api":"kv-example/v1","entries":[
 {"method":"GET","path":"/items","effect":"read"},
 {"method":"GET","path":"/items/{item}","effect":"read"},
 {"method":"PUT","path":"/items/{item}","effect":"change","form":["value","owner","version"],"precondition":"version"},
 {"method":"DELETE","path":"/items/{item}","effect":"read"}]}`

// declarationWith writes a declaration's tree whose Pome.toml replaces the provider's Pome with one
// inside the tree (a PR branch can write both), and returns the tree's directory.
func declarationWith(t *testing.T, program []byte, catalog string) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "decl/Pome.toml"), `[pome]
name = "example.com/decl"
[dependencies]
"example.com/kv" = "v0.1.0"
[replace]
"example.com/kv" = { path = "../evil" }
`)
	write(t, filepath.Join(root, "evil/provider.wasm"), string(program))
	write(t, filepath.Join(root, "evil/catalogs/kv.json"), catalog)
	write(t, filepath.Join(root, "evil", ProviderFile), providerToml("kv-example", "provider.wasm", program)+"\n[catalogs]\nkv = \"catalogs/kv.json\"\n")
	return filepath.Join(root, "decl")
}

// A3 (audit 2026-11-17): the declaration's tree chooses the provider's program and the catalog the
// operator's "pome:kv" names. A branch author who can write the tree ships a program that deletes
// during observe, and a catalog that calls the delete a read: planning, which is to change
// nothing, deleted another's item with the operator's credentials.
func TestATreeCannotMakeObserveDelete(t *testing.T) {
	api := kvapi.New()
	api.Put(kvapi.Item{Name: "victim", Value: "precious", Owner: "someone", Version: 1})
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		api.ServeHTTP(w, r)
	}))
	defer srv.Close()

	program := wasm(t, "../host/testdata/rogue", "-ldflags=-X main.mode=observe-delete")
	tree := declarationWith(t, program, deleteAsRead)
	// The operator's file, as every operator has it today: the API and "pome:kv".
	cfg := Config{Provider: map[string]APIConfig{"kv-example": {APIURL: srv.URL, Catalog: "pome:kv"}}}

	pkgs, err := Discover(tree, t.TempDir())
	if err != nil {
		t.Logf("the tree's Pome is refused at discovery: %v", err)
	} else {
		rt, err := Start(t.Context(), Programs(pkgs), cfg, wasmtime(t))
		if err != nil {
			t.Logf("the tree's provider is refused at start: %v", err)
		} else {
			defer rt.Close()
			rt.SetCreations(noCreations{})
			_, err = rt.Observe(t.Context(), declaration(t), time.Unix(0, 0))
			t.Logf("observe: %v", err)
		}
	}
	if deletes.Load() != 0 || len(api.Items()) != 1 {
		t.Fatalf("planning deleted: %d DELETE requests reached the API, items now %v", deletes.Load(), api.Items())
	}
}
