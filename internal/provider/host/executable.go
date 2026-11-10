package host

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/component"
	"github.com/ieyoukan/mamela/internal/toolchain"
)

// Format is how a provider program runs.
type Format string

const (
	FormatWASIp1    Format = "wasm-wasip1"    // a core module for WASI preview 1 (Go's GOOS=wasip1)
	FormatComponent Format = "wasm-component" // a WASI 0.3 component
	FormatNative    Format = "native"         // a native executable: only when trusted
)

// Executable is an inspected provider program.
type Executable struct {
	Path    string
	Digest  string // "sha256:..." of the file
	Format  Format
	Imports []component.Import // for WASM
	data    []byte             // what was inspected: Start runs these bytes, not the path
}

// maxExecutable bounds what Inspect reads.
const maxExecutable = 512 << 20

// WASIp1Policy is what a WASI preview 1 provider may import: standard input, output and error,
// arguments and environment (both empty), clocks, randomness, yielding and exiting. No path_*
// (files) and no sock_* (sockets) functions. wasmtime gives it no directories, environment or
// network anyway; the allowlist rejects a program that asks for more before it runs.
var WASIp1Policy = component.Policy{Allowed: prefixed("wasi_snapshot_preview1.",
	"args_get", "args_sizes_get", "clock_res_get", "clock_time_get", "environ_get", "environ_sizes_get",
	"fd_close", "fd_fdstat_get", "fd_fdstat_set_flags", "fd_filestat_get", "fd_prestat_dir_name",
	"fd_prestat_get", "fd_read", "fd_seek", "fd_write", "poll_oneoff", "proc_exit", "random_get", "sched_yield")}

// ComponentPolicy is what a provider component may import: standard input, output and error, and
// exiting.
var ComponentPolicy = component.Policy{Allowed: []string{
	"wasi:cli/exit@0.3.0",
	"wasi:cli/stderr@0.3.0",
	"wasi:cli/stdin@0.3.0",
	"wasi:cli/stdout@0.3.0",
	"wasi:cli/types@0.3.0",
}}

func prefixed(prefix string, names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = prefix + n
	}
	return out
}

// Inspect reads a provider program: its digest, its format and, for WASM, its imports, which
// must be allowed.
func Inspect(path string) (Executable, error) {
	path, err := filepath.Abs(path) // the provider runs in its own directory
	if err != nil {
		return Executable{}, &Error{Code: CodeExecutable, Message: err.Error()}
	}
	f, err := os.Open(path)
	if err != nil {
		return Executable{}, &Error{Code: CodeExecutable, Message: err.Error()}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxExecutable+1))
	if err != nil {
		return Executable{}, &Error{Code: CodeExecutable, Message: err.Error()}
	}
	if len(data) > maxExecutable {
		return Executable{}, &Error{Code: CodeExecutable, Message: fmt.Sprintf("%s is larger than %d MiB", path, maxExecutable>>20)}
	}
	sum := sha256.Sum256(data)
	exe := Executable{Path: path, Digest: "sha256:" + hex.EncodeToString(sum[:]), data: data}
	var policy component.Policy
	switch {
	case component.IsCoreModule(data):
		exe.Format, policy = FormatWASIp1, WASIp1Policy
		exe.Imports, err = component.CoreImports(data)
	case component.IsComponent(data):
		exe.Format, policy = FormatComponent, ComponentPolicy
		exe.Imports, err = component.Imports(data)
	default:
		exe.Format = FormatNative
		return exe, nil
	}
	if err != nil {
		return Executable{}, &Error{Code: CodeExecutable, Message: fmt.Sprintf("%s: reading the imports: %v", path, err)}
	}
	if err := policy.Check(exe.Imports); err != nil {
		var de *component.DeniedImportError
		if !errors.As(err, &de) {
			return Executable{}, &Error{Code: CodeExecutable, Message: err.Error()}
		}
		var names []string
		for _, d := range de.Denied {
			names = append(names, d.Name)
		}
		return Executable{}, &Error{Code: CodeExecutable,
			Message: fmt.Sprintf("%s imports %s, which a provider may not (it would reach files, sockets or other host capabilities)", path, strings.Join(slices.Compact(names), ", ")),
			Hint:    "a provider reaches its API only through host/request; remove the code that opens files, sockets or HTTP connections"}
	}
	return exe, nil
}

// Trust is the user's decision to run one native provider executable, outside any sandbox.
type Trust struct {
	Source  string `toml:"source"`  // where the provider comes from (a Pome path, a repository)
	Version string `toml:"version"` // its version
	Digest  string `toml:"digest"`  // "sha256:..." of the executable
}

// TrustFile is the reviewable record of trusted native providers (mamela.trust.toml).
type TrustFile struct {
	Native []Trust `toml:"native"`
}

// TrustWarning is shown before a native provider is trusted.
const TrustWarning = "A native provider runs outside any sandbox: it can read your files, your environment " +
	"variables and the network (on a cloud VM, also the instance metadata and its credentials). " +
	"Mamela's checks of its requests catch mistakes; they do not stop a malicious provider. " +
	"Trust it only if you trust its author and this exact build."

// LoadTrust reads a trust file. A missing file trusts nothing.
func LoadTrust(path string) (TrustFile, error) {
	var f TrustFile
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		return f, fmt.Errorf("%s: unknown keys %v", path, un)
	}
	for _, t := range f.Native {
		if t.Source == "" || t.Version == "" || !strings.HasPrefix(t.Digest, "sha256:") || len(t.Digest) != len("sha256:")+64 {
			return f, fmt.Errorf("%s: a trusted provider needs source, version and a sha256 digest: %+v", path, t)
		}
	}
	return f, nil
}

// Trusts reports whether the file trusts this executable as this source and version.
func (f TrustFile) Trusts(source, version, digest string) bool {
	return slices.Contains(f.Native, Trust{Source: source, Version: version, Digest: digest})
}

// Add records a trust decision (once).
func (f *TrustFile) Add(t Trust) {
	if !slices.Contains(f.Native, t) {
		f.Native = append(f.Native, t)
	}
}

// Save writes the trust file.
func (f TrustFile) Save(path string) error {
	var b strings.Builder
	b.WriteString("# Native providers trusted to run outside any sandbox (mamela provider trust).\n" +
		"# Each entry is one exact executable: a new build needs a new decision.\n")
	if err := toml.NewEncoder(&b).Encode(f); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// FindWasmtime finds the pinned wasmtime the way the mamela command does: $MAMELA_TOOLCHAIN_DIR,
// .tools/bin in cwd or above, PATH (only the pinned one), the user toolchain. Start checks it.
func FindWasmtime(cwd string) (string, error) {
	if dir := os.Getenv("MAMELA_TOOLCHAIN_DIR"); dir != "" {
		return filepath.Join(dir, "wasmtime"), nil
	}
	loc, err := toolchain.Pinned().Locate(cwd, exec.LookPath)
	if err != nil {
		return "", fmt.Errorf("%w (run mamela toolchain install)", err)
	}
	if loc.Dir != "" {
		return filepath.Join(loc.Dir, "wasmtime"), nil
	}
	return loc.Wasmtime, nil
}
