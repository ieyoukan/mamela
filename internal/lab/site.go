// Package lab defines the test lab boundary on a shared Proxmox cluster and checks it.
//
// The lab is declared in lab/site.local.toml (gitignored; lab/site.example.toml is the template).
// From it, Mamela generates the one-time bootstrap script that a person runs as root, and runs
// read-only preflight and postflight checks around every live test. See docs/lab.md.
package lab

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/graph"
)

// SiteSchema is the only accepted value of `schema`.
const SiteSchema = "mamela-lab/v1"

// Site is the lab definition.
type Site struct {
	Schema    string    `toml:"schema"`
	Proxmox   Proxmox   `toml:"proxmox"`
	Boundary  Boundary  `toml:"boundary"`
	Placement Placement `toml:"placement"`
	Guest     GuestInit `toml:"guest"`
	Budget    Budget    `toml:"budget"`
	Network   Network   `toml:"network"`
	Safety    Safety    `toml:"safety"`
	// ControlPlane is optional: without it, VMs get Proxmox's cloud-init drive and no mamelet.
	ControlPlane ControlPlane `toml:"controlplane"`
}

// ControlPlane is where mamelet in lab VMs reaches Mamela, and how Mamela serves it.
type ControlPlane struct {
	// URL is how mamelet reaches the Control Plane, for example https://127.0.0.1:8443 through an
	// SSH reverse tunnel (mamela lab tunnel).
	URL string `toml:"url"`
	// Listen is the address `mamela serve` listens on (keep it on loopback).
	Listen string `toml:"listen"`
	// StateDir holds the Control Plane's CA and signing keys (outside the repository; "~/" is expanded).
	StateDir string `toml:"state_dir"`
	// MameletBinary is the linux/amd64 build of mamelet put into each seed (scripts/build-mamelet.sh).
	MameletBinary string `toml:"mamelet_binary"`
	// TunnelJump is an optional SSH jump host for `mamela lab tunnel`.
	TunnelJump string `toml:"tunnel_jump"`
	// Host is the VM this Control Plane runs on, if it is one in the lab: plans of this Control
	// Plane never change it (ADR 0003).
	Host string `toml:"host"`
	// MoveTo is the URL of the Control Plane mamelet should move to: revisions carry it, and
	// mamelet uses it from then on (ADR 0003). Empty: revisions name no Control Plane (a mamelet
	// older than ADR 0003 refuses one that does).
	MoveTo string `toml:"move_to"`
}

// Enabled reports whether the site has a Control Plane for mamelet.
func (c ControlPlane) Enabled() bool { return c.URL != "" }

type Proxmox struct {
	APIURL               string `toml:"api_url"`
	Node                 string `toml:"node"`
	TokenID              string `toml:"token_id"`
	TokenSecretFile      string `toml:"token_secret_file"`
	AuditTokenID         string `toml:"audit_token_id"`
	AuditTokenSecretFile string `toml:"audit_token_secret_file"`
	TLSFingerprint       string `toml:"tls_fingerprint"`
	TLSCAFile            string `toml:"tls_ca_file"`
}

type Boundary struct {
	Pool    string `toml:"pool"`
	VMIDMin int    `toml:"vmid_min"`
	VMIDMax int    `toml:"vmid_max"`
	Tag     string `toml:"tag"`
}

type Placement struct {
	DiskStorage string `toml:"disk_storage"`
	// ImageStorage holds the pinned cloud images VMs are created from (content type "import").
	ImageStorage string `toml:"image_storage"`
	Bridge       string `toml:"bridge"`
	SDNZone      string `toml:"sdn_zone"`
}

// GuestInit is the cloud-init configuration of lab VMs.
type GuestInit struct {
	User             string `toml:"user"`
	SSHPublicKeyFile string `toml:"ssh_public_key_file"` // optional; "~/" is expanded
}

// Network says how lab VMs may get their addresses. Both are explicit: a site that forgets the
// section is refused, so that no VM takes an address nobody set aside for the lab.
type Network struct {
	// DHCP allows VMs declared with Dhcp (the LAN's DHCP server gives the address).
	DHCP *bool `toml:"dhcp"`
	// StaticFirst..StaticLast is the range set aside for the lab's static addresses: outside the
	// DHCP server's pool and every address in use. Empty: no static addresses.
	StaticFirst string `toml:"static_first"`
	StaticLast  string `toml:"static_last"`
}

