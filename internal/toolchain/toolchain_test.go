package toolchain

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPinnedLockIsValid(t *testing.T) {
	l := Pinned()
	for _, tool := range l.Tools() {
		// The platforms Emela publishes binaries for; CI runs on both.
		for _, p := range []string{"darwin/arm64", "linux/amd64"} {
			if _, err := tool.Asset(p); err != nil {
				t.Error(err)
			}
		}
	}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func targz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// release is a fake download server with one archive per tool.
type release struct {
	srv      *httptest.Server
	archives map[string][]byte // by URL path
	requests atomic.Int32
	emelaBin []byte
	wasmBin  []byte
	emelaTar []byte
	wasmTar  []byte
}

func newRelease(t *testing.T) *release {
	r := &release{emelaBin: []byte("#!/bin/sh\necho 'emela 1.2.3'\n"), wasmBin: []byte("#!/bin/sh\necho 'wasmtime 4.5.6'\n")}
	r.emelaTar = targz(t, map[string][]byte{"emela": r.emelaBin, "LICENSE": []byte("x")})
	r.wasmTar = targz(t, map[string][]byte{"wasmtime-v4.5.6-x/wasmtime": r.wasmBin, "wasmtime-v4.5.6-x/wasmtime-min": []byte("other")})
	r.archives = map[string][]byte{"/v1.2.3/emela.tar.gz": r.emelaTar, "/v4.5.6/wasmtime.tar.gz": r.wasmTar}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.requests.Add(1)
		b, ok := r.archives[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// text is a lock of the release's archives.
func (r *release) text() string {
	return fmt.Sprintf(`
[emela]
version = "1.2.3"
[emela.platforms."test/os"]
url = "%[1]s/v1.2.3/emela.tar.gz"
sha256 = "%[2]s"
path = "emela"
bin = "%[3]s"

[wasmtime]
version = "4.5.6"
[wasmtime.platforms."test/os"]
url = "%[1]s/v4.5.6/wasmtime.tar.gz"
sha256 = "%[4]s"
path = "wasmtime-v4.5.6-x/wasmtime"
bin = "%[5]s"
`, r.srv.URL, sum(r.emelaTar), sum(r.emelaBin), sum(r.wasmTar), sum(r.wasmBin))
}

func (r *release) lock(t *testing.T, edit func(s string) string) Lock {
	t.Helper()
	s := r.text()
	if edit != nil {
		s = edit(s)
	}
	l, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (r *release) options(t *testing.T) Options {
	base := t.TempDir()
	return Options{Dir: filepath.Join(base, "bin"), Downloads: filepath.Join(base, "dl"), Platform: "test/os", Client: r.srv.Client()}
}

func TestInstallChecksEverything(t *testing.T) {
	r := newRelease(t)
	l := r.lock(t, nil)
	o := r.options(t)
	if err := l.Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"emela": r.emelaBin, "wasmtime": r.wasmBin} {
		got, err := os.ReadFile(filepath.Join(o.Dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %q, %v", name, got, err)
		}
		if fi, _ := os.Stat(filepath.Join(o.Dir, name)); fi.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s is not executable", name)
		}
	}
	if err := l.Check(o.Dir, "test/os"); err != nil {
		t.Errorf("check after install: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(o.Dir, ".extract-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}

	// Installed: nothing is downloaded again.
	n := r.requests.Load()
	if err := l.Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	// Executables gone but archives cached: still no download.
	os.Remove(filepath.Join(o.Dir, "emela"))
	if err := l.Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if r.requests.Load() != n {
		t.Errorf("downloaded again: %d requests, want %d", r.requests.Load(), n)
	}

	// A replaced executable is found and replaced by the pinned one.
	os.WriteFile(filepath.Join(o.Dir, "wasmtime"), []byte("evil"), 0o755)
	var mm *MismatchError
	if err := l.Check(o.Dir, "test/os"); !errors.As(err, &mm) || mm.Tool.Name != "wasmtime" {
		t.Errorf("check of a replaced wasmtime: %v", err)
	}
	if err := l.Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(o.Dir, "test/os"); err != nil {
		t.Errorf("after reinstall: %v", err)
	}

	// A damaged cached archive is downloaded again.
	os.WriteFile(filepath.Join(o.Downloads, "emela.tar.gz"), []byte("damaged"), 0o644)
	os.Remove(filepath.Join(o.Dir, "emela"))
	if err := l.Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if r.requests.Load() != n+1 {
		t.Errorf("requests %d, want %d", r.requests.Load(), n+1)
	}
}

func TestInstallRefusesWhatTheLockDoesNotSay(t *testing.T) {
	r := newRelease(t)
	for name, edit := range map[string]func(string) string{
		"archive": func(s string) string { return strings.Replace(s, sum(r.emelaTar), strings.Repeat("0", 64), 1) },
		"binary":  func(s string) string { return strings.Replace(s, sum(r.wasmBin), strings.Repeat("0", 64), 1) },
		"missing": func(s string) string { return strings.Replace(s, "/v1.2.3/emela.tar.gz", "/v1.2.3/none.tar.gz", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			l := r.lock(t, edit)
			o := r.options(t)
			if err := l.Install(context.Background(), o); err == nil {
				t.Fatal("installed")
			}
			if err := l.Check(o.Dir, "test/os"); err == nil {
				t.Error("an unchecked executable was installed")
			}
			for _, tool := range l.Tools() {
				if got, err := os.ReadFile(filepath.Join(o.Dir, tool.Name)); err == nil && sum(got) != tool.Platforms["test/os"].Bin {
					t.Errorf("%s is installed with the wrong content", tool.Name)
				}
			}
			if left, _ := filepath.Glob(filepath.Join(o.Downloads, "*.part-*")); len(left) != 0 {
				t.Errorf("partial downloads left behind: %v", left)
			}
		})
	}
}

func TestInstallUnsupportedPlatform(t *testing.T) {
	r := newRelease(t)
	o := r.options(t)
	o.Platform = "plan9/386"
	err := r.lock(t, nil).Install(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "not available for plan9/386 (only for test/os)") {
		t.Errorf("got %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	r := newRelease(t)
	if _, err := Parse([]byte(r.text())); err != nil {
		t.Fatalf("the unedited lock: %v", err)
	}
	for name, edit := range map[string]func(string) string{
		"unknown key":     func(s string) string { return s + "\n[node]\nversion = \"26\"\n" },
		"http":            func(s string) string { return strings.Replace(s, "https://", "http://", 1) },
		"other version":   func(s string) string { return strings.Replace(s, "/v1.2.3/", "/v1.2.4/", 1) },
		"short sha":       func(s string) string { return strings.Replace(s, sum(r.emelaTar), "abc", 1) },
		"path escapes":    func(s string) string { return strings.Replace(s, `path = "emela"`, `path = "../emela"`, 1) },
		"other file":      func(s string) string { return strings.Replace(s, `path = "emela"`, `path = "bin/sh"`, 1) },
		"not an archive":  func(s string) string { return strings.Replace(s, "emela.tar.gz", "emela.zip", 1) },
		"no version":      func(s string) string { return strings.Replace(s, `version = "1.2.3"`, `version = "latest"`, 1) },
		"bad platform":    func(s string) string { return strings.Replace(s, `"test/os"`, `"testos"`, 1) },
		"missing section": func(s string) string { return s[:strings.Index(s, "[wasmtime]")] },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(edit(r.text()))); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestLocate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	l := Pinned()

	repo := t.TempDir()
	sub := filepath.Join(repo, "examples", "lab-l2")
	os.MkdirAll(sub, 0o755)
	if _, err := l.Locate(sub); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nothing installed: %v", err)
	}

	userBin, _, err := l.UserDirs()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(userBin, l.DirName()) {
		t.Errorf("user dir %s does not name the versions", userBin)
	}
	os.MkdirAll(userBin, 0o755)
	os.WriteFile(filepath.Join(userBin, "emela"), nil, 0o755)
	if loc, err := l.Locate(sub); err != nil || loc.Dir != userBin || loc.PomeCache != "" {
		t.Errorf("user toolchain: %+v, %v", loc, err)
	}

	// The repository toolchain (nearest .tools at or above the working directory) comes first,
	// with the Pome cache next to it.
	os.MkdirAll(filepath.Join(repo, ".tools", "bin"), 0o755)
	os.MkdirAll(filepath.Join(repo, ".tools", "pomes"), 0o755)
	os.WriteFile(filepath.Join(repo, ".tools", "bin", "emela"), nil, 0o755)
	loc, err := l.Locate(sub)
	if err != nil || loc.Dir != filepath.Join(repo, ".tools", "bin") || loc.PomeCache != filepath.Join(repo, ".tools", "pomes") {
		t.Errorf("repository toolchain: %+v, %v", loc, err)
	}
}
