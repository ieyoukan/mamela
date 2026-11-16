package host

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// secrets are what an error that reaches a pull request must not carry: where the API is.
func secrets(srv *httptest.Server, path string) []string {
	u, _ := url.Parse(srv.URL)
	return []string{u.Host, u.Hostname(), u.Port(), path}
}

func mustNotShow(t *testing.T, what string, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(err.Error(), s) {
			t.Errorf("%s: the error shows %q: %v", what, s, err)
		}
	}
}

// B10 (audit 2026-11-17): an API connection error carried the URL (host and path), and the plan's
// error reached a pull request comment: a declaration's public repository leaked the apiPath's address.
// The error says which kind of failure it was, with a stable code, and nothing of where.
func TestConnectionErrorsShowNoAddress(t *testing.T) {
	get := protocol.HostRequest{Method: "GET", Path: "/items/web"}
	const apiPath = "/site-internal/api"

	t.Run("nothing is listening", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL + apiPath
		secret := secrets(srv, apiPath)
		srv.Close()
		_, _, err := HTTP{Base: base, Client: &http.Client{Timeout: time.Second}}.Do(t.Context(), get)
		mustNotShow(t, "refused", err, secret...)
		if !errors.Is(err, ErrNotSent) || !strings.Contains(err.Error(), "MAM-PRV-0012") || !strings.Contains(err.Error(), "refused") {
			t.Errorf("refused: %v (want ErrNotSent, MAM-PRV-0012 and what happened)", err)
		}
	})
	t.Run("the response is lost", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close() // after the request was read
		}))
		defer srv.Close()
		_, _, err := HTTP{Base: srv.URL + apiPath, Client: &http.Client{Timeout: time.Second}}.Do(t.Context(), protocol.HostRequest{Method: "PUT", Path: "/items/web", Form: form("value", "v1")})
		mustNotShow(t, "lost", err, secrets(srv, apiPath)...)
		if errors.Is(err, ErrNotSent) || !strings.Contains(err.Error(), "MAM-PRV-0012") {
			t.Errorf("lost: %v (a change that may have taken effect is not 'not sent')", err)
		}
	})
	t.Run("no answer in time", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
		defer srv.Close()
		defer close(release)
		_, _, err := HTTP{Base: srv.URL + apiPath, Client: &http.Client{Timeout: 100 * time.Millisecond}}.Do(t.Context(), get)
		mustNotShow(t, "timeout", err, secrets(srv, apiPath)...)
		if err == nil || !strings.Contains(err.Error(), "did not answer in time") {
			t.Errorf("timeout: %v", err)
		}
	})
	t.Run("the certificate is not accepted", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.NotFoundHandler())
		defer srv.Close()
		// A client that trusts no CA the server's certificate is signed by.
		_, _, err := HTTP{Base: srv.URL + apiPath, Client: &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{}}}}.Do(t.Context(), get)
		mustNotShow(t, "certificate", err, secrets(srv, apiPath)...)
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("certificate: %v", err)
		}
	})
	t.Run("the pinned certificate does not match", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.NotFoundHandler())
		defer srv.Close()
		c := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true,
			VerifyConnection: func(tls.ConnectionState) error { return ErrCertificatePin }}}}
		_, _, err := HTTP{Base: srv.URL + apiPath, Client: c}.Do(t.Context(), get)
		mustNotShow(t, "pin", err, secrets(srv, apiPath)...)
		if err == nil || !strings.Contains(err.Error(), "pinned fingerprint") {
			t.Errorf("pin: %v", err)
		}
	})
	t.Run("a path that makes the URL invalid", func(t *testing.T) {
		// The gate checks paths before they get here; the upstream still must not repeat the URL.
		_, _, err := HTTP{Base: "https://host.example:8006/api", Client: &http.Client{}}.Do(t.Context(), protocol.HostRequest{Method: "GET", Path: "/items/\x7f"})
		mustNotShow(t, "bad path", err, "host.example", "8006", "/api")
		if !errors.Is(err, ErrNotSent) || !strings.Contains(err.Error(), "MAM-PRV-0012") {
			t.Errorf("bad path: %v", err)
		}
	})
	t.Run("a base that is not a URL", func(t *testing.T) {
		_, _, err := HTTP{Base: "https://host.example:8006/api\x7f", Client: &http.Client{}}.Do(t.Context(), get)
		mustNotShow(t, "bad base", err, "host.example", "8006")
	})
	t.Run("the response cannot be read", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(200)
			w.Write([]byte("short"))
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		}))
		defer srv.Close()
		_, _, err := HTTP{Base: srv.URL + apiPath, Client: &http.Client{Timeout: time.Second}}.Do(t.Context(), get)
		mustNotShow(t, "short body", err, secrets(srv, apiPath)...)
	})
}

