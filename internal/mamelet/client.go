package mamelet

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// EnrollRequest is sent once, with the one-time token (from the seed, or from the enrollment script
// of a machine Mamela did not make), to get a client certificate.
type EnrollRequest struct {
	Node  string `json:"node"`
	VMID  int    `json:"vmid"`
	Token string `json:"token"`
	CSR   string `json:"csr"` // PEM
}

// EnrollResponse carries the client certificate (and the renewed one, see RenewRequest).
type EnrollResponse struct {
	Certificate string `json:"certificate"` // PEM
}

// RenewRequest asks the Control Plane, with the current client certificate, for a new one before it
// ends. The request is signed by the key of the certificate: mamelet keeps its key, so that
// cert.pem and key.pem always belong together (one file is replaced, atomically).
type RenewRequest struct {
	CSR string `json:"csr"` // PEM
}

// ErrRenewUnsupported is what renewal returns when the Control Plane does not know the request (an
// older one): nothing was changed, and the certificate lasts as long as it does.
var ErrRenewUnsupported = errors.New("the Control Plane does not renew certificates (it is older than mamelet)")

const (
	// renewWhenLeft is the part of a certificate's life that is left when mamelet renews it.
	renewWhenLeft = 3 // a third
	// renewRetry is how long mamelet waits before asking again after a renewal that failed.
	renewRetry = time.Hour
)

const maxResponse = 1 << 20

// maxRevision bounds a signed revision as the Control Plane sends it. The files of the linux
// extension ride in it with their approved content (at most 1 MiB of it per node, which JSON
// escaping and the base64 of the signed payload make up to about 2.7 MiB): more than any other answer.
const maxRevision = 8 << 20

func (c Config) keyPath() string  { return filepath.Join(c.StateDir, "key.pem") }
func (c Config) certPath() string { return filepath.Join(c.StateDir, "cert.pem") }

// Enrolled reports whether mamelet already has a client certificate.
func Enrolled(c Config) bool {
	_, err := os.Stat(c.certPath())
	return err == nil
}

func (c Config) tlsConfig(cert *tls.Certificate) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(c.CA)) {
		return nil, errors.New("the Control Plane CA in the configuration is not a PEM certificate")
	}
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return cfg, nil
}

func httpClient(tlsCfg *tls.Config) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: nil}}
}

// Enroll creates a key, asks the Control Plane to certify it with the one-time token, and stores
// both. The key never leaves the VM.
func Enroll(ctx context.Context, c Config, token string) error {
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: c.Node}}, key)
	if err != nil {
		return err
	}
	tlsCfg, err := c.tlsConfig(nil)
	if err != nil {
		return err
	}
	body, err := json.Marshal(EnrollRequest{Node: c.Node, VMID: c.VMID, Token: token,
		CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))})
	if err != nil {
		return err
	}
	var resp EnrollResponse
	if err := do(ctx, httpClient(tlsCfg), http.MethodPost, c.Server+"/v1/enroll", body, &resp); err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	block, _ := pem.Decode([]byte(resp.Certificate))
	if block == nil {
		return errors.New("enroll: the Control Plane did not return a certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) || cert.Subject.CommonName != c.Node {
		return errors.New("enroll: the certificate is not for this key and node")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeFile(c.keyPath(), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return err
	}
	return writeFile(c.certPath(), []byte(resp.Certificate))
}

// writeFile replaces path with data, atomically and durably: after a power loss, the file is the
// old one or the new one, never an empty one (a node whose certificate file is empty cannot start).
func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Client talks to the Control Plane with the enrolled client certificate. One goroutine uses it
// at a time (mamelet's loop); renewing a certificate replaces the HTTP client.
type Client struct {
	cfg  Config
	http *http.Client
	cert tls.Certificate
	leaf *x509.Certificate
	// renewTried is when a renewal was last attempted (it is not retried within renewRetry).
	renewTried time.Time
	// moved is the URL a revision moved the Control Plane to (ADR 0003); "" when it did not.
	moved string
	// active is the URL the last revision came from; reports go there.
	active string
}

// MoveTo makes the client try url first, and the configured URL after it.
func (c *Client) MoveTo(url string) { c.moved = url }

func (c *Client) servers() []string {
	if c.moved != "" && c.moved != c.cfg.Server {
		return []string{c.moved, c.cfg.Server}
	}
	return []string{c.cfg.Server}
}

// NewClient loads the client certificate.
func NewClient(c Config) (*Client, error) {
	cert, err := tls.LoadX509KeyPair(c.certPath(), c.keyPath())
	if err != nil {
		return nil, fmt.Errorf("load the client certificate (not enrolled?): %w", err)
	}
	tlsCfg, err := c.tlsConfig(&cert)
	if err != nil {
		return nil, err
	}
	leaf := cert.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, fmt.Errorf("load the client certificate: %w", err)
		}
	}
	return &Client{cfg: c, http: httpClient(tlsCfg), cert: cert, leaf: leaf}, nil
}

