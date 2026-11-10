package evaluate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Pinned toolchain versions. Evaluation refuses other versions, because the graph digest is only
// reproducible with the same compiler and runtime. Keep in sync with scripts/install-toolchain.sh
// (a test checks this).
const (
	PinnedEmela    = "0.27.0"
	PinnedWasmtime = "49.0.5"
)

// Toolchain locates the emela and wasmtime executables.
type Toolchain struct {
	Emela    string
	Wasmtime string
}

// Versions are the versions reported by the toolchain.
type Versions struct {
	Emela    string
	Wasmtime string
}

// ToolchainInDir returns the toolchain installed by scripts/install-toolchain.sh in dir
// (normally <repo>/.tools/bin).
func ToolchainInDir(dir string) (Toolchain, error) {
	// Absolute paths: the processes run in a temporary working directory.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Toolchain{}, err
	}
	t := Toolchain{Emela: filepath.Join(abs, "emela"), Wasmtime: filepath.Join(abs, "wasmtime")}
	for _, p := range []string{t.Emela, t.Wasmtime} {
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			return Toolchain{}, &Error{Code: CodeToolchain, Stage: StageToolchain,
				Message: fmt.Sprintf("%s was not found", p),
				Hint:    "run scripts/install-toolchain.sh, or pass the directory that contains emela and wasmtime"}
		}
	}
	return t, nil
}

// Verify checks that the toolchain reports the pinned versions.
func (t Toolchain) Verify(ctx context.Context) (Versions, error) {
	emela, err := versionOf(ctx, t.Emela, "emela ")
	if err != nil {
		return Versions{}, err
	}
	wasmtime, err := versionOf(ctx, t.Wasmtime, "wasmtime ")
	if err != nil {
		return Versions{}, err
	}
	v := Versions{Emela: emela, Wasmtime: wasmtime}
	if v.Emela != PinnedEmela || v.Wasmtime != PinnedWasmtime {
		return v, &Error{Code: CodeToolchain, Stage: StageToolchain,
			Message: fmt.Sprintf("toolchain is emela %s / wasmtime %s, but Mamela is pinned to emela %s / wasmtime %s",
				v.Emela, v.Wasmtime, PinnedEmela, PinnedWasmtime),
			Hint: "run scripts/install-toolchain.sh; other versions could produce a different graph from the same source"}
	}
	return v, nil
}

func versionOf(ctx context.Context, exe, prefix string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	cmd.Env = minimalEnv(nil)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		detail := ""
		if errors.As(err, &ee) {
			detail = string(ee.Stderr)
		}
		return "", &Error{Code: CodeToolchain, Stage: StageToolchain,
			Message: fmt.Sprintf("cannot run %s --version: %v", exe, err), Detail: detail}
	}
	line := strings.TrimSpace(string(out))
	if !strings.HasPrefix(line, prefix) {
		return "", &Error{Code: CodeToolchain, Stage: StageToolchain,
			Message: fmt.Sprintf("unexpected version output from %s: %q", exe, line)}
	}
	fields := strings.Fields(strings.TrimPrefix(line, prefix))
	if len(fields) == 0 {
		return "", &Error{Code: CodeToolchain, Stage: StageToolchain,
			Message: fmt.Sprintf("unexpected version output from %s: %q", exe, line)}
	}
	return fields[0], nil
}

// minimalEnv is the whole environment given to toolchain processes: nothing is inherited.
func minimalEnv(extra []string) []string {
	env := []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "NO_COLOR=1", "TERM=dumb"}
	return append(env, extra...)
}
