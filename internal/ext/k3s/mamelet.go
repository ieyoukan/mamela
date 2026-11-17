package k3s

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

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// progK3s is the only program k3s adds to what mamelet runs: kubectl on the server, to list the
// nodes (K3sNodes) and to cordon, drain and delete one (RemoveK3sNode).
const progK3s = "/usr/local/bin/k3s"

// progSystemctl is the core's systemctl: k3s reloads systemd after writing its unit, and reads when
// its process started.
const progSystemctl = "/usr/bin/systemctl"

// system is mamelet's view of the node, with what k3s does on it.
type system struct{ *mamelet.System }

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

// K3s observes k3s against what the revision says: installed means a binary, the unit and the
// token are exactly what mamelet writes for want. A unit that is neither that nor absent was
// changed by hand, and is reported as an error.
func (s system) K3s(ctx context.Context, want noderev.K3s) noderev.K3sState {
	st := noderev.K3sState{Addresses: s.addresses()}
	sum, err := mamelet.FileSHA256(s.Path(k3sBinary))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		st.Error = err.Error()
		return st
	default:
		st.SHA256 = sum
	}
	st.Running, st.Stale = s.k3sProcess(ctx)
	unit, err := os.ReadFile(s.Path(k3sUnit))
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
	token, err := os.ReadFile(s.Path(noderev.K3sTokenFile))
	st.Installed = st.SHA256 != "" && string(unit) == K3sUnit(want.Role, want.Server) && err == nil && string(token) == want.Token+"\n"
	return st
}

// k3sProcess says whether the k3s service runs and, if so, whether the process started before the
// binary in place was written (a stale process runs another release than the one on disk). The
// start time is systemd's, in whole seconds, so the binary's time is cut to seconds too: a restart
// right after the binary was written is not stale. Where the start time cannot be read, a running
// process is taken as not stale: the other checks (the binary's sha256) still hold.
func (s system) k3sProcess(ctx context.Context) (running, stale bool) {
	out, err := s.Run(ctx, progSystemctl, []string{"show", "--timestamp=unix", "--property=ActiveState,ExecMainStartTimestamp", "--", "k3s"})
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
	fi, err := os.Stat(s.Path(k3sBinary))
	if !running || started.IsZero() || err != nil {
		return running, false
	}
	return true, started.Before(fi.ModTime().Truncate(time.Second))
}

// k3sNodesFormat lists the nodes of the cluster: name, kubelet release and Ready, tab separated.
const k3sNodesFormat = `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.status.nodeInfo.kubeletVersion}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`

