// Package proxmox is a minimal Proxmox VE API client.
//
// It can only send GET, POST and PUT, and only through the typed calls below: there is no generic
// request method and no DELETE at all, so nothing built on this client can delete a guest, a disk
// or a volume. The writes it offers are the ones the lab provider needs: upload an image, create a
// VM, grow a disk, start a VM and update a VM's description.
package proxmox

import (
	"bytes"
	"cmp"
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
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Client talks to one Proxmox VE API endpoint with one API token.
type Client struct {
	base    string // https://host:8006/api2/json
	auth    string // PVEAPIToken=user@realm!id=secret
	http    *http.Client
	timeout time.Duration // per request, except uploads (bounded by the caller's context)
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
		http:    &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf, Proxy: nil}},
		timeout: timeout,
		maxBody: 32 << 20,
	}, nil
}

// APIError is a response with a non-2xx status: the server answered, so the outcome is known.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("proxmox: %s %s: %d %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
	if e.Status == http.StatusForbidden {
		if why := ExplainPermission(e.Body); why != "" {
			msg += " — " + why
		}
	}
	return msg
}

// Uncertain reports whether a write that failed with err may still have taken effect: the request
// may have reached the server, but no definite answer came back (a lost connection, a timeout, or a
// gateway status from pveproxy). A definite API error means the request was refused or failed.
func Uncertain(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 595, 596, 599:
			return true
		}
		return false
	}
	return true
}

// methods is everything this client can send. DELETE is deliberately absent.
var methods = map[string]bool{http.MethodGet: true, http.MethodPost: true, http.MethodPut: true}

