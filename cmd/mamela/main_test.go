package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/examples/ec2/fakeec2"
	"github.com/ieyoukan/mamela/internal/evaluate"
)

func toolchainDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("MAMELA_TOOLCHAIN_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", ".tools", "bin")
	}
	if _, err := os.Stat(filepath.Join(dir, "emela")); err != nil {
		if os.Getenv("MAMELA_REQUIRE_TOOLCHAIN") == "1" {
			t.Fatalf("toolchain required: %v", err)
		}
		t.Skipf("toolchain not installed: %v", err)
	}
	// The examples need x/json from the Pome cache; the CLI finds it through EMELA_POME_CACHE.
	cache := os.Getenv("EMELA_POME_CACHE")
	if cache == "" {
		cache, _ = filepath.Abs(filepath.Join("..", "..", ".tools", "pomes"))
	}
	if _, err := os.Stat(filepath.Join(cache, "github.com", "emela-lang", "x", "json@v0.3.1")); err != nil {
		if os.Getenv("MAMELA_REQUIRE_TOOLCHAIN") == "1" {
			t.Fatalf("Pome cache required: %v", err)
		}
		t.Skipf("dependencies not fetched (run scripts/install-toolchain.sh): %v", err)
	}
	t.Setenv("EMELA_POME_CACHE", cache)
	return dir
}

func runCLI(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUsage(t *testing.T) {
	if code, _, stderr := runCLI(); code != 2 || !strings.Contains(stderr, "Usage") {
		t.Errorf("no args: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := runCLI("delete"); code != 2 || !strings.Contains(stderr, `unknown command "delete"`) {
		t.Errorf("unknown command: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := runCLI("act"); code != 2 || !strings.Contains(stderr, "--confirm") {
		t.Errorf("act without an operation: code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := runCLI("eval"); code != 2 {
		t.Errorf("eval without a directory: code=%d", code)
	}
}

func TestEvalSummary(t *testing.T) {
	dir := toolchainDir(t)
	code, stdout, stderr := runCLI("eval", "--toolchain", dir, "--twice", filepath.Join("..", "..", "examples", "homelab"))
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"3 resources",
		"evaluated twice, same digest",
		"emela " + evaluate.PinnedEmela + ", wasmtime " + evaluate.PinnedWasmtime,
		"Ec2Instance  app-server        via provider ec2-example",
		"service nginx enabled, running on app-server",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, stdout)
		}
	}
}

func TestEvalJSONIsCanonical(t *testing.T) {
	dir := toolchainDir(t)
	code, stdout, stderr := runCLI("eval", "--toolchain", dir, "--json", filepath.Join("..", "..", "examples", "homelab"))
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.HasPrefix(stdout, `{"resources":[{"kind":"NodePackage"`) || !strings.HasSuffix(stdout, `"schema":"mamela-graph/v2"}`+"\n") {
		t.Errorf("unexpected JSON: %s", stdout)
	}
}

func TestEvalReportsProblems(t *testing.T) {
	dir := toolchainDir(t)
	code, _, stderr := runCLI("eval", "--toolchain", dir, filepath.Join("..", "..", "internal", "evaluate", "testdata", "invalid-graph"))
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"3 problems", "MAM-GRAPH-0101", "MAM-GRAPH-0103", "MAM-GRAPH-0106", "hint:"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("report lacks %q:\n%s", want, stderr)
		}
	}
}

func TestEvalMissingToolchain(t *testing.T) {
	code, _, stderr := runCLI("eval", "--toolchain", t.TempDir(), ".")
	if code != 1 || !strings.Contains(stderr, "MAM-EVAL-0001") || !strings.Contains(stderr, "mamela toolchain install") ||
		!strings.Contains(stderr, "install-toolchain.sh") {
		t.Errorf("code=%d stderr=%s", code, stderr)
	}
}

