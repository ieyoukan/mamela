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

// kvLab is a Control Plane's database and a provider (the kv example, over a fake API) for tests of
// the commands: declarations are written to disk and driven through plan, approve and apply as a
// person would.
type kvLab struct {
	t         *testing.T
	tools     string
	kv        *kvapi.Server
	dir       string
	providers string
	db        string
	repo      string
}

func newKVLab(t *testing.T) *kvLab {
	t.Helper()
	l := &kvLab{t: t, tools: toolchainDir(t), kv: kvapi.New(), dir: t.TempDir()}
	srv := httptest.NewServer(kvapi.RequireToken("s3cret", l.kv))
	t.Cleanup(srv.Close)
	l.repo, _ = filepath.Abs("../..")
	os.WriteFile(filepath.Join(l.dir, "token"), []byte("s3cret\n"), 0o600)
	l.providers = filepath.Join(l.dir, "mamela.providers.toml")
	os.WriteFile(l.providers, []byte(`[provider.kv-example]
api_url = "`+srv.URL+`"
token_file = "token"
catalog = "`+filepath.Join(l.repo, "examples/kv-demo/kv-catalog.json")+`"

[provider.kv-example.guard]
max_items = 10
`), 0o600)
	l.db = filepath.Join(l.dir, "state.db")
	return l
}

// declaration writes a declaration named name with these items; its Pome is a copy of the demo's.
func (l *kvLab) declaration(name string, items ...string) string {
	l.t.Helper()
	d := filepath.Join(l.dir, "decl-"+strings.NewReplacer("/", "-", ".", "-").Replace(name))
	if err := os.CopyFS(d, os.DirFS(filepath.Join(l.repo, "examples/kv-demo"))); err != nil && !os.IsExist(err) {
		l.t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(d, "target"))
	pome := string(mustRead(l.t, filepath.Join(d, "Pome.toml")))
	pome = strings.ReplaceAll(pome, `"../../pome/mamela"`, `"`+filepath.Join(l.repo, "pome/mamela")+`"`)
	pome = strings.ReplaceAll(pome, `"../kv/pome"`, `"`+filepath.Join(l.repo, "examples/kv/pome")+`"`)
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

func (l *kvLab) mamela(args ...string) (int, string) {
	l.t.Helper()
	var out bytes.Buffer
	code := run(context.Background(), args, &out, &out)
	return code, out.String()
}

var planDigestRe = regexp.MustCompile(`To approve: mamela approve .* (sha256:[0-9a-f]+)`)

// plan plans the declaration (with extra flags) and returns the digest to approve ("" when there is
// nothing to change) and what was printed.
func (l *kvLab) plan(d string, flags ...string) (string, string) {
	l.t.Helper()
	args := append([]string{"plan", "--db", l.db, "--providers", l.providers, "--toolchain", l.tools}, flags...)
	code, out := l.mamela(append(args, d)...)
	if code != 0 {
		l.t.Fatalf("plan: %d\n%s", code, out)
	}
	if m := planDigestRe.FindStringSubmatch(out); m != nil {
		return m[1], out
	}
	return "", out
}

func (l *kvLab) approve(digest string) (int, string) {
	return l.mamela("approve", "--db", l.db, "--by", "tester", digest)
}

func (l *kvLab) apply(digest string, flags ...string) (int, string) {
	args := append([]string{"apply", "--db", l.db, "--providers", l.providers, "--toolchain", l.tools}, flags...)
	return l.mamela(append(args, digest)...)
}

// applyAll plans, approves and applies the declaration.
func (l *kvLab) applyAll(d string) {
	l.t.Helper()
	digest, _ := l.plan(d)
	if digest == "" {
		return
	}
	l.approve(digest)
	if code, out := l.apply(digest); code != 0 {
		l.t.Fatalf("apply: %d\n%s", code, out)
	}
}
