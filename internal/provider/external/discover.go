// Package external connects providers that run as separate programs to plan and apply.
//
// A provider comes with a Pome: the Pome's Emela library declares its kinds (Mamela.provided),
// and the Pome holds the program and mamela-provider.toml naming it. Importing the Pome in a
// declaration is how a user chooses a provider and its version; Pome.lock and the sumdb pin its
// contents, the program included. Which version runs is still checked: the plan records the
// program's digest and stores the program, and apply runs exactly that.
package external

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/component"
)

// ProviderFile is the file in a Pome that names its provider program.
const ProviderFile = "mamela-provider.toml"

// maxProgram bounds a provider program (a Pome holds at most 32 MiB).
const maxProgram = 32 << 20

// providerFile is mamela-provider.toml.
type providerFile struct {
	Provider string `toml:"provider"` // the provider's name, as in its manifest
	Version  string `toml:"version"`  // as in its manifest
	Program  string `toml:"program"`  // the WASM program, relative to the Pome
	SHA256   string `toml:"sha256"`   // of the program, in hex
}

// Package is a provider found in a declaration's dependencies.
type Package struct {
	Source   string // the Pome's source path
	Version  string // the Pome's version ("local" for a [replace] directory)
	Dir      string
	Provider string
	// ProviderVersion is the version mamela-provider.toml declares.
	ProviderVersion string
	Program         []byte
	Digest          string // "sha256:..." of Program
}

type pomeToml struct {
	Dependencies map[string]string         `toml:"dependencies"`
	Replace      map[string]map[string]any `toml:"replace"`
}

type pomeLock struct {
	Pome []struct {
		Path    string `toml:"path"`
		Version string `toml:"version"`
	} `toml:"pome"`
}

// Discover finds the providers among the dependencies of the Pome in dir: the Pomes of
// Pome.lock in the Pome cache, and the [replace] directories. The cache is the one the
// evaluation used, which checked it against Pome.lock (emela pome verify).
func Discover(dir, pomeCache string) ([]Package, error) {
	var manifest pomeToml
	if _, err := toml.DecodeFile(filepath.Join(dir, "Pome.toml"), &manifest); err != nil {
		return nil, fmt.Errorf("reading the Pome: %w", err)
	}
	type candidate struct{ source, version, dir string }
	var candidates []candidate
	for _, src := range slices.Sorted(maps.Keys(manifest.Replace)) {
		p, ok := manifest.Replace[src]["path"].(string)
		if !ok {
			continue
		}
		d := filepath.FromSlash(p)
		if !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		candidates = append(candidates, candidate{src, "local", d})
	}
	var lock pomeLock
	if _, err := toml.DecodeFile(filepath.Join(dir, "Pome.lock"), &lock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading Pome.lock: %w", err)
	}
	for _, p := range lock.Pome {
		if _, replaced := manifest.Replace[p.Path]; replaced {
			continue
		}
		candidates = append(candidates, candidate{p.Path, p.Version, filepath.Join(pomeCache, filepath.FromSlash(escape(p.Path))+"@"+p.Version)})
	}
	var out []Package
	seen := map[string]string{}
	for _, c := range candidates {
		pkg, ok, err := load(c.dir)
		if err != nil {
			return nil, fmt.Errorf("%s@%s: %w", c.source, c.version, err)
		}
		if !ok {
			continue
		}
		pkg.Source, pkg.Version = c.source, c.version
		if other, dup := seen[pkg.Provider]; dup {
			return nil, fmt.Errorf("provider %s comes from both %s and %s", pkg.Provider, other, c.source)
		}
		seen[pkg.Provider] = c.source
		out = append(out, pkg)
	}
	return out, nil
}

func load(dir string) (Package, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, ProviderFile))
	if errors.Is(err, os.ErrNotExist) {
		return Package{}, false, nil
	}
	if err != nil {
		return Package{}, false, err
	}
	var f providerFile
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return Package{}, false, fmt.Errorf("%s: %w", ProviderFile, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		return Package{}, false, fmt.Errorf("%s: unknown keys %v", ProviderFile, un)
	}
	clean := path.Clean(f.Program)
	if f.Provider == "" || f.Version == "" || f.Program == "" || clean != f.Program || path.IsAbs(clean) || strings.HasPrefix(clean, "..") || len(f.SHA256) != 64 {
		return Package{}, false, fmt.Errorf("%s: provider, version, program (a path inside the Pome) and sha256 are required", ProviderFile)
	}
	fi, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(clean)))
	if err != nil {
		return Package{}, false, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxProgram {
		return Package{}, false, fmt.Errorf("%s is not a regular file of at most %d MiB", f.Program, maxProgram>>20)
	}
	program, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(clean)))
	if err != nil {
		return Package{}, false, err
	}
	sum := sha256.Sum256(program)
	if hex.EncodeToString(sum[:]) != f.SHA256 {
		return Package{}, false, fmt.Errorf("%s is not the program %s names (sha256 %x, want %s)", f.Program, ProviderFile, sum, f.SHA256)
	}
	if !component.IsCoreModule(program) && !component.IsComponent(program) {
		return Package{}, false, fmt.Errorf("%s is not a WASM program; a provider in a Pome must be WASM (it runs isolated)", f.Program)
	}
	return Package{Dir: dir, Provider: f.Provider, ProviderVersion: f.Version, Program: program,
		Digest: "sha256:" + hex.EncodeToString(sum[:])}, true, nil
}

// escape writes a source path as the Pome cache does: an uppercase letter becomes "!" and the
// lowercase letter.
func escape(p string) string {
	var b strings.Builder
	for _, r := range p {
		if 'A' <= r && r <= 'Z' {
			b.WriteByte('!')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