// StaticRange is the static range (invalid addresses when there is none).
func (n Network) StaticRange() (netip.Addr, netip.Addr) {
	first, _ := netip.ParseAddr(n.StaticFirst)
	last, _ := netip.ParseAddr(n.StaticLast)
	return first, last
}

// Allows says whether lab VM name may use this network, and why not.
func (s *Site) Allows(name string, n graph.Network) error {
	first, last := s.Network.StaticRange()
	if err := n.Allowed(s.Network.DHCP != nil && *s.Network.DHCP, first, last); err != nil {
		return fmt.Errorf("VM %s: %w", name, err)
	}
	return nil
}

type Budget struct {
	MaxVMs            int   `toml:"max_vms"`
	MaxVCPUPerVM      int   `toml:"max_vcpu_per_vm"`
	MaxMemoryMiBPerVM int64 `toml:"max_memory_mib_per_vm"`
	MaxDiskGiBPerVM   int64 `toml:"max_disk_gib_per_vm"`
}

type Safety struct {
	MinHostMemAvailableMiB int64 `toml:"min_host_mem_available_mib"`
	MaxThinpoolUsedPercent int   `toml:"max_thinpool_used_percent"`
}

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	tokenRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+@pve![A-Za-z0-9._-]+$`)
	fprintRe = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){31}[0-9A-Fa-f]{2}$`)
)

