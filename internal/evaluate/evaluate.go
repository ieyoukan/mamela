// Package evaluate turns an Emela Pome into a validated Resource Graph without side effects.
//
// Steps:
//  1. build: check the cached dependencies against Pome.lock (`emela pome verify`), then
//     `emela build --target wasm` with no network (EMELA_POMEPROXY=off), a clean environment and
//     an output directory outside the source tree;
//  2. inspect: read the component's top-level imports and refuse anything beyond
//     stdout/stderr/exit (component.EvaluationPolicy) before running it;
//  3. run: `wasmtime run` with no environment, no stdin, no directories, no network, and limits on
//     time, memory and output;
//  4. decode: parse standard output strictly as one mamela-graph/v1 document.
//
// The same source, lock and toolchain give the same graph digest.
package evaluate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/component"
	"github.com/ieyoukan/mamela/internal/graph"
)

// Limits bound one evaluation.
type Limits struct {
	BuildTimeout   time.Duration
	RunTimeout     time.Duration // wasm execution time (wasmtime -W timeout)
	MaxMemoryBytes uint64        // per linear memory (wasmtime -W max-memory-size)
	MaxStdoutBytes int           // the graph document
	MaxStderrBytes int           // diagnostics kept for the report
}

// DefaultLimits suit a homelab-sized declaration with a wide margin.
var DefaultLimits = Limits{
	BuildTimeout:   2 * time.Minute,
	RunTimeout:     10 * time.Second,
	MaxMemoryBytes: 256 << 20,
	MaxStdoutBytes: graph.MaxDocumentBytes,
	MaxStderrBytes: 64 << 10,
}

// Evaluator evaluates Pomes with a fixed toolchain and limits.
type Evaluator struct {
	Toolchain Toolchain
	Limits    Limits
	// PomeCache is the directory of already fetched dependencies (EMELA_POME_CACHE). Builds never
	// fetch: a dependency missing from it fails the build. Empty means no dependencies at all.
	PomeCache string
	// Policy is the import allowlist; the zero value means component.EvaluationPolicy.
	Policy *component.Policy
}

// Result is a successful evaluation.
type Result struct {
	Graph      *graph.Graph
	Digest     string // graph digest (canonical form)
	WasmDigest string // sha256 of main.wasm
	Imports    []component.Import
	Versions   Versions
	Stderr     string // diagnostics the program wrote (dbg, print_error_line)
}

// Evaluate builds and runs the Pome in dir and returns its graph.
func (e *Evaluator) Evaluate(ctx context.Context, dir string) (*Result, error) {
	versions, err := e.Toolchain.Verify(ctx)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "mamela-eval-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	wasmPath, err := e.build(ctx, dir, work)
	if err != nil {
		return nil, err
	}
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(wasm)

	imports, err := component.Imports(wasm)
	if err != nil {
		return nil, &Error{Code: CodeNotComponent, Stage: StageInspect,
			Message: "the build output cannot be inspected: " + err.Error(),
			Hint:    "Mamela only runs WASI 0.3 components whose imports it can read"}
	}
	policy := component.EvaluationPolicy
	if e.Policy != nil {
		policy = *e.Policy
	}
	if err := policy.Check(imports); err != nil {
		return nil, &Error{Code: CodeImportDenied, Stage: StageInspect,
			Message: err.Error() + "; it was not run",
			Hint: "a declaration must be a pure function of its source: do not use Std.Env, Std.Random, " +
				"Std.Clock, Std.FileSystem or Std.Http, and provide only SystemConsole"}
	}

	stdout, stderr, err := e.run(ctx, wasmPath)
	if err != nil {
		return nil, err
	}
	g, err := graph.Decode(stdout)
	if err != nil {
		return nil, err
	}
	return &Result{
		Graph:      g,
		Digest:     g.Digest(),
		WasmDigest: "sha256:" + hex.EncodeToString(sum[:]),
		Imports:    imports,
		Versions:   versions,
		Stderr:     stderr,
	}, nil
}

// EvaluateTwice evaluates dir twice from scratch and fails unless both give the same graph digest.
// It is the determinism check run in CI and before activating a revision.
func (e *Evaluator) EvaluateTwice(ctx context.Context, dir string) (*Result, error) {
	a, err := e.Evaluate(ctx, dir)
	if err != nil {
		return nil, err
	}
	b, err := e.Evaluate(ctx, dir)
	if err != nil {
		return nil, err
	}
	if a.Digest != b.Digest {
		return nil, &Error{Code: CodeNotDeterministic, Stage: StageCompare,
			Message: fmt.Sprintf("two evaluations of the same source gave different graphs (%s, %s)", a.Digest, b.Digest),
			Hint:    "this is a bug in the toolchain or in Mamela; please report it with the source"}
	}
	return a, nil
}

