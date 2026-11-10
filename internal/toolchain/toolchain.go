// Package toolchain is the pinned Emela compiler and Wasmtime runtime (toolchain.lock): which
// executables Mamela may run, how to install them, and where to find them.
//
// Mamela runs only the exact executables of the lock. A different compiler or runtime could turn
// the same declaration into a different graph, so the sha256 of each executable is checked before
// it is run, not only its version.
package toolchain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela"
)

// Asset is one tool for one platform.
type Asset struct {
	URL    string `toml:"url"`    // the release archive (.tar.gz or .tar.xz), https only
	SHA256 string `toml:"sha256"` // of the archive
	Path   string `toml:"path"`   // the executable inside the archive
	Bin    string `toml:"bin"`    // sha256 of the executable
}

// Tool is one pinned tool.
type Tool struct {
	Name      string           `toml:"-"`
	Version   string           `toml:"version"`
	Platforms map[string]Asset `toml:"platforms"` // by GOOS/GOARCH
}

// Lock is a parsed toolchain.lock.
type Lock struct {
	Emela    Tool `toml:"emela"`
	Wasmtime Tool `toml:"wasmtime"`
}

// Tools returns the tools in a fixed order.
func (l Lock) Tools() []Tool { return []Tool{l.Emela, l.Wasmtime} }

var (
	hex64   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	version = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

// Parse reads a toolchain.lock strictly: unknown keys and incomplete entries are errors.
func Parse(data []byte) (Lock, error) {
	var l Lock
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&l)
	if err != nil {
		return Lock{}, fmt.Errorf("toolchain.lock: %w", err)
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		return Lock{}, fmt.Errorf("toolchain.lock: unknown key %s", extra[0])
	}
	l.Emela.Name, l.Wasmtime.Name = "emela", "wasmtime"
	for _, t := range l.Tools() {
		if !version.MatchString(t.Version) {
			return Lock{}, fmt.Errorf("toolchain.lock: %s: version %q is not MAJOR.MINOR.PATCH", t.Name, t.Version)
		}
		if len(t.Platforms) == 0 {
			return Lock{}, fmt.Errorf("toolchain.lock: %s: no platforms", t.Name)
		}
		for _, p := range slices.Sorted(maps.Keys(t.Platforms)) {
			a := t.Platforms[p]
			where := fmt.Sprintf("toolchain.lock: %s %s", t.Name, p)
			switch {
			case strings.Count(p, "/") != 1:
				return Lock{}, fmt.Errorf("%s: a platform is GOOS/GOARCH", where)
			case !strings.HasPrefix(a.URL, "https://"):
				return Lock{}, fmt.Errorf("%s: url must be https", where)
			case !strings.Contains(a.URL, "/v"+t.Version+"/"):
				return Lock{}, fmt.Errorf("%s: url %s is not of version %s", where, a.URL, t.Version)
			case !strings.HasSuffix(a.URL, ".tar.gz") && !strings.HasSuffix(a.URL, ".tar.xz"):
				return Lock{}, fmt.Errorf("%s: url must be a .tar.gz or .tar.xz archive", where)
			case !hex64.MatchString(a.SHA256) || !hex64.MatchString(a.Bin):
				return Lock{}, fmt.Errorf("%s: sha256 and bin must be 64 lowercase hex digits", where)
			case a.Path == "" || path.Clean(a.Path) != a.Path || path.IsAbs(a.Path) || strings.HasPrefix(a.Path, "..") || path.Base(a.Path) != t.Name:
				return Lock{}, fmt.Errorf("%s: path %q must be a clean relative path ending in %s", where, a.Path, t.Name)
			}
		}
	}
	return l, nil
}

var pinned = sync.OnceValues(func() (Lock, error) { return Parse(mamela.ToolchainLock) })

// Pinned is the lock built into this binary (toolchain.lock at the repository root).
func Pinned() Lock {
	l, err := pinned()
	if err != nil {
		panic(err) // TestPinnedLockIsValid keeps this from happening
	}
	return l
}

// Platform is the platform this binary runs on, as written in the lock.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Asset returns the tool's asset for a platform.
func (t Tool) Asset(platform string) (Asset, error) {
	a, ok := t.Platforms[platform]
	if !ok {
		return Asset{}, fmt.Errorf("%s %s is not available for %s (only for %s)", t.Name, t.Version, platform,
			strings.Join(slices.Sorted(maps.Keys(t.Platforms)), ", "))
	}
	return a, nil
}

// MismatchError says that an executable is missing or is not the pinned one.
type MismatchError struct {
	Tool    Tool
	Path    string
	Missing bool
	Got     string // sha256 of the file found
	Want    string
}

