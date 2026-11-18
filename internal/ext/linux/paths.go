package linux

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/mamelet"
)

// MaxPath bounds a path, in bytes.
const MaxPath = 255

// PathError says why a path is not one Mamela writes, and what to do instead.
type PathError struct {
	Path    string
	Message string // what is wrong with it, and why
	Hint    string // what to use instead
}

func (e *PathError) Error() string { return e.Message }

// A component of a path: letters, digits and . _ + @ = : , - (no space, no shell's or glob's
// character, no control character).
var componentRe = regexp.MustCompile(`^[A-Za-z0-9._+@=:,-]+$`)

// forbidden are the places no kind of linux writes: the kernel's and the boot's, mamelet's own, k3s's,
// what another kind (or the core) declares, what changes the programs mamelet runs as root (the
// dynamic linker's, APT's and dpkg's configuration, systemd's settings of every service), and the
// directories every user may write to. It is not every place a root file matters (a declaration
// approved as shown may write a unit that runs anything); it is those whose effect an approver would
// not see in the file itself. Each says why and what to use instead. A path is in
// one when it is the place or under it (dir), or when it is the place exactly (file); prefix: its
// last component starts with the name.
var forbidden = []struct {
	place string
	how   string // "dir", "file" or "prefix"
	why   string
	hint  string
}{
	{"/proc", "dir", "it is the kernel's view of processes, not a file system", ""},
	{"/sys", "dir", "it is the kernel's view of devices and settings, not a file system", "settings of the kernel go in /etc/sysctl.d"},
	{"/dev", "dir", "it holds devices", ""},
	{"/run", "dir", "it is a temporary file system that is empty after each boot", "a service's runtime files are its own (RuntimeDirectory= of its unit)"},
	{"/var/run", "dir", "it is /run, a temporary file system that is empty after each boot", "a service's runtime files are its own (RuntimeDirectory= of its unit)"},
	{"/var/lock", "dir", "it is /run/lock, a temporary file system", ""},
	{"/boot", "dir", "the boot loader and kernels are the system's packages'", "install a kernel or boot loader as a package (NodePackage)"},
	{"/usr/local/sbin/mamelet", "prefix", "it is mamelet itself", "mamelet is updated with NodeMamelet (Mamela.mamelet)"},
	{"/etc/mamelet", "dir", "it is mamelet's configuration and keys", "mamelet's configuration is written when its machine is made"},
	{"/var/lib/mamelet", "dir", "it is mamelet's state", ""},
	{"/etc/rancher/k3s", "dir", "it is k3s's configuration and join token, which the k3s extension writes", "declare k3s with K3s.server or K3s.agent"},
	{"/var/lib/rancher/k3s", "dir", "it is k3s's data and its manifests", "a manifest of k3s is K3s.manifest"},
	{"/usr/local/bin/k3s", "file", "it is the k3s binary, which the k3s extension installs", "declare k3s with K3s.server or K3s.agent"},
	{"/etc/passwd", "file", "users and groups are not files to write", "declare users with NodeUser (the linux extension)"},
	{"/etc/passwd-", "file", "users and groups are not files to write", "declare users with NodeUser (the linux extension)"},
	{"/etc/shadow", "file", "users and groups are not files to write", "declare users with NodeUser (the linux extension)"},
	{"/etc/shadow-", "file", "users and groups are not files to write", "declare users with NodeUser (the linux extension)"},
	{"/etc/group", "file", "users and groups are not files to write", "declare users and their groups with NodeUser (the linux extension)"},
	{"/etc/group-", "file", "users and groups are not files to write", "declare users and their groups with NodeUser (the linux extension)"},
	{"/etc/gshadow", "file", "users and groups are not files to write", "declare users and their groups with NodeUser (the linux extension)"},
	{"/etc/gshadow-", "file", "users and groups are not files to write", "declare users and their groups with NodeUser (the linux extension)"},
	{"/etc/subuid", "file", "users and groups are not files to write", "declare users with NodeUser (the linux extension)"},
	{"/etc/subuid-", "file", "users and groups are not files to write", "declare users with NodeUser (the linux extension)"},
	{"/etc/subgid", "file", "users and groups are not files to write", "declare users with NodeUser (the linux extension)"},
	{"/etc/subgid-", "file", "users and groups are not files to write", "declare users with NodeUser (the linux extension)"},
	{"/etc/sudoers", "file", "who may become root is not a file to write", sudoHint},
	{"/etc/sudoers.d", "dir", "who may become root is not a file to write", sudoHint},
	{"/etc/apt/sources.list.d/mamela-", "prefix", "it is where NodeAptRepo puts the APT sources it declares", "declare the APT source with NodeAptRepo (the linux extension), or name the file otherwise"},
	{"/etc/apt/keyrings/mamela-", "prefix", "it is where NodeAptRepo puts the keys of the APT sources it declares", "declare the APT source with NodeAptRepo (the linux extension), or name the file otherwise"},
	{"/var/lib/dpkg", "dir", "it is the database of the installed packages", "install packages with NodePackage"},
	// What every program mamelet runs (and their children) reads: a file here runs as root at once.
	{"/etc/ld.so.preload", "file", "the dynamic linker loads what it names into every program, mamelet and what it runs included", ""},
	{"/etc/ld.so.conf", "file", "it says where every program finds its libraries, mamelet's programs included", "install libraries as a package (NodePackage)"},
	{"/etc/ld.so.conf.d", "dir", "it says where every program finds its libraries, mamelet's programs included", "install libraries as a package (NodePackage)"},
	{"/etc/apt/apt.conf", "file", "APT's configuration can run commands as root at every installation (DPkg::Pre-Invoke and the like), and mamelet runs apt-get", "install a package that brings its APT configuration (NodePackage)"},
	{"/etc/apt/apt.conf.d", "dir", "APT's configuration can run commands as root at every installation (DPkg::Pre-Invoke and the like), and mamelet runs apt-get", "install a package that brings its APT configuration (NodePackage)"},
	{"/etc/dpkg/dpkg.cfg", "file", "dpkg's configuration can run commands as root at every installation (pre-invoke, post-invoke)", ""},
	{"/etc/dpkg/dpkg.cfg.d", "dir", "dpkg's configuration can run commands as root at every installation (pre-invoke, post-invoke)", ""},
	{"/usr/lib/apt", "dir", "it holds the programs apt-get runs as root (its methods)", ""},
	// systemd's settings of every service (mamelet's too), and what makes units at each daemon-reload.
	{"/etc/systemd/system.conf", "file", "it sets what every service runs with (DefaultEnvironment=, mamelet's too)", "set it in the unit of the one service (Environment=, a drop-in <unit>.service.d/*.conf)"},
	{"/etc/systemd/system.conf.d", "dir", "it sets what every service runs with (DefaultEnvironment=, mamelet's too)", "set it in the unit of the one service (Environment=, a drop-in <unit>.service.d/*.conf)"},
	{"/usr/local/lib/systemd/system.conf.d", "dir", "it sets what every service runs with (mamelet's too)", "set it in the unit of the one service"},
	{"/usr/lib/systemd/system.conf.d", "dir", "it sets what every service runs with (mamelet's too)", "set it in the unit of the one service"},
	{"/lib/systemd/system.conf.d", "dir", "it sets what every service runs with (mamelet's too)", "set it in the unit of the one service"},
	{"/etc/systemd/system-generators", "dir", "a generator runs as root at every daemon-reload and can replace any unit, mamelet's too", "write the unit itself"},
	{"/usr/local/lib/systemd/system-generators", "dir", "a generator runs as root at every daemon-reload and can replace any unit, mamelet's too", "write the unit itself"},
	{"/usr/lib/systemd/system-generators", "dir", "a generator runs as root at every daemon-reload and can replace any unit, mamelet's too", "write the unit itself"},
	{"/lib/systemd/system-generators", "dir", "a generator runs as root at every daemon-reload and can replace any unit, mamelet's too", "write the unit itself"},
	{"/etc/systemd/system-environment-generators", "dir", "it sets the environment of every service (mamelet's too)", "set it in the unit of the one service"},
	{"/usr/local/lib/systemd/system-environment-generators", "dir", "it sets the environment of every service (mamelet's too)", "set it in the unit of the one service"},
	{"/usr/lib/systemd/system-environment-generators", "dir", "it sets the environment of every service (mamelet's too)", "set it in the unit of the one service"},
	{"/lib/systemd/system-environment-generators", "dir", "it sets the environment of every service (mamelet's too)", "set it in the unit of the one service"},
	// Directories every user may write to: a user can put a link or a file there before root does,
	// and replace what root put (a root file there is a trap).
	{"/tmp", "dir", "every user of the node may write there, so any of them can put something in the way of what root writes, or replace it", "write it under /etc, /opt, /srv or /var/lib/<app>"},
	{"/var/tmp", "dir", "every user of the node may write there, so any of them can put something in the way of what root writes, or replace it", "write it under /etc, /opt, /srv or /var/lib/<app>"},
}