func (e *Evaluator) build(ctx context.Context, dir, work string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(filepath.Join(abs, "Pome.toml")); err != nil || fi.IsDir() {
		return "", &Error{Code: CodeBuildFailed, Stage: StageBuild,
			Message: fmt.Sprintf("%s is not a Pome (no Pome.toml)", dir),
			Hint:    "pass the directory that contains Pome.toml and src/main.emel"}
	}
	out := filepath.Join(work, "out")
	home := filepath.Join(work, "home")
	cache := e.PomeCache
	if cache == "" {
		cache = filepath.Join(work, "empty-pome-cache")
	}
	for _, d := range []string{out, home, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, e.Limits.BuildTimeout)
	defer cancel()
	env := minimalEnv([]string{
		"HOME=" + home,
		"EMELA_POMEPROXY=off",
		"EMELA_POMESUMDB=off",
		"EMELA_POME_CACHE=" + cache,
	})
	if err := e.verifyDependencies(ctx, abs, env); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, e.Toolchain.Emela, "build", "--target", "wasm", "--out-dir", out, abs)
	cmd.Dir = work
	cmd.Env = env
	var diag limitedBuffer
	diag.max = e.Limits.MaxStderrBytes
	cmd.Stdout = &diag
	cmd.Stderr = &diag
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", &Error{Code: CodeBuildTimeout, Stage: StageBuild,
			Message: fmt.Sprintf("the build did not finish within %s", e.Limits.BuildTimeout), Detail: diag.String()}
	}
	if err != nil {
		detail := diag.String()
		if strings.TrimSpace(detail) == "" {
			detail = err.Error()
		}
		if strings.Contains(detail, "nowhere to get it from") {
			return "", &Error{Code: CodeBuildFailed, Stage: StageBuild,
				Message: "a dependency in Pome.lock is not in the Pome cache " + cache + " (nothing is fetched while evaluating)",
				Hint: "fetch it with `emela pome install` in the Pome (scripts/install-toolchain.sh does this for this " +
					"repository), and point EMELA_POME_CACHE or --pome-cache at that cache",
				Detail: detail}
		}
		return "", &Error{Code: CodeBuildFailed, Stage: StageBuild,
			Message: "the Emela source did not compile",
			Hint:    "fix the errors below; `emela check` in the Pome shows the same diagnostics",
			Detail:  detail}
	}
	wasm := filepath.Join(out, "main.wasm")
	if _, err := os.Stat(wasm); err != nil {
		return "", &Error{Code: CodeBuildFailed, Stage: StageBuild,
			Message: "the build produced no main.wasm",
			Hint:    "the Pome needs src/main.emel with a main function", Detail: diag.String()}
	}
	return wasm, nil
}

// verifyDependencies checks the cached dependencies of the Pome in dir against the hashes in its
// Pome.lock. `emela build` compiles whatever the cache holds without checking it, so without this
// a modified cache could change what a declaration means while its source and lock stay the same.
// A dependency missing from the cache is left to the build, which fails on it (there is no network).
func (e *Evaluator) verifyDependencies(ctx context.Context, dir string, env []string) error {
	n, err := lockedDependencies(dir)
	if err != nil {
		return &Error{Code: CodeBuildFailed, Stage: StageBuild,
			Message: "Pome.lock cannot be read: " + err.Error(),
			Hint:    "run `emela pome install` in the Pome to write it again"}
	}
	if n == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, e.Toolchain.Emela, "pome", "verify")
	cmd.Dir = dir
	cmd.Env = env
	var diag limitedBuffer
	diag.max = e.Limits.MaxStderrBytes
	cmd.Stdout = &diag
	cmd.Stderr = &diag
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return &Error{Code: CodeBuildTimeout, Stage: StageBuild,
			Message: fmt.Sprintf("checking the dependencies did not finish within %s", e.Limits.BuildTimeout),
			Detail:  diag.String()}
	}
	if err != nil {
		return &Error{Code: CodeDependencyMismatch, Stage: StageBuild,
			Message: "the cached dependencies differ from the hashes in Pome.lock; nothing was built",
			Hint: "the dependency cache was modified or damaged: remove the entries named below from it " +
				"and fetch them again with `emela pome install` (scripts/install-toolchain.sh does this for this repository)",
			Detail: diag.String()}
	}
	return nil
}

