package seed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
)

var testBinary = bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 1500)

func enrollParams() EnrollParams {
	sum := sha256.Sum256(testBinary)
	sha := hex.EncodeToString(sum[:])
	m := &Maker{Server: "https://10.0.0.45:8443", CA: []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
		RevisionKey: make([]byte, 32), AllowedOrigins: []string{"Debian:"}, MameletBinary: testBinary}
	return EnrollParams{Mamelet: m.config("ct-web", 7090), EnrollToken: "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo", Hostname: "ct-web",
		BinaryURL: "https://10.0.0.45:8443/v1/mamelet/" + sha, BinarySHA256: sha,
		Issued: time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC), TTL: time.Hour}
}

// The script, byte for byte: what a person reads before running it as root.
func TestGoldenEnrollScript(t *testing.T) {
	script, err := EnrollScript(enrollParams())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "enroll.sh", script)
}

// The script installs exactly the files a seed writes (the same configuration, token, unit and CA,
// with the same modes), so that the two cannot drift apart.
func TestEnrollScriptHasTheSeedsFiles(t *testing.T) {
	p := enrollParams()
	script, err := EnrollScript(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	files, err := mameletFiles(p.Mamelet, p.EnrollToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := filepath.Base(f.Path)
		content := f.Content
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if !strings.Contains(text, "cat >\"$tmp/"+name+"\" <<'MAMELA_END'\n"+content+"MAMELA_END\n") {
			t.Errorf("the script does not write %s as the seed does", f.Path)
		}
		if !regexp.MustCompile(`(?m)^install -m ` + f.Mode + ` "\$tmp/` + regexp.QuoteMeta(name) + `" "\$(etc|unit)`).MatchString(text) {
			t.Errorf("the script does not install %s with mode %s", f.Path, f.Mode)
		}
	}
	var cfg mamelet.Config
	if err := json.Unmarshal([]byte(files[0].Content), &cfg, json.RejectUnknownMembers(true)); err != nil ||
		cfg.Node != "ct-web" || cfg.VMID != 7090 || cfg.EnrollTokenFile != TokenPath || cfg.StateDir != StateDir || cfg.AllowedOrigins[0] != "Debian:" {
		t.Fatalf("config.json: %v %+v", err, cfg)
	}
	for _, want := range []string{"set -eu\n", "--proto =https --cacert \"$tmp/ca.pem\"", "sha256sum -c -", "systemctl daemon-reload\nsystemctl enable --now mamelet.service\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	// The binary is verified before anything is installed: the first install comes after the check.
	if strings.Index(text, "sha256sum -c -") > strings.Index(text, "\ninstall ") {
		t.Error("the script installs something before it has checked the binary")
	}
	// No secret but the token: no private key, no other token.
	for _, not := range []string{"PRIVATE KEY", "tok-", "password"} {
		if strings.Contains(text, not) {
			t.Errorf("the script holds %q", not)
		}
	}
}

// The unit and the seed's install name the paths the script installs to, and the unit has nothing a
// container without nesting lacks (namespaces, a private /tmp, read-only mounts): mamelet runs in an
// unprivileged LXC container as in a VM.
func TestMameletsUnitRunsInAContainer(t *testing.T) {
	if !strings.Contains(unit, "\nExecStart="+BinaryPath+" -config "+ConfigPath+"\n") {
		t.Errorf("the unit does not run %s with %s:\n%s", BinaryPath, ConfigPath, unit)
	}
	for _, directive := range []string{"Private", "Protect", "Restrict", "ReadOnly", "ReadWrite", "Inaccessible", "NoNewPrivileges",
		"SystemCallFilter", "CapabilityBoundingSet", "DynamicUser", "MemoryDeny", "LockPersonality", "Namespace"} {
		if strings.Contains(unit, "\n"+directive) {
			t.Errorf("the unit sets %s..., which needs namespaces an unprivileged container may not give:\n%s", directive, unit)
		}
	}
}

func TestEnrollScriptRefusesBadInput(t *testing.T) {
	for name, change := range map[string]func(*EnrollParams){
		"a bad node":                  func(p *EnrollParams) { p.Mamelet.Node = "Web Server" },
		"no VMID":                     func(p *EnrollParams) { p.Mamelet.VMID = 0 },
		"a hostname with a quote":     func(p *EnrollParams) { p.Hostname = "web'; reboot; '" },
		"a token with a quote":        func(p *EnrollParams) { p.EnrollToken = "abcdefghijklmnop'q" },
		"a short token":               func(p *EnrollParams) { p.EnrollToken = "abc" },
		"an http URL":                 func(p *EnrollParams) { p.BinaryURL = "http://10.0.0.45:8443/v1/mamelet/x" },
		"a URL that breaks out":       func(p *EnrollParams) { p.BinaryURL = "https://10.0.0.45/x'; rm -f /etc/passwd; '" },
		"a sha256 that is not hex":    func(p *EnrollParams) { p.BinarySHA256 = strings.Repeat("z", 64) },
		"a URL of another binary":     func(p *EnrollParams) { p.BinaryURL = "https://10.0.0.45:8443/v1/mamelet/" + strings.Repeat("0", 64) },
		"another token file":          func(p *EnrollParams) { p.Mamelet.EnrollTokenFile = "/root/token" },
		"another state directory":     func(p *EnrollParams) { p.Mamelet.StateDir = "/tmp/mamelet" },
		"no CA":                       func(p *EnrollParams) { p.Mamelet.CA = "" },
		"a CA that ends the heredoc":  func(p *EnrollParams) { p.Mamelet.CA = "x\nMAMELA_END\nreboot\n" },
		"a token without a lifetime":  func(p *EnrollParams) { p.TTL = 0 },
		"a token without issue times": func(p *EnrollParams) { p.Issued = time.Time{} },
	} {
		p := enrollParams()
		change(&p)
		if _, err := EnrollScript(p); err == nil {
			t.Errorf("%s: made a script", name)
		}
	}
}

// tokenStore issues tokens as the database does (43 characters of base64url) and records how.
type tokenStore struct {
	issued []string // node/vmid/seed/ttl of each token
}

func (s *tokenStore) IssueEnrollToken(_ context.Context, node string, vmid int, seed string, ttl time.Duration) (string, error) {
	s.issued = append(s.issued, fmt.Sprintf("%s/%d/%q/%s", node, vmid, seed, ttl))
	return strings.Repeat("Ab3_-", 8) + "xyz", nil
}
func (s *tokenStore) SetEnrollSeed(context.Context, string, string) error { return nil }
func (s *tokenStore) ReusableSeed(context.Context, string, int, time.Duration) (string, bool, error) {
	return "", false, nil
}

// The maker checks what goes into the script before it issues a token (a token is never issued for
// a script that is not made), and issues it for the node and VMID, recorded as no seed: a VM's
// create never reuses it.
func TestNewEnrollScriptIssuesOneTokenForAGoodScript(t *testing.T) {
	st := &tokenStore{}
	m := &Maker{Store: st, Server: "https://10.0.0.45:8443", CA: []byte("-----BEGIN CERTIFICATE-----\n"), RevisionKey: make([]byte, 32),
		AllowedOrigins: []string{"Ubuntu:"}, MameletBinary: testBinary}
	now := time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
	if _, err := m.NewEnrollScript(context.Background(), "ct-web", 7090, "bad host!", "https://10.0.0.45:8443", time.Hour, now); err == nil || len(st.issued) != 0 {
		t.Fatalf("a bad hostname: %v, tokens issued %v", err, st.issued)
	}
	script, err := m.NewEnrollScript(context.Background(), "ct-web", 7090, "ct-web", "https://10.0.0.45:8443/", 30*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.issued) != 1 || st.issued[0] != `ct-web/7090/""/30m0s` {
		t.Fatalf("tokens issued: %v", st.issued)
	}
	sum := sha256.Sum256(testBinary)
	for _, want := range []string{"Ab3_-Ab3_-", "binary_url='https://10.0.0.45:8443/v1/mamelet/" + hex.EncodeToString(sum[:]) + "'", "good for 30m0s"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	m.Plain = true
	if _, err := m.NewEnrollScript(context.Background(), "ct-web", 7090, "ct-web", "https://10.0.0.45:8443", time.Hour, now); err == nil {
		t.Fatal("a plain maker made an enrollment script")
	}
}

// machine is a temporary root the script installs under (MAMELA_INSTALL_ROOT), with stand-ins for
// the commands that need root or a real machine: id, uname, systemctl (which, on enable, does what
// mamelet does when it enrolls) and curl (which serves the binary from a test server's address).
type machine struct {
	root, bin, log string
}

func newMachine(t *testing.T, hostname string, served []byte) machine {
	t.Helper()
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum")
	}
	m := machine{root: t.TempDir(), bin: t.TempDir()}
	m.log = filepath.Join(m.bin, "systemctl.log")
	if err := os.MkdirAll(filepath.Join(m.root, "run/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(served) }))
	t.Cleanup(srv.Close)
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(m.bin, name), []byte("#!/bin/sh\n"+text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("id", "echo 0\n")
	write("uname", "echo "+hostname+"\n")
	// mamelet, started, enrolls: it writes its certificate and deletes the token's file.
	write("systemctl", `echo "$*" >>"`+m.log+`"
if [ "$1" = enable ]; then mkdir -p "$MAMELA_INSTALL_ROOT/var/lib/mamelet"; : >"$MAMELA_INSTALL_ROOT/var/lib/mamelet/cert.pem"; rm -f "$MAMELA_INSTALL_ROOT/etc/mamelet/enroll-token"; fi
`)
	// curl -fsS --proto =https --cacert <ca> -o <out> <url>: fetched from the test server instead.
	write("curl", `out=; while [ $# -gt 1 ]; do [ "$1" = -o ] && out=$2; shift; done
case "$1" in https://*) ;; *) exit 22 ;; esac
exec `+mustLook(t, "curl")+` -fsS -o "$out" "`+srv.URL+`"
`)
	return m
}

func mustLook(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no %s", name)
	}
	return p
}

func (m machine) run(t *testing.T, script []byte, args ...string) (string, error) {
	t.Helper()
	return m.runWith(t, "sh", script, args...)
}

// runWith runs the script with a shell (sh, or another POSIX shell: dash is Debian's sh).
func (m machine) runWith(t *testing.T, shell string, script []byte, args ...string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mamelet-enroll.sh")
	if err := os.WriteFile(path, script, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shell, append([]string{path}, args...)...)
	cmd.Env = append(os.Environ(), "MAMELA_INSTALL_ROOT="+m.root, "PATH="+m.bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (m machine) files(t *testing.T) map[string]os.FileMode {
	t.Helper()
	out := map[string]os.FileMode{}
	filepath.WalkDir(m.root, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(m.root, p)
			fi, _ := d.Info()
			out[rel] = fi.Mode().Perm()
		}
		return nil
	})
	return out
}

// Run on a machine, the script installs mamelet's files with their modes, checks the binary, starts
// mamelet and waits until it has enrolled. It is POSIX sh: dash (Debian's sh) runs it as well.
func TestEnrollScriptInstallsAndEnrolls(t *testing.T) {
	script, err := EnrollScript(enrollParams())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("dash"); err == nil {
		m := newMachine(t, "ct-web", testBinary)
		if out, err := m.runWith(t, "dash", script); err != nil || !strings.Contains(out, "enrolled as ct-web") {
			t.Fatalf("dash: %v\n%s", err, out)
		}
	}
	m := newMachine(t, "ct-web", testBinary)
	out, err := m.run(t, script)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "enrolled as ct-web (VMID 7090)") || !strings.Contains(out, "mamela controlplane agents") {
		t.Errorf("output:\n%s", out)
	}
	want := map[string]os.FileMode{
		"usr/local/sbin/mamelet": 0o755, "etc/mamelet/config.json": 0o600, "etc/mamelet/ca.pem": 0o644,
		"etc/systemd/system/mamelet.service": 0o644, "var/lib/mamelet/cert.pem": 0o600,
	}
	got := m.files(t)
	for name, mode := range want {
		if got[name] != mode {
			t.Errorf("%s: mode %v, want %v", name, got[name], mode)
		}
	}
	if len(got) != len(want) {
		t.Errorf("files %v", got)
	}
	if fi, err := os.Stat(filepath.Join(m.root, "etc/mamelet")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("/etc/mamelet: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(m.root, "usr/local/sbin/mamelet")); !bytes.Equal(b, testBinary) {
		t.Error("the binary installed is not the one served")
	}
	unitFile, _ := os.ReadFile(filepath.Join(m.root, "etc/systemd/system/mamelet.service"))
	if string(unitFile) != unit {
		t.Error("the unit differs from the seed's")
	}
	log, _ := os.ReadFile(m.log)
	if string(log) != "daemon-reload\nenable --now mamelet.service\n" {
		t.Errorf("systemctl was run with:\n%s", log)
	}

	// Run again: mamelet is there, so it refuses and changes nothing.
	before := m.files(t)
	out, err = m.run(t, script)
	if err == nil || !strings.Contains(out, "mamelet is here already") || !strings.Contains(out, "--reenroll") {
		t.Fatalf("a second run: %v\n%s", err, out)
	}
	if after := m.files(t); len(after) != len(before) {
		t.Fatalf("the refused run changed files: %v", after)
	}

	// --reenroll replaces the files and enrolls again: the old certificate is removed first.
	out, err = m.run(t, script, "--reenroll")
	if err != nil || !strings.Contains(out, "enrolled as ct-web") {
		t.Fatalf("--reenroll: %v\n%s", err, out)
	}
	log, _ = os.ReadFile(m.log)
	if !strings.HasSuffix(string(log), "stop mamelet.service\ndaemon-reload\nenable --now mamelet.service\n") {
		t.Errorf("systemctl was run with:\n%s", log)
	}
	if out, err := m.run(t, script, "--force"); err == nil || !strings.Contains(out, "usage") {
		t.Errorf("an unknown flag: %v\n%s", err, out)
	}
}

// Each refusal comes before anything on the machine is changed.
func TestEnrollScriptRefusesBeforeChangingAnything(t *testing.T) {
	script, err := EnrollScript(enrollParams())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		host   string
		served []byte
		setup  func(m machine)
		want   string
	}{
		"another machine":   {host: "db-1", served: testBinary, want: "this machine is db-1, not ct-web"},
		"another binary":    {host: "ct-web", served: []byte("not mamelet"), want: "not the one this script names"},
		"mamelet's unit":    {host: "ct-web", served: testBinary, setup: func(m machine) { touch(t, m.root, "etc/systemd/system/mamelet.service") }, want: "mamelet is here already"},
		"mamelet's binary":  {host: "ct-web", served: testBinary, setup: func(m machine) { touch(t, m.root, "usr/local/sbin/mamelet") }, want: "mamelet is here already"},
		"mamelet's config":  {host: "ct-web", served: testBinary, setup: func(m machine) { touch(t, m.root, "etc/mamelet/config.json") }, want: "mamelet is here already"},
		"no systemd":        {host: "ct-web", served: testBinary, setup: func(m machine) { os.Remove(filepath.Join(m.root, "run/systemd/system")) }, want: "systemd does not run"},
		"not root":          {host: "ct-web", served: testBinary, setup: func(m machine) { os.WriteFile(filepath.Join(m.bin, "id"), []byte("#!/bin/sh\necho 1000\n"), 0o755) }, want: "run it as root"},
		"an unreachable CP": {host: "ct-web", served: testBinary, setup: func(m machine) { os.WriteFile(filepath.Join(m.bin, "curl"), []byte("#!/bin/sh\nexit 7\n"), 0o755) }, want: "could not download mamelet"},
	} {
		m := newMachine(t, tc.host, tc.served)
		if tc.setup != nil {
			tc.setup(m)
		}
		before := m.files(t)
		out, err := m.run(t, script)
		if err == nil || !strings.Contains(out, tc.want) || !strings.Contains(out, "Nothing was changed") {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
		if after := m.files(t); len(after) != len(before) {
			t.Errorf("%s: files changed: %v", name, after)
		}
		if log, _ := os.ReadFile(m.log); len(log) != 0 {
			t.Errorf("%s: systemctl ran: %s", name, log)
		}
	}
}

func touch(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}
