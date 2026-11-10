package toolchain

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/BurntSushi/toml"
)

// mise.toml and mise.lock (what `mise install` and CI install) must name the same tools as
// toolchain.lock (what Mamela runs) and go.mod (the Go it builds with).
func TestMiseMatchesTheLocks(t *testing.T) {
	root := filepath.Join("..", "..")
	var config struct {
		Tools map[string]string `toml:"tools"`
	}
	if _, err := toml.DecodeFile(filepath.Join(root, "mise.toml"), &config); err != nil {
		t.Fatal(err)
	}
	l := Pinned()
	for name, want := range map[string]string{"github:emela-lang/emela": l.Emela.Version, "wasmtime": l.Wasmtime.Version} {
		if got := config.Tools[name]; got != want {
			t.Errorf("mise.toml %s = %q, but toolchain.lock says %s", name, got, want)
		}
	}
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^toolchain go(\S+)$`).FindSubmatch(gomod)
	if m == nil || config.Tools["go"] != string(m[1]) {
		t.Errorf("mise.toml go = %q, but go.mod has toolchain %q", config.Tools["go"], m)
	}

	var lock struct {
		Tools map[string][]map[string]any `toml:"tools"`
	}
	if _, err := toml.DecodeFile(filepath.Join(root, "mise.lock"), &lock); err != nil {
		t.Fatal(err)
	}
	platforms := map[string]string{"linux-x64": "linux/amd64", "macos-arm64": "darwin/arm64"}
	for name, tool := range map[string]Tool{"github:emela-lang/emela": l.Emela, "wasmtime": l.Wasmtime} {
		entries := lock.Tools[name]
		if len(entries) != 1 || entries[0]["version"] != tool.Version {
			t.Errorf("mise.lock %s: %v, want one entry of version %s", name, entries, tool.Version)
			continue
		}
		for misePlatform, platform := range platforms {
			e, _ := entries[0]["platforms."+misePlatform].(map[string]any)
			if want := "sha256:" + tool.Platforms[platform].SHA256; e == nil || e["checksum"] != want {
				t.Errorf("mise.lock %s %s: checksum %v, but toolchain.lock says %s", name, misePlatform, e["checksum"], want)
			}
		}
	}
	for _, name := range []string{"go", "node"} {
		for misePlatform := range platforms {
			if e := lock.Tools[name]; len(e) != 1 || e[0]["platforms."+misePlatform] == nil {
				t.Errorf("mise.lock has no %s for %s (run: mise lock --platform linux-x64,macos-arm64)", name, misePlatform)
			}
		}
	}
}
