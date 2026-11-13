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
	"time"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
	"github.com/ieyoukan/mamela/internal/gitops"
	"github.com/ieyoukan/mamela/internal/gitops/fakegithub"
)

// A repository of declarations drives Mamela through pull requests (ADR 0010, 0012), with mamelacd
// asking `mamela api` over its socket: the plan of a trusted author's pull request is commented;
// only an approver's comment with its exact digest approves it; merging applies it when the
// declaration at the head of the branch is the one approved, and plans the head when it is not;
// forks and other authors are ignored; nothing is done twice.
func TestGitops(t *testing.T) {
	tools := toolchainDir(t)
	api := kvapi.New()
	srv := httptest.NewServer(kvapi.RequireToken("s3cret", api))
	defer srv.Close()
	gh := fakegithub.New("me/infra", "gh-token")
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()

	dir := t.TempDir()
	repo, _ := filepath.Abs("../..")
	os.WriteFile(filepath.Join(dir, "kv-token"), []byte("s3cret\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "gh-token"), []byte("gh-token\n"), 0o600)
	providers := filepath.Join(dir, "mamela.providers.toml")
	os.WriteFile(providers, []byte(`[provider.kv-example]
api_url = "`+srv.URL+`"
token_file = "kv-token"
catalog = "pome:kv"

[provider.kv-example.guard]
max_items = 10
`), 0o600)
	// The kv Pome ships no catalog: point at the demo's file instead.
	cat, _ := filepath.Abs("../../examples/kv-demo/kv-catalog.json")
	os.WriteFile(providers, bytes.Replace(mustRead(t, providers), []byte(`"pome:kv"`), []byte(`"`+cat+`"`), 1), 0o600)
	// The Control Plane's API, on a socket (in a short directory: socket paths are limited).
	sockDir, err := os.MkdirTemp("", "mapi")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	socket := filepath.Join(sockDir, "api.sock")
	db := filepath.Join(dir, "state.db")
	apiCtx, stopAPI := context.WithCancel(context.Background())
	apiDone := make(chan string)
	go func() {
		var out bytes.Buffer
		run(apiCtx, []string{"api", "--socket", socket, "--db", db, "--providers", providers, "--site", "", "--toolchain", tools}, &out, &out)
		apiDone <- out.String()
	}()
	defer func() { stopAPI(); <-apiDone }()
	for i := 0; ; i++ {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if i > 200 {
			t.Fatal("mamela api did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	config := filepath.Join(dir, "mamelacd.toml")
	os.WriteFile(config, []byte(`repo = "me/infra"
branch = "main"
dirs = ["infra"]
api_url = "`+ghSrv.URL+`"
token_file = "`+filepath.Join(dir, "gh-token")+`"
authors = ["alice"]
approvers = ["alice", "bob"]
mamela_socket = "`+socket+`"
state_db = "`+filepath.Join(dir, "mamelacd.db")+`"
`), 0o600)

	// A commit: the repository with the kv demo declaring web = value.
	commit := func(value string) string {
		t.Helper()
		tree := filepath.Join(t.TempDir(), "repo")
		if err := os.CopyFS(filepath.Join(tree, "infra"), os.DirFS("../../examples/kv-demo")); err != nil {
			t.Fatal(err)
		}
		os.RemoveAll(filepath.Join(tree, "infra", "target"))
		pome := mustRead(t, filepath.Join(tree, "infra/Pome.toml"))
		pome = bytes.ReplaceAll(pome, []byte(`"../../pome/mamela"`), []byte(`"`+filepath.Join(repo, "pome/mamela")+`"`))
		pome = bytes.ReplaceAll(pome, []byte(`"../kv/pome"`), []byte(`"`+filepath.Join(repo, "examples/kv/pome")+`"`))
		os.WriteFile(filepath.Join(tree, "infra/Pome.toml"), pome, 0o644)
		os.WriteFile(filepath.Join(tree, "infra/src/main.emel"), []byte(`import Std.Console.{SystemConsole}
import Mamela
import Kv.{KvItem}

fn main() {
  with SystemConsole {
    Mamela.empty() |> Kv.kv_item(KvItem(name: "web", value: "`+value+`", labels: [])) |> Mamela.print
  }
}
`), 0o644)
		sha, err := gh.AddTree(tree)
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	gh.SetBranch("main", commit("v0")) // pushed, not merged from a pull request: not planned
	engine, closeEngine, err := gitops.Open(context.Background(), config, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer closeEngine()
	once := func() {
		t.Helper()
		if err := engine.Once(context.Background()); err != nil {
			t.Fatalf("mamelacd once: %v", err)
		}
	}
	digestRe := regexp.MustCompile(`/mamela approve (sha256:[0-9a-f]{64})`)
	lastPlan := func(pr int) string {
		t.Helper()
		cs := gh.Comments(pr)
		for i := len(cs) - 1; i >= 0; i-- {
			if m := digestRe.FindStringSubmatch(cs[i].Body); m != nil && cs[i].Bot {
				return m[1]
			}
		}
		t.Fatalf("#%d: no plan commented: %+v", pr, cs)
		return ""
	}
	said := func(pr int, want string) bool {
		for _, c := range gh.Comments(pr) {
			if c.Bot && strings.Contains(c.Body, want) {
				return true
			}
		}
		return false
	}

	// #1 by a trusted author; #2 from a fork; #3 by someone else: only #1 is planned.
	head := commit("v1")
	gh.Open(fakegithub.PR{Number: 1, Author: "alice", Base: "main", HeadSHA: head})
	gh.Open(fakegithub.PR{Number: 2, Author: "alice", Fork: true, Base: "main", HeadSHA: commit("evil")})
	gh.Open(fakegithub.PR{Number: 3, Author: "mallory", Base: "main", HeadSHA: commit("evil")})
	once()
	digest := lastPlan(1)
	if !said(1, "sends PUT /items/{item}") || !said(1, "It is applied when the pull request is merged") {
		t.Fatalf("the plan comment: %+v", gh.Comments(1))
	}
	if len(gh.Comments(2)) != 0 || len(gh.Comments(3)) != 0 {
		t.Fatal("a fork's or an untrusted author's pull request was planned")
	}
	if s := gh.Statuses(head); len(s) != 1 || s[0].State != "pending" || s[0].Context != "mamela/infra" {
		t.Fatalf("statuses: %+v", s)
	}
	n := len(gh.Comments(1))
	once()
	if len(gh.Comments(1)) != n {
		t.Fatal("the same head was planned twice")
	}

	// Approvals: a non-approver, a wrong digest, then an approver.
	gh.Say(1, "mallory", "/mamela approve "+digest)
	gh.Say(1, "alice", "/mamela approve sha256:"+strings.Repeat("ab", 32))
	once()
	if !said(1, "@mallory is not an approver") || !said(1, "not a plan Mamela posted on this pull request") {
		t.Fatalf("refusals: %+v", gh.Comments(1))
	}
	if len(api.Items()) != 0 {
		t.Fatal("something was applied without approval")
	}
	gh.Say(1, "bob", "/mamela approve "+digest)
	once()
	if !said(1, "Approved `"+digest+"`") || len(api.Items()) != 0 {
		t.Fatalf("approval: %+v", gh.Comments(1))
	}

	// Merged as the same declaration: applied.
	merge := commit("v1")
	gh.Merge(1, merge)
	once()
	if api.Items()["web"].Value != "v1" || !said(1, "### Mamela apply: `infra` applied") {
		t.Fatalf("after the merge: %+v\n%+v", api.Items(), gh.Comments(1))
	}
	if s := gh.Statuses(merge); len(s) == 0 || s[len(s)-1].State != "success" {
		t.Fatalf("merge statuses: %+v", s)
	}
	n = len(gh.Comments(1))
	once()
	if len(gh.Comments(1)) != n || api.Items()["web"].Version != 1 {
		t.Fatal("the merge was applied twice")
	}

	// #4: approved at its head, but main moved: the head is another declaration, planned and
	// applied once approved.
	gh.Open(fakegithub.PR{Number: 4, Author: "alice", Base: "main", HeadSHA: commit("v2")})
	once()
	gh.Say(4, "alice", "/mamela approve "+lastPlan(4))
	once()
	gh.Merge(4, commit("v3"))
	once()
	if !said(4, "is not the one planned") || api.Items()["web"].Value != "v1" {
		t.Fatalf("a moved main: %+v %+v", gh.Comments(4), api.Items())
	}
	gh.Say(4, "alice", "/mamela approve "+lastPlan(4))
	once()
	if api.Items()["web"].Value != "v3" || !said(4, "applied") {
		t.Fatalf("after approving the merge: %+v\n%+v", api.Items(), gh.Comments(4))
	}
	// An applied plan cannot be approved again.
	gh.Say(4, "alice", "/mamela approve "+lastPlan(4))
	once()
	if !said(4, "it was applied already") {
		t.Fatalf("approving an applied plan: %+v", gh.Comments(4))
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