// CertNotAfter is when the client certificate ends.
func (c *Client) CertNotAfter() time.Time { return c.leaf.NotAfter }

// CertExpired reports whether the client certificate has ended at now. The Control Plane then
// refuses the connection itself; only enrolling again helps.
func (c *Client) CertExpired(now time.Time) bool { return !now.Before(c.leaf.NotAfter) }

// RenewDue reports whether less than a third of the certificate's life is left at now.
func (c *Client) RenewDue(now time.Time) bool {
	life := c.leaf.NotAfter.Sub(c.leaf.NotBefore)
	return !now.Before(c.leaf.NotAfter.Add(-life / renewWhenLeft))
}

// RenewIfDue asks the Control Plane for a new certificate when less than a third of the life of the
// current one is left, saves it, and uses it from then on. It reports whether it did. A renewal
// that failed is not tried again for an hour. A Control Plane that does not know the request (an
// older one) answers 404: ErrRenewUnsupported, and nothing changes. The key stays the same, so a
// crash can only leave the old certificate or the new one in cert.pem, each with its key.
func (c *Client) RenewIfDue(ctx context.Context, now time.Time) (bool, error) {
	if !c.RenewDue(now) || (!c.renewTried.IsZero() && now.Sub(c.renewTried) < renewRetry) {
		return false, nil
	}
	c.renewTried = now
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: c.cfg.Node}}, c.cert.PrivateKey)
	if err != nil {
		return false, fmt.Errorf("renew: %w", err)
	}
	body, err := json.Marshal(RenewRequest{CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))})
	if err != nil {
		return false, err
	}
	server := c.active
	if server == "" {
		server = c.servers()[0]
	}
	var resp EnrollResponse
	if err := do(ctx, c.http, http.MethodPost, c.nodeURL(server, "/renew"), body, &resp); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			return false, ErrRenewUnsupported
		}
		return false, fmt.Errorf("renew: %w", err)
	}
	block, _ := pem.Decode([]byte(resp.Certificate))
	if block == nil {
		return false, errors.New("renew: the Control Plane did not return a certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, fmt.Errorf("renew: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(c.cfg.CA)) {
		return false, errors.New("the Control Plane CA in the configuration is not a PEM certificate")
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return false, fmt.Errorf("renew: the certificate is not one of the Control Plane's: %w", err)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(c.leaf.PublicKey) || cert.Subject.CommonName != c.cfg.Node || !cert.NotAfter.After(c.leaf.NotAfter) {
		return false, errors.New("renew: the certificate is not a later one for this key and node")
	}
	renewed := tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: c.cert.PrivateKey, Leaf: cert}
	tlsCfg, err := c.cfg.tlsConfig(&renewed)
	if err != nil {
		return false, err
	}
	if err := writeFile(c.cfg.certPath(), []byte(resp.Certificate)); err != nil {
		return false, fmt.Errorf("renew: save the certificate: %w", err)
	}
	old := c.http
	c.http, c.cert, c.leaf = httpClient(tlsCfg), renewed, cert
	old.CloseIdleConnections()
	return true, nil
}

