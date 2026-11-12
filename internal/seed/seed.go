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
	if !noderev.ValidNode(p.Node) || p.VMID <= 0 || p.User == "" || p.EnrollToken == "" || len(p.MameletBinary) == 0 {
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
	sum := sha256.Sum256(p.MameletBinary)
	binSHA := hex.EncodeToString(sum[:])
	keys := p.SSHPublicKeys
	if keys == nil {
		keys = []string{}
	}
	// user-data is JSON, which YAML (and cloud-init) reads as is: no YAML quoting pitfalls. The
	// runcmd lines are fixed text; only the binary's checksum is filled in (hex).
	userData := map[string]any{
		"users": []any{map[string]any{
			"name": p.User, "groups": []string{"sudo"}, "sudo": "ALL=(ALL) NOPASSWD:ALL", "shell": "/bin/bash",
			"lock_passwd": true, "ssh_authorized_keys": keys,
		}},
		"ssh_pwauth":     false,
		"package_update": true,
		"packages":       []string{"qemu-guest-agent"},
		"write_files": []any{
			map[string]any{"path": "/etc/mamelet/config.json", "permissions": "0600", "owner": "root:root", "content": string(cfg)},
			map[string]any{"path": "/etc/mamelet/enroll-token", "permissions": "0600", "owner": "root:root", "content": p.EnrollToken + "\n"},
			map[string]any{"path": "/etc/systemd/system/mamelet.service", "permissions": "0644", "owner": "root:root", "content": unit},
		},
		"runcmd": []any{
			// Install mamelet from this seed, only if it is exactly the binary Mamela put there.
			[]string{"sh", "-c", `set -eu; d=$(mktemp -d); mount -o ro -L CIDATA "$d"; ` +
				`if echo "` + binSHA + `  $d/mamelet" | sha256sum -c -; then install -m 0755 "$d/mamelet" /usr/local/sbin/mamelet; fi; ` +
				`umount "$d"; rmdir "$d"`},
			[]string{"systemctl", "start", "qemu-guest-agent"},
			[]string{"systemctl", "daemon-reload"},
			[]string{"systemctl", "enable", "--now", "mamelet.service"},
		},
	}
	ud, err := json.Marshal(userData, json.Deterministic(true))
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
		{Name: "user-data", Data: append([]byte("#cloud-config\n"), ud...)},
		{Name: "network-config", Data: network},
		{Name: "mamelet", Data: p.MameletBinary},
	}, nil
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
		MameletBinary: m.MameletBinary, Network: network, Mamelet: mamelet.Config{
			Node: node, VMID: vmid, Server: m.Server, CA: string(m.CA), RevisionKey: m.RevisionKey,
			EnrollTokenFile: "/etc/mamelet/enroll-token", StateDir: "/var/lib/mamelet", IntervalSeconds: 30,
			AllowedOrigins: m.AllowedOrigins,
		}})
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
