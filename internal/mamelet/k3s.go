package mamelet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// Where k3s goes. Nothing about these comes from a revision: a revision names only the release
// and the sha256 of its binary.
const (
	k3sBinary = "/usr/local/bin/k3s"
	k3sUnit   = "/etc/systemd/system/k3s.service"
	// k3sKubeconfig is the server's own kubeconfig (readable by root only).
	k3sKubeconfig = "/etc/rancher/k3s/k3s.yaml"
	// maxK3sBinary bounds a download.
	maxK3sBinary = 256 << 20
)

// K3sUnit is the systemd unit mamelet writes (the one the k3s install script writes, with
// noderev.K3sArgs for the role).
func K3sUnit(role, server string) string {
	exec := k3sBinary
	for _, a := range noderev.K3sArgs(role, server) {
		exec += " " + a
	}
	return `[Unit]
Description=Lightweight Kubernetes (installed by mamelet)
Documentation=https://k3s.io
Wants=network-online.target
After=network-online.target

[Install]
WantedBy=multi-user.target

[Service]
Type=notify
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=` + exec + `
`
}

// K3sAsset is the release asset of the architecture mamelet runs on.
func K3sAsset() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "k3s", nil
	case "arm64":
		return "k3s-arm64", nil
	}
	return "", fmt.Errorf("k3s is not supported on %s", runtime.GOARCH)
}

// K3sURL is the download URL of a release's binary.
func K3sURL(base, version string) (string, error) {
	asset, err := K3sAsset()
	if err != nil {
		return "", err
	}
	return base + "/" + noderev.K3sTag(version) + "/" + asset, nil
}

func (s *System) path(p string) string { return filepath.Join(s.Root, p) }

// K3s observes k3s against what the revision says: installed means a binary, the unit and the
// token are exactly what mamelet writes for want. A unit that is neither that nor absent was
// changed by hand, and is reported as an error.
func (s *System) K3s(ctx context.Context, want noderev.K3s) noderev.K3sState {
	st := noderev.K3sState{Addresses: s.addresses()}
	sum, err := fileSHA256(s.path(k3sBinary))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		st.Error = err.Error()
		return st
	default:
		st.SHA256 = sum
	}
	st.Running, st.Stale = s.k3sProcess(ctx)
	unit, err := os.ReadFile(s.path(k3sUnit))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st
	case err != nil:
		st.Error = err.Error()
		return st
	case !isMameletUnit(string(unit)):
		st.Error = k3sUnit + " is not a unit mamelet writes (changed by hand?)"
		return st
	}
	st.Server = unitServer(string(unit))
	token, err := os.ReadFile(s.path(noderev.K3sTokenFile))
	st.Installed = st.SHA256 != "" && string(unit) == K3sUnit(want.Role, want.Server) && err == nil && string(token) == want.Token+"\n"
	return st
}

// k3sProcess says whether the k3s service runs and, if so, whether the process started before the
// binary in place was written (a stale process runs another release than the one on disk). The
// start time is systemd's, in whole seconds, so the binary's time is cut to seconds too: a restart
// right after the binary was written is not stale. Where the start time cannot be read, a running
// process is taken as not stale: the other checks (the binary's sha256) still hold.
func (s *System) k3sProcess(ctx context.Context) (running, stale bool) {
	out, err := s.run(ctx, progSystemctl, []string{"show", "--timestamp=unix", "--property=ActiveState,ExecMainStartTimestamp", "--", "k3s"})
	if err != nil {
		return false, false
	}
	var started time.Time
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ActiveState":
			running = v == "active"
		case "ExecMainStartTimestamp":
			if n, err := strconv.ParseInt(strings.TrimPrefix(v, "@"), 10, 64); err == nil && strings.HasPrefix(v, "@") && n > 0 {
				started = time.Unix(n, 0)
			}
		}
	}
	fi, err := os.Stat(s.path(k3sBinary))
	if !running || started.IsZero() || err != nil {
		return running, false
	}
	return true, started.Before(fi.ModTime().Truncate(time.Second))
}

// k3sNodesFormat lists the nodes of the cluster: name, kubelet release and Ready, tab separated.
const k3sNodesFormat = `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.status.nodeInfo.kubeletVersion}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`

// K3sNodes reads the Kubernetes nodes a running k3s server sees, with its own kubeconfig (a fixed
// read-only command). nil when they cannot be read: the plan then does not take the cluster as Ready.
func (s *System) K3sNodes(ctx context.Context) []noderev.K3sNode {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := s.run(cctx, progK3s, []string{"kubectl", "get", "nodes", "--output=" + k3sNodesFormat}, "KUBECONFIG="+k3sKubeconfig)
	if err != nil {
		return nil
	}
	return ParseK3sNodes(out)
}

// ParseK3sNodes parses the output of k3sNodesFormat.
func ParseK3sNodes(out string) []noderev.K3sNode {
	var nodes []noderev.K3sNode
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		nodes = append(nodes, noderev.K3sNode{Name: f[0], Version: f[1], Ready: f[2] == "True"})
	}
	slices.SortFunc(nodes, func(a, b noderev.K3sNode) int { return strings.Compare(a.Name, b.Name) })
	return nodes
}

// unitServer is the --server of an agent's unit, or "".
func unitServer(unit string) string {
	_, rest, ok := strings.Cut(unit, "ExecStart="+k3sBinary+" agent --server=")
	if !ok {
		return ""
	}
	server, _, _ := strings.Cut(rest, " ")
	return server
}

