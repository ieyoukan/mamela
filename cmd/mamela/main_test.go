package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if code, _, stderr := runCLI("destroy"); code != 2 || !strings.Contains(stderr, "--confirm") {
		t.Errorf("destroy without a VM: code=%d stderr=%q", code, stderr)
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
		"sha256:d108bb2c17f95992ec7b6082f65ad9849fc2fd21f39be45b735ce18df41c2ad4",
		"evaluated twice, same digest",
		"emela " + evaluate.PinnedEmela + ", wasmtime " + evaluate.PinnedWasmtime,
		"VirtualMachine  app-server",
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
	if !strings.HasPrefix(stdout, `{"resources":[{"kind":"VirtualMachine"`) || !strings.HasSuffix(stdout, `"schema":"mamela-graph/v1"}`+"\n") {
		t.Errorf("unexpected JSON: %s", stdout)
	}
}

func TestEvalReportsProblems(t *testing.T) {
	dir := toolchainDir(t)
	code, _, stderr := runCLI("eval", "--toolchain", dir, filepath.Join("..", "..", "internal", "evaluate", "testdata", "invalid-graph"))
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"3 problems", "MAM-GRAPH-0101", "MAM-GRAPH-0105", "MAM-GRAPH-0106", "hint:"} {
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
	t.Chdir(filepath.Join("..", "..", "examples", "lab-l2"))
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

func TestPlanReplayApprove(t *testing.T) {
	dir := toolchainDir(t)
	db := filepath.Join(t.TempDir(), "state.db")
	code, stdout, stderr := runCLI("plan", "--db", db, "--toolchain", dir, filepath.Join("..", "..", "examples", "homelab"))
	if code != 0 {
		t.Fatalf("plan: code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"+ VirtualMachine app-server  [needs approval]", "MAM-PLAN-0101", "Nothing has been changed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan output lacks %q:\n%s", want, stdout)
		}
	}
	i := strings.Index(stdout, "mamela approve ")
	if i < 0 {
		t.Fatalf("no approve hint:\n%s", stdout)
	}
	digest := strings.TrimSpace(stdout[i+len("mamela approve "):])

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

func TestPlanWithSnapshot(t *testing.T) {
	dir := toolchainDir(t)
	snap := filepath.Join(t.TempDir(), "snap.json")
	const body = `{"asOf":"2026-11-08T12:00:00Z","vms":{"app-server":{"managed":true,"cpu":1,"memoryMiB":1024,"diskGiB":20,"image":"ubuntu-24.04","observedAt":"2026-11-08T12:00:00Z"}},"packages":{},"services":{}}`
	if err := os.WriteFile(snap, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI("plan", "--db", filepath.Join(t.TempDir(), "s.db"), "--toolchain", dir, "--snapshot", snap,
		filepath.Join("..", "..", "examples", "homelab"))
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"[DENIED]", "diskGiB: 20 -> 10", "MAM-POL-0301", "MAM-PLAN-0103"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
}

func TestLabBootstrapPrintsScript(t *testing.T) {
	code, stdout, stderr := runCLI("lab", "bootstrap", "--site", filepath.Join("..", "..", "lab", "site.example.toml"))
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"#!/usr/bin/env bash", "set -euo pipefail", "pveum pool add mamela-lab", "PVEAuditor"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("script lacks %q", want)
		}
	}
}

func TestLabPreflightNeedsSecrets(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no token files, whatever this machine has
	code, _, stderr := runCLI("lab", "preflight", "--site", filepath.Join("..", "..", "lab", "site.example.toml"))
	if code != 1 || !strings.Contains(stderr, "mamela-audit@pve!audit") {
		t.Errorf("code=%d stderr=%s", code, stderr)
	}
}

func TestApplyNeedsALabAndAStoredPlan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db := filepath.Join(t.TempDir(), "state.db")
	site := filepath.Join("..", "..", "lab", "site.example.toml")
	code, _, stderr := runCLI("apply", "--db", db, "--site", site, "sha256:0000")
	if code != 1 || !strings.Contains(stderr, "no plan sha256:0000") {
		t.Errorf("unknown plan: code=%d stderr=%s", code, stderr)
	}
	if code, _, _ := runCLI("apply", "--db", db, "--site", site); code != 2 {
		t.Errorf("no digest: code=%d", code)
	}
}

func TestCheckJSON(t *testing.T) {
	dir := toolchainDir(t)
	code, stdout, stderr := runCLI("check", "--json", "--toolchain", dir, filepath.Join("..", "..", "examples", "lab-l2"))
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
		"VirtualMachine mamela-l2 create-vm proxmox VM.Allocate",
		"NodePackage mamela-l2/nginx install-package mamelet apt.install",
		"NodeService mamela-l2/nginx start-service mamelet systemd.start",
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