// lockedDependencies counts the dependencies recorded in dir/Pome.lock (0 when there is no lock).
func lockedDependencies(dir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(dir, "Pome.lock"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var lock struct {
		Pome []struct {
			Path string `toml:"path"`
		} `toml:"pome"`
	}
	if _, err := toml.Decode(string(data), &lock); err != nil {
		return 0, err
	}
	return len(lock.Pome), nil
}

func (e *Evaluator) run(ctx context.Context, wasmPath string) ([]byte, string, error) {
	// The process deadline is a backstop; wasmtime's own timeout normally fires first.
	ctx, cancel := context.WithTimeout(ctx, e.Limits.RunTimeout+5*time.Second)
	defer cancel()
	args := []string{
		"run",
		"-C", "cache=n",
		"-S", "p3=y",
		"-S", "inherit-env=n",
		"-S", "inherit-stdin=n",
		"-S", "inherit-network=n",
		"-S", "tcp=n",
		"-S", "udp=n",
		"-S", "allow-ip-name-lookup=n",
		"-S", "http=n",
		"-W", "timeout=" + strconv.FormatInt(e.Limits.RunTimeout.Milliseconds(), 10) + "ms",
		"-W", "max-memory-size=" + strconv.FormatUint(e.Limits.MaxMemoryBytes, 10),
		wasmPath,
	}
	cmd := exec.CommandContext(ctx, e.Toolchain.Wasmtime, args...)
	cmd.Dir = filepath.Dir(wasmPath)
	cmd.Env = minimalEnv(nil)
	cmd.Stdin = nil // /dev/null

	stdout := limitedBuffer{max: e.Limits.MaxStdoutBytes, onExceed: cancel}
	stderr := limitedBuffer{max: e.Limits.MaxStderrBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	if stdout.Exceeded() {
		return nil, stderr.String(), &Error{Code: CodeOutputLimit, Stage: StageRun,
			Message: fmt.Sprintf("the program wrote more than %d bytes to standard output; it was stopped", e.Limits.MaxStdoutBytes),
			Hint:    "print the graph once; write diagnostics to standard error"}
	}
	if runErr == nil {
		return stdout.Bytes(), stderr.String(), nil
	}

	errText := stderr.String()
	exitCode := -1
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		exitCode = ee.ExitCode()
	}
	switch {
	case strings.Contains(errText, "wasm trap: interrupt") || ctx.Err() == context.DeadlineExceeded:
		return nil, errText, &Error{Code: CodeTimeout, Stage: StageRun,
			Message: fmt.Sprintf("the program did not finish within %s; it was stopped", e.Limits.RunTimeout),
			Hint:    "a declaration should finish quickly; look for unbounded recursion", Detail: errText}
	case strings.Contains(errText, "emela: out of memory"):
		return nil, errText, &Error{Code: CodeMemoryLimit, Stage: StageRun,
			Message: fmt.Sprintf("the program needed more than %d MiB of memory; it was stopped", e.Limits.MaxMemoryBytes>>20),
			Hint:    "look for data that grows without bound", Detail: errText}
	case exitCode == 101:
		return nil, errText, &Error{Code: CodeProgramDefect, Stage: StageRun,
			Message: "the program stopped with a defect (a bug such as division by zero)", Detail: errText}
	case exitCode == 1 && hasLinePrefix(errText, "error: "):
		return nil, errText, &Error{Code: CodeProgramError, Stage: StageRun,
			Message: "the program ended with an error", Detail: errText}
	default:
		return nil, errText, &Error{Code: CodeAbnormalExit, Stage: StageRun,
			Message: fmt.Sprintf("wasmtime exited abnormally (exit code %d)", exitCode), Detail: errText}
	}
}

func hasLinePrefix(text, prefix string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// limitedBuffer keeps at most max bytes. Past the limit it records that it overflowed, calls
// onExceed once (to stop the process), and discards the rest while reporting success so the
// writer does not block.
type limitedBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	max      int
	exceeded bool
	onExceed func()
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exceeded {
		return len(p), nil
	}
	if room := b.max - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:max(room, 0)])
		b.exceeded = true
		if b.onExceed != nil {
			b.onExceed()
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	if b.exceeded {
		s += "\n[truncated]"
	}
	return s
}

func (b *limitedBuffer) Exceeded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exceeded
}