func (e *MismatchError) Error() string {
	if e.Missing {
		return fmt.Sprintf("%s was not found", e.Path)
	}
	return fmt.Sprintf("%s is not the pinned %s %s (sha256 %s, want %s)", e.Path, e.Tool.Name, e.Tool.Version, e.Got, e.Want)
}

// Check verifies that dir holds exactly the pinned executables for the platform. It reads them; it
// does not run them.
func (l Lock) Check(dir, platform string) error {
	var errs []error
	for _, t := range l.Tools() {
		a, err := t.Asset(platform)
		if err != nil {
			return err
		}
		p := filepath.Join(dir, t.Name)
		got, err := FileSHA256(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			errs = append(errs, &MismatchError{Tool: t, Path: p, Missing: true, Want: a.Bin})
		case err != nil:
			errs = append(errs, err)
		case got != a.Bin:
			errs = append(errs, &MismatchError{Tool: t, Path: p, Got: got, Want: a.Bin})
		}
	}
	return errors.Join(errs...)
}

// FileSHA256 is the sha256 of a regular file, in hex.
func FileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return "", err
	} else if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DirName names the directory of a user-wide installation of this lock's toolchain, so that
// Mamela builds pinned to different toolchains can be installed side by side.
func (l Lock) DirName() string {
	return "emela-" + l.Emela.Version + "-wasmtime-" + l.Wasmtime.Version
}

// UserDirs are the user-wide toolchain directory of the lock and the download cache
// (<user cache>/mamela/...).
func (l Lock) UserDirs() (bin, downloads string, err error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", "", fmt.Errorf("no user cache directory to install the toolchain in (pass --dir): %w", err)
	}
	return filepath.Join(base, "mamela", "toolchain", l.DirName()), filepath.Join(base, "mamela", "downloads"), nil
}

// Location is where a toolchain was found.
type Location struct {
	Dir       string // holds emela and wasmtime; empty when they are in different directories
	Emela     string // set when Dir is empty
	Wasmtime  string // set when Dir is empty
	PomeCache string // the Pome cache that belongs to it ("" if none: use Emela's default)
	Source    string // how it was found, for people
}

// ErrNotFound means no installed toolchain was found.
var ErrNotFound = errors.New("no Mamela toolchain found")

// OnPath returns emela and wasmtime from PATH (as installed by mise, for example) if both are
// exactly the pinned executables for the platform.
func (l Lock) OnPath(platform string, lookPath func(string) (string, error)) (emela, wasmtime string, ok bool) {
	var found []string
	for _, t := range l.Tools() {
		a, err := t.Asset(platform)
		if err != nil {
			return "", "", false
		}
		p, err := lookPath(t.Name)
		if err != nil {
			return "", "", false
		}
		if p, err = filepath.EvalSymlinks(p); err != nil {
			return "", "", false
		}
		if got, err := FileSHA256(p); err != nil || got != a.Bin {
			return "", "", false
		}
		found = append(found, p)
	}
	return found[0], found[1], true
}

