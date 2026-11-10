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
	MameletBinary []byte // linux/amd64
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
	network, err := json.Marshal(map[string]any{"version": 2, "ethernets": map[string]any{
		"primary": map[string]any{"match": map[string]string{"name": "e*"}, "dhcp4": true}}}, json.Deterministic(true))
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
func (m *Maker) Reusable(ctx context.Context, node string, vmid int) (string, bool, error) {
	return m.Store.ReusableSeed(ctx, node, vmid, time.Hour)
}

// New makes a seed with a fresh one-time token. The file name contains the image's checksum.
func (m *Maker) New(ctx context.Context, node string, vmid int) (string, []byte, error) {
	ttl := m.TokenTTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	token, err := m.Store.IssueEnrollToken(ctx, node, vmid, "", ttl)
	if err != nil {
		return "", nil, err
	}
	files, err := Files(Params{Node: node, VMID: vmid, User: m.User, SSHPublicKeys: m.SSHPublicKeys, EnrollToken: token,
		MameletBinary: m.MameletBinary, Mamelet: mamelet.Config{
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
	name := fmt.Sprintf("mamela-seed-%s-%d-%s.iso", node, vmid, hex.EncodeToString(sum[:8]))
	if err := m.Store.SetEnrollSeed(ctx, token, name); err != nil {
		return "", nil, err
	}
	return name, img, nil
}
