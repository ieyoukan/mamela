package main

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
	"github.com/ieyoukan/mamela/internal/api"
)

// Removing an item from the declaration that made it deletes it (ADR 0013): the plan says so and
// shows the request, nothing is deleted without an approval, the local API (GitHub's side) refuses
// to approve it and the CLI on the host does not, and only what that declaration made goes: an item
// of another declaration, one of someone else's and one made before stacks stay.
func TestRemovalFromADeclaration(t *testing.T) {
	tools := toolchainDir(t)
	kv := kvapi.New()
	srv := httptest.NewServer(kvapi.RequireToken("s3cret", kv))
	defer srv.Close()
	dir := t.TempDir()
	repo, _ := filepath.Abs("../..")
	os.WriteFile(filepath.Join(dir, "token"), []byte("s3cret\n"), 0o600)
	catalog := filepath.Join(repo, "examples/kv-demo/kv-catalog.json")
	providers := filepath.Join(dir, "mamela.providers.toml")
	os.WriteFile(providers, []byte(`[provider.kv-example]
api_url = "`+srv.URL+`"
token_file = "token"
catalog = "`+catalog+`"

[provider.kv-example.guard]
max_items = 10
`), 0o600)
	db := filepath.Join(dir, "state.db")

	// declaration writes a declaration named name with these items; its Pome is a copy of the demo's.
	declaration := func(name string, items ...string) string {
		t.Helper()
		d := filepath.Join(dir, "decl-"+strings.NewReplacer("/", "-", ".", "-").Replace(name))
		if err := os.CopyFS(d, os.DirFS(filepath.Join(repo, "examples/kv-demo"))); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
		os.RemoveAll(filepath.Join(d, "target"))
		pome := string(mustRead(t, filepath.Join(d, "Pome.toml")))
		pome = strings.ReplaceAll(pome, `"../../pome/mamela"`, `"`+filepath.Join(repo, "pome/mamela")+`"`)
		pome = strings.ReplaceAll(pome, `"../kv/pome"`, `"`+filepath.Join(repo, "examples/kv/pome")+`"`)
		pome = strings.Replace(pome, `name = "github.com/ieyoukan/mamela/examples/kv-demo"`, `name = "`+name+`"`, 1)
		os.WriteFile(filepath.Join(d, "Pome.toml"), []byte(pome), 0o644)
		var body strings.Builder
		for _, it := range items {
			body.WriteString(`    |> Kv.kv_item(KvItem(name: "` + it + `", value: "v1", labels: []))` + "\n")
		}
		os.WriteFile(filepath.Join(d, "src/main.emel"), []byte(`import Std.Console.{SystemConsole}
import Mamela
import Kv.{KvItem}

fn main() {
  with SystemConsole {
    Mamela.empty()
`+body.String()+`      |> Mamela.print
  }
}
`), 0o644)
		return d
	}
	mamela := func(args ...string) (int, string) {
		t.Helper()
		var out bytes.Buffer
		code := run(context.Background(), args, &out, &out)
		return code, out.String()
	}
	digestRe := regexp.MustCompile(`To approve: mamela approve .* (sha256:[0-9a-f]+)`)
	plan := func(d string) (string, string) {
		t.Helper()
		code, out := mamela("plan", "--db", db, "--providers", providers, "--toolchain", tools, d)
		if code != 0 {
			t.Fatalf("plan: %d\n%s", code, out)
		}
		m := digestRe.FindStringSubmatch(out)
		if m == nil {
			return "", out
		}
		return m[1], out
	}
	applyAll := func(d string) {
		t.Helper()
		digest, _ := plan(d)
		if digest == "" {
			return
		}
		mamela("approve", "--db", db, "--by", "tester", digest)
		if code, out := mamela("apply", "--db", db, "--providers", providers, "--toolchain", tools, digest); code != 0 {
			t.Fatalf("apply: %d\n%s", code, out)
		}
	}

	// Two declarations make items; one item was made before stacks were recorded; one is someone else's.
	infra := declaration("github.com/example/infra", "web", "db")
	applyAll(infra)
	applyAll(declaration("github.com/example/other", "theirs"))
	kv.Put(kvapi.Item{Name: "someone-elses", Value: "x", Owner: "someone", Version: 1})
	if got := len(kv.Items()); got != 4 {
		t.Fatalf("setup: %d items", got)
	}

	// infra drops db: the plan deletes it, and keeps the others with the reason.
	infra = declaration("github.com/example/infra", "web")
	digest, out := plan(infra)
	for _, want := range []string{"- KVItem db  [needs approval]  via kv-example", "delete-item: needs approval (MAM-POL-0310",
		"sends DELETE /items/{item}", "removed: Mamela made it for this declaration (github.com/example/infra)",
		"= KVItem theirs  [retained]", "MAM-PLAN-0302", "another declaration (github.com/example/other)", "= KVItem web  [in sync]"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "someone-elses") {
		t.Errorf("an item of someone else's is in the plan:\n%s", out)
	}
	if digest == "" {
		t.Fatalf("a plan with a deletion has no digest to approve:\n%s", out)
	}

	// Without an approval nothing is deleted.
	if code, out := mamela("apply", "--db", db, "--providers", providers, "--toolchain", tools, digest); code != 0 || !strings.Contains(out, "needs-approval") {
		t.Fatalf("apply without approval: %d\n%s", code, out)
	}
	if _, ok := kv.Items()["db"]; !ok {
		t.Fatal("db was deleted without an approval")
	}
	// GitHub's side, the local API, cannot approve it.
	b := testBackend(t, db, providers)
	err := b.Approve(context.Background(), digest, "github:bob", time.Hour)
	if !errors.Is(err, api.ErrNeedsHost) || !strings.Contains(err.Error(), "mamela approve "+digest) {
		t.Fatalf("the API approved a deletion, or does not say where it is approved: %v", err)
	}
	if ok, _ := b.Approved(context.Background(), digest); ok {
		t.Fatal("refused, yet approved")
	}
	// A person on the host can, and is told what it deletes.
	code, out := mamela("approve", "--db", db, "--by", "tester", digest)
	if code != 0 || !strings.Contains(out, "DELETES 1 object") || !strings.Contains(out, "KVItem db") {
		t.Fatalf("approve on the host: %d\n%s", code, out)
	}
	code, out = mamela("apply", "--db", db, "--providers", providers, "--toolchain", tools, digest)
	if code != 0 || !strings.Contains(out, "delete-item") || !strings.Contains(out, "KVItem db") {
		t.Fatalf("apply: %d\n%s", code, out)
	}
	items := kv.Items()
	if _, ok := items["db"]; ok {
		t.Fatal("db was not deleted")
	}
	for _, kept := range []string{"web", "theirs", "someone-elses"} {
		if _, ok := items[kept]; !ok {
			t.Errorf("%s was deleted", kept)
		}
	}
	// Mamela forgot db: declared again, it is a create.
	if _, out := plan(declaration("github.com/example/infra", "web", "db")); !strings.Contains(out, "+ KVItem db") {
		t.Errorf("db is not created again after its deletion:\n%s", out)
	}
	// An item made before stacks were recorded (its record names none) is kept, with that reason.
	kv.Put(kvapi.Item{Name: "older", Value: "v1", Owner: "mamela", Version: 1})
	if err := b.StoreForTest().RecordCreation(context.Background(), "", "kv-example", "KVItem older", map[string]string{"item": "older"}, "plan", "op"); err != nil {
		t.Fatal(err)
	}
	if _, out := plan(infra); !strings.Contains(out, "= KVItem older  [retained]") || !strings.Contains(out, "MAM-PLAN-0303") {
		t.Errorf("an item without a recorded declaration is not kept with its reason:\n%s", out)
	}
}
