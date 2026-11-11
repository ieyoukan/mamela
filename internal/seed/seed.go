package seed

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// Params is everything a VM's seed holds.
type Params struct {
	Node          string
	VMID          int
	User          string   // the login user (SSH keys only; no password)
	SSHPublicKeys []string // one key per entry
	Mamelet       mamelet.Config
	EnrollToken   string
	MameletBinary []byte        // linux/amd64
	Network       graph.Network // DHCP or a static address (validated by the graph)
}

// unit is mamelet's systemd service. mamelet needs root for apt and systemctl.
const unit = `[Unit]
Description=Mamela node agent (mamelet)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/sbin/mamelet -config /etc/mamelet/config.json
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`

// Files returns the files of the seed.
func Files(p Params) ([]File, error) {
	if len(p.MameletBinary) == 0 {
		return nil, errors.New("seed: node, vmid, user, enrollment token and the mamelet binary are required")
	}
	sum := sha256.Sum256(p.MameletBinary)
	// Install mamelet from this seed, only if it is exactly the binary Mamela put there.
	install := `set -eu; d=$(mktemp -d); mount -o ro -L CIDATA "$d"; ` +
		`if echo "` + hex.EncodeToString(sum[:]) + `  $d/mamelet" | sha256sum -c -; then install -m 0755 "$d/mamelet" /usr/local/sbin/mamelet; fi; ` +
		`umount "$d"; rmdir "$d"`
	ud, err := userData(p, install, true)
	if err != nil {
		return nil, err
	}
	network, err := networkConfig(p.Network)
	if err != nil {
		return nil, err
	}
	meta := fmt.Sprintf("instance-id: mamela-%s-%d\nlocal-hostname: %s\n", p.Node, p.VMID, p.Node)
	return []File{
		{Name: "meta-data", Data: []byte(meta)},
		{Name: "user-data", Data: ud},
		{Name: "network-config", Data: network},
		{Name: "mamelet", Data: p.MameletBinary},
	}, nil
}

// MaxUserData is the most a cloud accepts as a machine's user-data (EC2: 16 KiB, before base64).
const MaxUserData = 16 << 10

// UserData is the seed as cloud-init user-data alone, for a machine a cloud creates (EC2): it
// cannot carry mamelet, so it fetches it from the Control Plane (binaryURL, verified with the CA
// of the configuration) and installs it only if its sha256 is the one Mamela put here. The network
// is the cloud's (DHCP).
func UserData(p Params, binaryURL, binarySHA256 string) ([]byte, error) {
	if p.Network.Mode != graph.NetworkDHCP {
		return nil, errors.New("seed: user-data configures no network; a cloud machine gets its address from the cloud (dhcp)")
	}
	if !strings.HasPrefix(binaryURL, "https://") || len(binarySHA256) != 64 || strings.ContainsAny(binaryURL, "'\"$`\\ \n") {
		return nil, errors.New("seed: mamelet's URL must be https and its sha256 known")
	}
	install := `set -eu; f=$(mktemp); curl -fsS --proto =https --cacert /etc/mamelet/ca.pem -o "$f" '` + binaryURL + `'; ` +
		`if echo "` + binarySHA256 + `  $f" | sha256sum -c -; then install -m 0755 "$f" /usr/local/sbin/mamelet; fi; rm -f "$f"`
	ud, err := userData(p, install, false)
	if err != nil {
		return nil, err
	}
	if len(ud) > MaxUserData {
		return nil, fmt.Errorf("seed: user-data is %d bytes, over %d", len(ud), MaxUserData)
	}
	return ud, nil
}

// userData is the #cloud-config: the login user, mamelet's configuration and one-time token, its
// unit, and the commands that install and start it (install is fixed text but for checksums).
func userData(p Params, install string, guestAgent bool) ([]byte, error) {
	if !noderev.ValidNode(p.Node) || p.VMID <= 0 || p.User == "" || p.EnrollToken == "" {
		return nil, errors.New("seed: node, vmid, user, enrollment token and the mamelet binary are required")
	}
	for _, k := range p.SSHPublicKeys {
		if strings.ContainsAny(k, "\r\n") {
			return nil, errors.New("seed: an SSH key spans lines")
		}
	}
	cfg, err := json.Marshal(p.Mamelet, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	keys := p.SSHPublicKeys
	if keys == nil {
		keys = []string{}
	}
	runcmd := []any{[]string{"sh", "-c", install}}
	packages := []string{}
	if guestAgent {
		packages = append(packages, "qemu-guest-agent")
		runcmd = append(runcmd, []string{"systemctl", "start", "qemu-guest-agent"})
	}
	runcmd = append(runcmd, []string{"systemctl", "daemon-reload"}, []string{"systemctl", "enable", "--now", "mamelet.service"})
	// user-data is JSON, which YAML (and cloud-init) reads as is: no YAML quoting pitfalls.
	doc := map[string]any{
		"users": []any{map[string]any{
			"name": p.User, "groups": []string{"sudo"}, "sudo": "ALL=(ALL) NOPASSWD:ALL", "shell": "/bin/bash",
			"lock_passwd": true, "ssh_authorized_keys": keys,
		}},
		"ssh_pwauth":     false,
		"package_update": guestAgent,
		"packages":       packages,
		"write_files": []any{
			map[string]any{"path": "/etc/mamelet/config.json", "permissions": "0600", "owner": "root:root", "content": string(cfg)},
			map[string]any{"path": "/etc/mamelet/enroll-token", "permissions": "0600", "owner": "root:root", "content": p.EnrollToken + "\n"},
			map[string]any{"path": "/etc/systemd/system/mamelet.service", "permissions": "0644", "owner": "root:root", "content": unit},
			map[string]any{"path": "/etc/mamelet/ca.pem", "permissions": "0644", "owner": "root:root", "content": p.Mamelet.CA},
		},
		"runcmd": runcmd,
	}
	ud, err := json.Marshal(doc, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), ud...), nil
}

