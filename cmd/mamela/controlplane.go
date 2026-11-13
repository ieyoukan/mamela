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
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/provider/external"
	sitedef "github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/store"
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
	if len(args) > 0 && args[0] == "redirect" {
		return runRedirect(ctx, args[1:], stdout, stderr)
	}
	if len(args) == 0 || args[0] != "bundle" {
		fmt.Fprint(stderr, controlPlaneUsage)
		return 2
	}
	fs := flag.NewFlagSet("mamela controlplane bundle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := addSiteFlag(fs)
	providersFile := addProvidersFlag(fs)
	out := fs.String("out", ".mamela/controlplane-bundle.tar", "the archive to write (mode 0600; it holds tokens and private keys)")
	newURL := fs.String("url", "", "how mamelet reaches the Control Plane on its host, https://<address>:<port> (required)")
	host := fs.String("host", "", "the machine the Control Plane will run on, if Mamela manages it: its plans never change it")
	fs.Usage = func() { fmt.Fprint(stderr, controlPlaneUsage); fs.PrintDefaults() }
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *newURL == "" {
		fs.Usage()
		return 2
	}
	site, err := sitedef.Load(*sitePath)
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
		p, err := sitedef.ExpandHome(path)
		if err != nil {
			return nil, err
		}
		return os.ReadFile(p)
	}

	// The site, as the host sees it: its own URL, listening on its address, its files.
	moved := *site
	moved.ControlPlane.URL = *newURL
	moved.ControlPlane.Listen = net.JoinHostPort("0.0.0.0", u.Port())
	moved.ControlPlane.StateDir = cpVar + "/cp"
	moved.ControlPlane.MameletBinary = cpVar + "/mamelet-linux-amd64"
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
	if err := moved.Validate(); err != nil {
		return report(stderr, fmt.Errorf("the site for the host: %w", err))
	}
	var siteText bytes.Buffer
	fmt.Fprintf(&siteText, "# The site of the Control Plane host, written by `mamela controlplane bundle` on %s.\n", wallClock().Format(time.RFC3339))
	if err := toml.NewEncoder(&siteText).Encode(moved); err != nil {
		return report(stderr, err)
	}
	add("etc/site.toml", 0o600, siteText.Bytes())

	// The providers' APIs (plan and apply run on the host, with its database), with their secrets
	// and catalog files moved to the host's places.
	if _, err := os.Stat(*providersFile); err == nil {
		cfg, err := external.LoadConfig(*providersFile)
		if err != nil {
			return report(stderr, err)
		}
		for name, a := range cfg.Provider {
			for _, f := range []struct {
				path *string
				dest string
			}{{&a.TokenFile, name + "-token"}, {&a.GuardTokenFile, name + "-guard-token"}} {
				if *f.path == "" {
					continue
				}
				secret, err := sitedef.ReadSecret(*f.path)
				if err != nil {
					return report(stderr, fmt.Errorf("provider %s: %w", name, err))
				}
				add("etc/secrets/"+f.dest, 0o600, []byte(secret+"\n"))
				*f.path = cpSecrets + "/" + f.dest
			}
			// A catalog of the provider's Pome comes with the plan (ADR 0008); a file of the
			// operator's own goes into the bundle.
			if a.Catalog != "" && !strings.HasPrefix(a.Catalog, "pome:") {
				b, err := read(a.Catalog)
				if err != nil {
					return report(stderr, fmt.Errorf("provider %s: catalog: %w", name, err))
				}
				add("etc/catalog-"+name+".json", 0o644, b)
				a.Catalog = cpEtc + "/catalog-" + name + ".json"
			}
			cfg.Provider[name] = a
		}
		var text bytes.Buffer
		fmt.Fprintf(&text, "# The providers' APIs of the Control Plane host, written by `mamela controlplane bundle` on %s.\n", wallClock().Format(time.RFC3339))
		if err := toml.NewEncoder(&text).Encode(cfg); err != nil {
			return report(stderr, err)
		}
		add("etc/providers.toml", 0o640, text.Bytes())
	} else if !os.IsNotExist(err) {
		return report(stderr, err)
	}
	stateDir, err := sitedef.ExpandHome(site.ControlPlane.StateDir)
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
	// The destination already knows the redirect revision before agents move there. Otherwise
	// their new serial from the source CP would be rejected by the older bundled database.
	copyStore, err := store.Open(ctx, filepath.Join(tmp, "state.db"), wallClock)
	if err != nil {
		return report(stderr, err)
	}
	_, redirectErr := copyStore.RedirectAgents(ctx, *newURL)
	closeErr := copyStore.Close()
	if redirectErr != nil {
		return report(stderr, redirectErr)
	}
	if closeErr != nil {
		return report(stderr, closeErr)
	}
	dbBytes, err := os.ReadFile(filepath.Join(tmp, "state.db"))
	if err != nil {
		return report(stderr, err)
	}
	add("var/state.db", 0o600, dbBytes)
	add("mamela-serve.service", 0o644, []byte(serveUnit))
	add("mamela-api.service", 0o644, []byte(apiUnit))
	add("mamelacd.service", 0o644, []byte(mamelacdUnit))
	add("install.sh", 0o755, []byte(installScript))

	if err := writeBundle(*out, files); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "wrote %s (mode 0600: it holds the providers' tokens and the Control Plane's private keys)\n\n", *out)
	fmt.Fprintf(stdout, "On the host (Linux, amd64), as root:\n")
	fmt.Fprintf(stdout, "  1. put a linux/amd64 mamela at /usr/local/bin/mamela (GOOS=linux GOARCH=amd64 go build -o mamela ./cmd/mamela)\n")
	fmt.Fprintf(stdout, "  2. mkdir b && tar -xf %s -C b && b/install.sh b   (review install.sh first)\n", filepath.Base(*out))
	fmt.Fprintf(stdout, "  3. stop `mamela serve` here, then: systemctl enable --now mamela-serve\n")
	fmt.Fprintf(stdout, "  4. for gitops (optional): put a linux/amd64 mamelacd at /usr/local/bin/mamelacd, write /etc/mamelacd/mamelacd.toml\n"+
		"     and its GitHub token (docs/gitops.md), then: systemctl enable --now mamela-api mamelacd\n\n")
	fmt.Fprintf(stdout, "Mamelets created from now on reach %s directly. Existing ones move when their revision says so:\n", *newURL)
	fmt.Fprintf(stdout, "once the new endpoint is ready, run: mamela controlplane redirect --db %s --url %s\n", shellQuote(*db), shellQuote(*newURL))
	fmt.Fprintf(stdout, "On the host, plan and apply with --site /etc/mamela/site.toml --providers /etc/mamela/providers.toml --db /var/lib/mamela/state.db.\n")
	fmt.Fprintln(stdout, "\nnote: controlplane.move_to is no longer activated by plan; redirect is an explicit action.")
	return 0
}

