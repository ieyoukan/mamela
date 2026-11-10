package proxmox

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// Nothing leaves the client that the permission catalog does not describe: no undeclared API
// call, no undeclared parameter. The server here would fail the test on any write.
func TestOnlyCatalogedRequestsAreSent(t *testing.T) {
	srv, fp := server(t)
	c, err := New(srv.URL, "mamela@pve!audit", "secret", Options{Fingerprint: fp})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.get(ctx, "/nodes/n1/qemu/101/snapshot", nil); err == nil || !strings.Contains(err.Error(), "permission catalog") {
		t.Fatalf("undeclared endpoint: %v", err)
	}
	if _, err := c.CreateVM(ctx, "n1", url.Values{"vmid": {"7000"}, "tags": {"x"}}); err == nil || !strings.Contains(err.Error(), "tags") {
		t.Fatalf("undeclared create parameter: %v", err)
	}
	if _, err := c.CreateVM(ctx, "n1", url.Values{"vmid": {"7000"}, "hostpci0": {"0000:01:00"}}); err == nil {
		t.Fatal("sent a create parameter that is not in the catalog")
	}
}

func TestCatalogIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Endpoints {
		if seen[e.Method+" "+e.Path] {
			t.Errorf("%s %s appears twice", e.Method, e.Path)
		}
		seen[e.Method+" "+e.Path] = true
		if len(e.Tokens) == 0 {
			t.Errorf("%s %s: no token uses it", e.Method, e.Path)
		}
		for _, n := range e.Needs {
			if len(n.Privs) == 0 || n.Why == "" {
				t.Errorf("%s %s: incomplete need %+v", e.Method, e.Path, n)
			}
		}
	}
	for name, needs := range CreateParams {
		for _, n := range needs {
			if len(n.Privs) == 0 || n.Why == "" {
				t.Errorf("create parameter %s: incomplete need %+v", name, n)
			}
		}
	}
	if !strings.Contains(ExplainPermission("Permission check failed (/vms/7000, VM.Config.Options)"), "lacks VM.Config.Options") {
		t.Error("a catalogued privilege is not explained as missing from the token")
	}
	if !strings.Contains(ExplainPermission("Permission check failed (/, Sys.Modify)"), "bug in Mamela") {
		t.Error("an uncatalogued privilege is not explained as a gap in the catalog")
	}
	if ExplainPermission("some other error") != "" {
		t.Error("explained an error that is not a permission failure")
	}
}
