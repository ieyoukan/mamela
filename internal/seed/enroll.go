package seed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// EnrollParams is what the enrollment script of a machine that exists already holds: the machine was
// not made by Mamela, so it has no seed, and a person runs the script on it as root.
type EnrollParams struct {
	Mamelet      mamelet.Config // mamelet's configuration, as a seed has it
	EnrollToken  string
	Hostname     string // the script runs only on a machine of this hostname (uname -n)
	BinaryURL    string // where the Control Plane serves mamelet: /v1/mamelet/<sha256>
	BinarySHA256 string
	Issued       time.Time     // when the token was issued (for the person who reads the script)
	TTL          time.Duration // how long the token is good
}

// heredocEnd ends each file in the script (a here-document that expands nothing).
const heredocEnd = "MAMELA_END"

var (
	hostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	tokenRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// check checks what goes into the script, but the token (withToken false: before it is issued).
func (p EnrollParams) check(withToken bool) error {
	switch {
	case !noderev.ValidNode(p.Mamelet.Node) || p.Mamelet.VMID <= 0:
		return fmt.Errorf("seed: node %q and VMID %d are not a node's", p.Mamelet.Node, p.Mamelet.VMID)
	case !hostnameRe.MatchString(p.Hostname):
		return fmt.Errorf("seed: %q is not a hostname", p.Hostname)
	case !sha256Re.MatchString(p.BinarySHA256):
		return errors.New("seed: mamelet's sha256 must be 64 lowercase hex digits")
	case p.Mamelet.CA == "" || p.TTL <= 0 || p.Issued.IsZero():
		return errors.New("seed: the CA, the token's issue time and how long it is good are required")
	case withToken && !tokenRe.MatchString(p.EnrollToken):
		return errors.New("seed: the enrollment token is not one Mamela issues")
	case !strings.HasSuffix(p.BinaryURL, "/v1/mamelet/"+p.BinarySHA256):
		return errors.New("seed: mamelet's URL must be the Control Plane's /v1/mamelet/<its sha256>")
	}
	if p.Mamelet.EnrollTokenFile != TokenPath || p.Mamelet.StateDir != StateDir {
		return fmt.Errorf("seed: the script writes the token to %s and waits for mamelet's certificate in %s, as the configuration must say", TokenPath, StateDir)
	}
	return checkBinary(p.BinaryURL, p.BinarySHA256)
}

// EnrollScript is the script that installs mamelet on a machine that exists already (one Mamela did
// not make: an adopted container or VM) and enrolls it, as a seed does for a machine Mamela makes:
// the same files (mameletFiles), the same binary from the Control Plane checked by its sha256, the
// same unit. The Control Plane never logs into the machine: a person reads the script and runs it
// as root there. It is POSIX sh. It refuses, before it changes anything, when it is not root, not on
// the machine of the hostname, not under systemd, when mamelet is there already (unless it is run
// with --reenroll), when curl is missing, and when the binary cannot be fetched or is not the one
// named. Its only secret is the one-time token.
func EnrollScript(p EnrollParams) ([]byte, error) {
	if err := p.check(true); err != nil {
		return nil, err
	}
	files, err := mameletFiles(p.Mamelet, p.EnrollToken)
	if err != nil {
		return nil, err
	}
	node, vmid := p.Mamelet.Node, p.Mamelet.VMID
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w(`#!/bin/sh
# Installs mamelet, Mamela's agent, on a machine that exists already, and enrolls it with the
# Control Plane. Written by `+"`mamela controlplane enroll`"+`: read it before you run it.
#
#   node:           %s (VMID %d)
#   this machine:   its hostname (uname -n) must be %s
#   Control Plane:  %s (this machine must reach it)
#   token:          one-time, for this node and VMID only; issued %s, good for %s
#
# Run it as root on that machine:
#
#   sh mamelet-enroll.sh              install mamelet and enroll it (refuses if mamelet is there already)
#   sh mamelet-enroll.sh --reenroll   replace the files of a mamelet that is there, and enroll it again
#
# What it does, in this order, and nothing else (no package, user or network setting):
#   1. checks that it runs as root, on this hostname, under systemd, that mamelet is not there
#      (unless --reenroll), and that curl is there
#   2. writes the files below to a temporary directory, downloads mamelet from the Control Plane over
#      https (only from a server that Mamela's CA below vouches for) and checks its sha256. Until
#      here, nothing on the machine is changed
#   3. installs %s, %s, %s, %s
#      and %s; systemctl daemon-reload; systemctl enable --now mamelet
#   4. waits up to a minute for mamelet to enroll (it spends the token and deletes its file)
# mamelet then observes this machine and reports to the Control Plane. It changes something only
# when a plan approved on the Control Plane allows it.
#
# The token is the only secret here (the CA certificate and the revision key are public). Keep this
# file private, and delete it once the node has enrolled.
set -eu
umask 077

node='%s'
vmid=%d
host='%s'
binary_url='%s'
binary_sha256='%s'

r="${MAMELA_INSTALL_ROOT:-}" # a root to install under (tests); empty: /
etc="$r%s"
unit="$r%s"
bin="$r%s"
state="$r%s"
`, node, vmid, p.Hostname, p.Mamelet.Server, p.Issued.UTC().Format("2006-01-02 15:04 MST"), p.TTL,
		BinaryPath, ConfigPath, TokenPath, CAPath, UnitPath,
		node, vmid, p.Hostname, p.BinaryURL, p.BinarySHA256, mameletEtc, UnitPath, BinaryPath, StateDir)

	b.WriteString(`
fail() {
	printf 'mamelet-enroll: %s\n' "$1" >&2
	shift
	[ $# -eq 0 ] || printf '%s\n' "$@" >&2
	exit 1
}

reenroll=no
case $# in
0) ;;
1) [ "$1" = --reenroll ] || fail "usage: sh $0 [--reenroll]"; reenroll=yes ;;
*) fail "usage: sh $0 [--reenroll]" ;;
esac

# 1. Checks. Nothing is changed before they all pass.
[ "$(id -u)" = 0 ] || fail "run it as root. Nothing was changed."
here=$(uname -n)
[ "$here" = "$host" ] || fail "this machine is $here, not $host: the token is for node $node (VMID $vmid). Nothing was changed." \
	"  hint: run it on $host. If this is that machine and its hostname differs, write the script again with --hostname $here"
[ -d "$r/run/systemd/system" ] || fail "systemd does not run this machine, and mamelet runs as a systemd service. Nothing was changed."
if [ "$reenroll" = no ]; then
	for p in "$etc" "$unit" "$bin"; do
		[ ! -e "$p" ] || fail "mamelet is here already ($p exists). Nothing was changed." \
			"  hint: if its certificate ended, use mamela controlplane reenroll (docs/recovery.md). To replace its files and enroll it again with this token: sh $0 --reenroll"
	done
fi
command -v curl >/dev/null 2>&1 || fail "curl is not installed; it downloads mamelet from the Control Plane. Nothing was changed." \
	"  hint: apt-get install curl, then run this again"

# 2. The files, in a temporary directory first.
tmp=$(mktemp -d)
trap 'rm -f "$tmp/mamelet"`)
	for _, f := range files {
		fmt.Fprintf(&b, ` "$tmp/%s"`, path.Base(f.Path))
	}
	b.WriteString(`; rmdir "$tmp"' EXIT
trap 'exit 1' HUP INT TERM
`)
	for _, f := range files {
		content := f.Content
		if !strings.HasSuffix(content, "\n") {
			content += "\n" // a here-document ends with a newline (config.json: whitespace after the JSON)
		}
		for _, line := range strings.Split(content, "\n") {
			if line == heredocEnd {
				return nil, fmt.Errorf("seed: %s has a line %s", f.Path, heredocEnd)
			}
		}
		fmt.Fprintf(&b, "cat >\"$tmp/%s\" <<'%s'\n%s%s\n", path.Base(f.Path), heredocEnd, content, heredocEnd)
	}
	fmt.Fprintf(&b, `if ! %s; then
	fail "could not download mamelet from $binary_url. Nothing was changed." \
		"  hint: this machine must reach the Control Plane (mamela serve runs there, and no firewall is in between). If controlplane.mamelet_binary was replaced since this script was written, write it again"
fi
`, fetch(`"$tmp/ca.pem"`, `"$tmp/mamelet"`, `"$binary_url"`))
	b.WriteString(`echo "$binary_sha256  $tmp/mamelet" | sha256sum -c - >/dev/null 2>&1 ||
	fail "the mamelet downloaded is not the one this script names (sha256 $binary_sha256). Nothing was changed."
echo "mamelet-enroll: downloaded mamelet $binary_sha256"

# 3. Install.
if [ "$reenroll" = yes ]; then
	systemctl stop mamelet.service 2>/dev/null || true
	rm -f "$state/cert.pem" # it enrolls again; key.pem is replaced when it does
fi
[ -d "${bin%/*}" ] || install -d -m 0755 "${bin%/*}"
[ -d "${unit%/*}" ] || install -d -m 0755 "${unit%/*}"
install -d -m 0700 "$etc"
install -m 0755 "$tmp/mamelet" "$bin"
`)
	for _, f := range files {
		dest := `"$unit"`
		if f.Path != UnitPath {
			if path.Dir(f.Path) != mameletEtc {
				return nil, fmt.Errorf("seed: %s is not in %s", f.Path, mameletEtc)
			}
			dest = `"$etc/` + path.Base(f.Path) + `"`
		}
		fmt.Fprintf(&b, "install -m %s \"$tmp/%s\" %s\n", f.Mode, path.Base(f.Path), dest)
	}
	b.WriteString(`systemctl daemon-reload
systemctl enable --now mamelet.service

# 4. mamelet enrolls: it gets its certificate (and deletes the token's file).
i=0
while [ ! -e "$state/cert.pem" ]; do
	i=$((i + 1))
	[ "$i" -le 30 ] || fail "mamelet is installed and started, but has not enrolled within a minute. It tries again every 30 seconds: do not run this script again as it is." \
		"  hint: journalctl -u mamelet says why. The Control Plane unreachable: open the way to it, and mamelet enrolls by itself. The token used or ended: write a new script on the Control Plane's host (mamela controlplane enroll) and run it here with --reenroll"
	sleep 2
done
echo "mamelet-enroll: enrolled as $node (VMID $vmid). mamelet observes this machine and reports to the Control Plane."
echo "  confirm here: journalctl -u mamelet ('enrolled as $node', then 'connected to the Control Plane')"
echo "  and on the Control Plane's host: mamela controlplane agents (lists $node)"
echo "  then delete this script: rm $0"
`)
	return []byte(b.String()), nil
}

// BinaryURL is where the Control Plane at advertised serves the maker's mamelet (by its sha256).
func (m *Maker) BinaryURL(advertised string) string {
	sum := sha256.Sum256(m.MameletBinary)
	return strings.TrimRight(advertised, "/") + "/v1/mamelet/" + hex.EncodeToString(sum[:])
}

// NewEnrollScript issues a one-time token for node (vmid), good for ttl, and makes the enrollment
// script of that machine with it (EnrollScript). The script fetches mamelet from the Control Plane
// at advertised. What goes into it is checked before the token is issued.
func (m *Maker) NewEnrollScript(ctx context.Context, node string, vmid int, hostname, advertised string, ttl time.Duration, now time.Time) ([]byte, error) {
	if m.Plain {
		return nil, errors.New("seed: a site without a Control Plane enrolls no mamelet")
	}
	sum := sha256.Sum256(m.MameletBinary)
	p := EnrollParams{Mamelet: m.config(node, vmid), Hostname: hostname, BinaryURL: m.BinaryURL(advertised),
		BinarySHA256: hex.EncodeToString(sum[:]), Issued: now, TTL: ttl}
	if err := p.check(false); err != nil {
		return nil, err
	}
	token, err := m.Store.IssueEnrollToken(ctx, node, vmid, "", ttl)
	if err != nil {
		return nil, err
	}
	p.EnrollToken = token
	return EnrollScript(p)
}
