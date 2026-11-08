// Package proxmox is a minimal Proxmox VE API client.
//
// For now it is read-only by construction: the only request it can send is GET. Write operations
// arrive with the provider (step 2, L1), behind the lab boundary checks.
package proxmox

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client talks to one Proxmox VE API endpoint with one API token.
type Client struct {
	base    string // https://host:8006/api2/json
	auth    string // PVEAPIToken=user@realm!id=secret
	http    *http.Client
	maxBody int64
}

// Options configure TLS. Exactly one of Fingerprint and CAFile must be set.
type Options struct {
	Fingerprint string // SHA-256 of the server certificate, "AA:BB:..."
	CAFile      string
	Timeout     time.Duration
}

// New returns a client. apiURL is "https://host:8006".
func New(apiURL, tokenID, secret string, opts Options) (*Client, error) {
	u, err := url.Parse(apiURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("proxmox: api url must be https://host[:port], got %q", apiURL)
	}
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case opts.Fingerprint != "" && opts.CAFile == "":
		want, err := parseFingerprint(opts.Fingerprint)
		if err != nil {
			return nil, err
		}
		// The chain is not verified against a CA; the leaf certificate must match the pin exactly.
		tlsConf.InsecureSkipVerify = true
		tlsConf.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("proxmox: server sent no certificate")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(got[:]) != want {
				return fmt.Errorf("proxmox: server certificate fingerprint %s does not match the pinned one", formatFingerprint(got[:]))
			}
			return nil
		}
	case opts.CAFile != "" && opts.Fingerprint == "":
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("proxmox: no certificates in %s", opts.CAFile)
		}
		tlsConf.RootCAs = pool
	default:
		return nil, errors.New("proxmox: set exactly one of a certificate fingerprint and a CA file")
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		base:    strings.TrimRight(apiURL, "/") + "/api2/json",
		auth:    "PVEAPIToken=" + tokenID + "=" + secret,
		http:    &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: tlsConf, Proxy: nil}},
		maxBody: 32 << 20,
	}, nil
}

// APIError is a non-2xx response.
type APIError struct {
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("proxmox: GET %s: %d %s", e.Path, e.Status, strings.TrimSpace(e.Body))
}

// get performs GET <path> and decodes the "data" member into out.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("proxmox: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		if len(body) > 512 {
			body = body[:512]
		}
		return &APIError{Path: path, Status: resp.StatusCode, Body: string(body)}
	}
	var envelope struct {
		Data jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("proxmox: GET %s: %w", path, err)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("proxmox: GET %s: %w", path, err)
	}
	return nil
}

// ClusterStatusEntry is one entry of GET /cluster/status.
type ClusterStatusEntry struct {
	Type    string `json:"type"` // "cluster" or "node"
	Name    string `json:"name"`
	Quorate int    `json:"quorate"`
	Nodes   int    `json:"nodes"`
	Online  int    `json:"online"`
}

func (c *Client) ClusterStatus(ctx context.Context) ([]ClusterStatusEntry, error) {
	var out []ClusterStatusEntry
	return out, c.get(ctx, "/cluster/status", &out)
}

// Resource is one entry of GET /cluster/resources.
type Resource struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"` // qemu, lxc, node, storage, ...
	VMID     int     `json:"vmid"`
	Node     string  `json:"node"`
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	Pool     string  `json:"pool"`
	Tags     string  `json:"tags"`
	Template int     `json:"template"`
	Uptime   int64   `json:"uptime"`
	MaxMem   int64   `json:"maxmem"`
	MaxDisk  int64   `json:"maxdisk"`
	MaxCPU   float64 `json:"maxcpu"`
}

func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	var out []Resource
	return out, c.get(ctx, "/cluster/resources", &out)
}

// NodeStatus is the part of GET /nodes/{node}/status Mamela uses.
type NodeStatus struct {
	Memory struct {
		Total     int64 `json:"total"`
		Used      int64 `json:"used"`
		Available int64 `json:"available"`
	} `json:"memory"`
}

func (c *Client) NodeStatus(ctx context.Context, node string) (NodeStatus, error) {
	var out NodeStatus
	return out, c.get(ctx, "/nodes/"+url.PathEscape(node)+"/status", &out)
}

// StorageStatus is the part of GET /nodes/{node}/storage/{storage}/status Mamela uses.
type StorageStatus struct {
	Type  string `json:"type"`
	Total int64  `json:"total"`
	Used  int64  `json:"used"`
	Avail int64  `json:"avail"`
}

func (c *Client) StorageStatus(ctx context.Context, node, storage string) (StorageStatus, error) {
	var out StorageStatus
	return out, c.get(ctx, "/nodes/"+url.PathEscape(node)+"/storage/"+url.PathEscape(storage)+"/status", &out)
}

// Permissions returns the calling token's effective privileges by path (GET /access/permissions).
func (c *Client) Permissions(ctx context.Context) (map[string]map[string]int, error) {
	var out map[string]map[string]int
	return out, c.get(ctx, "/access/permissions", &out)
}

// ConfigDigest returns the digest Proxmox keeps for a guest's configuration. kind is "qemu" or "lxc".
func (c *Client) ConfigDigest(ctx context.Context, node, kind string, vmid int) (string, error) {
	if kind != "qemu" && kind != "lxc" {
		return "", fmt.Errorf("proxmox: unknown guest type %q", kind)
	}
	var out struct {
		Digest string `json:"digest"`
	}
	err := c.get(ctx, fmt.Sprintf("/nodes/%s/%s/%d/config", url.PathEscape(node), kind, vmid), &out)
	return out.Digest, err
}

func parseFingerprint(s string) (string, error) {
	h := strings.ToLower(strings.ReplaceAll(s, ":", ""))
	if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("proxmox: %q is not a SHA-256 fingerprint", s)
	}
	return h, nil
}

func formatFingerprint(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%02X", x)
	}
	return strings.Join(parts, ":")
}
