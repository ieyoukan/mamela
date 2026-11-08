package proxmox

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func server(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("client sent %s; it must be read-only", r.Method)
		}
		if r.Header.Get("Authorization") != "PVEAPIToken=mamela@pve!audit=secret" {
			http.Error(w, `{"data":null}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api2/json/cluster/status":
			w.Write([]byte(`{"data":[{"type":"cluster","name":"c","quorate":1,"nodes":3},{"type":"node","name":"n1","online":1}]}`))
		case "/api2/json/nodes/n1/qemu/101/config":
			w.Write([]byte(`{"data":{"digest":"abc123","memory":"2048"}}`))
		case "/api2/json/access/permissions":
			w.Write([]byte(`{"data":{"/pool/mamela-lab":{"VM.Allocate":1}}}`))
		default:
			http.Error(w, `{"data":null}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	return srv, formatFingerprint(sum[:])
}

func TestClientWithPinnedCertificate(t *testing.T) {
	srv, fp := server(t)
	c, err := New(srv.URL, "mamela@pve!audit", "secret", Options{Fingerprint: fp})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := c.ClusterStatus(ctx)
	if err != nil || len(st) != 2 || st[0].Quorate != 1 || st[1].Online != 1 {
		t.Fatalf("ClusterStatus: %v %+v", err, st)
	}
	d, err := c.ConfigDigest(ctx, "n1", "qemu", 101)
	if err != nil || d != "abc123" {
		t.Fatalf("ConfigDigest: %q %v", d, err)
	}
	perms, err := c.Permissions(ctx)
	if err != nil || perms["/pool/mamela-lab"]["VM.Allocate"] != 1 {
		t.Fatalf("Permissions: %v %v", perms, err)
	}
	var apiErr *APIError
	if _, err := c.NodeStatus(ctx, "missing"); !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("missing path: %v", err)
	}
	if _, err := c.ConfigDigest(ctx, "n1", "shell", 1); err == nil {
		t.Fatal("accepted an unknown guest type")
	}
}

func TestClientRejectsWrongFingerprint(t *testing.T) {
	srv, _ := server(t)
	wrong := strings.Repeat("00:", 31) + "00"
	c, err := New(srv.URL, "mamela@pve!audit", "secret", Options{Fingerprint: wrong})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ClusterStatus(context.Background()); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("want a fingerprint error, got %v", err)
	}
}

func TestNewRejectsUnsafeOptions(t *testing.T) {
	for name, tt := range map[string]struct {
		url  string
		opts Options
	}{
		"http":          {"http://pve:8006", Options{Fingerprint: strings.Repeat("00:", 31) + "00"}},
		"no tls option": {"https://pve:8006", Options{}},
		"both options":  {"https://pve:8006", Options{Fingerprint: strings.Repeat("00:", 31) + "00", CAFile: "/x"}},
		"bad pin":       {"https://pve:8006", Options{Fingerprint: "12:34"}},
	} {
		if _, err := New(tt.url, "a@pve!b", "s", tt.opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
