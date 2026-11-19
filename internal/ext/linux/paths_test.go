package linux

import (
	"strings"
	"testing"
)

func TestCheckPath(t *testing.T) {
	for _, p := range []string{"/etc/nginx/nginx.conf", "/etc/motd", "/opt/app/config.yaml", "/etc/systemd/system/app.service",
		"/etc/systemd/system/app.service.d/override.conf", "/home/alice/.bashrc", "/srv/www/index.html",
		"/etc/rancher/rke2.yaml", "/etc/passwd.d/x", "/x", "/etc/app/a_b-c+d@e=f:g,h.conf",
		"/etc/sudoers.dist", "/etc/groups.conf", "/" + strings.Repeat("a", 254),
		// A program of one's own under /usr/local, an application's own libraries, the skeleton of new
		// homes (copied as the new user's), a unit of a slice of one's own.
		"/usr/local/bin/app", "/usr/local/sbin/appctl", "/opt/app/lib/libapp.so.1", "/usr/local/lib/app/config.yaml", "/usr/lib/app/app.jar",
		"/etc/skel/.bashrc", "/etc/systemd/system/app.slice", "/etc/systemd/system/app.slice.d/limits.conf",
		"/usr/local/share/app/x", "/etc/apt/apt.conf.dist"} {
		if err := CheckPath(p); err != nil {
			t.Errorf("%s refused: %s", p, err.Message)
		}
	}
	for p, why := range map[string]string{
		"":                                    "empty",
		"etc/motd":                            "not an absolute path",
		"/":                                   "a directory",
		"/etc/":                               "a directory",
		"/etc//motd":                          "not a clean path",
		"/etc/./motd":                         "not a clean path",
		"/etc/../root/x":                      "not a clean path",
		"/etc/a b":                            "a character other than",
		"/etc/a*":                             "a character other than",
		"/etc/a\n":                            "a character other than",
		"/etc/$HOME":                          "a character other than",
		"/" + strings.Repeat("a", 255):        "at most 255",
		"/proc/sys/kernel/x":                  "kernel's view",
		"/sys/x":                              "kernel's view",
		"/dev/sda":                            "devices",
		"/run/app.pid":                        "temporary",
		"/var/run/app.pid":                    "temporary",
		"/boot/grub/grub.cfg":                 "boot loader",
		"/usr/local/sbin/mamelet":             "mamelet itself",
		"/usr/local/sbin/mamelet.prev":        "mamelet itself",
		"/etc/mamelet/config.json":            "mamelet's configuration",
		"/var/lib/mamelet/state.json":         "mamelet's state",
		"/etc/systemd/system/mamelet.service": "mamelet's own unit",
		"/etc/systemd/system/mamelet.service.d/x":    "mamelet's own unit",
		"/etc/rancher/k3s/config.yaml":               "k3s",
		"/etc/rancher/k3s/cluster-token":             "k3s",
		"/var/lib/rancher/k3s/server/manifests/a":    "k3s",
		"/usr/local/bin/k3s":                         "k3s binary",
		"/etc/systemd/system/k3s.service":            "k3s unit",
		"/etc/passwd":                                "users and groups",
		"/etc/shadow-":                               "users and groups",
		"/etc/group":                                 "users and groups",
		"/etc/gshadow":                               "users and groups",
		"/etc/subuid":                                "users and groups",
		"/etc/sudoers":                               "become root",
		"/etc/sudoers.d/90-app":                      "become root",
		"/etc/apt/sources.list.d/mamela-docker.list": "NodeAptRepo",
		"/etc/apt/keyrings/mamela-docker.asc":        "NodeAptRepo",
		"/root/.ssh/authorized_keys":                 "log in",
		"/home/alice/.ssh/authorized_keys2":          "log in",
		"/var/lib/dpkg/status":                       "installed packages",
		// Programs root runs by name that a file of /usr/local would run instead of (the static list:
		// mamelet refuses the others the node has, shadowed).
		"/usr/local/bin/sed":        "first in the PATH",
		"/usr/local/bin/grep":       "first in the PATH",
		"/usr/local/bin/awk":        "first in the PATH",
		"/usr/local/bin/find":       "first in the PATH",
		"/usr/local/bin/cp":         "first in the PATH",
		"/usr/local/bin/mv":         "first in the PATH",
		"/usr/local/bin/chown":      "first in the PATH",
		"/usr/local/bin/mktemp":     "first in the PATH",
		"/usr/local/bin/getent":     "first in the PATH",
		"/usr/local/bin/gzip":       "first in the PATH",
		"/usr/local/bin/install":    "first in the PATH",
		"/usr/local/bin/ln":         "first in the PATH",
		"/usr/local/bin/mkdir":      "first in the PATH",
		"/usr/local/bin/cat":        "first in the PATH",
		"/usr/local/bin/gpgv":       "first in the PATH",
		"/usr/local/sbin/depmod":    "first in the PATH",
		"/usr/local/sbin/run-parts": "first in the PATH",
		// What APT's hooks and the packages' scripts run or read as root.
		"/etc/python3.11/sitecustomize.py":      "sitecustomize",
		"/etc/python3/debian_config":            "sitecustomize",
		"/etc/needrestart/conf.d/50-local.conf": "needrestart",
		"/etc/needrestart/needrestart.conf":     "needrestart",
		"/etc/kernel/postinst.d/zz-local":       "kernel",
		"/etc/initramfs-tools/hooks/local":      "update-initramfs",
		"/etc/initramfs-tools/initramfs.conf":   "update-initramfs",
		"/etc/ca-certificates/update.d/local":   "update-ca-certificates",
	} {
		err := CheckPath(p)
		if err == nil {
			t.Errorf("%q accepted", p)
			continue
		}
		if !strings.Contains(err.Message, why) {
			t.Errorf("%q: %q does not say %q", p, err.Message, why)
		}
	}
	// Every refusal of a place says what to do instead, or why there is nothing to.
	for _, f := range forbidden {
		if f.why == "" {
			t.Errorf("%s: no reason", f.place)
		}
	}
}

