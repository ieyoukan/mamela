package main

import (
	"archive/tar"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/lab"
)

// Where a Control Plane host keeps what the bundle holds (ADR 0003).
const (
	cpEtc     = "/etc/mamela"
	cpVar     = "/var/lib/mamela"
	cpSecrets = cpEtc + "/secrets"
)

// runControlPlane moves the Control Plane to its own host (ADR 0003): `bundle` packs what that host
// needs into one archive for a person to copy there and install with the script inside.
func runControlPlane(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "bundle" {
		fmt.Fprint(stderr, controlPlaneUsage)
		return 2
	}
	fs := flag.NewFlagSet("mamela controlplane bundle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := fs.String("site", envOr("MAMELA_LAB_SITE", "lab/site.local.toml"), "lab site definition")
	out := fs.String("out", ".mamela/controlplane-bundle.tar", "the archive to write (mode 0600; it holds tokens and private keys)")
	newURL := fs.String("url", "", "how mamelet reaches the Control Plane on its host, https://<address>:<port> (required)")
	host := fs.String("host", "", "the lab VM the Control Plane will run on, if it is one: its plans never change it")
	fs.Usage = func() { fmt.Fprint(stderr, controlPlaneUsage); fs.PrintDefaults() }
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *newURL == "" {
		fs.Usage()
		return 2
	}
	site, err := lab.LoadSite(*sitePath)
	if err != nil {
		return report(stderr, err)
	}
	if !site.ControlPlane.Enabled() {
		return report(stderr, fmt.Errorf("the site has no [controlplane] section"))
	}
	u, err := url.Parse(*newURL)
	if err != nil || u.Scheme != "https" || u.Port() == "" || u.Path != "" || isLoopbackHost(u.Hostname()) {
		return report(stderr, fmt.Errorf("--url %q must be https://<address>:<port> of the host, not a loopback address", *newURL))
	}

	files := map[string]bundleFile{}
	add := func(name string, mode int64, data []byte) { files[name] = bundleFile{mode: mode, data: data} }
	read := func(path string) ([]byte, error) {
		p, err := lab.ExpandHome(path)
		if err != nil {
			return nil, err
		}
		return os.ReadFile(p)
	}

	// The site, as the host sees it: its own URL, listening on its address, its files.
	moved := *site
	moved.Proxmox.TokenSecretFile = cpSecrets + "/lab-token"
	moved.Proxmox.AuditTokenSecretFile = cpSecrets + "/audit-token"
	moved.ControlPlane.URL = *newURL
	moved.ControlPlane.Listen = net.JoinHostPort("0.0.0.0", u.Port())
	moved.ControlPlane.StateDir = cpVar + "/cp"
	moved.ControlPlane.MameletBinary = cpVar + "/mamelet-linux-amd64"
	moved.ControlPlane.TunnelJump = ""
	moved.ControlPlane.MoveTo = ""
	moved.ControlPlane.Host = *host
	if site.Guest.SSHPublicKeyFile != "" {
		moved.Guest.SSHPublicKeyFile = cpEtc + "/guest-ssh.pub"
		b, err := read(site.Guest.SSHPublicKeyFile)
		if err != nil {
			return report(stderr, err)
		}
		add("etc/guest-ssh.pub", 0o644, b)
	}
	if site.Proxmox.TLSCAFile != "" {
		moved.Proxmox.TLSCAFile = cpEtc + "/proxmox-ca.pem"
		b, err := read(site.Proxmox.TLSCAFile)
		if err != nil {
			return report(stderr, err)
		}
		add("etc/proxmox-ca.pem", 0o644, b)
	}
	if err := moved.Validate(); err != nil {
		return report(stderr, fmt.Errorf("the site for the host: %w", err))
	}
	var siteText bytes.Buffer
	fmt.Fprintf(&siteText, "# The lab site of the Control Plane host, written by `mamela controlplane bundle` on %s.\n", wallClock().Format(time.RFC3339))
	if err := toml.NewEncoder(&siteText).Encode(moved); err != nil {
		return report(stderr, err)
	}
	add("etc/site.toml", 0o600, siteText.Bytes())

	for name, path := range map[string]string{"secrets/lab-token": site.Proxmox.TokenSecretFile, "secrets/audit-token": site.Proxmox.AuditTokenSecretFile} {
		secret, err := lab.ReadSecret(path)
		if err != nil {
			return report(stderr, err)
		}
		add("etc/"+name, 0o600, []byte(secret+"\n"))
	}
	stateDir, err := lab.ExpandHome(site.ControlPlane.StateDir)
	if err != nil {
		return report(stderr, err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return report(stderr, err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(stateDir, e.Name()))
		if err != nil {
			return report(stderr, err)
		}
		add("var/cp/"+e.Name(), 0o600, b)
	}
	bin, err := read(site.ControlPlane.MameletBinary)
	if err != nil {
		return report(stderr, err)
	}
	add("var/mamelet-linux-amd64", 0o755, bin)

	// The database last, so that it is the newest of all.
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	tmp, err := os.MkdirTemp("", "mamela-bundle-")
	if err != nil {
		return report(stderr, err)
	}
	defer os.RemoveAll(tmp)
	copyErr := st.CopyTo(ctx, filepath.Join(tmp, "state.db"))
	st.Close()
	if copyErr != nil {
		return report(stderr, fmt.Errorf("copy the database: %w", copyErr))
	}
	dbBytes, err := os.ReadFile(filepath.Join(tmp, "state.db"))
	if err != nil {
		return report(stderr, err)
	}
	add("var/state.db", 0o600, dbBytes)
	add("mamela-serve.service", 0o644, []byte(serveUnit))
	add("install.sh", 0o755, []byte(installScript))

	if err := writeBundle(*out, files); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "wrote %s (mode 0600: it holds the Proxmox tokens and the Control Plane's private keys)\n\n", *out)
	fmt.Fprintf(stdout, "On the host (Linux, amd64), as root:\n")
	fmt.Fprintf(stdout, "  1. put a linux/amd64 mamela at /usr/local/bin/mamela (GOOS=linux GOARCH=amd64 go build -o mamela ./cmd/mamela)\n")
	fmt.Fprintf(stdout, "  2. mkdir b && tar -xf %s -C b && b/install.sh b   (review install.sh first)\n", filepath.Base(*out))
	fmt.Fprintf(stdout, "  3. stop `mamela serve` here, then: systemctl enable --now mamela-serve\n\n")
	fmt.Fprintf(stdout, "Mamelets created from now on reach %s directly. Existing ones move when their revision says so:\n", *newURL)
	fmt.Fprintf(stdout, "set controlplane.move_to = %q here and plan --site once before making this bundle (ADR 0003).\n", *newURL)
	if site.ControlPlane.MoveTo != *newURL {
		fmt.Fprintf(stdout, "\nnote: controlplane.move_to is %q here, not %q: existing mamelets have not been told to move.\n", site.ControlPlane.MoveTo, *newURL)
	}
	return 0
}

type bundleFile struct {
	mode int64
	data []byte
}

func writeBundle(path string, files map[string]bundleFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		bf := files[n]
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: bf.mode, Size: int64(len(bf.data)), Typeflag: tar.TypeReg}); err != nil {
			f.Close()
			return err
		}
		if _, err := tw.Write(bf.data); err != nil {
			f.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

const controlPlaneUsage = `Usage: mamela controlplane bundle --url https://<address>:<port> [--host <vm>] [flags]

Packs what a Control Plane host needs (ADR 0003) into one archive: a consistent copy of the state
database, the Control Plane's keys, the Proxmox tokens, the lab site rewritten for the host, the
mamelet binary, a systemd unit and an install script. The archive holds secrets: copy it only to
the host, over SSH, and delete it afterwards.

Flags:
`

// serveUnit runs the Control Plane on its host, as an unprivileged user.
const serveUnit = `[Unit]
Description=Mamela Control Plane (mamela serve)
After=network-online.target
Wants=network-online.target

[Service]
User=mamela-cp
Group=mamela-cp
WorkingDirectory=/var/lib/mamela
ExecStart=/usr/local/bin/mamela serve --site /etc/mamela/site.toml --db /var/lib/mamela/state.db
Restart=always
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/mamela
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`

// installScript places the bundle's files on the host. It is short on purpose: review it first.
const installScript = `#!/usr/bin/env bash
# Installs a Mamela Control Plane bundle (written by ` + "`mamela controlplane bundle`" + `). Run as root,
# with the directory the bundle was extracted to. It does not start anything.
set -euo pipefail
b="${1:?usage: install.sh <extracted bundle directory>}"
test -x /usr/local/bin/mamela || { echo "put a linux/amd64 mamela at /usr/local/bin/mamela first" >&2; exit 1; }
# A user of its own, never a login user: the guest login user of lab VMs is often "mamela", and may sudo.
id mamela-cp >/dev/null 2>&1 || useradd --system --user-group --home-dir /var/lib/mamela --shell /usr/sbin/nologin mamela-cp
install -d -m 0750 -o root -g mamela-cp /etc/mamela
install -d -m 0750 -o root -g mamela-cp /etc/mamela/secrets
install -d -m 0700 -o mamela-cp -g mamela-cp /var/lib/mamela /var/lib/mamela/cp
for f in "$b"/etc/secrets/*; do install -m 0600 -o mamela-cp -g mamela-cp "$f" /etc/mamela/secrets/; done
install -m 0640 -o root -g mamela-cp "$b/etc/site.toml" /etc/mamela/site.toml
for f in "$b"/etc/guest-ssh.pub "$b"/etc/proxmox-ca.pem; do
  if test -f "$f"; then install -m 0644 -o root -g root "$f" /etc/mamela/; fi
done
for f in "$b"/var/cp/*; do install -m 0600 -o mamela-cp -g mamela-cp "$f" /var/lib/mamela/cp/; done
install -m 0755 -o root -g root "$b/var/mamelet-linux-amd64" /var/lib/mamela/mamelet-linux-amd64
if test -e /var/lib/mamela/state.db; then
  echo "/var/lib/mamela/state.db exists; not overwriting it (move it away to install this bundle's)" >&2
  exit 1
fi
install -m 0600 -o mamela-cp -g mamela-cp "$b/var/state.db" /var/lib/mamela/state.db
install -m 0644 -o root -g root "$b/mamela-serve.service" /etc/systemd/system/mamela-serve.service
systemctl daemon-reload
echo "installed; start it with: systemctl enable --now mamela-serve"
`