// K3sNodes reads the Kubernetes nodes a running k3s server sees, with its own kubeconfig (a fixed
// read-only command). nil when they cannot be read: the plan then does not take the cluster as Ready.
func (s system) K3sNodes(ctx context.Context) []noderev.K3sNode {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := s.Run(cctx, progK3s, []string{"kubectl", "get", "nodes", "--output=" + k3sNodesFormat}, "KUBECONFIG="+k3sKubeconfig)
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
func (s system) addresses() []string {
	if s.Addresses != nil {
		return s.Addresses()
	}
	b, err := os.ReadFile(s.Path("/proc/net/route"))
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
func (s system) InstallK3s(ctx context.Context, k noderev.K3s) error {
	if sum, err := mamelet.FileSHA256(s.Path(k3sBinary)); err != nil || sum != k.SHA256 {
		if err := s.downloadK3s(ctx, k.Version, k.SHA256); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.Path(noderev.K3sTokenFile)), 0o700); err != nil {
		return err
	}
	if err := mamelet.WriteFile(s.Path(noderev.K3sTokenFile), []byte(k.Token+"\n"), 0o600); err != nil {
		return err
	}
	if err := mamelet.WriteFile(s.Path(k3sUnit), []byte(K3sUnit(k.Role, k.Server)), 0o644); err != nil {
		return err
	}
	if _, err := s.Run(ctx, progSystemctl, []string{"daemon-reload"}); err != nil {
		return err
	}
	if running, _ := s.k3sProcess(ctx); running {
		return s.Restart(ctx, "k3s")
	}
	return nil
}

func (s system) downloadK3s(ctx context.Context, version, wantSHA256 string) error {
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
	dir := filepath.Dir(s.Path(k3sBinary))
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
	return os.Rename(tmp.Name(), s.Path(k3sBinary))
}

// Manifest observes a manifest file: the sha256 of its content ("" when it is not there).
func (s system) Manifest(name string) noderev.ManifestState {
	st := noderev.ManifestState{Name: name}
	sum, err := mamelet.FileSHA256(s.Path(manifestPath(name)))
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
func (s system) WriteManifest(m noderev.Manifest) error {
	return mamelet.WriteFile(s.Path(manifestPath(m.Name)), []byte(m.Content), 0o600)
}

// How long a drain may take before mamelet gives up (the node is then left as it was).
const k3sDrainTimeout = 4 * time.Minute

// RemoveK3sNode removes the Kubernetes node name from the cluster a running server serves, with
// the server's own kubeconfig and fixed commands only. A node that is Ready is drained first (its
// pods move to other nodes: cordon, then drain without touching DaemonSets); a node that is not
// Ready cannot be drained, so it is only deleted. A drain that does not finish leaves nothing
// deleted and the node schedulable again. A node that is already gone is a success. The server's
// own node is never removed. The returned text says what was done.
func (s system) RemoveK3sNode(ctx context.Context, name, self string) (string, error) {
	if !noderev.ValidNode(name) {
		return "", fmt.Errorf("%q is not a node name mamelet removes", name)
	}
	if name == self {
		return "", fmt.Errorf("%s is this server's own node: mamelet never removes it", name)
	}
	env := "KUBECONFIG=" + k3sKubeconfig
	kubectl := func(timeout time.Duration, args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return s.Run(cctx, progK3s, append([]string{"kubectl"}, args...), env)
	}
	var node *noderev.K3sNode
	nodes := s.K3sNodes(ctx)
	if nodes == nil {
		return "", errors.New("the cluster's nodes cannot be read: not removing anything")
	}
	for i := range nodes {
		if nodes[i].Name == name {
			node = &nodes[i]
		}
	}
	if node == nil {
		return "Kubernetes node " + name + " is already gone", nil
	}
	done := "deleted Kubernetes node " + name
	if node.Ready {
		if _, err := kubectl(time.Minute, "cordon", name); err != nil {
			return "", err
		}
		if _, err := kubectl(k3sDrainTimeout+time.Minute, "drain", name, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout="+k3sDrainTimeout.String()); err != nil {
			_, _ = kubectl(time.Minute, "uncordon", name) // best effort: leave it as it was
			return "", fmt.Errorf("draining %s did not finish (nothing was deleted): %w", name, err)
		}
		done = "drained and deleted Kubernetes node " + name
	} else {
		done = "deleted Kubernetes node " + name + " (not Ready, so not drained)"
	}
	if _, err := kubectl(time.Minute, "delete", "node", name); err != nil {
		return "", err
	}
	return done, nil
}

func init() {
	mamelet.Register(mamelet.Extension{
		Name:     "k3s",
		Programs: []string{progK3s},
		Observe:  observeNode,
		Act:      act,
		Results:  results,
	})
}

// observeNode reports the manifests and the k3s the revision declares.
func observeNode(ctx context.Context, sys *mamelet.System, rev *noderev.Revision, rep *noderev.Report) {
	s := system{sys}
	for _, m := range rev.Manifests {
		rep.Manifests = append(rep.Manifests, s.Manifest(m.Name))
	}
	if rev.K3s != nil {
		k := s.K3s(ctx, *rev.K3s)
		if rev.K3s.Role == noderev.K3sServer && k.Running {
			k.Nodes = s.K3sNodes(ctx)
		}
		rep.K3s = &k
	}
}

// act carries out the grants: before mamelet's own update, the manifests' writes and k3s's
// installation; after it, the removal of a Kubernetes node.
func act(ctx context.Context, x *mamelet.Act, phase mamelet.Phase) bool {
	s, rev, online := system{x.System}, x.Revision, x.Online
	record := x.Record
	changed := false
	if phase == mamelet.AfterUpdate {
		if k := rev.K3s; k != nil && k.Remove != nil && online && k.Role == noderev.K3sServer {
			rm := k.Remove
			if !x.Done(rm.Operation, rm.Attempt) {
				// Only a server that runs removes nodes; a server that is not up yet waits for a round
				// in which it is.
				if running, _ := s.k3sProcess(ctx); running {
					changed = true
					if detail, err := s.RemoveK3sNode(ctx, rm.Node, x.Node); err != nil {
						record(rm.Operation, rm.Attempt, noderev.OutcomeFailed, err.Error())
					} else {
						record(rm.Operation, rm.Attempt, noderev.OutcomeDone, detail)
					}
				}
			}
		}
		return changed
	}
	for _, m := range rev.Manifests {
		in := m.Write
		if in == nil || !online {
			continue
		}
		if x.Done(in.Operation, in.Attempt) {
			continue
		}
		obs := s.Manifest(m.Name)
		switch {
		case obs.Error != "":
			record(in.Operation, in.Attempt, noderev.OutcomeRefused, obs.Error)
		case obs.SHA256 == noderev.ManifestSHA256(m.Content):
			record(in.Operation, in.Attempt, noderev.OutcomeDone, "manifest "+m.Name+" is in place")
		default:
			changed = true
			if err := s.WriteManifest(m); err != nil {
				record(in.Operation, in.Attempt, noderev.OutcomeFailed, err.Error())
			} else {
				record(in.Operation, in.Attempt, noderev.OutcomeDone, "wrote manifest "+m.Name)
			}
		}
	}

	if k := rev.K3s; k != nil && k.Install != nil && online {
		in := k.Install
		if !x.Done(in.Operation, in.Attempt) {
			obs := s.K3s(ctx, *k)
			switch {
			case obs.Installed && obs.SHA256 == k.SHA256 && !obs.Stale:
				record(in.Operation, in.Attempt, noderev.OutcomeDone, "k3s "+k.Version+" is installed")
			case obs.Error != "":
				record(in.Operation, in.Attempt, noderev.OutcomeRefused, obs.Error)
			default:
				changed = true
				if err := s.InstallK3s(ctx, *k); err != nil {
					record(in.Operation, in.Attempt, noderev.OutcomeFailed, err.Error())
				} else {
					detail := "installed k3s " + k.Version
					if obs.Running {
						detail += " and restarted it"
					}
					record(in.Operation, in.Attempt, noderev.OutcomeDone, detail)
				}
			}
		}
	}
	return changed
}

// results are the recorded outcomes of the grants: the manifests' before mamelet's update, k3s's
// installation and node removal after it (the order of the report).
func results(rev *noderev.Revision, phase mamelet.Phase, known func(op string, attempt int) (noderev.Result, bool)) []noderev.Result {
	var out []noderev.Result
	if phase == mamelet.BeforeUpdate {
		for _, m := range rev.Manifests {
			if m.Write != nil {
				if r, ok := known(m.Write.Operation, m.Write.Attempt); ok {
					out = append(out, r)
				}
			}
		}
		return out
	}
	if k := rev.K3s; k != nil && k.Install != nil {
		if r, ok := known(k.Install.Operation, k.Install.Attempt); ok {
			out = append(out, r)
		}
	}
	if k := rev.K3s; k != nil && k.Remove != nil {
		if r, ok := known(k.Remove.Operation, k.Remove.Attempt); ok {
			out = append(out, r)
		}
	}
	return out
}