// sudoHint: what to do instead of writing sudoers.
const sudoHint = "rules of sudo are not supported yet (NodeUser can add a user to group sudo, which lets it run anything as root); write them by hand on the node"

// selfUnits are the units of mamelet and of what an extension of it manages, whose files (the unit,
// its drop-ins, the links that enable it) no file of linux is: a file there could stop mamelet, or
// change what it runs as.
var selfUnits = []struct{ unit, why, hint string }{
	{"mamelet.service", "it is mamelet's own unit (its drop-ins and the links that enable it too)", "mamelet's unit is written when its machine is made"},
	{"k3s.service", "it is the k3s unit (its drop-ins and links too), which the k3s extension writes", "declare k3s with K3s.server or K3s.agent"},
}

// programs are the programs mamelet runs (mamelet.Programs: the core's and every extension's), with
// the names they have on a merged /usr (/bin, /sbin), and the programs those run as root at once
// (dpkg, under apt-get): writing one would change what mamelet runs.
func programs() []string {
	var out []string
	for _, p := range append(mamelet.Programs(), "/usr/bin/dpkg") {
		out = append(out, p)
		if strings.HasPrefix(p, "/usr/bin/") || strings.HasPrefix(p, "/usr/sbin/") {
			out = append(out, strings.TrimPrefix(p, "/usr"))
		}
	}
	return out
}