func (c *Client) nodeURL(server, suffix string) string {
	return server + "/v1/nodes/" + url.PathEscape(c.cfg.Node) + suffix
}

// Revision fetches the node's current signed revision: from the URL the Control Plane moved to, or
// if that does not answer, from the configured one.
func (c *Client) Revision(ctx context.Context) (noderev.Signed, error) {
	var errs []error
	for _, server := range c.servers() {
		var s noderev.Signed
		err := doLimit(ctx, c.http, http.MethodGet, c.nodeURL(server, "/revision"), nil, &s, maxRevision)
		if err == nil {
			c.active = server
			return s, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", server, err))
	}
	return noderev.Signed{}, errors.Join(errs...)
}

// Mamelet fetches the mamelet binary with this sha256 from the Control Plane, which serves it to
// anyone, over the TLS this client is pinned to; the caller checks the sha256.
func (c *Client) Mamelet(ctx context.Context, sha string) ([]byte, error) {
	server := c.active
	if server == "" {
		server = c.servers()[0]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/v1/mamelet/"+url.PathEscape(sha), nil)
	if err != nil {
		return nil, err
	}
	hc := *c.http
	hc.Timeout = 5 * time.Minute
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("the Control Plane does not serve a mamelet with this sha256 (it serves the binary of its controlplane.mamelet_binary)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", req.URL.Path, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, noderev.MaxMamelet+1))
	if err != nil {
		return nil, err
	}
	if len(data) > noderev.MaxMamelet {
		return nil, fmt.Errorf("the binary is larger than %d bytes", noderev.MaxMamelet)
	}
	return data, nil
}

// WaitChange waits up to d for a revision newer than serial; it returns the newest serial and whether it is newer. A
// Control Plane that does not know the request answers with an error: the caller then waits by itself.
func (c *Client) WaitChange(ctx context.Context, serial int64, d time.Duration) (int64, bool, error) {
	server := c.active
	if server == "" {
		server = c.servers()[0]
	}
	secs := max(int(d/time.Second), 1)
	u := c.nodeURL(server, "/wait") + "?after=" + strconv.FormatInt(serial, 10) + "&seconds=" + strconv.Itoa(secs)
	var out struct {
		Serial  int64 `json:"serial"`
		Changed bool  `json:"changed"`
	}
	wctx, cancel := context.WithTimeout(ctx, d+15*time.Second)
	defer cancel()
	if err := do(wctx, c.waitHTTP(), http.MethodGet, u, nil, &out); err != nil {
		return 0, false, err
	}
	return out.Serial, out.Changed, nil
}

// waitHTTP is the client for waiting requests: the same certificate, a longer timeout (the request
// itself carries the deadline).
func (c *Client) waitHTTP() *http.Client {
	hc := *c.http
	hc.Timeout = 0
	return &hc
}

// Report sends a report.
func (c *Client) Report(ctx context.Context, r noderev.Report) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	server := c.active
	if server == "" {
		server = c.servers()[0]
	}
	return do(ctx, c.http, http.MethodPost, c.nodeURL(server, "/report"), body, nil)
}

// statusError is an answer other than 2xx.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func do(ctx context.Context, client *http.Client, method, u string, body []byte, out any) error {
	return doLimit(ctx, client, method, u, body, out, maxResponse)
}

// doLimit is do for an answer of at most limit bytes.
func doLimit(ctx context.Context, client *http.Client, method, u string, body []byte, out any, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%s %s: the answer is larger than %d bytes", method, u, limit)
	}
	if resp.StatusCode/100 != 2 {
		return &statusError{resp.StatusCode, fmt.Sprintf("%s %s: %s: %s", method, u, resp.Status, bytes.TrimSpace(data))}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.Unmarshal(data, out, json.RejectUnknownMembers(true))
}
