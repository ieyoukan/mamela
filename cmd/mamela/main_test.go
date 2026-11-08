package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if code, _, stderr := runCLI("apply"); code != 2 || !strings.Contains(stderr, `unknown command "apply"`) {
		t.Errorf("unknown command: code=%d stderr=%q", code, stderr)
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
		"emela 0.27.0, wasmtime 49.0.5",
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
	if code != 1 || !strings.Contains(stderr, "MAM-EVAL-0001") || !strings.Contains(stderr, "install-toolchain.sh") {
		t.Errorf("code=%d stderr=%s", code, stderr)
	}
}