// isMameletUnit reports whether a unit is one mamelet writes, for some role.
func isMameletUnit(unit string) bool {
	head, _, ok := strings.Cut(K3sUnit(noderev.K3sServer, ""), "ExecStart=")
	if !ok || !strings.HasPrefix(unit, head+"ExecStart="+k3sBinary+" ") {
		return false
	}
	rest := strings.TrimPrefix(unit, head+"ExecStart=")
	return strings.Count(rest, "\n") == 1 && strings.HasSuffix(rest, "\n") &&
		(unit == K3sUnit(noderev.K3sServer, "") || strings.HasPrefix(rest, k3sBinary+" agent --server=https://"))
}

// addresses are the global IPv4 addresses of the interface that holds the default route, sorted:
// the address other nodes reach this one by. Addresses of other interfaces are not reported,
// because k3s itself adds some (flannel.1 and cni0 in 10.42.0.0/16) that sort before a LAN
// address, and an agent pointed at one of those cannot join after a reboot. Addresses in k3s's
// own ranges are left out even on that interface.
func (s *System) addresses() []string {
	if s.Addresses != nil {
		return s.Addresses()
	}
	b, err := os.ReadFile(s.path("/proc/net/route"))
	if err != nil {
		return nil
	}
	iface, ok := DefaultRouteInterface(string(b))
	if !ok {
		return nil
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			ips = append(ips, n.IP)
		}
	}
	return NodeAddresses(ips)
}

// k3sRanges are the default cluster and service ranges of k3s; a node's own address is never in them.
var k3sRanges = []*net.IPNet{
	{IP: net.IPv4(10, 42, 0, 0).To4(), Mask: net.CIDRMask(16, 32)},
	{IP: net.IPv4(10, 43, 0, 0).To4(), Mask: net.CIDRMask(16, 32)},
}

// NodeAddresses keeps the global unicast IPv4 addresses outside k3s's ranges, sorted.
func NodeAddresses(ips []net.IP) []string {
	var out []string
	for _, ip := range ips {
		v4 := ip.To4()
		if v4 == nil || !v4.IsGlobalUnicast() || slices.ContainsFunc(k3sRanges, func(n *net.IPNet) bool { return n.Contains(v4) }) {
			continue
		}
		out = append(out, v4.String())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// DefaultRouteInterface reads /proc/net/route and returns the interface of the default route with
// the lowest metric.
func DefaultRouteInterface(table string) (string, bool) {
	best, metric := "", -1
	for i, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		var flags, m int
		if _, err := fmt.Sscanf(f[3]+" "+f[6], "%x %d", &flags, &m); err != nil || flags&1 == 0 { // RTF_UP
			continue
		}
		if metric < 0 || m < metric {
			best, metric = f[0], m
		}
	}
	return best, metric >= 0
}

// InstallK3s installs k3s as the revision says: the release's binary (downloaded unless the one in
// place already has the approved sha256, and installed only if its sha256 is the approved one),
// the join token and Mamela's unit for the role. It does not start or enable the service (that is
// a NodeService); a k3s that runs is restarted, so that it runs what was installed (an upgrade).
func (s *System) InstallK3s(ctx context.Context, k noderev.K3s) error {
	if sum, err := fileSHA256(s.path(k3sBinary)); err != nil || sum != k.SHA256 {
		if err := s.downloadK3s(ctx, k.Version, k.SHA256); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.path(noderev.K3sTokenFile)), 0o700); err != nil {
		return err
	}
	if err := writeK3sFile(s.path(noderev.K3sTokenFile), []byte(k.Token+"\n"), 0o600); err != nil {
		return err
	}
	if err := writeK3sFile(s.path(k3sUnit), []byte(K3sUnit(k.Role, k.Server)), 0o644); err != nil {
		return err
	}
	if _, err := s.run(ctx, progSystemctl, []string{"daemon-reload"}); err != nil {
		return err
	}
	if running, _ := s.k3sProcess(ctx); running {
		return s.Restart(ctx, "k3s")
	}
	return nil
}

func (s *System) downloadK3s(ctx context.Context, version, wantSHA256 string) error {
	base := s.K3sReleases
	if base == "" {
		base = noderev.K3sReleases
	}
	u, err := K3sURL(base, version)
	if err != nil {
		return err
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", u, resp.StatusCode)
	}
	dir := filepath.Dir(s.path(k3sBinary))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".k3s-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxK3sBinary+1))
	if err == nil && n > maxK3sBinary {
		err = fmt.Errorf("the download is larger than %d MiB", maxK3sBinary>>20)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", u, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		return fmt.Errorf("the downloaded k3s %s has sha256 %s, not the approved %s; nothing was installed", version, got, wantSHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path(k3sBinary))
}

func writeK3sFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mamelet-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Manifest observes a manifest file: the sha256 of its content ("" when it is not there).
func (s *System) Manifest(name string) noderev.ManifestState {
	st := noderev.ManifestState{Name: name}
	sum, err := fileSHA256(s.path(manifestPath(name)))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		st.Error = err.Error()
	default:
		st.SHA256 = sum
	}
	return st
}

func manifestPath(name string) string {
	return filepath.Join(noderev.K3sManifestDir, noderev.K3sManifestFile(name))
}

// WriteManifest writes a manifest as the revision says, atomically, readable by root only (k3s
// applies it with cluster-admin rights, and it may hold values).
func (s *System) WriteManifest(m noderev.Manifest) error {
	return writeK3sFile(s.path(manifestPath(m.Name)), []byte(m.Content), 0o600)
}
