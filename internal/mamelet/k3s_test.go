package mamelet

import (
	"context"
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
	e.k3sBody = body
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset, _ := K3sAsset()
		if r.URL.EscapedPath() != "/"+strings.ReplaceAll(k3sVersion, "+", "%2B")+"/"+asset {
			http.NotFound(w, r)
			return
		}
		downloads++
		w.Write([]byte(e.k3sBody))
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

// blockingRunner never finishes a command until its context ends.
type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ string, _ []string, _ []string) ([]byte, []byte, int, error) {
	<-ctx.Done()
	return nil, nil, -1, ctx.Err()
}

// A service that does not finish starting (a k3s agent waiting for its server) does not hold
// mamelet: the start returns at the deadline and says so.
func TestStartHasADeadline(t *testing.T) {
	defer func(d time.Duration) { StartTimeout = d }(StartTimeout)
	StartTimeout = 50 * time.Millisecond
	s := &System{Runner: blockingRunner{}}
	start := time.Now()
	err := s.Start(context.Background(), "k3s")
	if err == nil || !strings.Contains(err.Error(), "did not finish starting") || time.Since(start) > 5*time.Second {
		t.Fatalf("start: %v after %s", err, time.Since(start))
	}
}

// An upgrade replaces the binary and restarts a k3s that runs, so that it runs what was approved.
// A process that started before the binary on disk was written is reported Stale, and an approved
// install for the binary already in place restarts it without downloading again. Nothing restarts
// a k3s that does not run (starting it is a NodeService).
func TestK3sUpgradeRestartsARunningK3s(t *testing.T) {
	v1, v2 := "\x7fELF k3s one", "\x7fELF k3s two"
	e, downloads := k3sEnv(t, v1)
	root := e.a.System.Root
	grant := func(serial int64, binary string, attempt int) {
		e.publishK3s(serial, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary), Role: "server", Token: testToken,
			Install: &noderev.Once{Operation: k3sOp, Attempt: attempt, Action: noderev.ActionInstall}})
	}
	restarts := func() int {
		n := 0
		for _, c := range e.os.commands {
			if strings.HasPrefix(c, progSystemctl+" restart") {
				n++
			}
		}
		return n
	}

	// The first installation does not restart anything: k3s does not run yet.
	grant(1, v1, 1)
	if rep := e.round(); !rep.K3s.Installed || rep.K3s.Running || restarts() != 0 {
		t.Fatalf("install: %+v, %d restarts", rep.K3s, restarts())
	}

	// k3s runs, started after the binary was written: in sync, nothing is stale.
	future := time.Now().Add(time.Hour).Unix()
	e.os.services["k3s"] = &fakeService{exists: true, enabled: true, running: true}
	e.os.started["k3s"] = future
	e.os.kubectl = "srv\tv1.33.5+k3s1\tTrue\nag\tv1.33.5+k3s1\tFalse\n"
	e.publishK3s(2, &noderev.K3s{Version: k3sVersion, SHA256: sum(v1), Role: "server", Token: testToken})
	rep := e.round()
	if !rep.K3s.Installed || !rep.K3s.Running || rep.K3s.Stale {
		t.Fatalf("a fresh process: %+v", rep.K3s)
	}
	if len(rep.K3s.Nodes) != 2 || rep.K3s.Nodes[0] != (noderev.K3sNode{Name: "ag", Version: "v1.33.5+k3s1"}) || !rep.K3s.Nodes[1].Ready {
		t.Fatalf("nodes: %+v", rep.K3s.Nodes)
	}

	// The approved upgrade: another binary replaces the one in place, and k3s is restarted.
	e.k3sBody = v2
	e.os.restartedAt = future + 10
	grant(3, v2, 2)
	rep = e.round()
	if b, _ := os.ReadFile(filepath.Join(root, k3sBinary)); string(b) != v2 || *downloads != 2 {
		t.Fatalf("the binary was not replaced (%d downloads)", *downloads)
	}
	if restarts() != 1 || !rep.K3s.Installed || rep.K3s.Stale || rep.Results[0].Outcome != noderev.OutcomeDone || !strings.Contains(rep.Results[0].Detail, "restarted") {
		t.Fatalf("after the upgrade: %+v %+v, %d restarts", rep.K3s, rep.Results, restarts())
	}

	// A process older than the binary is Stale (the binary was replaced under it, or the restart
	// did not happen): it is not in sync, and an approved install restarts it without a download.
	e.os.started["k3s"] = time.Now().Add(-time.Hour).Unix()
	e.publishK3s(4, &noderev.K3s{Version: k3sVersion, SHA256: sum(v2), Role: "server", Token: testToken})
	if rep := e.round(); !rep.K3s.Stale || !rep.K3s.Installed || restarts() != 1 {
		t.Fatalf("a stale process: %+v, %d restarts", rep.K3s, restarts())
	}
	grant(5, v2, 3)
	rep = e.round()
	if rep.K3s.Stale || restarts() != 2 || *downloads != 2 {
		t.Fatalf("restart of a stale process: %+v, %d restarts, %d downloads", rep.K3s, restarts(), *downloads)
	}
}

