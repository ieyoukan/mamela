package evaluate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
)

// toolchainForTest returns the pinned toolchain, or skips when it is not installed.
// Set MAMELA_REQUIRE_TOOLCHAIN=1 (as CI does) to fail instead of skipping.
func toolchainForTest(t *testing.T) Toolchain {
	t.Helper()
	dir := os.Getenv("MAMELA_TOOLCHAIN_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", ".tools", "bin")
	}
	tc, err := ToolchainInDir(dir)
	if err != nil {
		if os.Getenv("MAMELA_REQUIRE_TOOLCHAIN") == "1" {
			t.Fatalf("toolchain required: %v", err)
		}
		t.Skipf("toolchain not installed (run scripts/install-toolchain.sh): %v", err)
	}
	return tc
}

func testEvaluator(t *testing.T) *Evaluator {
	t.Helper()
	return &Evaluator{
		Toolchain: toolchainForTest(t),
		Limits: Limits{
			BuildTimeout:   2 * time.Minute,
			RunTimeout:     2 * time.Second,
			MaxMemoryBytes: 64 << 20,
			MaxStdoutBytes: 1 << 20,
			MaxStderrBytes: 16 << 10,
		},
	}
}

func TestEvaluateHomelab(t *testing.T) {
	t.Parallel()
	e := testEvaluator(t)
	res, err := e.EvaluateTwice(context.Background(), filepath.Join("..", "..", "examples", "homelab"))
	if err != nil {
		t.Fatalf("EvaluateTwice: %v", err)
	}
	if got := len(res.Graph.Resources); got != 3 {
		t.Errorf("got %d resources, want 3", got)
	}
	// The same digest as the Go-only fixture in internal/graph: Emela output and the Go
	// canonical form agree byte for byte.
	const want = "sha256:d108bb2c17f95992ec7b6082f65ad9849fc2fd21f39be45b735ce18df41c2ad4"
	if res.Digest != want {
		t.Errorf("digest = %s, want %s", res.Digest, want)
	}
	if res.Versions.Emela != PinnedEmela || res.Versions.Wasmtime != PinnedWasmtime {
		t.Errorf("versions = %+v", res.Versions)
	}
	for _, im := range res.Imports {
		if !strings.HasPrefix(im.Name, "wasi:cli/") {
			t.Errorf("unexpected import %s", im.Name)
		}
	}
}

func TestBuildIsReproducible(t *testing.T) {
	t.Parallel()
	e := testEvaluator(t)
	dir := filepath.Join("..", "..", "examples", "homelab")
	a, err := e.Evaluate(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Evaluate(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.WasmDigest != b.WasmDigest {
		t.Errorf("two builds of the same source gave different main.wasm: %s vs %s", a.WasmDigest, b.WasmDigest)
	}
}

func TestEvaluateAcceptsDiagnosticsOnStderr(t *testing.T) {
	t.Parallel()
	e := testEvaluator(t)
	res, err := e.Evaluate(context.Background(), filepath.Join("testdata", "diagnostics-on-stderr"))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(res.Graph.Resources) != 0 {
		t.Errorf("want an empty graph, got %d resources", len(res.Graph.Resources))
	}
	if !strings.Contains(res.Stderr, "nothing declared yet") {
		t.Errorf("stderr not kept: %q", res.Stderr)
	}
}

func TestEvaluateRejects(t *testing.T) {
	tests := []struct {
		dir    string
		code   Code
		graph  graph.Code // set when the failure is in the graph itself
		detail string     // a substring the diagnostics must contain, to pin the reason
	}{
		{dir: "uses-env", code: CodeImportDenied},
		{dir: "uses-random", code: CodeImportDenied},
		{dir: "uses-clock", code: CodeImportDenied},
		{dir: "infinite-loop", code: CodeTimeout, detail: "wasm trap: interrupt"},
		{dir: "huge-output", code: CodeOutputLimit},
		{dir: "memory-hog", code: CodeMemoryLimit, detail: "out of memory"},
		{dir: "fails", code: CodeProgramError, detail: "the declaration is incomplete"},
		{dir: "defect", code: CodeProgramDefect, detail: "division by zero"},
		{dir: "compile-error", code: CodeBuildFailed, detail: "undefined_function"},
		// The dependency is not cached and fetching is off: the build must not reach the network.
		{dir: "needs-fetch", code: CodeBuildFailed, detail: "nowhere to get it from"},
		{dir: "not-json", graph: graph.CodeInvalidJSON},
		{dir: "two-documents", graph: graph.CodeTrailingData},
		{dir: "invalid-graph", graph: graph.CodeInvalidName},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			t.Parallel()
			e := testEvaluator(t)
			start := time.Now()
			res, err := e.Evaluate(context.Background(), filepath.Join("testdata", tt.dir))
			if err == nil {
				t.Fatalf("accepted; graph digest %s", res.Digest)
			}
			if tt.graph != "" {
				var ge *graph.Error
				if !errors.As(err, &ge) || !ge.Has(tt.graph) {
					t.Fatalf("want graph error %s, got: %v", tt.graph, err)
				}
				return
			}
			var ee *Error
			if !errors.As(err, &ee) || ee.Code != tt.code {
				t.Fatalf("want %s, got: %v", tt.code, err)
			}
			if ee.Message == "" {
				t.Error("error without a message")
			}
			if tt.detail != "" && !strings.Contains(ee.Detail, tt.detail) {
				t.Errorf("diagnostics do not mention %q:\n%s", tt.detail, ee.Detail)
			}
			if elapsed := time.Since(start); elapsed > 30*time.Second {
				t.Errorf("took %s; limits did not stop it promptly", elapsed)
			}
		})
	}
}