// do sends one request and decodes the "data" member of the response into out (when out is not nil).
func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader, long bool, out any) error {
	if !methods[method] {
		return fmt.Errorf("proxmox: method %s is not allowed", method)
	}
	if _, ok := LookupEndpoint(method, path); !ok {
		return fmt.Errorf("proxmox: %s %s is not in Mamela's permission catalog (internal/proxmox/permissions.go); "+
			"add it with the privileges Proxmox checks before calling it", method, path)
	}
	if !long {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if sr, ok := body.(*sizedReader); ok {
		req.ContentLength = sr.n
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("proxmox: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody))
	if err != nil {
		return fmt.Errorf("proxmox: %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		if len(data) > 512 {
			data = data[:512]
		}
		msg := string(data)
		if m := resp.Status; strings.TrimSpace(msg) == "" || msg == `{"data":null}` {
			msg = m // pveproxy puts the reason in the status line
		}
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: msg}
	}
	if out == nil {
		return nil
	}
	var envelope struct {
		Data jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("proxmox: %s %s: %w", method, path, err)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("proxmox: %s %s: %w", method, path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, "", nil, false, out)
}

func (c *Client) form(ctx context.Context, method, path string, values url.Values, out any) error {
	return c.do(ctx, method, path, "application/x-www-form-urlencoded", strings.NewReader(values.Encode()), false, out)
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

// nodePath is /nodes/<node> with the node name escaped.
func nodePath(node string) string { return "/nodes/" + url.PathEscape(node) }

// VMIDFree asks the cluster whether a VMID is unused (GET /cluster/nextid?vmid=N). Any user may ask.
func (c *Client) VMIDFree(ctx context.Context, vmid int) (bool, error) {
	var out jsontext.Value
	err := c.get(ctx, "/cluster/nextid?vmid="+strconv.Itoa(vmid), &out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest && strings.Contains(apiErr.Body, "already exists") {
		return false, nil
	}
	return err == nil, err
}

// VMConfig is a VM's configuration (GET /nodes/{node}/qemu/{vmid}/config). Values are kept as
// Proxmox returns them; numbers arrive as numbers or strings depending on the option.
type VMConfig map[string]jsontext.Value

// String returns option key as text ("" when absent).
func (c VMConfig) String(key string) string {
	v, ok := c[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(v))
}

// Int returns option key as an integer (0 when absent or not a number).
func (c VMConfig) Int(key string) int64 {
	n, _ := strconv.ParseInt(c.String(key), 10, 64)
	return n
}

func (c *Client) VMConfig(ctx context.Context, node string, vmid int) (VMConfig, error) {
	var out VMConfig
	return out, c.get(ctx, fmt.Sprintf("%s/qemu/%d/config", nodePath(node), vmid), &out)
}

// VMStatus is the part of GET /nodes/{node}/qemu/{vmid}/status/current Mamela uses.
type VMStatus struct {
	Status string `json:"status"` // running, stopped
	Lock   string `json:"lock"`
}

func (c *Client) VMStatus(ctx context.Context, node string, vmid int) (VMStatus, error) {
	var out VMStatus
	return out, c.get(ctx, fmt.Sprintf("%s/qemu/%d/status/current", nodePath(node), vmid), &out)
}

// Volume is one entry of GET /nodes/{node}/storage/{storage}/content.
type Volume struct {
	VolID  string `json:"volid"`
	Format string `json:"format"`
	Size   int64  `json:"size"`
}

func (c *Client) StorageContent(ctx context.Context, node, storage, content string) ([]Volume, error) {
	var out []Volume
	return out, c.get(ctx, nodePath(node)+"/storage/"+url.PathEscape(storage)+"/content?content="+url.QueryEscape(content), &out)
}

// Storages lists the storages on node that are enabled for a content type and visible to the token.
func (c *Client) Storages(ctx context.Context, node, content string) ([]string, error) {
	var out []struct {
		Storage string `json:"storage"`
	}
	if err := c.get(ctx, nodePath(node)+"/storage?content="+url.QueryEscape(content), &out); err != nil {
		return nil, err
	}
	names := make([]string, len(out))
	for i, s := range out {
		names[i] = s.Storage
	}
	return names, nil
}

// Task is the state of a background task (GET /nodes/{node}/tasks/{upid}/status).
type Task struct {
	Status     string `json:"status"`     // running, stopped
	ExitStatus string `json:"exitstatus"` // OK, WARNINGS: n, or an error message (when stopped)
}

// Succeeded reports whether a stopped task ended without an error.
func (t Task) Succeeded() bool {
	return t.Status == "stopped" && (t.ExitStatus == "OK" || strings.HasPrefix(t.ExitStatus, "WARNINGS"))
}

// TaskEntry is one task in a node's task list (GET /nodes/{node}/tasks).
type TaskEntry struct {
	UPID      string `json:"upid"`
	Type      string `json:"type"`
	ID        string `json:"id"` // the VMID for guest tasks
	StartTime int64  `json:"starttime"`
	EndTime   int64  `json:"endtime"` // 0 while the task runs
	Status    string `json:"status"`  // "RUNNING" while it runs, then OK, WARNINGS: n or an error
}

// Running reports whether the task has not ended.
func (t TaskEntry) Running() bool { return t.EndTime == 0 }

// Tasks lists the tasks of one type for a guest on node, running ones included, newest first.
// Without Sys.Audit on the node, Proxmox lists only the token's own tasks.
func (c *Client) Tasks(ctx context.Context, node string, vmid int, typ string) ([]TaskEntry, error) {
	q := url.Values{"vmid": {strconv.Itoa(vmid)}, "typefilter": {typ}, "source": {"all"}, "limit": {"50"}}
	var out []TaskEntry
	if err := c.get(ctx, nodePath(node)+"/tasks?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b TaskEntry) int { return cmp.Compare(b.StartTime, a.StartTime) })
	return out, nil
}

func (c *Client) TaskStatus(ctx context.Context, node, upid string) (Task, error) {
	var out Task
	return out, c.get(ctx, nodePath(node)+"/tasks/"+url.PathEscape(upid)+"/status", &out)
}

// GuestIPv4 returns the IPv4 addresses that qemu-guest-agent in the VM reports, loopback excluded
// (GET .../agent/network-get-interfaces; needs VM.GuestAgent.Audit).
func (c *Client) GuestIPv4(ctx context.Context, node string, vmid int) ([]string, error) {
	var out struct {
		Result []struct {
			Name        string `json:"name"`
			IPAddresses []struct {
				Address string `json:"ip-address"`
				Type    string `json:"ip-address-type"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if err := c.get(ctx, fmt.Sprintf("%s/qemu/%d/agent/network-get-interfaces", nodePath(node), vmid), &out); err != nil {
		return nil, err
	}
	var ips []string
	for _, iface := range out.Result {
		for _, a := range iface.IPAddresses {
			if ip := net.ParseIP(a.Address); a.Type == "ipv4" && ip != nil && !ip.IsLoopback() {
				ips = append(ips, a.Address)
			}
		}
	}
	return ips, nil
}

// CreateVM starts creating a VM (POST /nodes/{node}/qemu) and returns the task ID.
func (c *Client) CreateVM(ctx context.Context, node string, params url.Values) (string, error) {
	if err := checkParams(CreateParams, slices.Collect(maps.Keys(params)), "create VM"); err != nil {
		return "", err
	}
	var upid string
	return upid, c.form(ctx, http.MethodPost, nodePath(node)+"/qemu", params, &upid)
}

// GrowDisk extends a disk to size (PUT .../resize). Proxmox refuses to shrink.
func (c *Client) GrowDisk(ctx context.Context, node string, vmid int, disk, size string) (string, error) {
	var upid string
	return upid, c.form(ctx, http.MethodPut, fmt.Sprintf("%s/qemu/%d/resize", nodePath(node), vmid),
		url.Values{"disk": {disk}, "size": {size}}, &upid)
}

// StartVM starts a VM and returns the task ID.
func (c *Client) StartVM(ctx context.Context, node string, vmid int) (string, error) {
	var upid string
	return upid, c.form(ctx, http.MethodPost, fmt.Sprintf("%s/qemu/%d/status/start", nodePath(node), vmid), url.Values{}, &upid)
}

// SetMarker replaces a VM's description and tags in one change (PUT .../config, applied at once).
// digest makes the change fail if the configuration changed since it was read. Proxmox checks tag
// permissions on /vms/<vmid> without the pool, so a pool-scoped token can set tags only once the
// VM is in the pool, i.e. after its create task has ended.
func (c *Client) SetMarker(ctx context.Context, node string, vmid int, description, tags, digest string) error {
	v := url.Values{"description": {description}, "tags": {tags}}
	if digest != "" {
		v.Set("digest", digest)
	}
	if err := checkParams(ConfigParams, slices.Collect(maps.Keys(v)), "change VM configuration"); err != nil {
		return err
	}
	return c.form(ctx, http.MethodPut, fmt.Sprintf("%s/qemu/%d/config", nodePath(node), vmid), v, nil)
}

// SetSize sets a VM's vCPUs and memory (PUT .../config). digest makes it fail if the configuration
// changed since it was read. On a running VM the change stays pending until a restart.
func (c *Client) SetSize(ctx context.Context, node string, vmid int, cores, memoryMiB int64, digest string) error {
	v := url.Values{"cores": {strconv.FormatInt(cores, 10)}, "memory": {strconv.FormatInt(memoryMiB, 10)}, "digest": {digest}}
	if err := checkParams(ConfigParams, slices.Collect(maps.Keys(v)), "change VM configuration"); err != nil {
		return err
	}
	return c.form(ctx, http.MethodPut, fmt.Sprintf("%s/qemu/%d/config", nodePath(node), vmid), v, nil)
}

// RebootVM restarts a VM (shutdown, then start), which applies pending changes. Returns the task ID.
func (c *Client) RebootVM(ctx context.Context, node string, vmid int) (string, error) {
	var upid string
	return upid, c.form(ctx, http.MethodPost, fmt.Sprintf("%s/qemu/%d/status/reboot", nodePath(node), vmid), url.Values{}, &upid)
}

// Upload sends a file of size bytes to a storage (POST .../upload) and returns the task ID.
// Proxmox verifies the SHA-256 checksum before it keeps the file. pveproxy needs the exact length
// of the multipart body, so it is computed up front. The request is bounded only by ctx.
func (c *Client) Upload(ctx context.Context, node, storage, content, filename, sha256hex string, file io.Reader, size int64) (string, error) {
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	for _, kv := range [][2]string{{"content", content}, {"checksum-algorithm", "sha256"}, {"checksum", sha256hex}} {
		if err := mw.WriteField(kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	if _, err := mw.CreateFormFile("filename", filename); err != nil {
		return "", err
	}
	headLen := head.Len()
	if err := mw.Close(); err != nil {
		return "", err
	}
	tail := append([]byte(nil), head.Bytes()[headLen:]...)
	body := io.MultiReader(bytes.NewReader(head.Bytes()[:headLen]), io.LimitReader(file, size), bytes.NewReader(tail))
	length := int64(headLen) + size + int64(len(tail))

	var upid string
	err := c.do(ctx, http.MethodPost, nodePath(node)+"/storage/"+url.PathEscape(storage)+"/upload",
		mw.FormDataContentType(), &sizedReader{body, length}, true, &upid)
	return upid, err
}

// sizedReader lets net/http send a Content-Length instead of a chunked body.
type sizedReader struct {
	io.Reader
	n int64
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