func TestParseK3sNodes(t *testing.T) {
	got := ParseK3sNodes("b\tv1.33.5+k3s1\tFalse\na\tv1.33.5+k3s1\tTrue\n\nodd line\n")
	if len(got) != 2 || got[0].Name != "a" || !got[0].Ready || got[1].Ready {
		t.Fatalf("nodes: %+v", got)
	}
	if len(ParseK3sNodes("")) != 0 {
		t.Fatal("nodes of nothing")
	}
}

// A manifest is written only with an approved grant, exactly as the revision says, once per
// attempt; without a grant it is only observed.
func TestK3sManifestIsWrittenOnlyAsApproved(t *testing.T) {
	e := newEnv(t)
	e.a.System.Root = t.TempDir()
	path := filepath.Join(e.a.System.Root, noderev.K3sManifestDir, "mamela-argocd.yaml")
	const content = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: argocd\n"
	publish := func(serial int64, content string, w *noderev.Once) {
		s, err := noderev.Sign(e.key, noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: serial,
			IssuedAt: e.now, ExpiresAt: e.now.Add(time.Hour), Packages: []noderev.Package{}, Services: []noderev.Service{},
			Manifests: []noderev.Manifest{{Name: "argocd", Content: content, Write: w}}})
		if err != nil {
			t.Fatal(err)
		}
		e.cp.signed, e.cp.err = s, nil
	}
	write := func(attempt int) *noderev.Once {
		return &noderev.Once{Operation: k3sOp, Attempt: attempt, Action: noderev.ActionWrite}
	}

	// Without a grant: observed, not written.
	publish(1, content, nil)
	rep := e.round()
	if len(rep.Manifests) != 1 || rep.Manifests[0].Name != "argocd" || rep.Manifests[0].SHA256 != "" {
		t.Fatalf("observation: %+v", rep.Manifests)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("written without a grant: %v", err)
	}

	// With a grant: written exactly, readable by root only, and reported.
	publish(2, content, write(1))
	rep = e.round()
	if b, _ := os.ReadFile(path); string(b) != content {
		t.Fatalf("content: %q", b)
	}
	if fi, _ := os.Stat(path); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Error("the manifest is readable by others")
	}
	if rep.Manifests[0].SHA256 != noderev.ManifestSHA256(content) || len(rep.Results) != 1 || rep.Results[0].Outcome != noderev.OutcomeDone {
		t.Fatalf("after the write: %+v %+v", rep.Manifests, rep.Results)
	}

	// The same attempt does not write again (a change by hand stays until a new approval).
	os.WriteFile(path, []byte("changed by hand\n"), 0o600)
	e.round()
	if b, _ := os.ReadFile(path); string(b) != "changed by hand\n" {
		t.Error("the same attempt wrote again")
	}
	if rep := e.round(); rep.Manifests[0].SHA256 != noderev.ManifestSHA256("changed by hand\n") {
		t.Errorf("the change by hand is not reported: %+v", rep.Manifests)
	}

	// A new approval of new content replaces the file.
	publish(3, content+"# next\n", write(2))
	e.round()
	if b, _ := os.ReadFile(path); string(b) != content+"# next\n" {
		t.Fatalf("content after the second write: %q", b)
	}
}

