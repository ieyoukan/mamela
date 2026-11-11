package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

const example = "../../mamela.site.example.toml"

func exampleSite(t *testing.T) *Site {
	t.Helper()
	s, err := Load(example)
	if err != nil {
		t.Fatalf("the example site must be valid: %v", err)
	}
	return s
}

func TestLoadRejectsUnknownKeysAndWeakSettings(t *testing.T) {
	text, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(string) string{
		"unknown key":         func(s string) string { return s + "\n[extra]\nx = 1\n" },
		"a Proxmox section":   func(s string) string { return s + "\n[proxmox]\nnode = \"pve1\"\n" },
		"other schema":        func(s string) string { return strings.Replace(s, `"mamela-site/v1"`, `"mamela-lab/v1"`, 1) },
		"no dhcp setting":     func(s string) string { return strings.Replace(s, "dhcp = true", "", 1) },
		"public static range": func(s string) string { return strings.ReplaceAll(s, `"10.0.0.4`, `"8.8.8.4`) },
		"reversed range": func(s string) string {
			return strings.Replace(s, `static_last = "10.0.0.49"`, `static_last = "10.0.0.30"`, 1)
		},
		"tiny memory budget": func(s string) string {
			return strings.Replace(s, "max_memory_mib_per_vm = 4096", "max_memory_mib_per_vm = 64", 1)
		},
		"user with a space": func(s string) string { return strings.Replace(s, `user = "mamela"`, `user = "a b"`, 1) },
		"http control plane": func(s string) string {
			return strings.Replace(s, `"https://10.0.0.45:8443"`, `"http://10.0.0.45:8443"`, 1)
		},
		"control plane no key": func(s string) string { return strings.Replace(s, `state_dir = "~/.config/mamela/cp"`, ``, 1) },
	} {
		path := filepath.Join(t.TempDir(), "site.toml")
		os.WriteFile(path, []byte(edit(string(text))), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An old lab site says where its Proxmox settings went.
	path := filepath.Join(t.TempDir(), "site.toml")
	os.WriteFile(path, []byte(strings.Replace(string(text), `"mamela-site/v1"`, `"mamela-lab/v1"`, 1)+"\n[proxmox]\nnode = \"pve1\"\n"), 0o600)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "ADR 0006") {
		t.Errorf("an old lab site: %v", err)
	}
}

func TestControlPlaneListen(t *testing.T) {
	s := exampleSite(t)
	for _, c := range []struct {
		url, listen, moveTo, host string
		ok                        bool
	}{
		{"https://127.0.0.1:8443", "127.0.0.1:8443", "", "", true},
		{"https://127.0.0.1:8443", "0.0.0.0:8443", "", "", false}, // reached through a tunnel: exposed for nothing
		{"https://10.0.0.45:8443", "0.0.0.0:8443", "", "mamela-cp", true},
		{"https://10.0.0.45:8443", "10.0.0.45:8443", "", "", true},
		{"https://127.0.0.1:8443", "127.0.0.1:8443", "https://10.0.0.45:8443", "", true},
		{"https://127.0.0.1:8443", "127.0.0.1:8443", "http://10.0.0.45:8443", "", false},
		{"https://127.0.0.1:8443", "127.0.0.1:8443", "https://10.0.0.45", "", false},
		{"https://10.0.0.45:8443", "0.0.0.0:8443", "", "bad name!", false},
	} {
		v := *s
		v.ControlPlane.URL, v.ControlPlane.Listen, v.ControlPlane.MoveTo, v.ControlPlane.Host = c.url, c.listen, c.moveTo, c.host
		if err := v.Validate(); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestReadSecretRequiresPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(path, []byte("abc\n"), 0o644)
	if _, err := ReadSecret(path); err == nil {
		t.Error("accepted a world-readable secret file")
	}
	os.Chmod(path, 0o600)
	if s, err := ReadSecret(path); err != nil || s != "abc" {
		t.Errorf("ReadSecret: %q %v", s, err)
	}
}

func TestAllows(t *testing.T) {
	s := exampleSite(t)
	static := func(addr string) graph.Network {
		return graph.Network{Mode: graph.NetworkStatic, Address: addr, Gateway: "10.0.0.1", DNS: []string{"10.0.0.1"}}
	}
	if s.Allows("a", static("10.0.0.40/24")) != nil || s.Allows("a", graph.Network{Mode: graph.NetworkDHCP}) != nil {
		t.Error("the site refuses what it allows")
	}
	if err := s.Allows("a", static("10.0.0.50/24")); err == nil || !strings.Contains(err.Error(), "machine a") {
		t.Errorf("outside the range: %v", err)
	}
	f := false
	s.Network.DHCP = &f
	if s.Allows("a", graph.Network{Mode: graph.NetworkDHCP}) == nil {
		t.Error("DHCP allowed by a site that does not allow it")
	}
}
