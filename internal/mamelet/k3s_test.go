package mamelet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

const (
	k3sVersion = "v1.33.5+k3s1"
	testToken  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// k3sEnv serves a release whose binary is body.
func k3sEnv(t *testing.T, body string) (*env, *int) {
	e := newEnv(t)
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset, _ := K3sAsset()
		if r.URL.EscapedPath() != "/"+strings.ReplaceAll(k3sVersion, "+", "%2B")+"/"+asset {
			http.NotFound(w, r)
			return
		}
		downloads++
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	e.a.System.Root, e.a.System.HTTP, e.a.System.K3sReleases = t.TempDir(), srv.Client(), srv.URL
	return e, &downloads
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (e *env) publishK3s(serial int64, k *noderev.K3s) {
	e.t.Helper()
	s, err := noderev.Sign(e.key, noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: serial,
		IssuedAt: e.now, ExpiresAt: e.now.Add(time.Hour), Packages: []noderev.Package{}, Services: []noderev.Service{}, K3s: k})
	if err != nil {
		e.t.Fatal(err)
	}
	e.cp.signed, e.cp.err = s, nil
}

const k3sOp = "op-0000000000000000000000f3"

// k3s is installed only with an approved grant, only if the download has the approved sha256,
// and only once per attempt.
func TestK3sIsInstalledOnlyAsApproved(t *testing.T) {
	binary := "\x7fELF k3s"
	e, downloads := k3sEnv(t, binary)
	root := e.a.System.Root

	e.publishK3s(1, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary), Role: "server", Token: testToken})
	if rep := e.round(); rep.K3s == nil || rep.K3s.Installed || *downloads != 0 {
		t.Fatalf("without a grant: %+v, %d downloads", rep.K3s, *downloads)
	}

	// Another binary than the approved one: nothing is installed.
	e.publishK3s(2, &noderev.K3s{Version: k3sVersion, SHA256: sum("another binary"), Role: "server", Token: testToken,
		Install: &noderev.Once{Operation: k3sOp, Attempt: 1, Action: noderev.ActionInstall}})
	rep := e.round()
	if len(rep.Results) != 1 || rep.Results[0].Outcome != noderev.OutcomeFailed || !strings.Contains(rep.Results[0].Detail, "not the approved") {
		t.Fatalf("results: %+v", rep.Results)
	}
	if _, err := os.Stat(filepath.Join(root, k3sBinary)); !errors.Is(err, os.ErrNotExist) || rep.K3s.Installed {
		t.Fatalf("a binary with another sha256 was installed: %v", err)
	}

	e.publishK3s(3, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary), Role: "server", Token: testToken,
		Install: &noderev.Once{Operation: k3sOp, Attempt: 2, Action: noderev.ActionInstall}})
	rep = e.round()
	if !rep.K3s.Installed || rep.K3s.SHA256 != sum(binary) || rep.Results[0].Outcome != noderev.OutcomeDone {
		t.Fatalf("approved install: %+v %+v", rep.K3s, rep.Results)
	}
	if b, _ := os.ReadFile(filepath.Join(root, k3sBinary)); string(b) != binary {
		t.Error("the binary is not the download")
	}
	if fi, _ := os.Stat(filepath.Join(root, k3sBinary)); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Error("the binary is not executable")
	}
	if u, _ := os.ReadFile(filepath.Join(root, k3sUnit)); string(u) != K3sUnit("server", "") ||
		!strings.Contains(string(u), "ExecStart=/usr/local/bin/k3s server --token-file=/etc/rancher/k3s/cluster-token --disable=traefik --disable=metrics-server --write-kubeconfig-mode=0600\n") {
		t.Errorf("unit:\n%s", u)
	}
	if tok, _ := os.ReadFile(filepath.Join(root, "etc/rancher/k3s/cluster-token")); string(tok) != testToken+"\n" {
		t.Errorf("token file: %q", tok)
	}
	if fi, _ := os.Stat(filepath.Join(root, "etc/rancher/k3s/cluster-token")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Error("the token file is readable by others")
	}
	if !strings.Contains(strings.Join(e.os.commands, "\n"), progSystemctl+" daemon-reload") {
		t.Error("no daemon-reload")
	}
	n := *downloads
	e.round()
	if *downloads != n {
		t.Error("the same attempt downloaded again")
	}

	// A unit changed by hand is reported, not believed.
	os.WriteFile(filepath.Join(root, k3sUnit), []byte("[Service]\nExecStart=/bin/sh -c evil\n"), 0o644)
	if rep := e.round(); rep.K3s.Installed || rep.K3s.Error == "" {
		t.Errorf("a changed unit: %+v", rep.K3s)
	}
}

