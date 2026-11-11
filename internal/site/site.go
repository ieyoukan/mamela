// Package site reads the site definition: what Mamela needs to know of where it runs, whatever the
// providers (ADR 0006). Everything about one infrastructure (its API, tokens, pools, storages,
// safety thresholds) belongs to that infrastructure's provider: mamela.providers.toml and the
// declarations.
//
// The site is declared in mamela.site.toml (gitignored; mamela.site.example.toml is the template):
//
//   - the Control Plane that mamelet on the machines reaches;
//   - the login user and SSH keys of the machines;
//   - the network rules (whether DHCP is allowed, the range of static addresses);
//   - the limits per machine (vCPUs, memory, disk) that every provider's create is held to.
package site

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

// Schema is the only accepted value of `schema`.
const Schema = "mamela-site/v1"

// Site is the site definition.
type Site struct {
	Schema  string    `toml:"schema"`
	Guest   GuestInit `toml:"guest"`
	Budget  Budget    `toml:"budget"`
	Network Network   `toml:"network"`
	// ControlPlane is optional: without it, machines get no mamelet.
	ControlPlane ControlPlane `toml:"controlplane"`
}

// ControlPlane is where mamelet on the machines reaches Mamela, and how Mamela serves it.
type ControlPlane struct {
	// URL is how mamelet reaches the Control Plane.
	URL string `toml:"url"`
	// Listen is the address `mamela serve` listens on.
	Listen string `toml:"listen"`
	// StateDir holds the Control Plane's CA and signing keys (outside the repository; "~/" is expanded).
	StateDir string `toml:"state_dir"`
	// MameletBinary is the linux/amd64 build of mamelet the Control Plane serves to new machines
	// (scripts/build-mamelet.sh).
	MameletBinary string `toml:"mamelet_binary"`
	// Host is the machine this Control Plane runs on, if Mamela manages it: plans of this Control
	// Plane never change it (ADR 0003).
	Host string `toml:"host"`
	// MoveTo is the URL of the Control Plane mamelet should move to: revisions carry it, and
	// mamelet uses it from then on (ADR 0003). Empty: revisions name no Control Plane (a mamelet
	// older than ADR 0003 refuses one that does).
	MoveTo string `toml:"move_to"`
}

// Advertised is the URL mamelet reaches the Control Plane at: where it moves to, if it moves.
func (c ControlPlane) Advertised() string {
	if c.MoveTo != "" {
		return c.MoveTo
	}
	return c.URL
}

// Enabled reports whether the site has a Control Plane for mamelet.
func (c ControlPlane) Enabled() bool { return c.URL != "" }

// GuestInit is the login of the machines (their seed).
type GuestInit struct {
	User             string `toml:"user"`
	SSHPublicKeyFile string `toml:"ssh_public_key_file"` // optional; "~/" is expanded
}

// Network says how machines may get their addresses. Both are explicit: a site that forgets the
// section is refused, so that no machine takes an address nobody set aside for it.
type Network struct {
	// DHCP allows machines declared with Dhcp (the network's DHCP server gives the address).
	DHCP *bool `toml:"dhcp"`
	// StaticFirst..StaticLast is the range set aside for static addresses: outside the DHCP
	// server's pool and every address in use. Empty: no static addresses.
	StaticFirst string `toml:"static_first"`
	StaticLast  string `toml:"static_last"`
}

// StaticRange is the static range (invalid addresses when there is none).
func (n Network) StaticRange() (netip.Addr, netip.Addr) {
	first, _ := netip.ParseAddr(n.StaticFirst)
	last, _ := netip.ParseAddr(n.StaticLast)
	return first, last
}

// Allows says whether machine name may use this network, and why not.
func (s *Site) Allows(name string, n graph.Network) error {
	first, last := s.Network.StaticRange()
	if err := n.Allowed(s.Network.DHCP != nil && *s.Network.DHCP, first, last); err != nil {
		return fmt.Errorf("machine %s: %w", name, err)
	}
	return nil
}

// Budget is the most one machine may have, whatever its provider (checked against the measures of
// the host's catalogs, at plan and at apply).
type Budget struct {
	MaxVCPUPerVM      int   `toml:"max_vcpu_per_vm"`
	MaxMemoryMiBPerVM int64 `toml:"max_memory_mib_per_vm"`
	MaxDiskGiBPerVM   int64 `toml:"max_disk_gib_per_vm"`
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Load reads and validates a site file. Unknown keys are errors.
func Load(path string) (*Site, error) {
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
		hint := ""
		if s.Schema == "mamela-lab/v1" {
			hint = " (a mamela-lab/v1 site: its Proxmox settings moved to mamela.providers.toml, see ADR 0006)"
		}
		return nil, fmt.Errorf("%s: unknown keys: %s%s", path, strings.Join(keys, ", "), hint)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// Validate checks the site.
func (s *Site) Validate() error {
	var errs []string
	bad := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if s.Schema != Schema {
		bad("schema must be %q", Schema)
	}
	if !nameRe.MatchString(s.Guest.User) {
		bad("guest.user %q is missing or not a plain name", s.Guest.User)
	}
	bu := s.Budget
	if bu.MaxVCPUPerVM < 1 || bu.MaxMemoryMiBPerVM < 256 || bu.MaxDiskGiBPerVM < 1 {
		bad("budget values must be positive (memory at least 256 MiB)")
	}
	if s.Network.DHCP == nil {
		bad("network.dhcp must be set: true if machines may use the network's DHCP, false if not")
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
	if cp := s.ControlPlane; cp != (ControlPlane{}) {
		u, err := url.Parse(cp.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
			bad("controlplane.url %q must be https://host:port", cp.URL)
		}
		if host, _, err := net.SplitHostPort(cp.Listen); err != nil {
			bad("controlplane.listen %q must be host:port", cp.Listen)
		} else if u != nil && isLoopback(u.Hostname()) && !isLoopback(host) {
			// mamelet reaches a loopback address only through a tunnel: listening on any other
			// address would expose the Control Plane for nothing.
			bad("controlplane.listen %q must be a loopback address while controlplane.url is one", cp.Listen)
		}
		if cp.MoveTo != "" {
			if m, err := url.Parse(cp.MoveTo); err != nil || m.Scheme != "https" || m.Host == "" || m.Path != "" || m.Port() == "" {
				bad("controlplane.move_to %q must be https://host:port", cp.MoveTo)
			}
		}
		if cp.Host != "" && !nameRe.MatchString(cp.Host) {
			bad("controlplane.host %q is not a machine name", cp.Host)
		}
		if cp.StateDir == "" || cp.MameletBinary == "" {
			bad("controlplane.state_dir and controlplane.mamelet_binary are required")
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

// ReadSecret reads a secret file ("~/" is expanded). The file must not be readable by others.
func ReadSecret(path string) (string, error) {
	path, err := ExpandHome(path)
	if err != nil {
		return "", err
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

// SSHPublicKeys reads the machines' SSH public key file ("" when none is configured).
func (s *Site) SSHPublicKeys() (string, error) {
	if s == nil || s.Guest.SSHPublicKeyFile == "" {
		return "", nil
	}
	path, err := ExpandHome(s.Guest.SSHPublicKeyFile)
	if err != nil {
		return "", err
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