// A Kubernetes node is removed only as approved: a Ready one is cordoned, drained and deleted, in
// that order; one that is not Ready is only deleted; a drain that fails deletes nothing and leaves
// the node schedulable; the server's own node and a node already gone are handled; and an old
// grant does nothing twice.
func TestK3sNodeIsRemovedOnlyAsApproved(t *testing.T) {
	e, _ := k3sEnv(t, "\x7fELF k3s")
	e.os.services["k3s"] = &fakeService{exists: true, enabled: true, running: true}
	e.os.started["k3s"] = time.Now().Add(time.Hour).Unix()
	nodes := "web\tv1\tTrue\nag1\tv1\tTrue\nag2\tv1\tFalse\n"
	remove := func(serial int64, op, node string) noderev.Report {
		e.publishK3s(serial, &noderev.K3s{Version: k3sVersion, SHA256: sum("\x7fELF k3s"), Role: "server", Token: testToken,
			Remove: &noderev.NodeRemoval{Node: node, Once: noderev.Once{Operation: op, Attempt: 1, Action: noderev.ActionRemove}}})
		return e.round()
	}
	kubectl := func() []string {
		var out []string
		for _, c := range e.os.commands {
			if strings.HasPrefix(c, progK3s) && !strings.Contains(c, " get ") {
				out = append(out, strings.TrimPrefix(c, progK3s+" kubectl "))
			}
		}
		return out
	}
	opA, opB, opC, opD := "op-0000000000000000000000a1", "op-0000000000000000000000a2", "op-0000000000000000000000a3", "op-0000000000000000000000a4"

	// A Ready node: cordon, drain, delete.
	e.os.kubectl = nodes
	rep := remove(1, opA, "ag1")
	got := kubectl()
	if len(got) != 3 || got[0] != "cordon ag1" || !strings.HasPrefix(got[1], "drain ag1 --ignore-daemonsets --delete-emptydir-data") || got[2] != "delete node ag1" {
		t.Fatalf("commands: %v", got)
	}
	if rep.Results[0].Outcome != noderev.OutcomeDone || !strings.Contains(rep.Results[0].Detail, "drained and deleted") {
		t.Fatalf("result: %+v", rep.Results)
	}
	// The same grant does not run again.
	e.os.commands = nil
	remove(2, opA, "ag1")
	if len(kubectl()) != 0 {
		t.Fatalf("a grant ran twice: %v", kubectl())
	}

	// A node that is not Ready: deleted, not drained.
	e.os.commands = nil
	rep = remove(3, opB, "ag2")
	if got := kubectl(); len(got) != 1 || got[0] != "delete node ag2" || !strings.Contains(rep.Results[0].Detail, "not drained") {
		t.Fatalf("a Not Ready node: %v %+v", got, rep.Results)
	}

	// A node that is already gone is a success without commands.
	e.os.commands = nil
	if rep = remove(4, opC, "ag2"); rep.Results[0].Outcome != noderev.OutcomeDone || len(kubectl()) != 0 {
		t.Fatalf("already gone: %v %+v", kubectl(), rep.Results)
	}

	// The server's own node is never removed.
	e.os.commands = nil
	if rep = remove(5, opD, "web"); rep.Results[0].Outcome != noderev.OutcomeFailed || len(kubectl()) != 0 || !strings.Contains(e.os.kubectl, "web\t") {
		t.Fatalf("own node: %v %+v", kubectl(), rep.Results)
	}

	// A drain that does not finish deletes nothing and uncordons.
	e.os.kubectl, e.os.kubectlFails, e.os.commands = "web\tv1\tTrue\nag3\tv1\tTrue\n", "drain", nil
	rep = remove(6, "op-0000000000000000000000a5", "ag3")
	got = kubectl()
	if rep.Results[0].Outcome != noderev.OutcomeFailed || len(got) != 3 || got[2] != "uncordon ag3" || !strings.Contains(e.os.kubectl, "ag3\t") {
		t.Fatalf("a failed drain: %v %+v", got, rep.Results)
	}
}

// A node is removed only by a server that runs, and only from a revision fetched this round.
func TestK3sNodeRemovalWaitsForARunningServer(t *testing.T) {
	e, _ := k3sEnv(t, "\x7fELF k3s")
	e.os.kubectl = "web\tv1\tTrue\nag1\tv1\tFalse\n"
	e.publishK3s(1, &noderev.K3s{Version: k3sVersion, SHA256: sum("\x7fELF k3s"), Role: "server", Token: testToken,
		Remove: &noderev.NodeRemoval{Node: "ag1", Once: noderev.Once{Operation: "op-0000000000000000000000b1", Attempt: 1, Action: noderev.ActionRemove}}})
	rep := e.round() // k3s does not run
	for _, c := range e.os.commands {
		if strings.Contains(c, " delete ") {
			t.Fatalf("a stopped server removed a node: %v", e.os.commands)
		}
	}
	if len(rep.Results) != 0 {
		t.Fatalf("results: %+v", rep.Results)
	}
}