// How each kind of failure is told, whatever the address in the Go error.
func TestSayConnectionFailure(t *testing.T) {
	u := "https://pve.example:8006/api2/json/nodes"
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: u, Err: err} }
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"dns":     {wrap(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "pve.example", IsNotFound: true}}), "name of the API's host did not resolve"},
		"refused": {wrap(&net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(192, 168, 1, 10), Port: 8006}, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}), "refused"},
		"timeout": {wrap(&net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}), "did not answer in time"},
		"reset":   {wrap(fmt.Errorf("read tcp 192.0.2.2:5555->192.0.2.10:8006: read: connection reset by peer")), "connection failed"},
	} {
		got := sayConnectionFailure(c.err)
		if !strings.Contains(got, "MAM-PRV-0012") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q (want MAM-PRV-0012 and %q)", name, got, c.want)
		}
		for _, secret := range []string{"pve.example", "192.0.2.", "8006", "api2"} {
			if strings.Contains(got, secret) {
				t.Errorf("%s: %q shows %q", name, got, secret)
			}
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// The last defence: text that reaches a person with something that looks like an address in it.
func TestRedactAddresses(t *testing.T) {
	for in, want := range map[string]string{
		`Get "https://pve.example:8006/api2/json": EOF`:                        `Get "<address removed>": EOF`,
		`see http://192.0.2.5/x?y=1 and https://a.example/b.`:                  `see <address removed> and <address removed>.`,
		`dial tcp 192.0.2.10:8006: connect: connection refused`:                `dial tcp <address removed>: connect: connection refused`,
		`read tcp 192.0.2.2:5555->192.0.2.10:8006: reset`:                      `read tcp <address removed>-><address removed>: reset`,
		`dial tcp [2001:db8::1]:8006: no route`:                                `dial tcp <address removed>: no route`,
		`MAM-PRV-0004: PUT /items/web at 2026-11-17T12:30:00Z (version 1.2.3)`: `MAM-PRV-0004: PUT /items/web at 2026-11-17T12:30:00Z (version 1.2.3)`,
		`sha256:` + strings.Repeat("a", 64) + ` is not the approved one`:       `sha256:` + strings.Repeat("a", 64) + ` is not the approved one`,
		// A value of the declaration that a person compares is not an address of a connection.
		`parameter ipconfig0=ip=192.0.2.5/24 is not the approved ip=192.0.2.6/24`: `parameter ipconfig0=ip=192.0.2.5/24 is not the approved ip=192.0.2.6/24`,
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	// An error formats redacted, and keeps its code.
	e := &Error{Code: CodeFailed, Message: "the API at https://pve.example:8006/api is down", Hint: "check https://pve.example:8006/", Detail: "dial tcp 198.51.100.3:8006: refused"}
	if s := e.Error(); strings.Contains(s, "pve.example") || strings.Contains(s, "198.51.100.3") || !strings.Contains(s, "MAM-PRV-0007") {
		t.Errorf("Error(): %s", s)
	}
	// What an upstream says, whatever it is, is redacted on its way to the provider and the record.
	if r := (redacted{errors.New(`Get "https://pve.example/x": EOF`)}); strings.Contains(r.Error(), "pve.example") {
		t.Errorf("redacted: %v", r)
	}
}

// leaky is an upstream that, unlike HTTP, puts the address in its errors.
type leaky struct{}

func (leaky) Do(context.Context, protocol.HostRequest) (int, []byte, error) {
	return 0, nil, fmt.Errorf(`Get "https://pve.example:8006/api2/json/items/web": dial tcp 198.51.100.3:8006: connect: connection refused`)
}

// Whatever sends the request, what goes back to the provider and into the record has no address.
func TestGateRedactsWhatAnUpstreamSays(t *testing.T) {
	g := &gate{phase: phaseObserve, calls: []protocol.Call{{Method: "GET", Path: "/items/{item}"}}, catalog: kvapi.Catalog, upstream: leaky{}, now: time.Now}
	_, perr := g.send(t.Context(), protocol.HostRequest{Method: "GET", Path: "/items/web"})
	if perr == nil || len(g.record) != 1 {
		t.Fatalf("%v %v", perr, g.record)
	}
	for _, text := range []string{perr.Message, g.record[0].Reason, g.record[0].String()} {
		if strings.Contains(text, "pve.example") || strings.Contains(text, "198.51.100.3") || strings.Contains(text, "api2") || !strings.Contains(text, "connection refused") {
			t.Errorf("an upstream's error reached the provider or the record with an address in it: %q", text)
		}
	}
}
