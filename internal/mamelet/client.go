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
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// EnrollRequest is sent once, with the one-time token from cloud-init, to get a client certificate.
type EnrollRequest struct {
	Node  string `json:"node"`
	VMID  int    `json:"vmid"`
	Token string `json:"token"`
	CSR   string `json:"csr"` // PEM
}

// EnrollResponse carries the client certificate.
type EnrollResponse struct {
	Certificate string `json:"certificate"` // PEM
}

const maxResponse = 1 << 20

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

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Client talks to the Control Plane with the enrolled client certificate.
type Client struct {
	cfg  Config
	http *http.Client
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
	return &Client{cfg: c, http: httpClient(tlsCfg)}, nil
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
		err := do(ctx, c.http, http.MethodGet, c.nodeURL(server, "/revision"), nil, &s)
		if err == nil {
			c.active = server
			return s, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", server, err))
	}
	return noderev.Signed{}, errors.Join(errs...)
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

func do(ctx context.Context, client *http.Client, method, u string, body []byte, out any) error {
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, u, resp.Status, bytes.TrimSpace(data))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.Unmarshal(data, out, json.RejectUnknownMembers(true))
}