func runRedirect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela controlplane redirect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr("MAMELA_DB", defaultDB), "state database")
	url := fs.String("url", "", "new https://host:port endpoint, already serving the same CA and revision key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || !noderev.ValidControlPlane(*url) {
		fmt.Fprintln(stderr, "Usage: mamela controlplane redirect --db <state.db> --url https://host:port")
		return 2
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	n, err := st.RedirectAgents(ctx, *url)
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "redirected %d enrolled agents to %s; they move on their next successful pull\n", n, *url)
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
       mamela controlplane redirect --db <state.db> --url https://<address>:<port>

Packs what a Control Plane host needs (ADR 0003) into one archive: a consistent copy of the state
database, the Control Plane's keys, the site and the providers' APIs (mamela.providers.toml, with
their tokens) rewritten for the host, the mamelet binary, a systemd unit and an install script. The archive holds secrets: copy it only to
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

// apiUnit serves the Control Plane's local API to mamelacd (ADR 0012), on a socket of the group
// mamela-api. /etc/mamela/api.env may set MAMELA_TOOLCHAIN_DIR and EMELA_POME_CACHE. It is
// enabled by hand, with mamelacd.
const apiUnit = `[Unit]
Description=Mamela Control Plane local API (mamela api, for mamelacd)
After=network-online.target
Wants=network-online.target

[Service]
User=mamela-cp
Group=mamela-cp
SupplementaryGroups=mamela-api
WorkingDirectory=/var/lib/mamela
EnvironmentFile=-/etc/mamela/api.env
RuntimeDirectory=mamela
RuntimeDirectoryMode=0755
ExecStart=/usr/local/bin/mamela api --socket /run/mamela/api.sock --socket-group mamela-api --site /etc/mamela/site.toml --db /var/lib/mamela/state.db --providers /etc/mamela/providers.toml
Restart=always
RestartSec=10
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/mamela
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`

