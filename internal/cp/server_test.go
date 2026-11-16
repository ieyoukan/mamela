package cp

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

// A mamelet that waits for a newer revision gets it as soon as it is published (not at its next
// poll), and an answer "nothing new" when the wait ends; the request is the node's own.
func TestWaitForANewRevision(t *testing.T) {
	e := newEnv(t)
	cfg := e.enroll("web", 7001)
	publish := func(pkg string) {
		t.Helper()
		if _, err := e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
			return noderev.Revision{VMID: 7001, Packages: []noderev.Package{{Name: pkg}}}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	publish("curl")
	c, err := mamelet.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing newer than serial 1: the wait ends unchanged.
	start := time.Now()
	serial, changed, err := c.WaitChange(e.ctx, 1, time.Second)
	if err != nil || changed || serial != 1 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("no news: %d %v %v after %v", serial, changed, err, time.Since(start))
	}
	// Already newer than what the node has: at once.
	start = time.Now()
	if serial, changed, err = c.WaitChange(e.ctx, 0, 20*time.Second); err != nil || !changed || serial != 1 || time.Since(start) > time.Second {
		t.Fatalf("already newer: %d %v %v after %v", serial, changed, err, time.Since(start))
	}
	// Published during the wait: it ends with it, long before the deadline.
	go func() {
		time.Sleep(300 * time.Millisecond)
		publish("jq")
	}()
	start = time.Now()
	if serial, changed, err = c.WaitChange(e.ctx, 1, 20*time.Second); err != nil || !changed || serial != 2 || time.Since(start) > 5*time.Second {
		t.Fatalf("published during the wait: %d %v %v after %v", serial, changed, err, time.Since(start))
	}
	// A node waits for its own revisions only (the certificate is checked as for a revision request,
	// see TestOnlyTheNodesCurrentCertificateIsAccepted); one that has none yet just waits.
	other := e.enroll("db", 7002)
	oc, _ := mamelet.NewClient(other)
	if _, _, err := oc.WaitChange(e.ctx, 0, time.Second); err != nil {
		t.Fatalf("a node without a revision waiting: %v", err)
	}
}

// mamelet fetches the binary an approved update names from the Control Plane, with the client it
// already has (pinned to the CA); a sha256 the Control Plane does not serve is a clear error.
func TestMameletClientFetchesTheBinary(t *testing.T) {
	e := newEnv(t)
	cfg := e.enroll("web", 7001)
	bin := []byte("\x7fELF mamelet, the next release")
	e.srv.Config.Handler = (&Server{Store: e.st, PKI: e.pki, Now: func() time.Time { return e.now }, MameletBinary: bin}).Handler()
	c, err := mamelet.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bin)
	got, err := c.Mamelet(e.ctx, hex.EncodeToString(sum[:]))
	if err != nil || string(got) != string(bin) {
		t.Fatalf("fetch: %q %v", got, err)
	}
	if _, err := c.Mamelet(e.ctx, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "does not serve") {
		t.Fatalf("a binary that is not served: %v", err)
	}
}

// installingOS is noOS that counts what mamelet installs.
type installingOS struct {
	noOS
	installs int
}

func (o *installingOS) Run(ctx context.Context, program string, args []string, env []string) ([]byte, []byte, int, error) {
	if strings.HasSuffix(program, "apt-get") && !slices.Contains(args, "-s") {
		o.installs++
		return nil, nil, 0, nil
	}
	return o.noOS.Run(ctx, program, args, env)
}

