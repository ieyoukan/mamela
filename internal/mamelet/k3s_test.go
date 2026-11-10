package mamelet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

const k3sVersion = "v1.33.5+k3s1"

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

	e.publishK3s(1, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary)})
	if rep := e.round(); rep.K3s == nil || rep.K3s.Installed || *downloads != 0 {
		t.Fatalf("without a grant: %+v, %d downloads", rep.K3s, *downloads)
	}

	// Another binary than the approved one: nothing is installed.
	e.publishK3s(2, &noderev.K3s{Version: k3sVersion, SHA256: sum("another binary"),
		Install: &noderev.Once{Operation: k3sOp, Attempt: 1, Action: noderev.ActionInstall}})
	rep := e.round()
	if len(rep.Results) != 1 || rep.Results[0].Outcome != noderev.OutcomeFailed || !strings.Contains(rep.Results[0].Detail, "not the approved") {
		t.Fatalf("results: %+v", rep.Results)
	}
	if _, err := os.Stat(filepath.Join(root, k3sBinary)); !errors.Is(err, os.ErrNotExist) || rep.K3s.Installed {
		t.Fatalf("a binary with another sha256 was installed: %v", err)
	}

	e.publishK3s(3, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary),
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
	if u, _ := os.ReadFile(filepath.Join(root, k3sUnit)); string(u) != K3sUnit() ||
		!strings.Contains(string(u), "ExecStart=/usr/local/bin/k3s server --disable=traefik --disable=metrics-server --write-kubeconfig-mode=0600\n") {
		t.Errorf("unit:\n%s", u)
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
	e.publishK3s(1, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary)})
	e.round()
	e.publishK3s(2, &noderev.K3s{Version: k3sVersion, SHA256: sum(binary),
		Install: &noderev.Once{Operation: k3sOp, Attempt: 1, Action: noderev.ActionInstall}})
	signed := e.cp.signed
	e.cp.err = errors.New("offline")
	e.round()
	if *downloads != 0 {
		t.Fatalf("installed offline from a revision it had not fetched")
	}
	_ = signed
}