// Without flags or environment, eval finds the repository toolchain and its Pome cache from any
// directory in the repository.
func TestEvalFindsRepositoryToolchain(t *testing.T) {
	toolchainDir(t)
	t.Setenv("MAMELA_TOOLCHAIN_DIR", "")
	t.Setenv("EMELA_POME_CACHE", "")
	t.Chdir(filepath.Join("..", "..", "examples", "homelab"))
	code, stdout, stderr := runCLI("eval", ".")
	if code != 0 || !strings.Contains(stdout, "3 resources") {
		t.Errorf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	code, stdout, stderr = runCLI("toolchain")
	if code != 0 || !strings.Contains(stdout, "the repository toolchain in") || !strings.Contains(stdout, "ok: the pinned executables") ||
		!strings.Contains(stdout, filepath.Join(".tools", "pomes")) {
		t.Errorf("status: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

func TestEvalWithoutAnyToolchain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("MAMELA_TOOLCHAIN_DIR", "")
	t.Setenv("PATH", t.TempDir())
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"eval", "."}, {"toolchain", "status"}} {
		code, _, stderr := runCLI(args...)
		if code != 1 || !strings.Contains(stderr, "MAM-EVAL-0001") || !strings.Contains(stderr, "is not installed") ||
			!strings.Contains(stderr, "mamela toolchain install") {
			t.Errorf("%v: code=%d stderr=%s", args, code, stderr)
		}
	}
}

// The pinned emela and wasmtime on PATH (as mise installs them) are used when there is no
// repository toolchain; anything else on PATH is not.
func TestEvalUsesPinnedToolsOnPath(t *testing.T) {
	dir := toolchainDir(t)
	abs, _ := filepath.Abs(dir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("MAMELA_TOOLCHAIN_DIR", "")
	t.Setenv("PATH", abs)
	t.Chdir(t.TempDir())
	code, stdout, stderr := runCLI("toolchain")
	if code != 0 || !strings.Contains(stdout, "PATH (the pinned executables") {
		t.Errorf("pinned tools on PATH: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	other := t.TempDir()
	for _, name := range []string{"emela", "wasmtime"} {
		os.WriteFile(filepath.Join(other, name), []byte("#!/bin/sh\necho "+name+" 0.0.0\n"), 0o755)
	}
	t.Setenv("PATH", other)
	if code, _, stderr := runCLI("toolchain"); code != 1 || !strings.Contains(stderr, "is not installed") {
		t.Errorf("other tools on PATH: code=%d stderr=%s", code, stderr)
	}
}

func TestToolchainStatusRefusesOtherBuilds(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"emela", "wasmtime"} {
		os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho "+name+" 0.0.0\n"), 0o755)
	}
	code, _, stderr := runCLI("toolchain", "status", "--toolchain", dir)
	if code != 1 || !strings.Contains(stderr, "is not the pinned emela") {
		t.Errorf("code=%d stderr=%s", code, stderr)
	}
}

// fakeEC2 serves the example EC2 API and returns a mamela.providers.toml for it (requests are
// signed with a fake key).
func fakeEC2(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(fakeec2.New())
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret"), []byte("fake-secret\n"), 0o600)
	config := filepath.Join(dir, "mamela.providers.toml")
	os.WriteFile(config, []byte(`[provider.ec2-example]
api_url = "`+srv.URL+`/"
auth = "aws-sigv4"
token_id = "AKIAFAKE"
token_file = "`+filepath.Join(dir, "secret")+`"
region = "ap-northeast-1"
service = "ec2"
catalog = "pome:ec2"
`), 0o600)
	return config
}

func TestPlanReplayApprove(t *testing.T) {
	dir := toolchainDir(t)
	db := filepath.Join(t.TempDir(), "state.db")
	code, stdout, stderr := runCLI("plan", "--db", db, "--providers", fakeEC2(t), "--toolchain", dir, filepath.Join("..", "..", "examples", "homelab"))
	if code != 0 {
		t.Fatalf("plan: code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"+ Ec2Instance app-server  [needs approval]  via ec2-example", "MAM-PLAN-0101", "Nothing has been changed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan output lacks %q:\n%s", want, stdout)
		}
	}
	i := strings.Index(stdout, "mamela approve ")
	if i < 0 {
		t.Fatalf("no approve hint:\n%s", stdout)
	}
	hint := strings.Fields(stdout[i+len("mamela approve "):])
	digest := hint[len(hint)-1]
	if !strings.Contains(stdout, "--db "+shellQuote(db)) {
		t.Fatal("approval hint lost the database")
	}

	if code, out, errOut := runCLI("plan", "--db", db, "--replay", digest); code != 0 || !strings.Contains(out, "recomputes to the same digest") {
		t.Errorf("replay: code=%d out=%s err=%s", code, out, errOut)
	}
	if code, out, errOut := runCLI("approve", "--db", db, "--by", "alice", digest); code != 0 || !strings.Contains(out, "approved "+digest+" by alice") {
		t.Errorf("approve: code=%d out=%s err=%s", code, out, errOut)
	}
	if code, _, _ := runCLI("approve", "--db", db, "--by", "alice", "sha256:unknown"); code != 1 {
		t.Errorf("approving an unknown plan: code=%d", code)
	}
}

// A snapshot of the built-in observation is read strictly: one with VMs (before ADR 0006) is
// refused, not half-read.
func TestPlanWithSnapshot(t *testing.T) {
	dir := toolchainDir(t)
	snap := filepath.Join(t.TempDir(), "snap.json")
	plan := func(body string) (int, string, string) {
		if err := os.WriteFile(snap, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return runCLI("plan", "--db", filepath.Join(t.TempDir(), "s.db"), "--providers", fakeEC2(t), "--toolchain", dir, "--snapshot", snap,
			filepath.Join("..", "..", "examples", "homelab"))
	}
	if code, stdout, stderr := plan(`{"asOf":"2026-11-08T12:00:00Z","packages":{},"services":{}}`); code != 0 || !strings.Contains(stdout, "MAM-PLAN-0101") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if code, _, stderr := plan(`{"asOf":"2026-11-08T12:00:00Z","vms":{},"packages":{},"services":{}}`); code != 1 || !strings.Contains(stderr, "vms") {
		t.Fatalf("a snapshot with VMs: code=%d stderr=%s", code, stderr)
	}
}

func TestApplyNeedsAStoredPlan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir()) // no mamela.site.toml here
	db := filepath.Join(t.TempDir(), "state.db")
	code, _, stderr := runCLI("apply", "--db", db, "sha256:0000")
	if code != 1 || !strings.Contains(stderr, "no plan sha256:0000") {
		t.Errorf("unknown plan: code=%d stderr=%s", code, stderr)
	}
	if code, _, _ := runCLI("apply", "--db", db); code != 2 {
		t.Errorf("no digest: code=%d", code)
	}
	if code, _, stderr := runCLI("apply", "--db", db, "--site", "missing.toml", "sha256:0000"); code != 1 || !strings.Contains(stderr, "missing.toml") {
		t.Errorf("a site named and missing: code=%d stderr=%s", code, stderr)
	}
}

func TestCheckJSON(t *testing.T) {
	dir := toolchainDir(t)
	code, stdout, stderr := runCLI("check", "--json", "--toolchain", dir, filepath.Join("..", "..", "examples", "homelab"))
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	var out CheckResult
	if err := json.Unmarshal([]byte(stdout), &out, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if out.Schema != CheckSchema || out.Graph == "" || len(out.Diagnostics) != 0 || len(out.Providers) != 2 || len(out.Observation) == 0 {
		t.Errorf("unexpected: %+v", out)
	}
	found := map[string]bool{}
	for _, q := range out.Requirements {
		found[q.Resource+" "+string(q.Op)+" "+q.Provider+" "+q.Capability] = true
	}
	for _, want := range []string{
		"Ec2Instance app-server create-vm ec2-example ec2:RunInstances",
		"NodePackage app-server/nginx install-package mamelet apt.install",
		"NodeService app-server/nginx start-service mamelet systemd.start",
	} {
		if !found[want] {
			t.Errorf("requirements lack %q", want)
		}
	}

	code, stdout, _ = runCLI("check", "--json", "--toolchain", dir, filepath.Join("..", "..", "internal", "evaluate", "testdata", "invalid-graph"))
	out = CheckResult{}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || code != 1 || len(out.Diagnostics) != 3 ||
		out.Diagnostics[0].Severity != "error" || !strings.HasPrefix(out.Diagnostics[0].Code, "MAM-GRAPH-") || out.Diagnostics[0].Resource == "" {
		t.Errorf("invalid graph: code=%d %+v %v", code, out.Diagnostics, err)
	}
}