// A one-time grant is handed out until its deadline and not after (audit A2). The revision's own
// validity is renewed with every fetch, so it ends nothing: the Control Plane takes the grant out
// of what it serves, under the same serial (a mamelet accepts a serial it already has), and what
// it signs has no deadlines in it (a mamelet of an older release rejects members it does not know).
func TestAGrantIsHandedOutOnlyUntilItsDeadline(t *testing.T) {
	e := newEnv(t)
	cfg := e.enroll("web", 7001)
	runner := &installingOS{}
	c, err := mamelet.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	agent := &mamelet.Agent{Config: cfg, CP: c, System: &mamelet.System{Runner: runner, AllowedOrigins: cfg.AllowedOrigins},
		State: mamelet.FileState{Path: filepath.Join(cfg.StateDir, "state.json")}, Now: func() time.Time { return e.now }, Version: "test"}
	publish := func(grant *noderev.Install) {
		t.Helper()
		if _, err := e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
			return noderev.Revision{VMID: 7001, Packages: []noderev.Package{{Name: "curl", Install: grant}}}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	publish(nil)
	rep, err := agent.Round(e.ctx)
	if err != nil || rep.Packages[0].ChangeSet == nil {
		t.Fatalf("first round: %+v %v", rep, err)
	}
	digest := rep.Packages[0].ChangeSet.Digest
	grant := func(op string, deadline time.Time) *noderev.Install {
		return &noderev.Install{Operation: op, Attempt: 1, ChangeSet: digest, Expires: deadline}
	}

	// Before the deadline it is handed out, and what is signed has no deadline in it.
	publish(grant("op-000000000000000000000001", e.now.Add(10*time.Minute)))
	signed, err := c.Revision(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(signed.Payload), `"expires"`) {
		t.Fatalf("the signed revision has the grant's deadline: %s", signed.Payload)
	}
	if _, err := agent.Round(e.ctx); err != nil || runner.installs != 1 {
		t.Fatalf("a live grant: %v, installs %d", err, runner.installs)
	}

	// A later grant, whose deadline passes before mamelet comes back (it was away): the revision is
	// the same serial and is still accepted, without the grant, though the revision's own validity
	// (an hour from each fetch) has not ended.
	publish(grant("op-000000000000000000000002", e.now.Add(10*time.Minute)))
	serial, _ := e.st.NodeRevisionSerial(e.ctx, "web")
	e.now = e.now.Add(11 * time.Minute)
	rep, err = agent.Round(e.ctx)
	if err != nil || rep.Refused != "" || rep.Serial != serial || runner.installs != 1 {
		t.Fatalf("a grant past its deadline: refused %q, serial %d (want %d), installs %d, %v", rep.Refused, rep.Serial, serial, runner.installs, err)
	}
	if signed, _ = c.Revision(e.ctx); strings.Contains(string(signed.Payload), "op-000000000000000000000002") {
		t.Fatalf("an expired grant is handed out: %s", signed.Payload)
	}

	// A grant that an older Control Plane stored has no deadline: it is not handed out either.
	publish(grant("op-000000000000000000000003", time.Time{}))
	if _, err := agent.Round(e.ctx); err != nil || runner.installs != 1 {
		t.Fatalf("a grant without a deadline: %v, installs %d", err, runner.installs)
	}
}

// The Control Plane says in its log when a mamelet starts refusing its revision, and when it stops:
// once each, not at every report (audit A6: nothing on the Control Plane looked at the reason).
func TestTheLogSaysWhenAMameletStartsAndStopsRefusing(t *testing.T) {
	e := newEnv(t)
	var buf strings.Builder
	e.srv.Config.Handler = (&Server{Store: e.st, PKI: e.pki, Now: func() time.Time { return e.now }, RevisionTTL: time.Hour,
		Log: log.New(&buf, "", 0)}).Handler()
	cfg := e.enroll("web", 7001)
	agent := e.agent(cfg)
	if _, err := e.st.PublishNodeRevision(e.ctx, "web", func(*noderev.Revision) (noderev.Revision, error) {
		return noderev.Revision{VMID: 7001}, nil
	}); err != nil {
		t.Fatal(err)
	}
	setLast := func(serial int64) {
		t.Helper()
		st, err := agent.State.Load()
		if err != nil {
			t.Fatal(err)
		}
		st.LastSerial, st.Revision = serial, nil
		if err := agent.State.Save(st); err != nil {
			t.Fatal(err)
		}
	}
	round := func() {
		t.Helper()
		if _, err := agent.Round(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	round()
	setLast(99) // a database restored from an older copy: the Control Plane's serial is lower than the node's
	round()
	round()
	if out := buf.String(); strings.Count(out, "MAM-PLAN-0130") != 1 || !strings.Contains(out, "web:") || !strings.Contains(out, "older than one already accepted") {
		t.Fatalf("the log of two refusing rounds:\n%s", out)
	}
	setLast(0)
	round()
	round()
	if out := buf.String(); strings.Count(out, "accepts the Control Plane's revision again") != 1 || strings.Count(out, "MAM-PLAN-0130") != 1 {
		t.Fatalf("the log after it accepts again:\n%s", out)
	}
}