// Without a fresh revision (offline), a grant in the cached one installs nothing.
func TestK3sNeedsAFreshRevision(t *testing.T) {
	binary := "\x7fELF k3s"
	e, downloads := k3sEnv(t, binary)
	e.publishK3s(1, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary), Role: "server", Token: testToken})
	e.round()
	e.publishK3s(2, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary), Role: "server", Token: testToken,
		Install: &noderev.Once{Operation: k3sOp, Attempt: 1, Action: noderev.ActionInstall}})
	signed := e.cp.signed
	e.cp.err = errors.New("offline")
	e.round()
	if *downloads != 0 {
		t.Fatalf("installed offline from a revision it had not fetched")
	}
	_ = signed
}

// An agent joins the server its revision names; when the server's address changes, the installed
// agent is no longer what the revision says, and a new approved install rewrites only its unit.
func TestK3sAgentFollowsItsRevision(t *testing.T) {
	binary := "\x7fELF k3s"
	e, downloads := k3sEnv(t, binary)
	e.a.System.Addresses = func() []string { return []string{"192.0.2.20"} }
	root := e.a.System.Root
	agent := func(serial int64, server string, attempt int) {
		e.publishK3s(serial, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary), Role: "agent", Server: server, Token: testToken,
			Install: &noderev.Once{Operation: k3sOp, Attempt: attempt, Action: noderev.ActionInstall}})
	}
	agent(1, "https://192.0.2.10:6443", 1)
	rep := e.round()
	if !rep.K3s.Installed || len(rep.K3s.Addresses) != 1 || rep.K3s.Addresses[0] != "192.0.2.20" {
		t.Fatalf("agent: %+v", rep.K3s)
	}
	if u, _ := os.ReadFile(filepath.Join(root, k3sUnit)); !strings.Contains(string(u),
		"ExecStart=/usr/local/bin/k3s agent --server=https://192.0.2.10:6443 --token-file=/etc/rancher/k3s/cluster-token\n") {
		t.Errorf("unit:\n%s", u)
	}
	if rep.K3s.Server != "https://192.0.2.10:6443" {
		t.Errorf("reported server %q", rep.K3s.Server)
	}
	e.publishK3s(2, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary), Role: "agent", Server: "https://192.0.2.11:6443", Token: testToken})
	if rep := e.round(); rep.K3s.Installed || rep.K3s.Error != "" || rep.K3s.Server != "https://192.0.2.10:6443" {
		t.Fatalf("after the server moved: %+v", rep.K3s)
	}
	agent(3, "https://192.0.2.11:6443", 2)
	if rep := e.round(); !rep.K3s.Installed || *downloads != 1 {
		t.Fatalf("reinstalled: %+v, %d downloads (the binary in place is kept)", rep.K3s, *downloads)
	}
}

// A node reports the address it is reached by: that of the default route's interface, never one
// k3s adds (flannel.1 and cni0 are in 10.42.0.0/16, and sort before a LAN address).
func TestNodeAddresses(t *testing.T) {
	got := NodeAddresses([]net.IP{net.ParseIP("10.42.0.0"), net.ParseIP("10.42.0.1"), net.ParseIP("192.168.0.40"),
		net.ParseIP("10.43.0.1"), net.ParseIP("127.0.0.1"), net.ParseIP("fe80::1"), net.ParseIP("169.254.1.1"), net.ParseIP("192.168.0.40")})
	if strings.Join(got, ",") != "192.168.0.40" {
		t.Fatalf("addresses %v", got)
	}
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"cni0\t00002A0A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"wlan0\t00000000\t0100A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"ens18\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"down0\t00000000\t0100A8C0\t0002\t0\t0\t1\t00000000\t0\t0\t0\n"
	if iface, ok := DefaultRouteInterface(table); !ok || iface != "ens18" {
		t.Fatalf("default route interface %q %v", iface, ok)
	}
	if _, ok := DefaultRouteInterface("Iface\tDestination\n"); ok {
		t.Fatal("a table without a default route has an interface")
	}
}
