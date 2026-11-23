#!/bin/sh
# Installs mamelet, Mamela's agent, on a machine that exists already, and enrolls it with the
# Control Plane. Written by `mamela controlplane enroll`: read it before you run it.
#
#   node:           ct-web (VMID 7090)
#   this machine:   its hostname (uname -n) must be ct-web
#   Control Plane:  https://10.0.0.45:8443 (this machine must reach it)
#   token:          one-time, for this node and VMID only; issued 2026-11-23 09:30 UTC, good for 1h0m0s
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
#   3. installs /usr/local/sbin/mamelet, /etc/mamelet/config.json, /etc/mamelet/enroll-token, /etc/mamelet/ca.pem
#      and /etc/systemd/system/mamelet.service; systemctl daemon-reload; systemctl enable --now mamelet
#   4. waits up to a minute for mamelet to enroll (it spends the token and deletes its file)
# mamelet then observes this machine and reports to the Control Plane. It changes something only
# when a plan approved on the Control Plane allows it.
#
# The token is the only secret here (the CA certificate and the revision key are public). Keep this
# file private, and delete it once the node has enrolled.
set -eu
umask 077

node='ct-web'
vmid=7090
host='ct-web'
binary_url='https://10.0.0.45:8443/v1/mamelet/d7e7e7f5dab1c90b7610b9c7ae66a9203694aa46caec2055aaa45b6fc39b64ca'
binary_sha256='d7e7e7f5dab1c90b7610b9c7ae66a9203694aa46caec2055aaa45b6fc39b64ca'

r="${MAMELA_INSTALL_ROOT:-}" # a root to install under (tests); empty: /
etc="$r/etc/mamelet"
unit="$r/etc/systemd/system/mamelet.service"
bin="$r/usr/local/sbin/mamelet"
state="$r/var/lib/mamelet"

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
trap 'rm -f "$tmp/mamelet" "$tmp/config.json" "$tmp/enroll-token" "$tmp/mamelet.service" "$tmp/ca.pem"; rmdir "$tmp"' EXIT
trap 'exit 1' HUP INT TERM
cat >"$tmp/config.json" <<'MAMELA_END'
{"node":"ct-web","vmid":7090,"server":"https://10.0.0.45:8443","ca":"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n","revisionKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","enrollTokenFile":"/etc/mamelet/enroll-token","stateDir":"/var/lib/mamelet","intervalSeconds":30,"allowedOrigins":["Debian:"]}
MAMELA_END
cat >"$tmp/enroll-token" <<'MAMELA_END'
QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo
MAMELA_END
cat >"$tmp/mamelet.service" <<'MAMELA_END'
[Unit]
Description=Mamela node agent (mamelet)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/sbin/mamelet -config /etc/mamelet/config.json
Restart=always
RestartSec=10
KillMode=mixed
TimeoutStopSec=180

[Install]
WantedBy=multi-user.target
MAMELA_END
cat >"$tmp/ca.pem" <<'MAMELA_END'
-----BEGIN CERTIFICATE-----
MIIB
-----END CERTIFICATE-----
MAMELA_END
if ! curl -fsS --proto =https --cacert "$tmp/ca.pem" -o "$tmp/mamelet" "$binary_url"; then
	fail "could not download mamelet from $binary_url. Nothing was changed." \
		"  hint: this machine must reach the Control Plane (mamela serve runs there, and no firewall is in between). If controlplane.mamelet_binary was replaced since this script was written, write it again"
fi
echo "$binary_sha256  $tmp/mamelet" | sha256sum -c - >/dev/null 2>&1 ||
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
install -m 0600 "$tmp/config.json" "$etc/config.json"
install -m 0600 "$tmp/enroll-token" "$etc/enroll-token"
install -m 0644 "$tmp/mamelet.service" "$unit"
install -m 0644 "$tmp/ca.pem" "$etc/ca.pem"
systemctl daemon-reload
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
