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

	"github.com/ieyoukan/mamela/internal/toolchain"
)

// Pinned toolchain versions (toolchain.lock). Evaluation runs only the pinned executables, because
// the graph digest is only reproducible with the same compiler and runtime.
var (
	PinnedEmela    = toolchain.Pinned().Emela.Version
	PinnedWasmtime = toolchain.Pinned().Wasmtime.Version
)

// installHint says how to get the pinned toolchain.
const installHint = "run `mamela toolchain install` (in the Mamela repository: scripts/install-toolchain.sh), " +
	"or pass --toolchain with the directory that holds the pinned emela and wasmtime"

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

// ToolchainInDir returns the toolchain in dir (installed by `mamela toolchain install`, or by
// scripts/install-toolchain.sh in <repo>/.tools/bin).
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
				Hint:    installHint}
		}
	}
	return t, nil
}

// Verify checks that the executables are the pinned ones (by sha256, before running anything) and
// that they report the pinned versions.
func (t Toolchain) Verify(ctx context.Context) (Versions, error) {
	if err := t.checkPinned(); err != nil {
		return Versions{}, err
	}
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
			Hint: installHint + "; other versions could produce a different graph from the same source"}
	}
	return v, nil
}

// checkPinned compares the executables with toolchain.lock without running them.
func (t Toolchain) checkPinned() error {
	l := toolchain.Pinned()
	var problems []string
	for _, c := range []struct {
		tool toolchain.Tool
		path string
	}{{l.Emela, t.Emela}, {l.Wasmtime, t.Wasmtime}} {
		a, err := c.tool.Asset(toolchain.Platform())
		if err != nil {
			return &Error{Code: CodeToolchain, Stage: StageToolchain, Message: err.Error(),
				Hint: "Mamela evaluates only with the pinned toolchain, which exists for the platforms named above"}
		}
		got, err := toolchain.FileSHA256(c.path)
		if err != nil {
			return &Error{Code: CodeToolchain, Stage: StageToolchain, Message: err.Error(), Hint: installHint}
		}
		if got != a.Bin {
			problems = append(problems, fmt.Sprintf("%s is not the pinned %s %s (sha256 %s, want %s)",
				c.path, c.tool.Name, c.tool.Version, got, a.Bin))
		}
	}
	if len(problems) > 0 {
		return &Error{Code: CodeToolchain, Stage: StageToolchain, Message: strings.Join(problems, "\n"),
			Hint: installHint + "; Mamela runs no other build, because it could turn the same source into a different graph"}
	}
	return nil
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
