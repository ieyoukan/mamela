package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/provider/external"
	"github.com/ieyoukan/mamela/internal/seed"
	sitedef "github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// The bundle holds what the host needs, rewritten for it, with secrets readable only by their
// owner; the site and the providers' APIs in it are valid as is. It refuses a loopback URL.
func TestControlPlaneBundle(t *testing.T) {
	dir := t.TempDir()
	example, err := os.ReadFile(filepath.Join("..", "..", "mamela.site.example.toml"))
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
	site := strings.NewReplacer(`"~/.ssh/id_ed25519.pub"`, `"`+key+`"`, `"~/.config/mamela/cp"`, `"`+cpDir+`"`,
		`".mamela/bin/mamelet-linux-amd64"`, `"`+bin+`"`).Replace(string(example))
	sitePath := write("site.toml", site, 0o600)
	providers := write("providers.toml", `[provider.proxmox]
api_url = "https://pve.example:8006/api2/json"
auth = "proxmox"
token_id = "mamela@pve!lab"
token_file = "`+lt+`"
catalog = "pome:proxmox"
guard_token_id = "mamela-audit@pve!audit"
guard_token_file = "`+at+`"

[provider.proxmox.guard]
node = "pve1"
max_vms = 3
`, 0o600)
	db := filepath.Join(dir, "state.db")
	out := filepath.Join(dir, "bundle.tar")

	if code, _, stderr := runCLI("controlplane", "bundle", "--site", sitePath, "--providers", providers, "--db", db, "--out", out, "--url", "https://127.0.0.1:8443"); code == 0 ||
		!strings.Contains(stderr, "loopback") {
		t.Fatalf("a loopback URL: %d %s", code, stderr)
	}
	code, stdout, stderr := runCLI("controlplane", "bundle", "--site", sitePath, "--providers", providers, "--db", db, "--out", out,
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
	for _, name := range []string{"etc/site.toml", "etc/providers.toml", "etc/secrets/proxmox-token", "etc/secrets/proxmox-guard-token", "etc/guest-ssh.pub",
		"var/cp/ca.pem", "var/mamelet-linux-amd64", "var/state.db", "mamela-serve.service", "mamela-api.service", "mamelacd.service", "install.sh"} {
		if _, ok := got[name]; !ok {
			t.Errorf("bundle lacks %s", name)
		}
	}
	if got["etc/secrets/proxmox-token"] != "secret-lab\n" || modes["etc/secrets/proxmox-token"] != 0o600 || modes["var/state.db"] != 0o600 ||
		got["etc/secrets/proxmox-guard-token"] != "secret-audit\n" {
		t.Errorf("secrets: %q mode %o", got["etc/secrets/proxmox-token"], modes["etc/secrets/proxmox-token"])
	}
	moved := filepath.Join(dir, "moved.toml")
	if err := os.WriteFile(moved, []byte(got["etc/site.toml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := sitedef.Load(moved)
	if err != nil {
		t.Fatalf("the host's site is not valid: %v\n%s", err, got["etc/site.toml"])
	}
	c := s.ControlPlane
	if c.URL != "https://10.0.0.45:8443" || c.Listen != "0.0.0.0:8443" || c.Host != "mamela-cp" || c.StateDir != "/var/lib/mamela/cp" ||
		s.Guest.SSHPublicKeyFile != "/etc/mamela/guest-ssh.pub" {
		t.Fatalf("host site: %+v %+v", c, s.Guest)
	}
	movedProviders := filepath.Join(dir, "moved-providers.toml")
	if err := os.WriteFile(movedProviders, []byte(got["etc/providers.toml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := external.LoadConfig(movedProviders)
	if err != nil {
		t.Fatalf("the host's providers are not valid: %v\n%s", err, got["etc/providers.toml"])
	}
	if a := cfg.Provider["proxmox"]; a.TokenFile != "/etc/mamela/secrets/proxmox-token" || a.GuardTokenFile != "/etc/mamela/secrets/proxmox-guard-token" ||
		a.TokenID != "mamela@pve!lab" || a.Guard["node"] != "pve1" {
		t.Fatalf("host providers: %+v", a)
	}
	if !strings.Contains(stdout, "move_to") {
		t.Errorf("does not say how existing mamelets move:\n%s", stdout)
	}
	// Never overwrites an archive.
	if code, _, _ := runCLI("controlplane", "bundle", "--site", sitePath, "--providers", providers, "--db", db, "--out", out, "--url", "https://10.0.0.45:8443"); code == 0 {
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
	for _, existing := range []string{"var/lib/mamela/state.db", "var/lib/mamela/cp", "etc/mamela/site.toml", "etc/mamela/providers.toml", "etc/mamela/secrets"} {
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

// A cloud machine's seed: Mamela gives the node a number, and makes user-data that fetches mamelet
// from the Control Plane by its sha256, with a fresh one-time token.
func TestUserDataSeedArtifact(t *testing.T) {
	ctx := context.Background()
	site, err := sitedef.Load(filepath.Join("..", "..", "mamela.site.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	site.ControlPlane.URL = "https://cp.example:8443"
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"), wallClock)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bin := []byte("\x7fELF mamelet")
	m := &artifacts{Site: site, Nodes: st, Seeds: &seed.Maker{Store: st, Server: site.ControlPlane.URL, CA: []byte("CA"),
		RevisionKey: make([]byte, 32), User: "mamela", AllowedOrigins: []string{"Ubuntu:"}, MameletBinary: bin}}
	p, err := m.Make(ctx, protocol.Artifact{ID: "seed", Kind: protocol.ArtifactSeed, Node: "web", Network: &protocol.Network{Mode: "dhcp"}, Format: protocol.SeedUserData})
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := p.Open()
	ud, _ := io.ReadAll(rc)
	sum := sha256.Sum256(bin)
	if p.Info.NodeID != store.FirstNodeNumber || !strings.Contains(string(ud), "https://cp.example:8443/v1/mamelet/"+hex.EncodeToString(sum[:])) ||
		!strings.HasPrefix(string(ud), "#cloud-config") {
		t.Fatalf("node %d, user-data:\n%s", p.Info.NodeID, ud)
	}
}

// A site without a Control Plane makes seeds without mamelet (the machine is made and configured;
// nothing of Mamela runs on it): no token, no binary, and the node still gets its number.
func TestPlainSeedArtifact(t *testing.T) {
	ctx := context.Background()
	site, err := sitedef.Load(filepath.Join("..", "..", "mamela.site.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	site.ControlPlane = sitedef.ControlPlane{}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"), wallClock)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := &artifacts{Site: site, Nodes: st, Seeds: &seed.Maker{User: "mamela", Plain: true}}
	for _, format := range []string{"", protocol.SeedUserData} {
		p, err := m.Make(ctx, protocol.Artifact{ID: "seed", Kind: protocol.ArtifactSeed, Node: "web", Network: &protocol.Network{Mode: "dhcp"}, Format: format})
		if err != nil {
			t.Fatalf("format %q: %v", format, err)
		}
		rc, _ := p.Open()
		img, _ := io.ReadAll(rc)
		if p.Info.NodeID != store.FirstNodeNumber || bytes.Contains(img, []byte("mamelet")) || bytes.Contains(img, []byte("enroll")) {
			t.Fatalf("format %q: node %d, mentions mamelet: %v", format, p.Info.NodeID, bytes.Contains(img, []byte("mamelet")))
		}
	}
	// Without a seed maker at all, a seed is still refused.
	if _, err := (&artifacts{Site: site, Nodes: st}).Make(ctx, protocol.Artifact{ID: "seed", Kind: protocol.ArtifactSeed, Node: "web", Network: &protocol.Network{Mode: "dhcp"}}); err == nil {
		t.Fatal("a seed without a maker")
	}
}

// A host that has a Control Plane already gets mamelacd's units and a script that installs them,
// without the rest of a bundle: the script is valid, and mamelacd's database is private to it.
func TestGitopsUnits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "units")
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"controlplane", "gitops-units", "--out", dir}, &out, &errb); code != 0 {
		t.Fatalf("%d: %s", code, errb.String())
	}
	for _, f := range []string{"mamela-api.service", "mamelacd.service", "install-gitops.sh"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := exec.Command("bash", "-n", filepath.Join(dir, "install-gitops.sh")).CombinedOutput(); err != nil {
		t.Fatalf("install-gitops.sh: %v\n%s", err, b)
	}
	script, _ := os.ReadFile(filepath.Join(dir, "install-gitops.sh"))
	if !bytes.Contains(script, []byte("groupadd --system mamela-api")) || bytes.Contains(script, []byte("mamela-cp")) {
		t.Errorf("the script prepares mamelacd only:\n%s", script)
	}
	unit, _ := os.ReadFile(filepath.Join(dir, "mamelacd.service"))
	for _, want := range []string{"UMask=0077", "InaccessiblePaths=-/etc/mamela -/var/lib/mamela", "NoNewPrivileges=yes"} {
		if !bytes.Contains(unit, []byte(want)) {
			t.Errorf("mamelacd.service lacks %q", want)
		}
	}
}

// A declaration that pins a mamelet this Control Plane does not serve is warned about at plan time.
func TestMameletServedWarning(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mamelet")
	if err := os.WriteFile(bin, []byte("\x7fELF mamelet"), 0o755); err != nil {
		t.Fatal(err)
	}
	site := &sitedef.Site{ControlPlane: sitedef.ControlPlane{URL: "https://cp:8443", MameletBinary: bin}}
	sum := sha256.Sum256([]byte("\x7fELF mamelet"))
	served := hex.EncodeToString(sum[:])
	graphOf := func(sha string) *graph.Graph {
		g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
			`{"kind":"TestVM","name":"app","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}},` +
			`{"kind":"NodeMamelet","name":"app/mamelet","spec":{"node":"app","sha256":"` + sha + `"}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	if w := mameletServedWarning(site, graphOf(served)); w != "" {
		t.Errorf("the served release is pinned: %s", w)
	}
	if w := mameletServedWarning(site, graphOf(strings.Repeat("ab", 32))); !strings.Contains(w, "MAM-NODE-0103") || !strings.Contains(w, served[:12]) {
		t.Errorf("another release is pinned: %q", w)
	}
	g, _ := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[]}`))
	if w := mameletServedWarning(site, g); w != "" {
		t.Errorf("nothing pinned: %s", w)
	}
}