// networkConfig is the seed's network-config (netplan version 2) for the VM's one NIC.
func networkConfig(n graph.Network) ([]byte, error) {
	nic := map[string]any{"match": map[string]string{"name": "e*"}}
	switch n.Mode {
	case graph.NetworkDHCP:
		nic["dhcp4"] = true
	case graph.NetworkStatic:
		if n.Address == "" || n.Gateway == "" || len(n.DNS) == 0 {
			return nil, errors.New("seed: a static network needs an address, a gateway and name servers")
		}
		nic["dhcp4"] = false
		nic["addresses"] = []string{n.Address}
		nic["routes"] = []any{map[string]string{"to": "default", "via": n.Gateway}}
		nic["nameservers"] = map[string]any{"addresses": n.DNS}
	default:
		return nil, fmt.Errorf("seed: network mode %q is not dhcp or static", n.Mode)
	}
	return json.Marshal(map[string]any{"version": 2, "ethernets": map[string]any{"primary": nic}}, json.Deterministic(true))
}

// netTag names a network in a seed's file name, so that a seed made for one network is never
// attached to a VM declared with another.
func netTag(n graph.Network) string {
	sum := sha256.Sum256([]byte(n.String()))
	return "n" + hex.EncodeToString(sum[:4])
}

// NewUserData makes a seed as user-data with a fresh one-time token (UserData), for a machine
// whose mamelet fetches its binary from binaryURL.
func (m *Maker) NewUserData(ctx context.Context, node string, id int, network graph.Network, binaryURL string) (string, []byte, error) {
	ttl := m.TokenTTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	sum := sha256.Sum256(m.MameletBinary)
	name := fmt.Sprintf("mamela-seed-%s-%d.user-data", node, id)
	token, err := m.Store.IssueEnrollToken(ctx, node, id, name, ttl)
	if err != nil {
		return "", nil, err
	}
	ud, err := UserData(Params{Node: node, VMID: id, User: m.User, SSHPublicKeys: m.SSHPublicKeys, EnrollToken: token,
		Network: network, Mamelet: m.config(node, id)}, binaryURL, hex.EncodeToString(sum[:]))
	if err != nil {
		return "", nil, err
	}
	return name, ud, nil
}

// Store is what the seed maker needs from Mamela's database.
type Store interface {
	IssueEnrollToken(ctx context.Context, node string, vmid int, seed string, ttl time.Duration) (string, error)
	SetEnrollSeed(ctx context.Context, token, seed string) error
	ReusableSeed(ctx context.Context, node string, vmid int, minLeft time.Duration) (string, bool, error)
}

// Maker makes seeds for the Proxmox provider.
type Maker struct {
	Store          Store
	Server         string // how mamelet reaches the Control Plane
	CA             []byte // PEM
	RevisionKey    ed25519.PublicKey
	User           string
	SSHPublicKeys  []string
	AllowedOrigins []string
	MameletBinary  []byte
	TokenTTL       time.Duration // default 24h
}

func (m *Maker) config(node string, vmid int) mamelet.Config {
	return mamelet.Config{Node: node, VMID: vmid, Server: m.Server, CA: string(m.CA), RevisionKey: m.RevisionKey,
		EnrollTokenFile: "/etc/mamelet/enroll-token", StateDir: "/var/lib/mamelet", IntervalSeconds: 30,
		AllowedOrigins: m.AllowedOrigins}
}

// Reusable returns the file name of a seed already made for this VM whose token is unused and
// valid for a while longer: a retried create attaches the same seed.
func (m *Maker) Reusable(ctx context.Context, node string, vmid int, network graph.Network) (string, bool, error) {
	file, ok, err := m.Store.ReusableSeed(ctx, node, vmid, time.Hour)
	if err != nil || !ok || !strings.Contains(file, "-"+netTag(network)+"-") {
		return "", false, err
	}
	return file, true, nil
}

// New makes a seed with a fresh one-time token. The file name contains the image's checksum.
func (m *Maker) New(ctx context.Context, node string, vmid int, network graph.Network) (string, []byte, error) {
	ttl := m.TokenTTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	token, err := m.Store.IssueEnrollToken(ctx, node, vmid, "", ttl)
	if err != nil {
		return "", nil, err
	}
	files, err := Files(Params{Node: node, VMID: vmid, User: m.User, SSHPublicKeys: m.SSHPublicKeys, EnrollToken: token,
		MameletBinary: m.MameletBinary, Network: network, Mamelet: m.config(node, vmid)})
	if err != nil {
		return "", nil, err
	}
	img, err := ISO("CIDATA", files)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(img)
	name := fmt.Sprintf("mamela-seed-%s-%d-%s-%s.iso", node, vmid, netTag(network), hex.EncodeToString(sum[:8]))
	if err := m.Store.SetEnrollSeed(ctx, token, name); err != nil {
		return "", nil, err
	}
	return name, img, nil
}
