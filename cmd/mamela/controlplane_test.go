package main

import (
	"archive/tar"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/lab"
)

// The bundle holds what the host needs, rewritten for it, with secrets readable only by their
// owner; the site in it is valid as is. It refuses a loopback URL.
func TestControlPlaneBundle(t *testing.T) {
	dir := t.TempDir()
	example, err := os.ReadFile(filepath.Join("..", "..", "lab", "site.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	lt, at := write("lab-token", "secret-lab\n", 0o600), write("audit-token", "secret-audit\n", 0o600)
	key := write("id.pub", "ssh-ed25519 AAAA alice@laptop\n", 0o644)
	bin := write("mamelet", "\x7fELF", 0o755)
	cpDir := filepath.Join(dir, "cp")
	if err := os.Mkdir(cpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write("cp/ca.pem", "CA", 0o600)
	site := strings.NewReplacer(`"~/.config/mamela/lab-token"`, `"`+lt+`"`, `"~/.config/mamela/audit-token"`, `"`+at+`"`,
		`"~/.ssh/id_ed25519.pub"`, `"`+key+`"`, `"~/.config/mamela/cp"`, `"`+cpDir+`"`,
		`".mamela/bin/mamelet-linux-amd64"`, `"`+bin+`"`, `tls_ca_file = ""`, ``).Replace(string(example))
	sitePath := write("site.toml", site, 0o600)
	db := filepath.Join(dir, "state.db")
	out := filepath.Join(dir, "bundle.tar")

	if code, _, stderr := runCLI("controlplane", "bundle", "--site", sitePath, "--db", db, "--out", out, "--url", "https://127.0.0.1:8443"); code == 0 ||
		!strings.Contains(stderr, "loopback") {
		t.Fatalf("a loopback URL: %d %s", code, stderr)
	}
	code, stdout, stderr := runCLI("controlplane", "bundle", "--site", sitePath, "--db", db, "--out", out,
		"--url", "https://10.0.0.45:8443", "--host", "mamela-cp")
	if code != 0 {
		t.Fatalf("%d %s %s", code, stdout, stderr)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode %v %v", fi.Mode(), err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	got := map[string]string{}
	modes := map[string]int64{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name], modes[h.Name] = string(b), h.Mode
	}
	for _, name := range []string{"etc/site.toml", "etc/secrets/lab-token", "etc/secrets/audit-token", "etc/guest-ssh.pub",
		"var/cp/ca.pem", "var/mamelet-linux-amd64", "var/state.db", "mamela-serve.service", "install.sh"} {
		if _, ok := got[name]; !ok {
			t.Errorf("bundle lacks %s", name)
		}
	}
	if got["etc/secrets/lab-token"] != "secret-lab\n" || modes["etc/secrets/lab-token"] != 0o600 || modes["var/state.db"] != 0o600 {
		t.Errorf("secrets: %q mode %o", got["etc/secrets/lab-token"], modes["etc/secrets/lab-token"])
	}
	moved := filepath.Join(dir, "moved.toml")
	if err := os.WriteFile(moved, []byte(got["etc/site.toml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := lab.LoadSite(moved)
	if err != nil {
		t.Fatalf("the host's site is not valid: %v\n%s", err, got["etc/site.toml"])
	}
	c := s.ControlPlane
	if c.URL != "https://10.0.0.45:8443" || c.Listen != "0.0.0.0:8443" || c.Host != "mamela-cp" || c.StateDir != "/var/lib/mamela/cp" ||
		c.TunnelJump != "" || s.Proxmox.TokenSecretFile != "/etc/mamela/secrets/lab-token" || s.Guest.SSHPublicKeyFile != "/etc/mamela/guest-ssh.pub" {
		t.Fatalf("host site: %+v %+v", c, s.Proxmox)
	}
	if !strings.Contains(stdout, "move_to") {
		t.Errorf("does not say how existing mamelets move:\n%s", stdout)
	}
	// Never overwrites an archive.
	if code, _, _ := runCLI("controlplane", "bundle", "--site", sitePath, "--db", db, "--out", out, "--url", "https://10.0.0.45:8443"); code == 0 {
		t.Fatal("overwrote an existing bundle")
	}
}

// The installer refuses a host that has a Control Plane already, before writing anything: another
// bundle's keys, tokens or site must never be mixed with the database that is there.
func TestInstallRefusesAnExistingControlPlane(t *testing.T) {
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "install.sh"), []byte(installScript), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, existing := range []string{"var/lib/mamela/state.db", "var/lib/mamela/cp", "etc/mamela/site.toml", "etc/mamela/secrets"} {
		root := t.TempDir()
		mustWrite := func(rel string, mode os.FileMode) {
			p := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), mode); err != nil {
				t.Fatal(err)
			}
		}
		mustWrite("usr/local/bin/mamela", 0o755)
		mustWrite(existing, 0o600)
		cmd := exec.Command("bash", filepath.Join(bundle, "install.sh"), bundle)
		cmd.Env = append(os.Environ(), "MAMELA_INSTALL_ROOT="+root)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "has a Control Plane already") {
			t.Fatalf("%s: %v\n%s", existing, err, out)
		}
		var files []string
		filepath.WalkDir(root, func(p string, d os.DirEntry, _ error) error {
			if !d.IsDir() {
				rel, _ := filepath.Rel(root, p)
				files = append(files, rel)
			}
			return nil
		})
		if len(files) != 2 {
			t.Fatalf("%s: the installer wrote %v", existing, files)
		}
	}
}
