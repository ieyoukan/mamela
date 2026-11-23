package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
	"github.com/ieyoukan/mamela/internal/gitops"
	"github.com/ieyoukan/mamela/internal/gitops/fakegithub"
	"github.com/ieyoukan/mamela/internal/sumfile"
)

// pinnedRepo writes a repository whose declaration (infra/) has a machine and an artifact at url,
// whose sha256 is sum, or, when sum is "", is left to mamela.sum (Mamela.artifact_pinned).
func pinnedRepo(t *testing.T, url, sum string) (repo, dir string) {
	t.Helper()
	repo = t.TempDir()
	dir = filepath.Join(repo, "infra")
	root, _ := filepath.Abs("../..")
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "Pome.toml"), []byte(`[pome]
name = "example.invalid/pinned"
version = "0.1.0"
emela = "0.27.0"

[dependencies]
"github.com/ieyoukan/mamela/pome/mamela" = "v0.4.2"

[replace]
"github.com/ieyoukan/mamela/pome/mamela" = { path = "`+filepath.Join(root, "pome/mamela")+`" }
`), 0o644)
	os.WriteFile(filepath.Join(dir, "Pome.lock"), mustRead(t, filepath.Join(root, "internal/evaluate/testdata/invalid-graph/Pome.lock")), 0o644)
	artifact := `Mamela.artifact_pinned("/usr/local/bin/app", "` + url + `")`
	if sum != "" {
		artifact = `Mamela.artifact_of("/usr/local/bin/app", "` + url + `", "` + sum + `")`
	}
	os.WriteFile(filepath.Join(dir, "src", "main.emel"), []byte(`import Std.Console.{SystemConsole}
import Mamela
import Mamela.{Node}

fn main() {
  with SystemConsole {
    Mamela.empty()
      |> Mamela.provided("test", "TestVM", "web", [])
      |> Mamela.artifact_with(Node(name: "web"), `+artifact+`)
      |> Mamela.print
  }
}
`), 0o644)
	return repo, dir
}

// mamela pin downloads what the declaration leaves to mamela.sum and writes its sha256; the
// declaration then evaluates to the graph of the one that writes it. --check tells what is missing.
func TestPinThenEvaluate(t *testing.T) {
	tools := toolchainDir(t)
	body := []byte("the release's binary")
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	pinClient = srv.Client()
	defer func() { pinClient = nil }()
	url := srv.URL + "/app"
	_, dir := pinnedRepo(t, url, "")
	_, inlineDir := pinnedRepo(t, url, want)

	if code, _, errOut := runCLI("eval", "--toolchain", tools, dir); code != 1 || !strings.Contains(errOut, "MAM-EVAL-0601") ||
		!strings.Contains(errOut, url) || !strings.Contains(errOut, "mamela pin "+dir) {
		t.Errorf("eval without mamela.sum: exit %d\n%s", code, errOut)
	}
	if code, out, errOut := runCLI("pin", "--check", "--toolchain", tools, dir); code != 1 || !strings.Contains(errOut, "MAM-PIN-0301") || !strings.Contains(out, "missing  "+url) {
		t.Errorf("pin --check: exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, sumfile.Name)); !os.IsNotExist(err) {
		t.Errorf("--check wrote mamela.sum: %v", err)
	}
	code, out, errOut := runCLI("pin", "--toolchain", tools, dir)
	if code != 0 || !strings.Contains(out, "1 added, 0 removed") || !strings.Contains(out, "+ "+url+" sha256:"+want) {
		t.Fatalf("pin: exit %d\n%s%s", code, out, errOut)
	}
	if got := string(mustRead(t, filepath.Join(dir, sumfile.Name))); got != sumfile.Header+"\n"+url+" sha256:"+want+"\n" {
		t.Errorf("mamela.sum:\n%s", got)
	}
	if code, out, errOut := runCLI("pin", "--check", "--toolchain", tools, dir); code != 0 {
		t.Errorf("pin --check after pin: exit %d\n%s%s", code, out, errOut)
	}
	_, pinned, _ := runCLI("eval", "--json", "--toolchain", tools, dir)
	_, inline, _ := runCLI("eval", "--json", "--toolchain", tools, inlineDir)
	if pinned == "" || pinned != inline {
		t.Errorf("from mamela.sum:\n%s\ninline:\n%s", pinned, inline)
	}
	if code, _, _ := runCLI("pin", "--check", "--verify", dir); code != 2 {
		t.Errorf("--check with --verify: exit %d, want 2", code)
	}
}

// The Control Plane evaluates the tree mamelacd sends it, as GitHub serves it: mamela.sum, next to
// the declaration's Pome.toml, is in it, and the sha256 the declaration leaves out come from it.
// Without it, the API says what to run, without the Control Plane's paths.
func TestAPIEvaluatesWithTheTreesMamelaSum(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	socket, done := startAPI(t, ctx)
	defer func() { stop(); <-done }()
	client := api.NewClient(socket)

	gh := fakegithub.New("me/infra", "gh-token")
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	hub := &gitops.GitHub{API: ghSrv.URL, Repo: "me/infra", Token: "gh-token", HTTP: ghSrv.Client(), Now: time.Now}
	// revision sends the tree of repo, as mamelacd does, and asks for the digest of infra/.
	revision := func(repo string) (string, error) {
		t.Helper()
		sha, err := gh.AddTree(repo)
		if err != nil {
			t.Fatal(err)
		}
		tarball, err := hub.Tarball(ctx, sha)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := client.Tree(ctx, tarball)
		if err != nil {
			t.Fatal(err)
		}
		return client.Revision(ctx, ref, "infra")
	}

	const url = "https://example.org/releases/app"
	const sum = "3333333333333333333333333333333333333333333333333333333333333333"
	inlineRepo, _ := pinnedRepo(t, url, sum)
	inline, err := revision(inlineRepo)
	if err != nil {
		t.Fatal(err)
	}
	repo, dir := pinnedRepo(t, url, "")
	_, err = revision(repo)
	if err == nil || !strings.Contains(err.Error(), "MAM-EVAL-0601") || !strings.Contains(err.Error(), url) ||
		!strings.Contains(err.Error(), "mamela pin <declaration>") || strings.Contains(err.Error(), os.TempDir()) {
		t.Errorf("without mamela.sum: %v", err)
	}
	data, _ := sumfile.Format([]sumfile.Entry{{URL: url, SHA256: sum}})
	os.WriteFile(filepath.Join(dir, sumfile.Name), data, 0o644)
	got, err := revision(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got != inline {
		t.Errorf("the digest from the tree's mamela.sum is %s, the inline declaration's %s", got, inline)
	}
}
