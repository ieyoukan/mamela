package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/store"
)

// enrollFixture is a Control Plane with its keys and a mamelet binary, whose last plan saw ct-web
// (VMID 7090) as Mamela's: it published a revision for it, as for every machine Mamela manages.
func enrollFixture(t *testing.T) (cpFixture, []byte) {
	t.Helper()
	f := newCPFixture(t)
	f.withKeys(t)
	bin := append([]byte("\x7fELF\x02\x01\x01"), make([]byte, 64)...)
	bin[18] = 0x3e // x86-64
	binPath := filepath.Join(filepath.Dir(f.site), "mamelet-linux-amd64")
	if err := os.WriteFile(binPath, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(f.site)
	if err != nil {
		t.Fatal(err)
	}
	text = bytes.Replace(text, []byte(`mamelet_binary = ".mamela/bin/mamelet-linux-amd64"`), []byte(`mamelet_binary = "`+binPath+`"`), 1)
	if err := os.WriteFile(f.site, text, 0o600); err != nil {
		t.Fatal(err)
	}
	withRevision(t, f, "ct-web", 7090)
	return f, bin
}

// withRevision publishes a revision for a node, as a plan does for a machine Mamela manages.
func withRevision(t *testing.T, f cpFixture, node string, vmid int) {
	t.Helper()
	st, err := openStore(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.PublishNodeRevision(context.Background(), node, func(*noderev.Revision) (noderev.Revision, error) {
		return noderev.Revision{VMID: vmid}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// tokens counts the enrollment tokens in the database.
func tokens(t *testing.T, f cpFixture) int {
	t.Helper()
	db, err := sql.Open("sqlite", f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM enroll_tokens`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// scriptFile returns the content of a file the script writes (between its here-document's lines).
func scriptFile(t *testing.T, script, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(script, "cat >\"$tmp/"+name+"\" <<'MAMELA_END'\n")
	if !ok {
		t.Fatalf("the script writes no %s", name)
	}
	content, _, _ := strings.Cut(rest, "MAMELA_END\n")
	return content
}

// The script of an adopted machine: written with mode 0600, with the Control Plane's CA and revision
// key, mamelet's URL by its sha256, and a one-time token that works for that node and VMID only, once.
func TestEnrollWritesAScriptForAManagedMachine(t *testing.T) {
	ctx := context.Background()
	f, bin := enrollFixture(t)
	out := filepath.Join(t.TempDir(), "mamelet-enroll-ct-web.sh")
	code, stdout, stderr := runCLI(f.args("controlplane", "enroll", "--node", "ct-web", "--vmid", "7090", "--out", out, "--ttl", "30m")...)
	if code != 0 {
		t.Fatalf("enroll: %d %s", code, stderr)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the script: %v %v", fi, err)
	}
	b, _ := os.ReadFile(out)
	script := string(b)
	sum := sha256.Sum256(bin)
	for _, want := range []string{"#!/bin/sh\n", "set -eu\n", "node='ct-web'\n", "vmid=7090\n", "host='ct-web'\n",
		"binary_url='https://10.0.0.45:8443/v1/mamelet/" + hex.EncodeToString(sum[:]) + "'\n", "good for 30m0s"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	pki, err := loadPKIFor(t, f)
	if err != nil {
		t.Fatal(err)
	}
	var cfg mamelet.Config
	if err := json.Unmarshal([]byte(scriptFile(t, script, "config.json")), &cfg, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if cfg.Node != "ct-web" || cfg.VMID != 7090 || cfg.Server != "https://10.0.0.45:8443" || cfg.CA != string(pki.CAPEM()) ||
		!bytes.Equal(cfg.RevisionKey, pki.RevisionPublicKey()) || strings.Join(cfg.AllowedOrigins, ",") != "Ubuntu:" {
		t.Fatalf("config.json: %+v", cfg)
	}
	if scriptFile(t, script, "ca.pem") != string(pki.CAPEM()) {
		t.Error("ca.pem is not the Control Plane's CA")
	}
	// Nothing secret but the token: none of the Control Plane's private keys.
	for _, name := range []string{"ca-key.pem", "revision-key.pem", "server-key.pem"} {
		key, err := os.ReadFile(filepath.Join(f.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(script, strings.TrimSpace(string(key))) || strings.Contains(script, "PRIVATE KEY") {
			t.Fatalf("the script holds %s", name)
		}
	}
	for _, want := range []string{"pct push 7090 mamelet-enroll-ct-web.sh /root/mamelet-enroll.sh --perms 0600", "pct exec 7090 -- sh /root/mamelet-enroll.sh",
		"scp " + out, "sudo sh mamelet-enroll.sh", "must reach the Control Plane at https://10.0.0.45:8443", "controlplane agents --db " + f.db,
		"hostname is ct-web", "Ubuntu:", "Debian-Security:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output lacks %q:\n%s", want, stdout)
		}
	}

	// The token: for this node and VMID, once.
	token := strings.TrimSpace(scriptFile(t, script, "enroll-token"))
	st, err := openStore(ctx, f.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []struct {
		node string
		vmid int
	}{{"ct-web", 7091}, {"ct-db", 7090}} {
		if ok, _ := st.EnrollTokenUsable(ctx, token, other.node, other.vmid); ok {
			t.Errorf("the token works for %s (VMID %d)", other.node, other.vmid)
		}
		if err := st.Enroll(ctx, token, other.node, other.vmid, "x"); !errors.Is(err, store.ErrEnrollRefused) {
			t.Errorf("enrolled %s (VMID %d) with it: %v", other.node, other.vmid, err)
		}
	}
	if err := st.Enroll(ctx, token, "ct-web", 7090, "serial-1"); err != nil {
		t.Fatalf("the node could not enroll with its token: %v", err)
	}
	if err := st.Enroll(ctx, token, "ct-web", 7090, "serial-2"); !errors.Is(err, store.ErrEnrollRefused) {
		t.Fatalf("the token worked twice: %v", err)
	}
	st.Close()

	// The node is registered now: another script only with --reenroll, which the script needs too.
	again := filepath.Join(t.TempDir(), "again.sh")
	if code, _, stderr := runCLI(f.args("controlplane", "enroll", "--node", "ct-web", "--vmid", "7090", "--out", again)...); code == 0 || !strings.Contains(stderr, "MAM-PKI-0305") || !strings.Contains(stderr, "reenroll") {
		t.Fatalf("a registered node: %d %s", code, stderr)
	}
	if _, err := os.Stat(again); err == nil {
		t.Fatal("a refused script was written")
	}
	if code, _, stderr := runCLI(f.args("controlplane", "enroll", "--node", "ct-web", "--vmid", "7090", "--out", again, "--reenroll")...); code != 0 {
		t.Fatalf("--reenroll: %d %s", code, stderr)
	}
	// Never over a file that exists: refused before a token is issued.
	n := tokens(t, f)
	if code, _, stderr := runCLI(f.args("controlplane", "enroll", "--node", "ct-web", "--vmid", "7090", "--out", out, "--reenroll")...); code == 0 || !strings.Contains(stderr, "exists") {
		t.Fatalf("over an existing script: %d %s", code, stderr)
	}
	if after, _ := os.ReadFile(out); !bytes.Equal(after, b) || tokens(t, f) != n {
		t.Fatal("a refused enroll changed the script or issued a token")
	}
}

// The token ends after --ttl: a script run later is refused by the Control Plane.
func TestEnrollTokenEnds(t *testing.T) {
	ctx := context.Background()
	f, _ := enrollFixture(t)
	out := filepath.Join(t.TempDir(), "s.sh")
	if code, _, stderr := runCLI(f.args("controlplane", "enroll", "--node", "ct-web", "--vmid", "7090", "--out", out, "--ttl", "10m")...); code != 0 {
		t.Fatalf("enroll: %d %s", code, stderr)
	}
	b, _ := os.ReadFile(out)
	token := strings.TrimSpace(scriptFile(t, string(b), "enroll-token"))
	later, err := store.Open(ctx, f.db, func() time.Time { return time.Now().Add(11 * time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	defer later.Close()
	if ok, _ := later.EnrollTokenUsable(ctx, token, "ct-web", 7090); ok {
		t.Fatal("the token is good after its time")
	}
	if err := later.Enroll(ctx, token, "ct-web", 7090, "x"); !errors.Is(err, store.ErrEnrollRefused) {
		t.Fatalf("enrolled with a token that ended: %v", err)
	}
}

// Only a machine Mamela manages gets a script, and only for the ID the plan observed: each refusal
// says why and what to do, writes no file and issues no token.
func TestEnrollRefusals(t *testing.T) {
	f, _ := enrollFixture(t)
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"a machine that is not adopted", []string{"--node", "legacy", "--vmid", "7095"}, []string{"MAM-PKI-0303", "adopt", "MAM-PLAN-0114", "nothing was issued"}},
		{"another VMID", []string{"--node", "ct-web", "--vmid", "7091"}, []string{"MAM-PKI-0304", "VMID 7090", "not 7091"}},
		{"a token for a month", []string{"--node", "ct-web", "--vmid", "7090", "--ttl", "720h"}, []string{"24h"}},
		{"a bad node name", []string{"--node", "Web Server", "--vmid", "7090"}, []string{"not a node's name"}},
		{"a bad hostname", []string{"--node", "ct-web", "--vmid", "7090", "--hostname", "x'; reboot"}, []string{"not a hostname"}},
	}
	for i, c := range cases {
		out := filepath.Join(dir, c.name+".sh")
		args := append([]string{"controlplane", "enroll", "--out", out}, c.args...)
		code, _, stderr := runCLI(f.args(args...)...)
		if code == 0 {
			t.Errorf("%s: made a script", c.name)
		}
		for _, want := range c.want {
			if !strings.Contains(stderr, want) {
				t.Errorf("%s: the refusal lacks %q:\n%s", c.name, want, stderr)
			}
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s: wrote %s", c.name, out)
		}
		if n := tokens(t, f); n != 0 {
			t.Fatalf("%s (case %d): %d tokens issued", c.name, i, n)
		}
	}
	// Required flags.
	if code, _, stderr := runCLI(f.args("controlplane", "enroll", "--node", "ct-web", "--vmid", "7090")...); code != 2 || !strings.Contains(stderr, "--out") {
		t.Errorf("without --out: %d %s", code, stderr)
	}
	// A Control Plane that has no keys yet (or the wrong site): none are made for an enrollment.
	g := newCPFixture(t)
	withRevision(t, g, "ct-web", 7090)
	if code, _, stderr := runCLI(g.args("controlplane", "enroll", "--node", "ct-web", "--vmid", "7090", "--out", filepath.Join(dir, "nokeys.sh"))...); code == 0 || !strings.Contains(stderr, "MAM-PKI-0201") {
		t.Errorf("without keys: %d %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "ca.pem")); err == nil {
		t.Error("an enrollment made a CA")
	}
}

// An observe-only node is enrolled (its mamelet observes and reports, which is what observe-only is
// for), and the output says it is given no permission; the APT origins come from the policy in force.
func TestEnrollObserveOnlyNodeWithAPolicy(t *testing.T) {
	f, _ := enrollFixture(t)
	text, _ := os.ReadFile(f.site)
	if err := os.WriteFile(f.site, append([]byte("observe_only = [\"ct-web\"]\n"), text...), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(pol, []byte(`{"version":"site/1","maxObservationAgeSeconds":600,"autoStartServices":[],"allowedAptOrigins":["Debian:","Debian-Security:"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "s.sh")
	code, stdout, stderr := runCLI(f.args("controlplane", "enroll", "--node", "ct-web", "--vmid", "7090", "--out", out, "--policy", pol, "--hostname", "legacy-web")...)
	if code != 0 {
		t.Fatalf("enroll: %d %s", code, stderr)
	}
	if !strings.Contains(stdout, "MAM-POL-0312") || !strings.Contains(stdout, "no permission") || strings.Contains(stdout, "For a Debian machine") {
		t.Errorf("output:\n%s", stdout)
	}
	b, _ := os.ReadFile(out)
	var cfg mamelet.Config
	if err := json.Unmarshal([]byte(scriptFile(t, string(b), "config.json")), &cfg); err != nil || strings.Join(cfg.AllowedOrigins, ",") != "Debian:,Debian-Security:" {
		t.Fatalf("config.json: %v %+v", err, cfg)
	}
	if !strings.Contains(string(b), "host='legacy-web'\n") {
		t.Error("the script does not check the hostname given")
	}
}