// unitDirProblem says why p, under one of systemd's unit directories, is not a file Mamela writes
// ("" when it may be): mamelet's own unit (and k3s's), with their drop-ins and the links that enable
// them, and the drop-ins of every service (service.d), which mamelet's is one of.
func unitDirProblem(p string) (why, hint string) {
	for _, d := range allUnitDirs {
		if !strings.HasPrefix(p, d+"/") {
			continue
		}
		parts := strings.Split(p[len(d)+1:], "/")
		if parts[0] == "service.d" {
			return "it is a drop-in of every service, mamelet's too", "write a drop-in of the one service: " + d + "/<name>.service.d/<file>.conf"
		}
		for _, c := range parts {
			for _, u := range selfUnits {
				if c == u.unit || strings.HasPrefix(c, u.unit+".") {
					return u.why, u.hint
				}
			}
		}
	}
	return "", ""
}

// CheckRestart says why a file does not restart the service svc ("" when it may): mamelet's own
// service, whose restart would stop the round that carries the write out (and mamelet with it).
func CheckRestart(svc string) string {
	if strings.TrimSuffix(svc, ".service") == "mamelet" {
		return "mamelet does not restart itself after a write: its restart would stop the round that carries the write out"
	}
	return ""
}

// CheckPath says why Mamela does not write a file at p (nil when it may): p is absolute and clean,
// of at most MaxPath bytes, its components of letters, digits and . _ + @ = : , - only, and it is in
// none of the places that are not files to write (the kernel's, mamelet's, k3s's, the users', the
// APT sources that NodeAptRepo declares, an authorized_keys that NodeUser declares). The graph checks
// a declaration with it, and mamelet checks it again before it writes, whatever the revision says.
func CheckPath(p string) *PathError {
	bad := func(message, hint string) *PathError { return &PathError{Path: p, Message: message, Hint: hint} }
	const shape = "write an absolute path of letters, digits and . _ + @ = : , - such as \"/etc/nginx/conf.d/app.conf\""
	switch {
	case p == "":
		return bad("the path is empty", shape)
	case len(p) > MaxPath:
		return bad(fmt.Sprintf("the path is %d bytes; at most %d", len(p), MaxPath), "")
	case !strings.HasPrefix(p, "/"):
		return bad(fmt.Sprintf("%q is not an absolute path", p), shape)
	case p == "/" || strings.HasSuffix(p, "/"):
		return bad(fmt.Sprintf("%q is a directory, not a file", p), "name the file, such as \"/etc/app/app.conf\"")
	case path.Clean(p) != p:
		return bad(fmt.Sprintf("%q is not a clean path (it would be %q)", p, path.Clean(p)), "write it as "+fmt.Sprintf("%q", path.Clean(p)))
	}
	for _, c := range strings.Split(p[1:], "/") {
		switch {
		case c == "." || c == "..":
			return bad(fmt.Sprintf("%q has a %q component", p, c), shape)
		case !componentRe.MatchString(c):
			return bad(fmt.Sprintf("%q has a character other than letters, digits and . _ + @ = : , - in %q", p, c), shape)
		}
	}
	for _, f := range forbidden {
		in := false
		switch f.how {
		case "dir":
			in = p == f.place || strings.HasPrefix(p, f.place+"/")
		case "file":
			in = p == f.place
		case "prefix":
			in = strings.HasPrefix(p, f.place)
		}
		if in {
			return bad(fmt.Sprintf("Mamela does not write %s: %s", p, f.why), f.hint)
		}
	}
	if why, hint := unitDirProblem(p); why != "" {
		return bad(fmt.Sprintf("Mamela does not write %s: %s", p, why), hint)
	}
	if slices.Contains(programs(), p) {
		return bad(fmt.Sprintf("Mamela does not write %s: it is one of the programs mamelet runs as root (or that they run), whose code would then be what the file says", p),
			"install it as a package (NodePackage)")
	}
	if base := path.Base(p); (base == "authorized_keys" || base == "authorized_keys2") && path.Base(path.Dir(p)) == ".ssh" {
		return bad(fmt.Sprintf("Mamela does not write %s: who may log in as a user is not a file to write", p),
			"declare the user's SSH keys with NodeUser (the linux extension)")
	}
	return nil
}

// Under reports whether p is dir or a path under it.
func Under(p, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// allUnitDirs are where systemd's manager reads units from (/run aside, which no kind writes): a
// file written there takes effect after systemctl daemon-reload.
var allUnitDirs = []string{"/etc/systemd/system.control", "/etc/systemd/system.attached", "/etc/systemd/system",
	"/usr/local/lib/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"}

// IsUnitFile reports whether the file at p is read by systemd's manager as a unit or a drop-in.
func IsUnitFile(p string) bool {
	for _, d := range allUnitDirs {
		if strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// parents are the directories above p, outermost first ("/etc", "/etc/nginx" for
// "/etc/nginx/nginx.conf").
func parents(p string) []string {
	var out []string
	for i := 1; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, p[:i])
		}
	}
	return out
}