func TestImportDeniedIsNotRun(t *testing.T) {
	t.Parallel()
	e := testEvaluator(t)
	_, err := e.Evaluate(context.Background(), filepath.Join("testdata", "uses-env"))
	var ee *Error
	if !errors.As(err, &ee) || ee.Stage != StageInspect {
		t.Fatalf("want a failure at the inspect stage (before running), got: %v", err)
	}
	if !strings.Contains(ee.Message, "wasi:cli/environment@0.3.0") {
		t.Errorf("message does not name the denied import: %s", ee.Message)
	}
}

func TestEvaluateNotAPome(t *testing.T) {
	t.Parallel()
	e := testEvaluator(t)
	_, err := e.Evaluate(context.Background(), t.TempDir())
	var ee *Error
	if !errors.As(err, &ee) || ee.Code != CodeBuildFailed {
		t.Fatalf("want %s, got %v", CodeBuildFailed, err)
	}
}

func TestToolchainVersionMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, out string) {
		script := "#!/bin/sh\necho '" + out + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("emela", "emela 0.26.1")
	write("wasmtime", "wasmtime 49.0.5 (805a0f153 2026-10-29)")
	tc, err := ToolchainInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tc.Verify(context.Background())
	var ee *Error
	if !errors.As(err, &ee) || ee.Code != CodeToolchain {
		t.Fatalf("want %s, got %v", CodeToolchain, err)
	}
}

func TestToolchainMissing(t *testing.T) {
	t.Parallel()
	_, err := ToolchainInDir(t.TempDir())
	var ee *Error
	if !errors.As(err, &ee) || ee.Code != CodeToolchain {
		t.Fatalf("want %s, got %v", CodeToolchain, err)
	}
}

func TestPinnedVersionsMatchInstallScript(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install-toolchain.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"EMELA_VERSION": "v" + PinnedEmela, "WASMTIME_VERSION": "v" + PinnedWasmtime} {
		m := regexp.MustCompile(name + `="([^"]+)"`).FindSubmatch(script)
		if m == nil || string(m[1]) != want {
			t.Errorf("install-toolchain.sh %s = %q, want %q", name, m, want)
		}
	}
}

func TestLimitedBuffer(t *testing.T) {
	t.Parallel()
	called := 0
	b := limitedBuffer{max: 5, onExceed: func() { called++ }}
	for _, s := range []string{"abc", "defg", "hij"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if !b.Exceeded() || called != 1 || string(b.Bytes()) != "abcde" {
		t.Fatalf("exceeded=%v called=%d bytes=%q", b.Exceeded(), called, b.Bytes())
	}
}
