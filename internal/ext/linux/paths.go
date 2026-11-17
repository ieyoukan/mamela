package linux

import (
	"fmt"
	"path"
	"regexp"
	"strings"
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
// and what another kind (or the core) declares. Each says why and what to use instead. A path is in
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
	{"/etc/systemd/system/mamelet.service", "prefix", "it is mamelet's own unit (and its drop-ins)", ""},
	{"/etc/rancher/k3s", "dir", "it is k3s's configuration and join token, which the k3s extension writes", "declare k3s with K3s.server or K3s.agent"},
	{"/var/lib/rancher/k3s", "dir", "it is k3s's data and its manifests", "a manifest of k3s is K3s.manifest"},
	{"/usr/local/bin/k3s", "file", "it is the k3s binary, which the k3s extension installs", "declare k3s with K3s.server or K3s.agent"},
	{"/etc/systemd/system/k3s.service", "prefix", "it is the k3s unit, which the k3s extension writes", ""},
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
	{"/etc/sudoers", "file", "who may become root is not a file to write", "declare what a user may do with NodeUser (the linux extension)"},
	{"/etc/sudoers.d", "dir", "who may become root is not a file to write", "declare what a user may do with NodeUser (the linux extension)"},
	{"/etc/apt/sources.list.d/mamela-", "prefix", "it is where NodeAptRepo puts the APT sources it declares", "declare the APT source with NodeAptRepo (the linux extension), or name the file otherwise"},
	{"/etc/apt/keyrings/mamela-", "prefix", "it is where NodeAptRepo puts the keys of the APT sources it declares", "declare the APT source with NodeAptRepo (the linux extension), or name the file otherwise"},
	{"/var/lib/dpkg", "dir", "it is the database of the installed packages", "install packages with NodePackage"},
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

// unitDirs are where systemd reads units from (of the system's manager): a file written there takes
// effect after systemctl daemon-reload.
var unitDirs = []string{"/etc/systemd/system", "/usr/local/lib/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"}

// IsUnitFile reports whether the file at p is read by systemd's manager as a unit or a drop-in.
func IsUnitFile(p string) bool {
	for _, d := range unitDirs {
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