// Locate finds the toolchain to use when none is given, in this order:
//  1. the repository toolchain in the nearest .tools/bin at or above cwd
//     (scripts/install-toolchain.sh), with the Pome cache next to it;
//  2. emela and wasmtime on PATH (mise install), only if they are exactly the pinned ones;
//  3. the user-wide installation (mamela toolchain install).
//
// Except for PATH, it only looks for the files; Check says whether they are the pinned ones.
func (l Lock) Locate(cwd string, lookPath func(string) (string, error)) (Location, error) {
	for d := cwd; ; {
		bin := filepath.Join(d, ".tools", "bin")
		if exists(filepath.Join(bin, "emela")) {
			loc := Location{Dir: bin, Source: "the repository toolchain in " + filepath.Join(d, ".tools")}
			if pomes := filepath.Join(d, ".tools", "pomes"); exists(pomes) {
				loc.PomeCache = pomes
			}
			return loc, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if emela, wasmtime, ok := l.OnPath(Platform(), lookPath); ok {
		return Location{Emela: emela, Wasmtime: wasmtime, Source: "PATH (the pinned executables, as mise installs them)"}, nil
	}
	if bin, _, err := l.UserDirs(); err == nil && exists(filepath.Join(bin, "emela")) {
		return Location{Dir: bin, Source: "the user toolchain, from mamela toolchain install"}, nil
	}
	return Location{}, ErrNotFound
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Options control Install.
type Options struct {
	Dir       string       // where emela and wasmtime are installed
	Downloads string       // cache of downloaded archives (kept: a reinstall needs no network)
	Platform  string       // default: Platform()
	Client    *http.Client // default: a client with a timeout
	Log       io.Writer    // progress; default: discarded
	// LookPath finds a tool already installed elsewhere (mise install puts the pinned ones on PATH);
	// one with the pinned sha256 is copied instead of downloaded. Nil: never look.
	LookPath func(string) (string, error)
}

// Install puts the pinned executables into o.Dir. An executable already there with the pinned
// sha256 is kept. Archives are downloaded only when the cache lacks a good copy, and every
// archive and executable is checked against the lock; nothing unchecked is ever left in o.Dir.
// Extraction uses the system tar (it reads .tar.xz on macOS and on Linux with xz installed).
func (l Lock) Install(ctx context.Context, o Options) error {
	if o.Platform == "" {
		o.Platform = Platform()
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Minute}
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Dir == "" || o.Downloads == "" {
		return errors.New("toolchain install: a directory and a download cache are needed")
	}
	for _, d := range []string{o.Dir, o.Downloads} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	for _, t := range l.Tools() {
		a, err := t.Asset(o.Platform)
		if err != nil {
			return err
		}
		dest := filepath.Join(o.Dir, t.Name)
		if got, err := FileSHA256(dest); err == nil && got == a.Bin {
			fmt.Fprintf(o.Log, "%s %s is already installed\n", t.Name, t.Version)
			continue
		}
		if o.LookPath != nil {
			if p, err := o.LookPath(t.Name); err == nil {
				if p, err = filepath.EvalSymlinks(p); err == nil {
					if got, err := FileSHA256(p); err == nil && got == a.Bin {
						if err := copyChecked(p, dest, a.Bin); err != nil {
							return err
						}
						fmt.Fprintf(o.Log, "installed %s %s (copied from %s)\n", t.Name, t.Version, p)
						continue
					}
				}
			}
		}
		archive, err := l.fetch(ctx, o, t, a)
		if err != nil {
			return err
		}
		if err := extract(ctx, archive, a, t, dest); err != nil {
			return err
		}
		fmt.Fprintf(o.Log, "installed %s %s\n", t.Name, t.Version)
	}
	return l.Check(o.Dir, o.Platform)
}

// fetch returns the path of the tool's archive in the download cache, downloading it if needed.
func (l Lock) fetch(ctx context.Context, o Options, t Tool, a Asset) (string, error) {
	name := path.Base(a.URL)
	p := filepath.Join(o.Downloads, name)
	if got, err := FileSHA256(p); err == nil && got == a.SHA256 {
		return p, nil
	}
	fmt.Fprintf(o.Log, "downloading %s\n", a.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s %s: %w", t.Name, t.Version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s %s: %s from %s", t.Name, t.Version, resp.Status, a.URL)
	}
	if resp.Request != nil && resp.Request.URL.Scheme != "https" {
		return "", fmt.Errorf("download %s %s: redirected to %s, which is not https", t.Name, t.Version, resp.Request.URL)
	}
	tmp, err := os.CreateTemp(o.Downloads, name+".part-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download %s %s: %w", t.Name, t.Version, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return "", fmt.Errorf("download %s %s: the archive's sha256 is %s, but toolchain.lock says %s; nothing was installed",
			t.Name, t.Version, got, a.SHA256)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", err
	}
	return p, nil
}

// extract takes the executable out of a checked archive and installs it at dest, only if its
// sha256 is the pinned one.
func extract(ctx context.Context, archive string, a Asset, t Tool, dest string) error {
	work, err := os.MkdirTemp(filepath.Dir(dest), ".extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	cmd := exec.CommandContext(ctx, "tar", "-xf", archive, "-C", work, "--", a.Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		hint := ""
		if strings.HasSuffix(archive, ".xz") && runtime.GOOS == "linux" {
			hint = " (tar needs xz for .tar.xz: install xz-utils)"
		}
		return fmt.Errorf("extract %s from %s: %v: %s%s", a.Path, filepath.Base(archive), err, strings.TrimSpace(string(out)), hint)
	}
	got := filepath.Join(work, filepath.FromSlash(a.Path))
	sum, err := FileSHA256(got)
	if err != nil {
		return fmt.Errorf("extract %s from %s: %w", a.Path, filepath.Base(archive), err)
	}
	if sum != a.Bin {
		return fmt.Errorf("%s in %s has sha256 %s, but toolchain.lock says %s; nothing was installed",
			a.Path, filepath.Base(archive), sum, a.Bin)
	}
	if err := os.Chmod(got, 0o755); err != nil {
		return err
	}
	return os.Rename(got, dest)
}

// copyChecked copies an executable to dest through a temporary file, and keeps it only if the copy
// has the pinned sha256.
func copyChecked(src, dest, want string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s changed while it was copied (sha256 %s, want %s); nothing was installed", src, got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}