func TestParseMode(t *testing.T) {
	for _, m := range []string{"0644", "0600", "0640", "0755", "0000", "0775"} {
		if _, err := ParseMode(m); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
	for m, why := range map[string]string{"644": "four octal", "0648": "four octal", "4755": "setuid", "2755": "setgid", "1777": "sticky",
		"0666": "every user", "0777": "every user", "0646": "every user", "": "four octal", "u+rw": "four octal"} {
		if _, err := ParseMode(m); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %v", m, err)
		}
	}
}

func TestAccounts(t *testing.T) {
	a := parseAccounts("root:x:0:0:root:/root:/bin/bash\n# comment\nbroken\nwww-data:x:33:33::/var/www:/usr/sbin/nologin\nalias:x:33:33::/:/bin/false\n+nis\n",
		"root:x:0:\nwww-data:x:33:\n")
	if a.users["www-data"] != 33 || a.userName(33) != "www-data" || a.userName(1001) != "#1001" || a.groupName(0) != "root" {
		t.Errorf("%+v", a)
	}
	if _, ok := a.users["+nis"]; ok {
		t.Error("a NIS line is a user")
	}
	for _, n := range []string{"root", "www-data", "_apt", "systemd-network"} {
		if !ValidAccount(n) {
			t.Errorf("%s refused", n)
		}
	}
	for _, n := range []string{"", "-x", "Root", "a b", "a:b", "../x", strings.Repeat("a", 33)} {
		if ValidAccount(n) {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestUnitFiles(t *testing.T) {
	for p, want := range map[string]bool{"/etc/systemd/system/app.service": true, "/etc/systemd/system/app.service.d/x.conf": true,
		"/usr/local/lib/systemd/system/a.timer": true, "/etc/systemd/journald.conf": false, "/etc/app.conf": false} {
		if IsUnitFile(p) != want {
			t.Errorf("%s: %v", p, !want)
		}
	}
}
