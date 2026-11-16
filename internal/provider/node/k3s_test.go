package node_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/reconcile"
)

// k3sOS is tinyOS whose k3s unit appears when systemd reloads (mamelet wrote it).
type k3sOS struct{ *tinyOS }

func (o k3sOS) Run(ctx context.Context, program string, args []string, env []string) ([]byte, []byte, int, error) {
	if len(args) > 0 && args[0] == "daemon-reload" {
		o.exists = true
		return nil, nil, 0, nil
	}
	if len(args) > 0 && args[0] == "restart" {
		o.restarts++
		o.running = true
		return nil, nil, 0, nil
	}
	if strings.HasSuffix(program, "/k3s") {
		if len(args) > 2 && args[1] == "delete" { // kubectl delete node <name>
			var keep []string
			for _, l := range strings.Split(o.kubectl, "\n") {
				if l != "" && !strings.HasPrefix(l, args[len(args)-1]+"\t") {
					keep = append(keep, l)
				}
			}
			o.kubectl = strings.Join(keep, "\n") + "\n"
			o.kubectlCalls = append(o.kubectlCalls, strings.Join(args[1:], " "))
			return nil, nil, 0, nil
		}
		if len(args) > 1 && args[1] != "get" {
			o.kubectlCalls = append(o.kubectlCalls, strings.Join(args[1:], " "))
		}
		return []byte(o.kubectl), nil, 0, nil
	}
	return o.tinyOS.Run(ctx, program, args, env)
}

