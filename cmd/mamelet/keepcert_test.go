package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/cp"
	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/store"
)

// certEnv is a Control Plane and a mamelet enrolled with it, whose certificate was issued at
// `issued` (the Control Plane's clock for the enrollment).
type certEnv struct {
	cfg    mamelet.Config
	client *mamelet.Client
	srv    *httptest.Server
}

func newCertEnv(t *testing.T, issued time.Time, validity time.Duration, handler func(http.Handler) http.Handler) *certEnv {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pki, err := cp.LoadPKI(filepath.Join(t.TempDir(), "cp"), []string{"127.0.0.1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	pki.AgentValidity = validity
	clock := issued
	s := &cp.Server{Store: st, PKI: pki, Now: func() time.Time { return clock }}
	h := http.Handler(s.Handler())
	if handler != nil {
		h = handler(h)
	}
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = s.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	cfg := mamelet.Config{Node: "web", VMID: 7001, Server: ts.URL, CA: string(pki.CAPEM()), RevisionKey: pki.RevisionPublicKey(),
		StateDir: t.TempDir(), EnrollTokenFile: "/etc/mamelet/enroll-token", AllowedOrigins: []string{"Ubuntu:"}}
	token, err := st.IssueEnrollToken(ctx, "web", 7001, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := mamelet.Enroll(ctx, cfg, token); err != nil {
		t.Fatal(err)
	}
	clock = now
	client, err := mamelet.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &certEnv{cfg: cfg, client: client, srv: ts}
}

// captureLog collects what mamelet logs.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(nil) })
	return &buf
}

// A certificate in the last third of its life is renewed by the loop, and said so once.
func TestKeepCertificateRenews(t *testing.T) {
	now := time.Now().UTC()
	e := newCertEnv(t, now.Add(-2*time.Hour), 3*time.Hour, nil) // a quarter of its life left
	out := captureLog(t)
	var last string
	end := e.client.CertNotAfter()
	keepCertificate(context.Background(), e.cfg, e.client, now, &last)
	if !strings.Contains(out.String(), "renewed the client certificate") || last != "" || !e.client.CertNotAfter().After(end) {
		t.Fatalf("not renewed: %q, last %q", out.String(), last)
	}
	before := out.Len()
	keepCertificate(context.Background(), e.cfg, e.client, now, &last) // no longer due
	if out.Len() != before {
		t.Fatalf("logged again: %q", out.String())
	}
}

// A Control Plane that does not know the renewal request (an older one) leaves the mamelet as it
// was: it says once, with what to do, that the certificate is not renewed.
func TestKeepCertificateWithAnOlderControlPlane(t *testing.T) {
	now := time.Now().UTC()
	// Due, and with enough left to be asked again after an hour: ends in two hours, of seven.
	e := newCertEnv(t, now.Add(-4*time.Hour), 6*time.Hour, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/renew") {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	out := captureLog(t)
	var last string
	end := e.client.CertNotAfter()
	keepCertificate(context.Background(), e.cfg, e.client, now, &last)
	if !strings.Contains(out.String(), "MAM-PKI-0104") || !strings.Contains(out.String(), "hint:") || !e.client.CertNotAfter().Equal(end) {
		t.Fatalf("log %q", out.String())
	}
	before := out.Len()
	keepCertificate(context.Background(), e.cfg, e.client, now.Add(90*time.Minute), &last) // asked again after an hour; the same news, said once
	if out.Len() != before {
		t.Fatalf("said again: %q", out.String())
	}
	// The certificate still authenticates (there is no revision yet: 404, not 403 or a TLS error).
	if _, err := e.client.Revision(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("revision: %v", err)
	}
}

// A certificate that has ended is named, with the steps to enroll again and this node's own paths.
func TestKeepCertificateSaysWhenItHasEnded(t *testing.T) {
	now := time.Now().UTC()
	e := newCertEnv(t, now.AddDate(-2, 0, 0), 0, nil)
	out := captureLog(t)
	var last string
	keepCertificate(context.Background(), e.cfg, e.client, now, &last)
	for _, want := range []string{"MAM-PKI-0102", "mamela controlplane reenroll --node web", "/etc/mamelet/enroll-token", e.cfg.StateDir} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the message lacks %q:\n%s", want, out.String())
		}
	}
}
