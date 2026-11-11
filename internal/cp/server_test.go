package cp

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/store"
)

type env struct {
	t   *testing.T
	ctx context.Context
	st  *store.Store
	pki *PKI
	srv *httptest.Server
	now time.Time
	dir string // the Control Plane's keys
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), now: time.Now().UTC().Truncate(time.Second)} // TLS checks validity against the real clock
	st, err := store.Open(e.ctx, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return e.now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := filepath.Join(t.TempDir(), "cp")
	pki, err := LoadPKI(dir, []string{"127.0.0.1"}, e.now)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, PKI: pki, Now: func() time.Time { return e.now }, RevisionTTL: time.Hour}
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.TLS = s.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	e.st, e.pki, e.srv, e.dir = st, pki, srv, dir
	return e
}

func (e *env) config(node string, vmid int) mamelet.Config {
	return mamelet.Config{Node: node, VMID: vmid, Server: e.srv.URL, CA: string(e.pki.CAPEM()),
		RevisionKey: e.pki.RevisionPublicKey(), StateDir: filepath.Join(e.t.TempDir(), node), AllowedOrigins: []string{"Ubuntu:"}}
}

func (e *env) enroll(node string, vmid int) mamelet.Config {
	e.t.Helper()
	token, err := e.st.IssueEnrollToken(e.ctx, node, vmid, "seed.iso", time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := e.config(node, vmid)
	if err := mamelet.Enroll(e.ctx, cfg, token); err != nil {
		e.t.Fatal(err)
	}
	return cfg
}

// noOS answers that nothing is installed and no service exists, and changes nothing.
type noOS struct{}

func (noOS) Run(_ context.Context, program string, args []string, _ []string) ([]byte, []byte, int, error) {
	if strings.HasSuffix(program, "systemctl") {
		return []byte("LoadState=not-found\nUnitFileState=\nActiveState=inactive\n"), nil, 0, nil
	}
	if strings.HasSuffix(program, "dpkg-query") {
		return nil, []byte("no packages found"), 1, nil
	}
	return []byte("Inst curl (8.5.0-2ubuntu10.6 Ubuntu:24.04/noble-updates [amd64])\n"), nil, 0, nil
}

func (e *env) agent(cfg mamelet.Config) *mamelet.Agent {
	e.t.Helper()
	c, err := mamelet.NewClient(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return &mamelet.Agent{Config: cfg, CP: c, System: &mamelet.System{Runner: noOS{}, AllowedOrigins: cfg.AllowedOrigins},
		State: mamelet.FileState{Path: filepath.Join(cfg.StateDir, "state.json")}, Now: func() time.Time { return e.now }, Version: "test"}
}

func TestEnrollPullAndReport(t *testing.T) {
	e := newEnv(t)
	cfg := e.enroll("web", 7001)
	if _, err := e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
		return noderev.Revision{VMID: 7001, Packages: []noderev.Package{{Name: "curl"}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := e.agent(cfg).Round(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Serial != 1 || rep.Refused != "" {
		t.Fatalf("report %+v", rep)
	}
	got, ok, err := e.st.NodeReportOf(e.ctx, "web")
	if err != nil || !ok || got.Report.Packages[0].ChangeSet == nil || got.Report.Packages[0].ChangeSet.Changes[0].Package != "curl" {
		t.Fatalf("stored report %+v %v %v", got, ok, err)
	}
}

func TestEnrollmentIsOneTime(t *testing.T) {
	e := newEnv(t)
	token, _ := e.st.IssueEnrollToken(e.ctx, "web", 7001, "seed.iso", time.Hour)
	if err := mamelet.Enroll(e.ctx, e.config("web", 7001), token); err != nil {
		t.Fatal(err)
	}
	if err := mamelet.Enroll(e.ctx, e.config("web", 7001), token); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("the token was accepted twice: %v", err)
	}
	other, _ := e.st.IssueEnrollToken(e.ctx, "db", 7002, "seed.iso", time.Hour)
	if err := mamelet.Enroll(e.ctx, e.config("web", 7001), other); err == nil {
		t.Fatal("a token for db enrolled web")
	}
}

func TestOnlyTheNodesCurrentCertificateIsAccepted(t *testing.T) {
	e := newEnv(t)
	web := e.enroll("web", 7001)
	db := e.enroll("db", 7002)
	for _, n := range []string{"web", "db"} {
		e.st.PublishNodeRevision(e.ctx, n, func(*noderev.Revision) (noderev.Revision, error) {
			vmid := map[string]int{"web": 7001, "db": 7002}[n]
			return noderev.Revision{VMID: vmid}, nil
		})
	}

	// db's certificate cannot read web's revision.
	stolen := db
	stolen.Node = "web"
	c, err := mamelet.NewClient(stolen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Revision(e.ctx); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("db read web's revision: %v", err)
	}

	// No client certificate at all.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get(e.srv.URL + "/v1/nodes/web/revision")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a certificate: %s", resp.Status)
	}

	// Enrolling again replaces the certificate: the old one stops working.
	oldCert, _ := os.ReadFile(filepath.Join(web.StateDir, "cert.pem"))
	oldKey, _ := os.ReadFile(filepath.Join(web.StateDir, "key.pem"))
	web2 := e.enroll("web", 7001)
	if _, err := e.agent(web2).Round(e.ctx); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(web2.StateDir, "cert.pem"), oldCert, 0o600)
	os.WriteFile(filepath.Join(web2.StateDir, "key.pem"), oldKey, 0o600)
	c, _ = mamelet.NewClient(web2)
	if _, err := c.Revision(e.ctx); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("the replaced certificate still works: %v", err)
	}
}

func TestReportsAreChecked(t *testing.T) {
	e := newEnv(t)
	cfg := e.enroll("web", 7001)
	c, _ := mamelet.NewClient(cfg)
	cs := noderev.NewChangeSet([]noderev.Change{{Action: "install", Package: "curl", Version: "1", Origins: []string{"Ubuntu:x"}}})
	forged := cs
	forged.Changes = append(forged.Changes, noderev.Change{Action: "remove", Package: "apache2"})
	for name, rep := range map[string]noderev.Report{
		"other node":        {Node: "db", VMID: 7001},
		"other vmid":        {Node: "web", VMID: 7002},
		"forged change set": {Node: "web", VMID: 7001, Packages: []noderev.PackageState{{Name: "curl", ChangeSet: &forged}}},
		"bad package name":  {Node: "web", VMID: 7001, Packages: []noderev.PackageState{{Name: "-x"}}},
		"bad service name":  {Node: "web", VMID: 7001, Services: []noderev.ServiceState{{Name: "../x"}}},
	} {
		if err := c.Report(e.ctx, rep); err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	if err := c.Report(e.ctx, noderev.Report{Node: "web", VMID: 7001, Packages: []noderev.PackageState{{Name: "curl", ChangeSet: &cs}}}); err != nil {
		t.Fatal(err)
	}
}

func TestPKIIsReusedAndProtected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cp")
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	a, err := LoadPKI(dir, []string{"127.0.0.1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadPKI(dir, []string{"127.0.0.1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !a.CA.Equal(b.CA) || !a.RevisionKey.Equal(b.RevisionKey) {
		t.Fatal("keys were not reused")
	}
	os.Chmod(dir, 0o755)
	if _, err := LoadPKI(dir, []string{"127.0.0.1"}, now); err == nil {
		t.Fatal("accepted a key directory readable by others")
	}
}

// The Control Plane moves (ADR 0003): a signed revision names the new URL, mamelet goes there,
// and falls back to the configured URL while the new one does not answer. The same CA vouches for
// both, so nothing about mamelet's trust changes.
func TestMameletFollowsTheControlPlane(t *testing.T) {
	e := newEnv(t)
	cfg := e.enroll("web", 7001)
	agent := e.agent(cfg)
	// The new Control Plane: the same keys and database, another address (its certificate is
	// issued for "localhost", the old one's for 127.0.0.1).
	moved, err := LoadPKI(e.dir, []string{"localhost"}, e.now)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: e.st, PKI: moved, Now: func() time.Time { return e.now }, RevisionTTL: time.Hour}
	newCP := httptest.NewUnstartedServer(s.Handler())
	newCP.TLS = s.TLSConfig()
	newCP.StartTLS()
	defer newCP.Close()
	newURL := strings.Replace(newCP.URL, "127.0.0.1", "localhost", 1)

	publish := func(cp string, pkgs ...noderev.Package) {
		t.Helper()
		if _, err := e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
			return noderev.Revision{VMID: 7001, ControlPlane: cp, Packages: pkgs}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	reported := func() int64 {
		t.Helper()
		r, ok, err := e.st.NodeReportOf(e.ctx, "web")
		if err != nil || !ok {
			t.Fatalf("no report: %v", err)
		}
		return r.Report.Serial
	}
	publish("")
	if _, err := agent.Round(e.ctx); err != nil || reported() != 1 {
		t.Fatalf("before the move: %v", err)
	}
	publish(newURL)
	if _, err := agent.Round(e.ctx); err != nil || reported() != 2 {
		t.Fatalf("the round that learns the new URL: %v", err)
	}
	st, _ := agent.State.Load()
	if st.ControlPlane != newURL {
		t.Fatalf("state %q", st.ControlPlane)
	}
	// The old Control Plane stops; mamelet keeps working through the new one.
	e.srv.Close()
	publish(newURL, noderev.Package{Name: "curl"})
	if rep, err := agent.Round(e.ctx); err != nil || rep.Serial != 3 || reported() != 3 {
		t.Fatalf("after the move: %+v %v", rep, err)
	}
	// A revision with a URL that is not https://host:port is refused by mamelet, and changes nothing.
	if _, err := e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
		return noderev.Revision{VMID: 7001, ControlPlane: "http://evil.example:80"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := agent.Round(e.ctx)
	if st, _ := agent.State.Load(); (rep.Refused == "" && err == nil) || st.ControlPlane != newURL {
		t.Fatalf("plain HTTP: refused %q, err %v, state %q", rep.Refused, err, st.ControlPlane)
	}
	t.Logf("plain HTTP: refused %q, err %v", rep.Refused, err)
}

// mamelet is served to anyone, only under its own sha256.
func TestServesMameletBySHA256(t *testing.T) {
	e := newEnv(t)
	bin := []byte("\x7fELF mamelet")
	s := &Server{Store: e.st, PKI: e.pki, Now: func() time.Time { return e.now }, MameletBinary: bin}
	h := s.Handler()
	sum := sha256.Sum256(bin)
	for path, want := range map[string]int{"/v1/mamelet/" + hex.EncodeToString(sum[:]): 200, "/v1/mamelet/" + strings.Repeat("0", 64): 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want || (want == 200 && rec.Body.String() != string(bin)) {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}