// A single-node Kubernetes through mamelet: the binary is installed only after approval and only
// with the declared sha256, then the service is enabled and started after its own approval.
func TestK3sThroughMamelet(t *testing.T) {
	binary := "\x7fELF k3s server"
	sum := sha256.Sum256([]byte(binary))
	downloads := 0
	graphOf := func(sha, version string) *graph.Graph {
		g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
			`{"kind":"TestVM","name":"app","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}},` +
			`{"kind":"NodeK3s","name":"app/k3s","spec":{"node":"app","role":"server","sha256":"` + sha + `","version":"` + version + `"}},` +
			`{"kind":"NodeService","name":"app/k3s","spec":{"enabled":true,"node":"app","running":true,"service":"k3s"}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		w.Write([]byte(binary))
	}))
	defer srv.Close()

	e := newEnv(t)
	e.g = graphOf(hex.EncodeToString(sum[:]), "v1.37.2+k3s1")
	e.agent.System = &mamelet.System{Runner: k3sOS{e.os}, AllowedOrigins: []string{"Ubuntu:"}, Root: t.TempDir(),
		HTTP: srv.Client(), K3sReleases: srv.URL, Addresses: func() []string { return []string{"192.0.2.10"} }}
	e.publish()
	e.round()

	p := e.plan()
	k := change(t, p, "NodeK3s app/k3s")
	if k.Status != plan.StatusChange || k.Verdict != policy.ApprovalRequired || k.Operations[0].Op != policy.OpInstallK3s ||
		!strings.Contains(k.Diffs[2].To, "v1.37.2%2Bk3s1/k3s") || !strings.Contains(k.Diffs[4].To, "--disable=traefik") ||
		strings.Contains(fmt.Sprint(k.Diffs), token(t, e)) {
		t.Fatalf("k3s: %+v", k)
	}
	if svc := change(t, p, "NodeService app/k3s"); svc.Status != plan.StatusWaiting {
		t.Fatalf("service before k3s is installed: %+v", svc)
	}
	if downloads != 0 {
		t.Fatal("downloaded before approval")
	}
	if rep := e.applyApproved(p); rep.Stopped != nil || rep.Steps[0].Outcome != reconcile.OutcomeDone || downloads != 1 {
		t.Fatalf("install: %+v (downloads %d)", rep, downloads)
	}

	p = e.plan()
	if k := change(t, p, "NodeK3s app/k3s"); k.Status != plan.StatusInSync {
		t.Fatalf("k3s after install: %+v", k)
	}
	if svc := change(t, p, "NodeService app/k3s"); svc.Verdict != policy.ApprovalRequired || len(svc.Operations) != 2 {
		t.Fatalf("service: %+v", svc)
	}
	if rep := e.applyApproved(p); rep.Stopped != nil || !e.os.enabled || !e.os.running {
		t.Fatalf("service: %+v", rep)
	}
	for _, c := range e.plan().Changes {
		if c.Status != plan.StatusInSync {
			t.Errorf("not converged: %s %s", c.ID(), c.Status)
		}
	}
	if downloads != 1 {
		t.Errorf("%d downloads", downloads)
	}

	// An agent of this server joins it at the address its mamelet reported, with the same token.
	url, ok, err := node.K3sServerURL(e.ctx, e.st, "app")
	if err != nil || !ok || url != "https://192.0.2.10:6443" {
		t.Fatalf("server URL: %q %v %v", url, ok, err)
	}

	// A new release is declared: with k3s running, that is an upgrade. It waits until the server
	// reports its Kubernetes nodes Ready, needs its own approval, and replaces the binary and
	// restarts k3s; afterwards the node is in sync.
	binary2 := "\x7fELF k3s server, the next release"
	binary = binary2
	sum2 := sha256.Sum256([]byte(binary2))
	e.g = graphOf(hex.EncodeToString(sum2[:]), "v1.37.3+k3s1")
	e.publish()
	e.round()
	if k := change(t, e.plan(), "NodeK3s app/k3s"); k.Status != plan.StatusWaiting || k.Note.Code != "MAM-PLAN-0115" {
		t.Fatalf("upgrade before the cluster is reported Ready: %+v", k)
	}
	e.os.kubectl = "app\tv1.37.2+k3s1\tTrue\n"
	e.round()
	p = e.plan()
	k = change(t, p, "NodeK3s app/k3s")
	if k.Status != plan.StatusChange || k.Verdict != policy.ApprovalRequired || len(k.Operations) != 1 || k.Operations[0].Op != policy.OpUpgradeK3s ||
		k.Diffs[0].From != "v1.37.2+k3s1" || k.Diffs[0].To != "v1.37.3+k3s1" {
		t.Fatalf("upgrade: %+v", k)
	}
	if downloads != 1 || e.os.restarts != 0 {
		t.Fatal("changed before approval")
	}
	if rep := e.applyApproved(p); rep.Stopped != nil || rep.Steps[0].Outcome != reconcile.OutcomeDone || downloads != 2 || e.os.restarts != 1 {
		t.Fatalf("upgrade: %+v (downloads %d, restarts %d)", rep, downloads, e.os.restarts)
	}
	e.round()
	if k := change(t, e.plan(), "NodeK3s app/k3s"); k.Status != plan.StatusInSync {
		t.Fatalf("k3s after the upgrade: %+v", k)
	}
}

func token(t *testing.T, e *env) string {
	tok, err := e.st.K3sToken(e.ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A manifest goes from the declaration to the server's auto-deploy directory only through an
// approved plan, and exactly with the content the plan showed; changing the content is a new plan.
func TestK3sManifestThroughMamelet(t *testing.T) {
	binary := "\x7fELF k3s server"
	sum := sha256.Sum256([]byte(binary))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(binary)) }))
	defer srv.Close()
	root := t.TempDir()
	graphOf := func(content string) *graph.Graph {
		c, _ := json.Marshal(content)
		g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
			`{"kind":"TestVM","name":"app","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}},` +
			`{"kind":"NodeK3s","name":"app/k3s","spec":{"node":"app","role":"server","sha256":"` + hex.EncodeToString(sum[:]) + `","version":"v1.37.2+k3s1"}},` +
			`{"kind":"NodeK3sManifest","name":"app/manifest/argocd","spec":{"node":"app","name":"argocd","content":` + string(c) + `}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	const id = "NodeK3sManifest app/manifest/argocd"
	file := filepath.Join(root, noderev.K3sManifestDir, "mamela-argocd.yaml")

	e := newEnv(t)
	e.g = graphOf("kind: Namespace\nmetadata:\n  name: argocd\n")
	e.agent.System = &mamelet.System{Runner: k3sOS{e.os}, AllowedOrigins: []string{"Ubuntu:"}, Root: root,
		HTTP: srv.Client(), K3sReleases: srv.URL, Addresses: func() []string { return []string{"192.0.2.10"} }}
	e.publish()
	e.round()

	p := e.plan()
	m := change(t, p, id)
	if m.Status != plan.StatusChange || m.Verdict != policy.ApprovalRequired || m.Operations[0].Op != policy.OpWriteK3sManifest {
		t.Fatalf("manifest: %+v", m)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("written before approval")
	}
	if rep := e.applyApproved(p); rep.Stopped != nil {
		t.Fatalf("apply: %+v", rep)
	}
	if b, _ := os.ReadFile(file); string(b) != "kind: Namespace\nmetadata:\n  name: argocd\n" {
		t.Fatalf("the manifest on the server: %q", b)
	}
	e.round()
	if m := change(t, e.plan(), id); m.Status != plan.StatusInSync {
		t.Fatalf("after the write: %+v", m)
	}

	// Another content is a new change with its own approval.
	e.g = graphOf("kind: Namespace\nmetadata:\n  name: argocd\n  labels: {a: b}\n")
	e.publish()
	e.round()
	p = e.plan()
	if m := change(t, p, id); m.Status != plan.StatusChange {
		t.Fatalf("changed content: %+v", m)
	}
	if rep := e.applyApproved(p); rep.Stopped != nil {
		t.Fatalf("second apply: %+v", rep)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "labels") {
		t.Fatalf("the changed manifest: %q", b)
	}
}

// A Kubernetes node that is left over (Not Ready, its machine gone) is removed through an approved
// plan that names it: the plan is a deletion (only the host approves it), the grant carries exactly
// that node, mamelet deletes it (a Not Ready node is not drained), and the next plan is in sync.
func TestK3sNodeRemovalThroughMamelet(t *testing.T) {
	binary := "\x7fELF k3s server"
	sum := sha256.Sum256([]byte(binary))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(binary)) }))
	defer srv.Close()
	e := newEnv(t)
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
		`{"kind":"TestVM","name":"app","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}},` +
		`{"kind":"NodeK3s","name":"app/k3s","spec":{"node":"app","role":"server","sha256":"` + hex.EncodeToString(sum[:]) + `","version":"v1.37.2+k3s1"}},` +
		`{"kind":"NodeService","name":"app/k3s","spec":{"enabled":true,"node":"app","running":true,"service":"k3s"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e.g = g
	e.agent.System = &mamelet.System{Runner: k3sOS{e.os}, AllowedOrigins: []string{"Ubuntu:"}, Root: t.TempDir(),
		HTTP: srv.Client(), K3sReleases: srv.URL, Addresses: func() []string { return []string{"192.0.2.10"} }}
	e.publish()
	e.round()
	e.applyApproved(e.plan()) // install
	e.round()
	e.applyApproved(e.plan()) // enable and start
	e.os.kubectl = "app\tv1.37.2+k3s1\tTrue\nghost\tv1.37.2+k3s1\tFalse\nkeep\tv1.37.2+k3s1\tTrue\n"
	e.round()
	const id = "NodeK3s app/k3s"
	if k := change(t, e.plan(), id); k.Status != plan.StatusChange {
		// "keep" is Ready and "ghost" is Not Ready; both are undeclared and have no machine: only
		// the Not Ready one is a left-over.
		t.Fatalf("no removal planned: %+v", k)
	}
	p := e.plan()
	k := change(t, p, id)
	if len(k.Operations) != 1 || k.Operations[0].Op != policy.OpRemoveK3sNode || !k.Deletes() || p.Removals() != 1 ||
		len(k.Deleted()) != 1 || k.Deleted()[0] != "Kubernetes node ghost" || k.Verdict != policy.ApprovalRequired {
		t.Fatalf("removal: %+v", k)
	}
	if len(e.os.kubectlCalls) != 0 {
		t.Fatal("changed before approval")
	}
	if rep := e.applyApproved(p); rep.Stopped != nil || rep.Steps[0].Outcome != reconcile.OutcomeDone {
		t.Fatalf("apply: %+v", rep)
	}
	if fmt.Sprint(e.os.kubectlCalls) != "[delete node ghost]" || !strings.Contains(e.os.kubectl, "keep\t") || strings.Contains(e.os.kubectl, "ghost") {
		t.Fatalf("kubectl: %v, nodes %q", e.os.kubectlCalls, e.os.kubectl)
	}
	e.round()
	if k := change(t, e.plan(), id); k.Status != plan.StatusInSync || len(k.Operations) != 0 {
		t.Fatalf("after the removal: %+v", k)
	}
}

// mameletOS runs a staged mamelet (-version) as a mamelet does, and is tinyOS for the rest.
type mameletOS struct{ *tinyOS }

func (o mameletOS) Run(ctx context.Context, program string, args []string, env []string) ([]byte, []byte, int, error) {
	if strings.HasSuffix(program, "mamelet.next") {
		return []byte("mamelet test\n"), nil, 0, nil
	}
	return o.tinyOS.Run(ctx, program, args, env)
}

// The mamelet a node runs is declared by its sha256. Another one in the declaration is a plan with
// an approval; mamelet then fetches exactly that binary, keeps the previous one, and (after its
// restart) reports it, and the next plan is in sync.
func TestMameletUpdateThroughMamelet(t *testing.T) {
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	root := t.TempDir()
	self := filepath.Join(root, "running")
	setRunning := func(content string) {
		t.Helper()
		os.MkdirAll(filepath.Join(root, "usr/local/sbin"), 0o755)
		for _, p := range []string{self, filepath.Join(root, noderev.MameletPath)} {
			if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	graphOf := func(sha string) *graph.Graph {
		g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
			`{"kind":"TestVM","name":"app","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}},` +
			`{"kind":"NodeMamelet","name":"app/mamelet","spec":{"node":"app","version":"0.9.1","sha256":"` + sha + `"}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	e := newEnv(t)
	oldSHA, newSHA := sum("old mamelet"), sum("new mamelet")
	setRunning("old mamelet")
	newAgent := func() *mamelet.Agent {
		cp := e.agent.CP.(storeCP)
		cp.binaries = map[string][]byte{newSHA: []byte("new mamelet")}
		return &mamelet.Agent{Config: e.agent.Config, CP: cp, State: e.agent.State, Now: e.agent.Now,
			System: &mamelet.System{Runner: mameletOS{e.os}, AllowedOrigins: []string{"Ubuntu:"}, Root: root, SelfPath: self}}
	}
	e.agent = newAgent()
	e.g = graphOf(oldSHA)
	e.publish()
	e.round()
	if m := change(t, e.plan(), "NodeMamelet app/mamelet"); m.Status != plan.StatusInSync {
		t.Fatalf("the declared mamelet runs: %+v", m)
	}

	// Declare the next release: a plan with an approval, naming the sha256.
	e.g = graphOf(newSHA)
	e.publish()
	e.round()
	p := e.plan()
	m := change(t, p, "NodeMamelet app/mamelet")
	if m.Status != plan.StatusChange || m.Verdict != policy.ApprovalRequired || len(m.Operations) != 1 || m.Operations[0].Op != policy.OpUpdateMamelet ||
		!slices.Contains(m.Diffs, plan.Diff{Field: "sha256", From: oldSHA, To: newSHA}) {
		t.Fatalf("update: %+v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(root, noderev.MameletPath)); string(b) != "old mamelet" {
		t.Fatal("changed before approval")
	}
	if rep := e.applyApproved(p); rep.Stopped != nil || rep.Steps[0].Outcome != reconcile.OutcomeDone {
		t.Fatalf("apply: %+v", rep)
	}
	b, _ := os.ReadFile(filepath.Join(root, noderev.MameletPath))
	prev, _ := os.ReadFile(filepath.Join(root, noderev.MameletPrevPath))
	if string(b) != "new mamelet" || string(prev) != "old mamelet" {
		t.Fatalf("installed %q, previous %q", b, prev)
	}

	// The restart: the new binary runs and reports itself; the plan is in sync.
	setRunning("new mamelet")
	e.agent = newAgent()
	if back, err := e.agent.Boot(); err != nil || back {
		t.Fatalf("boot: %v %v", back, err)
	}
	e.round()
	if m := change(t, e.plan(), "NodeMamelet app/mamelet"); m.Status != plan.StatusInSync {
		t.Fatalf("after the update: %+v", m)
	}
}
