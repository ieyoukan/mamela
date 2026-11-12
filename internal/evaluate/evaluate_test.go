package evaluate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// examplePome is the dependency every example needs in the Pome cache.
const examplePome = "github.com/emela-lang/x/json@v0.3.1"

// pomeCacheForTest returns the cache of fetched dependencies (EMELA_POME_CACHE, normally
// <repo>/.tools/pomes, filled by scripts/install-toolchain.sh).
func pomeCacheForTest() string {
	if dir := os.Getenv("EMELA_POME_CACHE"); dir != "" {
		return dir
	}
	dir, _ := filepath.Abs(filepath.Join("..", "..", ".tools", "pomes"))
	return dir
}

// requirePomeCache skips (or fails under MAMELA_REQUIRE_TOOLCHAIN=1) when the dependencies of the
// examples have not been fetched.
func requirePomeCache(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(pomeCacheForTest(), filepath.FromSlash(examplePome))); err != nil {
		if os.Getenv("MAMELA_REQUIRE_TOOLCHAIN") == "1" {
			t.Fatalf("Pome cache required: %v", err)
		}
		t.Skipf("dependencies not fetched (run scripts/install-toolchain.sh): %v", err)
	}
}

func testEvaluator(t *testing.T) *Evaluator {
	t.Helper()
	return &Evaluator{
		PomeCache: pomeCacheForTest(),
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
	requirePomeCache(t)
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
	const want = "sha256:106ce1583fdfaf00e799f2ad39ebe7c94932c4856ca0a276c388c3a9a8ddd34c"
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

func TestRelativePomeCacheIsResolvedFromTheCallersDirectory(t *testing.T) {
	t.Parallel()
	requirePomeCache(t)
	e := testEvaluator(t)
	rel, err := filepath.Rel(mustGetwd(t), e.PomeCache)
	if err != nil || filepath.IsAbs(rel) {
		t.Skipf("the Pome cache %s has no relative path from here", e.PomeCache)
	}
	e.PomeCache = rel
	if _, err := e.Evaluate(context.Background(), filepath.Join("..", "..", "examples", "homelab")); err != nil {
		t.Fatalf("Evaluate with relative Pome cache %s: %v", rel, err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func TestBuildIsReproducible(t *testing.T) {
	t.Parallel()
	requirePomeCache(t)
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

func TestModifiedDependencyCacheIsRejected(t *testing.T) {
	t.Parallel()
	requirePomeCache(t)
	e := testEvaluator(t)
	// A copy of the cache in which x/json writes null differently. The lock and the source of the
	// example are unchanged, so only the hashes in Pome.lock can tell.
	cache := filepath.Join(t.TempDir(), "pomes")
	if err := copyTree(pomeCacheForTest(), cache); err != nil {
		t.Fatal(err)
	}
	write := filepath.Join(cache, filepath.FromSlash(examplePome), "src", "write.emel")
	src, err := os.ReadFile(write)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(write, bytes.Replace(src, []byte(`"null"`), []byte(`"NULL"`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	e.PomeCache = cache
	res, err := e.Evaluate(context.Background(), filepath.Join("..", "..", "examples", "homelab"))
	var ee *Error
	if !errors.As(err, &ee) || ee.Code != CodeDependencyMismatch {
		t.Fatalf("want %s, got %v (result %+v)", CodeDependencyMismatch, err, res)
	}
	if !strings.Contains(ee.Detail, "github.com/emela-lang/x/json v0.3.1") {
		t.Errorf("the detail does not name the modified dependency: %q", ee.Detail)
	}
}

// copyTree copies a directory of regular files, making the copy writable (the Pome cache is
// read-only).
func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
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
		dir     string
		code    Code
		graph   graph.Code // set when the failure is in the graph itself
		detail  string     // a substring the diagnostics must contain, to pin the reason
		noCache bool       // build with an empty Pome cache instead of the shared one
		hint    string     // a substring the hint must contain
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
		// Its lock names a dependency that is in no cache: the build must fail, never fetch.
		{dir: "needs-fetch", code: CodeBuildFailed, detail: "nowhere to get it from", noCache: true, hint: "/emela pome install"},
		{dir: "not-json", graph: graph.CodeInvalidJSON},
		{dir: "two-documents", graph: graph.CodeTrailingData},
		{dir: "invalid-graph", graph: graph.CodeInvalidName},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			t.Parallel()
			e := testEvaluator(t)
			if tt.noCache {
				e.PomeCache = t.TempDir()
			}
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
			if !strings.Contains(ee.Hint, tt.hint) {
				t.Errorf("hint does not say %q: %s", tt.hint, ee.Hint)
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

// A toolchain that is not the pinned build is refused before any of it runs, even when it claims
// the pinned versions.
func TestToolchainNotPinnedIsNotRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	write := func(name, out string) {
		script := "#!/bin/sh\ntouch '" + ran + "'\necho '" + out + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("emela", "emela "+PinnedEmela)
	write("wasmtime", "wasmtime "+PinnedWasmtime+" (fake)")
	tc, err := ToolchainInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tc.Verify(context.Background())
	var ee *Error
	if !errors.As(err, &ee) || ee.Code != CodeToolchain || !strings.Contains(ee.Message, "is not the pinned emela") ||
		!strings.Contains(ee.Message, "is not the pinned wasmtime") {
		t.Fatalf("want %s naming both tools, got %v", CodeToolchain, err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("a toolchain that is not the pinned one was run")
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
