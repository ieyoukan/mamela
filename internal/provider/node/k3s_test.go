package node_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/mamelet"
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
	return o.tinyOS.Run(ctx, program, args, env)
}

// A single-node Kubernetes through mamelet: the binary is installed only after approval and only
// with the declared sha256, then the service is enabled and started after its own approval.
func TestK3sThroughMamelet(t *testing.T) {
	binary := "\x7fELF k3s server"
	sum := sha256.Sum256([]byte(binary))
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		w.Write([]byte(binary))
	}))
	defer srv.Close()

	e := newEnv(t)
	g, err := graph.Decode([]byte(`{"schema":"mamela-graph/v1","resources":[` +
		`{"kind":"VirtualMachine","name":"app","spec":{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}},` +
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
}

func token(t *testing.T, e *env) string {
	tok, err := e.st.K3sToken(e.ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
