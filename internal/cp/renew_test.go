package cp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// enrollDue enrolls node with a certificate that is due for renewal now: valid for three hours,
// issued two hours ago (a quarter of its life left). The clock the Control Plane uses is the real
// one again afterwards, so that TLS (which uses the real clock) accepts what is issued next.
func (e *env) enrollDue(node string, vmid int) mamelet.Config {
	e.t.Helper()
	e.pki.AgentValidity = 3 * time.Hour
	real := e.now
	e.now = real.Add(-2 * time.Hour)
	cfg := e.enroll(node, vmid)
	e.now = real
	e.st.PublishNodeRevision(e.ctx, node, func(*noderev.Revision) (noderev.Revision, error) { return noderev.Revision{VMID: vmid}, nil })
	return cfg
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// copyIdentity makes another state directory with the same certificate and key files, as mamelet had
// them before a renewal.
func copyIdentity(t *testing.T, cfg mamelet.Config, cert, key []byte) mamelet.Config {
	t.Helper()
	other := cfg
	other.StateDir = filepath.Join(t.TempDir(), "old")
	if err := os.MkdirAll(other.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(other.StateDir, "cert.pem"), cert, 0o600)
	os.WriteFile(filepath.Join(other.StateDir, "key.pem"), key, 0o600)
	return other
}

// A mamelet whose certificate is in the last third of its life gets a new one, keeps working with
// it, and is the same registration as before: the database is not changed (plans, approvals and
// explicit operations are bound to the registration), and the old certificate is still good until
// it ends, so a response that never arrived does not lock the node out.
func TestMameletRenewsItsCertificate(t *testing.T) {
	e := newEnv(t)
	cfg := e.enrollDue("web", 7001)
	oldCert, oldKey := read(t, filepath.Join(cfg.StateDir, "cert.pem")), read(t, filepath.Join(cfg.StateDir, "key.pem"))
	before, _, _ := e.st.Agent(e.ctx, "web")
	c, err := mamelet.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	endBefore := c.CertNotAfter()
	if !c.RenewDue(e.now) {
		t.Fatalf("a certificate with a quarter of its life left is not due for renewal (ends %s)", endBefore)
	}

	renewed, err := c.RenewIfDue(e.ctx, e.now)
	if err != nil || !renewed {
		t.Fatalf("renew: %v %v", renewed, err)
	}
	if !c.CertNotAfter().After(endBefore.Add(time.Hour)) {
		t.Fatalf("the certificate ends %s, it ended %s before", c.CertNotAfter(), endBefore)
	}
	// Saved: a new certificate, the same key.
	if bytes.Equal(read(t, filepath.Join(cfg.StateDir, "cert.pem")), oldCert) {
		t.Fatal("cert.pem was not replaced")
	}
	if !bytes.Equal(read(t, filepath.Join(cfg.StateDir, "key.pem")), oldKey) {
		t.Fatal("key.pem changed: the files would not belong together after a crash between two writes")
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "cert.pem.tmp")); err == nil {
		t.Fatal("a temporary file is left behind")
	}
	// The renewed certificate works, for a client made from the saved files.
	fresh, err := mamelet.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, cl := range map[string]*mamelet.Client{"the client that renewed": c, "a restarted mamelet": fresh} {
		if _, err := cl.Revision(e.ctx); err != nil {
			t.Fatalf("%s cannot fetch its revision: %v", name, err)
		}
	}
	if rep, err := e.agent(cfg).Round(e.ctx); err != nil || rep.Serial != 1 {
		t.Fatalf("a round after the renewal: %+v %v", rep, err)
	}
	// The registration is untouched; the old certificate is good until it ends.
	after, _, _ := e.st.Agent(e.ctx, "web")
	if after.CertSerial != before.CertSerial || !after.Enrolled.Equal(before.Enrolled) {
		t.Fatalf("the database changed: %+v, was %+v", after, before)
	}
	if _, err := e.st.PublishNodeRevisionForAgent(e.ctx, "web", before.CertSerial, func(*noderev.Revision) (noderev.Revision, error) {
		return noderev.Revision{VMID: 7001, Packages: []noderev.Package{{Name: "curl"}}}, nil
	}); err != nil {
		t.Fatalf("an approval made against the registration before the renewal is refused: %v", err)
	}
	old, err := mamelet.NewClient(copyIdentity(t, cfg, oldCert, oldKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Revision(e.ctx); err != nil {
		t.Fatalf("the old certificate stopped working at once: %v", err)
	}
	// The certificate that was renewed is a certificate of this node's registration, from the CA only:
	// the node's own request cannot say otherwise (see TestRenewalTakesTheRegistrationFromTheDatabase).
	leaf := parseCert(t, read(t, filepath.Join(cfg.StateDir, "cert.pem")))
	if leaf.Subject.CommonName != "web" || Registration(leaf) != before.CertSerial {
		t.Fatalf("the renewed certificate stands for %q, the registration is %q", Registration(leaf), before.CertSerial)
	}

	// Enrolling again revokes every certificate of the registration before it, renewed ones too.
	e.enroll("web", 7001)
	for name, cl := range map[string]*mamelet.Client{"the old certificate": old, "the renewed one": fresh} {
		if _, err := cl.Revision(e.ctx); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("%s still works after the node enrolled again: %v", name, err)
		}
	}
}

// Nothing is sent while the certificate has more than a third of its life left.
func TestMameletDoesNotRenewEarly(t *testing.T) {
	e := newEnv(t)
	e.pki.AgentValidity = 3 * time.Hour
	cfg := e.enroll("web", 7001) // issued now: three quarters of its life left
	var asked atomic.Int32
	inner := e.srv.Config.Handler
	e.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/renew") {
			asked.Add(1)
		}
		inner.ServeHTTP(w, r)
	})
	c, _ := mamelet.NewClient(cfg)
	if renewed, err := c.RenewIfDue(e.ctx, e.now); renewed || err != nil || asked.Load() != 0 {
		t.Fatalf("renewed %v, err %v, requests %d", renewed, err, asked.Load())
	}
	// The Control Plane, too, renews only a certificate with less than half of its life left.
	resp := e.post(t, cfg, "/v1/nodes/web/renew", e.csr(t, cfg, "web"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a renewal request for a fresh certificate: %s", resp.Status)
	}
}

