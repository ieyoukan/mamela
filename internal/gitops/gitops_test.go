package gitops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarball(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "abc"}})
	for _, h := range entries {
		data := []byte("x")
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(data))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(data)
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestExtract(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tree")
	data := tarball(t, tar.Header{Name: "o-r-1/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "o-r-1/infra/src/main.emel", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "o-r-1/run.sh", Typeflag: tar.TypeReg, Mode: 0o755})
	if err := Extract(bytes.NewReader(data), dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "infra/src/main.emel")); err != nil || string(b) != "x" {
		t.Fatalf("%q %v", b, err)
	}
	if err := Extract(bytes.NewReader(data), dir); err == nil {
		t.Error("extracted over an existing tree")
	}
}

// A tree with a link, a path that leaves it, or a device is refused whole.
func TestExtractRefused(t *testing.T) {
	for name, h := range map[string]tar.Header{
		"symlink":  {Name: "o-r-1/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"hardlink": {Name: "o-r-1/link", Typeflag: tar.TypeLink, Linkname: "o-r-1/a"},
		"dotdot":   {Name: "o-r-1/../../escape", Typeflag: tar.TypeReg, Mode: 0o644},
		"device":   {Name: "o-r-1/dev", Typeflag: tar.TypeChar},
	} {
		dir := filepath.Join(t.TempDir(), "tree")
		err := Extract(bytes.NewReader(tarball(t, tar.Header{Name: "o-r-1/a", Typeflag: tar.TypeReg, Mode: 0o644}, h)), dir)
		if err == nil {
			t.Errorf("%s: extracted", name)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("%s: the refused tree is there", name)
		}
	}
}

func TestConfigCheck(t *testing.T) {
	ok := Config{Repo: "me/infra", Branch: "main", Dirs: []string{"infra"}, TokenFile: "t", Authors: []string{"a"}, Approvers: []string{"a"}, WorkDir: "w"}
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*Config){
		"repo":      func(c *Config) { c.Repo = "infra" },
		"dirs":      func(c *Config) { c.Dirs = nil },
		"escape":    func(c *Config) { c.Dirs = []string{"../x"} },
		"absolute":  func(c *Config) { c.Dirs = []string{"/etc"} },
		"token":     func(c *Config) { c.TokenFile = "" },
		"approvers": func(c *Config) { c.Approvers = nil },
		"authors":   func(c *Config) { c.Authors = nil },
		"ttl":       func(c *Config) { c.ApprovalTTL = "-1h" },
		"interval":  func(c *Config) { c.Interval = "1s" },
	} {
		c := ok
		f(&c)
		if err := c.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if !strings.Contains(clip(strings.Repeat("a", 60000)), "(cut)") || strings.Contains(clip("```x```"), "```") {
		t.Error("clip")
	}
}
