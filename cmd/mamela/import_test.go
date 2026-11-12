package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
)

// What Terraform manages moves to Mamela: import writes the declarations and records them to adopt;
// plan changes nothing of them; adopting each makes it Mamela's, and then it is in sync.
func TestImportTerraform(t *testing.T) {
	tools := toolchainDir(t)
	api := kvapi.New()
	api.Put(kvapi.Item{Name: "legacy", Value: "old", Owner: "terraform", Labels: []string{"a", "b"}, Version: 7})
	api.Put(kvapi.Item{Name: "theirs", Value: "x", Owner: "someone", Version: 1})
	srv := httptest.NewServer(kvapi.RequireToken("s3cret", api))
	defer srv.Close()
	dir := t.TempDir()

	// The declaration: a copy of examples/kv-demo, with no resources of its own yet.
	repo, _ := filepath.Abs("../..")
	decl := filepath.Join(dir, "decl")
	if err := os.CopyFS(decl, os.DirFS("../../examples/kv-demo")); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(decl, "target"))
	pome, _ := os.ReadFile(filepath.Join(decl, "Pome.toml"))
	pome = bytes.ReplaceAll(pome, []byte(`"../../pome/mamela"`), []byte(`"`+filepath.Join(repo, "pome/mamela")+`"`))
	pome = bytes.ReplaceAll(pome, []byte(`"../kv/pome"`), []byte(`"`+filepath.Join(repo, "examples/kv/pome")+`"`))
	os.WriteFile(filepath.Join(decl, "Pome.toml"), pome, 0o644)
	os.WriteFile(filepath.Join(dir, "token"), []byte("s3cret\n"), 0o600)
	config := filepath.Join(dir, "mamela.providers.toml")
	os.WriteFile(config, []byte(`[provider.kv-example]
api_url = "`+srv.URL+`"
token_file = "token"
catalog = "`+filepath.Join(decl, "kv-catalog.json")+`"

[provider.kv-example.guard]
max_items = 10
`), 0o600)
	state := filepath.Join(dir, "state.json")
	os.WriteFile(state, []byte(`{"format_version":"1.0","values":{"root_module":{"resources":[
	  {"address":"kv_item.legacy","mode":"managed","type":"kv_item","name":"legacy","values":{"id":"legacy","value":"old","labels":["b","a"]}},
	  {"address":"kv_item.noid","mode":"managed","type":"kv_item","name":"noid","values":{"value":"?"}},
	  {"address":"random_pet.p","mode":"managed","type":"random_pet","name":"p","values":{"id":"x"}}]}}}`), 0o644)
	db := filepath.Join(dir, "state.db")
	mamela := func(args ...string) (int, string) {
		t.Helper()
		var out bytes.Buffer
		code := run(context.Background(), args, &out, &out)
		return code, out.String()
	}
	imp := func(args ...string) (int, string) {
		return mamela(append([]string{"import", "terraform", "--db", db, "--toolchain", tools, "--by", "tester", "--state", state}, args...)...)
	}

	if code, out := imp("--dry-run", decl); code != 0 || !strings.Contains(out, "KVItem legacy") || !strings.Contains(out, "--dry-run: nothing was written") {
		t.Fatalf("dry run: %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(decl, "src/imported.emel")); err == nil {
		t.Fatal("--dry-run wrote the file")
	}
	code, out := imp(decl)
	for _, want := range []string{
		"1 of 3 resources import", "kv_item.legacy", "KVItem legacy (provider kv-example)",
		"kv_item.noid", "no id: the state does not say which item it is",
		"random_pet.p", "no provider of the declaration imports its type",
		`mamela act --confirm legacy adopt-item "KVItem legacy"`, "terraform state rm 'kv_item.legacy'",
	} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("import: no %q: %d\n%s", want, code, out)
		}
	}
	src, _ := os.ReadFile(filepath.Join(decl, "src/imported.emel"))
	if !strings.Contains(string(src), `kv_item_legacy = KvItem(name: "legacy", value: "old", labels: ["a", "b"])`) {
		t.Fatalf("imported.emel:\n%s", src)
	}
	if code, out := imp(decl); code != 1 || !strings.Contains(out, "MAM-IMP-0002") {
		t.Fatalf("importing over the file: %d\n%s", code, out)
	}

	// The declaration uses what was imported.
	os.WriteFile(filepath.Join(decl, "src/main.emel"), []byte(`import Std.Console.{SystemConsole}
import Mamela
import Imported

fn main() {
  with SystemConsole {
    Mamela.empty() |> Imported.imported |> Mamela.print
  }
}
`), 0o644)
	planDigest := regexp.MustCompile(`mamela approve (sha256:[0-9a-f]+)`)
	plan := func() string {
		t.Helper()
		code, out := mamela("plan", "--db", db, "--providers", config, "--toolchain", tools, decl)
		if code != 0 {
			t.Fatalf("plan: %d\n%s", code, out)
		}
		return out
	}
	if out := plan(); !strings.Contains(out, "MAM-PLAN-0114") || !strings.Contains(out, "KVItem legacy") || planDigest.MatchString(out) && strings.Contains(out, "PUT") {
		t.Fatalf("before adopting, the plan:\n%s", out)
	}
	act := func(args ...string) (int, string) {
		return mamela(append([]string{"act", "--db", db, "--providers", config, "--toolchain", tools, "--by", "tester"}, args...)...)
	}
	if code, out := act("--confirm", "legacy", "adopt-item", "KVItem legacy", decl); code != 0 || !strings.Contains(out, "owner=mamela") {
		t.Fatalf("adopt: %d\n%s", code, out)
	}
	if it := api.Items()["legacy"]; it.Owner != "mamela" || it.Value != "old" {
		t.Fatalf("after adopting: %+v", it)
	}
	if out := plan(); !strings.Contains(out, "= KVItem legacy  [in sync]") {
		t.Fatalf("after adopting, the plan:\n%s", out)
	}
	if code, out := act("--confirm", "legacy", "adopt-item", "KVItem legacy", decl); code != 1 || !strings.Contains(out, "MAM-ACT-0103") {
		t.Fatalf("adopting again: %d\n%s", code, out)
	}
	if api.Items()["theirs"].Version != 1 {
		t.Fatal("someone else's item changed")
	}
}