// An older Control Plane does not know the request (404): nothing changes, the mamelet carries on
// with the certificate it has and does not ask again for an hour.
func TestRenewalAgainstAnOlderControlPlaneDoesNothing(t *testing.T) {
	e := newEnv(t)
	cfg := e.enrollDue("web", 7001)
	var asked atomic.Int32
	inner := e.srv.Config.Handler
	e.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/renew") {
			asked.Add(1)
			http.NotFound(w, r) // what a Control Plane without the endpoint answers
			return
		}
		inner.ServeHTTP(w, r)
	})
	certBefore := read(t, filepath.Join(cfg.StateDir, "cert.pem"))
	c, _ := mamelet.NewClient(cfg)
	renewed, err := c.RenewIfDue(e.ctx, e.now)
	if renewed || !errors.Is(err, mamelet.ErrRenewUnsupported) {
		t.Fatalf("renewed %v, err %v", renewed, err)
	}
	if !bytes.Equal(read(t, filepath.Join(cfg.StateDir, "cert.pem")), certBefore) {
		t.Fatal("the certificate file changed")
	}
	if renewed, err := c.RenewIfDue(e.ctx, e.now.Add(time.Minute)); renewed || err != nil || asked.Load() != 1 {
		t.Fatalf("asked again within the hour: %v %v (%d requests)", renewed, err, asked.Load())
	}
	if _, err := c.Revision(e.ctx); err != nil {
		t.Fatalf("the mamelet cannot work with its certificate after the refusal: %v", err)
	}
	if _, err := c.RenewIfDue(e.ctx, e.now.Add(2*time.Hour)); !errors.Is(err, mamelet.ErrRenewUnsupported) || asked.Load() != 2 {
		t.Fatalf("not asked again after an hour: %v (%d requests)", err, asked.Load())
	}
}

