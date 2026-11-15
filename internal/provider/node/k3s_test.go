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