// mamelacdUnit follows a repository of declarations (ADR 0010, 0012) as its own user: it holds the
// GitHub token and its database, reaches the Control Plane only through the API's socket, and
// cannot read the Control Plane's configuration, secrets or database. It starts only once the
// operator writes /etc/mamelacd/mamelacd.toml.
const mamelacdUnit = `[Unit]
Description=mamelacd (follows a repository of declarations through the Control Plane's API)
After=network-online.target mamela-api.service
Wants=network-online.target mamela-api.service
ConditionPathExists=/etc/mamelacd/mamelacd.toml

[Service]
User=mamelacd
Group=mamelacd
SupplementaryGroups=mamela-api
StateDirectory=mamelacd
StateDirectoryMode=0700
WorkingDirectory=/var/lib/mamelacd
ExecStart=/usr/local/bin/mamelacd run --config /etc/mamelacd/mamelacd.toml
Restart=always
RestartSec=30
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/var/lib/mamelacd
InaccessiblePaths=-/etc/mamela -/var/lib/mamela

[Install]
WantedBy=multi-user.target
`

// installScript places the bundle's files on the host. It is short on purpose: review it first.
const installScript = `#!/usr/bin/env bash
# Installs a Mamela Control Plane bundle (written by ` + "`mamela controlplane bundle`" + `). Run as root,
# with the directory the bundle was extracted to. It does not start anything.
set -euo pipefail
b="${1:?usage: install.sh <extracted bundle directory>}"
r="${MAMELA_INSTALL_ROOT:-}" # a root to install under (tests); empty: /
test -x $r/usr/local/bin/mamela || { echo "put a linux/amd64 mamela at $r/usr/local/bin/mamela first" >&2; exit 1; }
# Refuse before writing anything when this host has a Control Plane already: installing another
# bundle over it would mix its database with other keys, tokens or site.
for existing in $r/var/lib/mamela/state.db $r/var/lib/mamela/cp $r/etc/mamela/site.toml $r/etc/mamela/providers.toml $r/etc/mamela/secrets $r/etc/systemd/system/mamela-serve.service; do
  if test -e "$existing"; then
    echo "$existing exists: this host has a Control Plane already; nothing was changed (move it away to install this bundle)" >&2
    exit 1
  fi
done
# A user of its own, never a login user: the login user of the machines is often "mamela", and may sudo.
id mamela-cp >/dev/null 2>&1 || useradd --system --user-group --home-dir /var/lib/mamela --shell /usr/sbin/nologin mamela-cp
install -d -m 0750 -o root -g mamela-cp $r/etc/mamela
install -d -m 0750 -o root -g mamela-cp $r/etc/mamela/secrets
install -d -m 0700 -o mamela-cp -g mamela-cp $r/var/lib/mamela $r/var/lib/mamela/cp
for f in "$b"/etc/secrets/*; do if test -f "$f"; then install -m 0600 -o mamela-cp -g mamela-cp "$f" $r/etc/mamela/secrets/; fi; done
install -m 0640 -o root -g mamela-cp "$b/etc/site.toml" $r/etc/mamela/site.toml
if test -f "$b/etc/providers.toml"; then install -m 0640 -o root -g mamela-cp "$b/etc/providers.toml" $r/etc/mamela/providers.toml; fi
for f in "$b"/etc/guest-ssh.pub "$b"/etc/catalog-*.json; do
  if test -f "$f"; then install -m 0644 -o root -g root "$f" $r/etc/mamela/; fi
done
for f in "$b"/var/cp/*; do install -m 0600 -o mamela-cp -g mamela-cp "$f" $r/var/lib/mamela/cp/; done
install -m 0755 -o root -g root "$b/var/mamelet-linux-amd64" $r/var/lib/mamela/mamelet-linux-amd64
install -m 0600 -o mamela-cp -g mamela-cp "$b/var/state.db" $r/var/lib/mamela/state.db
install -m 0644 -o root -g root "$b/mamela-serve.service" $r/etc/systemd/system/mamela-serve.service
# mamelacd (ADR 0012): a user of its own, which reaches the Control Plane only through the API's
# socket (group mamela-api) and cannot read /etc/mamela or /var/lib/mamela.
getent group mamela-api >/dev/null 2>&1 || groupadd --system mamela-api
id mamelacd >/dev/null 2>&1 || useradd --system --user-group --groups mamela-api --home-dir /var/lib/mamelacd --shell /usr/sbin/nologin mamelacd
install -d -m 0750 -o root -g mamelacd $r/etc/mamelacd
for u in mamela-api.service mamelacd.service; do
  if test -f "$b/$u"; then install -m 0644 -o root -g root "$b/$u" $r/etc/systemd/system/$u; fi
done
systemctl daemon-reload
echo "installed; start it with: systemctl enable --now mamela-serve (gitops: mamela-api and mamelacd, see docs/gitops.md)"
`