// LoadSite reads and validates a site file. Unknown keys are errors.
func LoadSite(path string) (*Site, error) {
	var s Site
	md, err := toml.DecodeFile(path, &s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// Validate checks the site for mistakes that would weaken the boundary.
func (s *Site) Validate() error {
	var errs []string
	bad := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if s.Schema != SiteSchema {
		bad("schema must be %q", SiteSchema)
	}
	p := s.Proxmox
	if !strings.HasPrefix(p.APIURL, "https://") {
		bad("proxmox.api_url must start with https://")
	}
	for k, v := range map[string]string{"proxmox.node": p.Node, "boundary.pool": s.Boundary.Pool, "boundary.tag": s.Boundary.Tag,
		"placement.disk_storage": s.Placement.DiskStorage, "placement.image_storage": s.Placement.ImageStorage, "guest.user": s.Guest.User,
		"placement.bridge": s.Placement.Bridge, "placement.sdn_zone": s.Placement.SDNZone} {
		if !nameRe.MatchString(v) {
			bad("%s %q is missing or not a plain name", k, v)
		}
	}
	for k, v := range map[string]string{"proxmox.token_id": p.TokenID, "proxmox.audit_token_id": p.AuditTokenID} {
		if !tokenRe.MatchString(v) {
			bad("%s %q must look like user@pve!name (a @pve user, not root@pam)", k, v)
		}
	}
	opUser, _, _ := strings.Cut(p.TokenID, "!")
	auditUser, _, _ := strings.Cut(p.AuditTokenID, "!")
	if opUser == auditUser {
		// The audit user holds PVEAuditor on /. Sharing a user with the operating token would
		// widen what any token of that user (for example one created without privsep) can do.
		bad("the audit token must belong to a different user than the operating token")
	}
	if p.TokenSecretFile == "" || p.AuditTokenSecretFile == "" {
		bad("token secret files are required (outside the repository)")
	}
	if (p.TLSFingerprint == "") == (p.TLSCAFile == "") {
		bad("set exactly one of proxmox.tls_fingerprint and proxmox.tls_ca_file")
	}
	if p.TLSFingerprint != "" && !fprintRe.MatchString(p.TLSFingerprint) {
		bad("proxmox.tls_fingerprint must be a SHA-256 fingerprint (32 hex bytes separated by ':')")
	}
	b := s.Boundary
	if b.VMIDMin < 100 || b.VMIDMax < b.VMIDMin || b.VMIDMax > 999999999 || b.VMIDMax-b.VMIDMin > 999 {
		bad("boundary.vmid_min..vmid_max must be a range of at most 1000 IDs, from 100 up")
	}
	bu := s.Budget
	if bu.MaxVMs < 1 || bu.MaxVMs > 10 || bu.MaxVCPUPerVM < 1 || bu.MaxMemoryMiBPerVM < 256 || bu.MaxDiskGiBPerVM < 1 {
		bad("budget values must be positive (max_vms 1..10, memory at least 256 MiB)")
	}
	if s.Network.DHCP == nil {
		bad("network.dhcp must be set: true if lab VMs may use the LAN's DHCP, false if not")
	}
	if nw := s.Network; nw.StaticFirst != "" || nw.StaticLast != "" {
		first, last := nw.StaticRange()
		switch {
		case !first.Is4() || !last.Is4():
			bad("network.static_first and static_last must both be IPv4 addresses")
		case last.Less(first):
			bad("network.static_last %s is before static_first %s", last, first)
		case !first.IsPrivate() || !last.IsPrivate():
			bad("network.static_first..static_last must be private addresses (RFC 1918)")
		case rangeSize(first, last) > 256:
			bad("network.static_first..static_last is %d addresses; at most 256", rangeSize(first, last))
		}
	}
	if s.Safety.MinHostMemAvailableMiB < 512 {
		bad("safety.min_host_mem_available_mib must be at least 512")
	}
	if s.Safety.MaxThinpoolUsedPercent < 1 || s.Safety.MaxThinpoolUsedPercent > 90 {
		bad("safety.max_thinpool_used_percent must be 1..90")
	}
	if cp := s.ControlPlane; cp != (ControlPlane{}) {
		u, err := url.Parse(cp.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
			bad("controlplane.url %q must be https://host:port", cp.URL)
		}
		if host, _, err := net.SplitHostPort(cp.Listen); err != nil {
			bad("controlplane.listen %q must be host:port", cp.Listen)
		} else if err == nil && u != nil && isLoopback(u.Hostname()) && !isLoopback(host) {
			// Through a tunnel, mamelet reaches a loopback address: listening on any other would
			// expose the Control Plane for nothing.
			bad("controlplane.listen %q must be a loopback address while controlplane.url is one (mamelet reaches it through the tunnel)", cp.Listen)
		}
		if cp.MoveTo != "" {
			if m, err := url.Parse(cp.MoveTo); err != nil || m.Scheme != "https" || m.Host == "" || m.Path != "" || m.Port() == "" {
				bad("controlplane.move_to %q must be https://host:port", cp.MoveTo)
			}
		}
		if cp.Host != "" && !nameRe.MatchString(cp.Host) {
			bad("controlplane.host %q is not a VM name", cp.Host)
		}
		if cp.StateDir == "" || cp.MameletBinary == "" {
			bad("controlplane.state_dir and controlplane.mamelet_binary are required")
		}
		if cp.TunnelJump != "" && !nameRe.MatchString(cp.TunnelJump) {
			bad("controlplane.tunnel_jump %q is not a plain host name", cp.TunnelJump)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid site:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// rangeSize is the number of IPv4 addresses from first to last.
func rangeSize(first, last netip.Addr) int {
	f, l := first.As4(), last.As4()
	v := func(b [4]byte) int { return int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3]) }
	return v(l) - v(f) + 1
}

// ExpandHome expands a leading "~/".
func ExpandHome(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, path[2:]), nil
}

// TokenUser is the Proxmox user of the operating token ("mamela@pve").
func (s *Site) TokenUser() string { u, _, _ := strings.Cut(s.Proxmox.TokenID, "!"); return u }

// ReadSecret reads a token secret file ("~/" is expanded). The file must not be readable by others.
func ReadSecret(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by group or others (mode %v); run: chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return secret, nil
}

// SSHPublicKeys reads the guest's SSH public key file ("" when none is configured).
func (s *Site) SSHPublicKeys() (string, error) {
	path := s.Guest.SSHPublicKeyFile
	if path == "" {
		return "", nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	keys := strings.TrimSpace(string(b))
	for _, line := range strings.Split(keys, "\n") {
		if strings.Contains(line, "PRIVATE KEY") || !strings.HasPrefix(line, "ssh-") && !strings.HasPrefix(line, "ecdsa-") {
			return "", fmt.Errorf("%s does not look like an SSH public key file", path)
		}
	}
	return keys, nil
}