// If the answer to a renewal never arrives (the connection breaks after the Control Plane issued
// the certificate), the mamelet is not locked out: its old certificate works, and it renews on the
// next try.
func TestLostRenewalResponseDoesNotLockTheNodeOut(t *testing.T) {
	e := newEnv(t)
	cfg := e.enrollDue("web", 7001)
	var drop atomic.Bool
	drop.Store(true)
	inner := e.srv.Config.Handler
	e.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/renew") && drop.Load() {
			inner.ServeHTTP(httptest.NewRecorder(), r) // issued, and the answer goes nowhere
			panic(http.ErrAbortHandler)
		}
		inner.ServeHTTP(w, r)
	})
	c, _ := mamelet.NewClient(cfg)
	if renewed, err := c.RenewIfDue(e.ctx, e.now); renewed || err == nil {
		t.Fatalf("the lost answer was taken for a renewal: %v %v", renewed, err)
	}
	if _, err := c.Revision(e.ctx); err != nil {
		t.Fatalf("locked out after a renewal whose answer was lost: %v", err)
	}
	drop.Store(false)
	if renewed, err := c.RenewIfDue(e.ctx, e.now.Add(2*time.Hour)); !renewed || err != nil {
		t.Fatalf("the next try: %v %v", renewed, err)
	}
	if _, err := c.Revision(e.ctx); err != nil {
		t.Fatal(err)
	}
}

// A request for a renewal is the node's own: another node's certificate cannot renew it, and the
// certificate request must name the node.
func TestRenewalIsForTheNodeItself(t *testing.T) {
	e := newEnv(t)
	web := e.enrollDue("web", 7001)
	db := e.enrollDue("db", 7002)
	if resp := e.post(t, db, "/v1/nodes/web/renew", e.csr(t, db, "db")); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("db renewed web's certificate: %s", resp.Status)
	}
	if resp := e.post(t, web, "/v1/nodes/web/renew", e.csr(t, web, "db")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a request that names another node: %s", resp.Status)
	}
	if resp := e.post(t, web, "/v1/nodes/web/renew", `{"csr":"x","extra":1}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a request with an unknown member: %s", resp.Status)
	}
	if resp := e.post(t, web, "/v1/nodes/web/renew", e.csr(t, web, "web")); resp.StatusCode != http.StatusOK {
		t.Fatalf("the node's own request: %s", resp.Status)
	}
	// Without a certificate there is nothing to renew.
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.caPool()}}}
	resp, err := plain.Post(e.srv.URL+"/v1/nodes/web/renew", "application/json", strings.NewReader(e.csr(t, web, "web")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a renewal without a certificate: %s", resp.Status)
	}
}

// The registration a renewed certificate stands for comes from the database, not from the request:
// a node cannot ask for another registration (or an enrollment certificate's identity) in its
// request, and an enrollment never carries one.
func TestRenewalTakesTheRegistrationFromTheDatabase(t *testing.T) {
	e := newEnv(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "web", SerialNumber: "abc123"}}, key)
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	now := e.now

	enrolled, _, err := e.pki.SignAgent(csr, "web", now)
	if err != nil {
		t.Fatal(err)
	}
	leaf := parseCert(t, enrolled)
	if leaf.Subject.SerialNumber != "" || Registration(leaf) == "abc123" {
		t.Fatalf("an enrollment certificate carries the registration the request asked for: %q", leaf.Subject.SerialNumber)
	}
	renewed, _, err := e.pki.SignRenewal(csr, "web", "feedbeef", now)
	if err != nil {
		t.Fatal(err)
	}
	if got := Registration(parseCert(t, renewed)); got != "feedbeef" {
		t.Fatalf("the renewed certificate stands for %q", got)
	}
	if _, _, err := e.pki.SignRenewal(csr, "web", "", now); err == nil {
		t.Fatal("a renewal without a registration was signed")
	}
}

func parseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// csr is a certificate request, signed by the key in cfg's state directory, for node.
func (e *env) csr(t *testing.T, cfg mamelet.Config, node string) string {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(filepath.Join(cfg.StateDir, "cert.pem"), filepath.Join(cfg.StateDir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: node}}, pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return `{"csr":` + jsonString(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))) + `}`
}

func jsonString(s string) string { return `"` + strings.NewReplacer("\n", `\n`).Replace(s) + `"` }

// post sends body to path with cfg's client certificate.
func (e *env) post(t *testing.T, cfg mamelet.Config, path, body string) *http.Response {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(filepath.Join(cfg.StateDir, "cert.pem"), filepath.Join(cfg.StateDir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.caPool(), Certificates: []tls.Certificate{pair}}}}
	resp, err := client.Post(e.srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}
