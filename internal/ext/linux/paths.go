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
// dynamic linker's, APT's and dpkg's configuration, what APT trusts, what useradd reads, systemd's
// settings of every service, the libraries and modules they load), and the directories every user
// may write to. The programs that run by name (pathProblem) and the system's libraries
// (libraryProblem) are checked besides. It is not every place a root file matters (a declaration
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
	// What APT trusts: a source written by hand ("[trusted=yes]", a key of its own) would bypass the
	// key NodeAptRepo scopes to it (Signed-By) and the origins mamelet allows, and a key replaced
	// would make apt trust another's packages; pinning chooses the version and origin apt installs.
	{"/etc/apt/sources.list", "file", "APT's sources say where packages come from and which keys sign them: one written by hand bypasses the key NodeAptRepo scopes to each source (Signed-By) and the origins mamelet allows", aptSourcesHint},
	{"/etc/apt/sources.list.d", "dir", "APT's sources say where packages come from and which keys sign them: one written by hand bypasses the key NodeAptRepo scopes to each source (Signed-By) and the origins mamelet allows", aptSourcesHint},
	{"/etc/apt/trusted.gpg", "prefix", "it is a key APT trusts for every source", aptRepoHint},
	{"/etc/apt/keyrings", "dir", "it holds the keys APT trusts for the sources that name them (and NodeAptRepo's own)", aptRepoHint},
	{"/usr/share/keyrings", "dir", "it holds the keys APT trusts for the system's sources (the distribution's archive key)", aptRepoHint},
	{"/etc/apt/preferences", "file", "pinning chooses which version, and from which source, apt installs: mamelet's installations and every other", "pin by hand on the node if it must be; Mamela does not declare APT preferences"},
	{"/etc/apt/preferences.d", "dir", "pinning chooses which version, and from which source, apt installs: mamelet's installations and every other", "pin by hand on the node if it must be; Mamela does not declare APT preferences"},
	{"/var/lib/dpkg", "dir", "it is the database of the installed packages", "install packages with NodePackage"},
	// What useradd and usermod (which mamelet runs), and adduser (which the packages' scripts run),
	// read or run as root.
	{"/etc/login.defs", "file", "it sets how useradd and usermod make users (the uid ranges, the mode of a new home), which an approval of a NodeUser does not show", "set it by hand on the node if it must be"},
	{"/etc/default/useradd", "file", "it sets how useradd makes users (where homes go, the directory copied into a new home: SKEL), which an approval of a NodeUser does not show", "declare the user's home and shell with NodeUser"},
	{"/etc/shadow-maint", "dir", "useradd and usermod run what is there as root", ""},
	{"/etc/adduser.conf", "file", "it sets how adduser, which the packages' scripts run as root, makes users", "set it by hand on the node if it must be"},
	{"/usr/local/sbin/adduser.local", "file", "adduser, which the packages' scripts run, runs it as root", ""},
	{"/usr/local/sbin/deluser.local", "file", "deluser, which the packages' scripts run, runs it as root", ""},
	// What every program mamelet runs (and their children) reads: a file here runs as root at once.
	{"/etc/ld.so.preload", "file", "the dynamic linker loads what it names into every program, mamelet and what it runs included", ""},
	{"/etc/ld.so.conf", "file", "it says where every program finds its libraries, mamelet's programs included", libHint},
	{"/etc/ld.so.conf.d", "dir", "it says where every program finds its libraries, mamelet's programs included", libHint},
	{"/etc/ld.so.cache", "file", "the dynamic linker finds every program's libraries through it, mamelet's programs included", libHint},
	{"/etc/nsswitch.conf", "file", "it names the modules every program loads to look users and hosts up (useradd, apt and dpkg included), and so which users exist", "install a package that brings its name service (NodePackage)"},
	// The directories of the system's libraries (the dynamic linker's, and the modules programs load by
	// name: NSS, PAM, gconv, Perl's and Python's), whole: a file there is loaded into what runs as root.
	{"/lib64", "dir", libWhy, libHint},
	{"/usr/lib64", "dir", libWhy, libHint},
	{"/lib32", "dir", libWhy, libHint},
	{"/usr/lib32", "dir", libWhy, libHint},
	{"/libx32", "dir", libWhy, libHint},
	{"/usr/libx32", "dir", libWhy, libHint},
	// The modules of the interpreters apt and dpkg run as root (debconf is Perl, APT's hooks are
	// Python), in the order they are looked for.
	{"/etc/perl", "dir", moduleWhy, libHint},
	{"/usr/local/lib/site_perl", "dir", moduleWhy, libHint},
	{"/usr/local/share/perl", "dir", moduleWhy, libHint},
	{"/usr/share/perl", "dir", moduleWhy, libHint},
	{"/usr/share/perl5", "dir", moduleWhy, libHint},
	{"/usr/local/lib/python3", "prefix", moduleWhy, libHint},
	{"/usr/lib/python3", "prefix", moduleWhy, libHint},
	{"/usr/share/debconf", "dir", "the packages' scripts run what is there as root (debconf's confmodule)", ""},
	{"/usr/lib/dpkg", "dir", "it holds the programs dpkg runs as root", ""},
	{"/etc/apt/apt.conf", "file", "APT's configuration can run commands as root at every installation (DPkg::Pre-Invoke and the like), and mamelet runs apt-get", "install a package that brings its APT configuration (NodePackage)"},
	{"/etc/apt/apt.conf.d", "dir", "APT's configuration can run commands as root at every installation (DPkg::Pre-Invoke and the like), and mamelet runs apt-get", "install a package that brings its APT configuration (NodePackage)"},
	{"/etc/dpkg/dpkg.cfg", "file", "dpkg's configuration can run commands as root at every installation (pre-invoke, post-invoke)", ""},
	{"/etc/dpkg/dpkg.cfg.d", "dir", "dpkg's configuration can run commands as root at every installation (pre-invoke, post-invoke)", ""},
	{"/usr/lib/apt", "dir", "it holds the programs apt-get runs as root (its methods)", ""},
	// What the packages' scripts and APT's hooks run or read as root besides: Python's site
	// configuration (sitecustomize, which every python3 loads), needrestart's (Perl it evaluates, from
	// an APT hook on Ubuntu), the kernel's and the initramfs's hooks, ca-certificates' hooks.
	{"/etc/python3", "prefix", "Python reads its site configuration from there (sitecustomize, /etc/python3.X) whenever it starts, and APT's hooks and the packages' scripts run python3 as root", "configure an application's Python under /opt/<app> (a virtual environment)"},
	{"/etc/needrestart", "dir", "needrestart, which an APT hook runs as root after each installation (Ubuntu), evaluates its configuration as Perl and runs its hooks", "set it by hand on the node if it must be"},
	{"/etc/kernel", "dir", "the kernel's packages run the scripts there as root at each installation of a kernel (postinst.d, postrm.d)", "install what needs them as a package (NodePackage)"},
	{"/etc/initramfs-tools", "dir", "update-initramfs runs its hooks and scripts as root, and puts them into the initramfs the node boots", "install what needs them as a package (NodePackage)"},
	{"/etc/ca-certificates/update.d", "dir", "update-ca-certificates runs the scripts there as root (when ca-certificates is installed or upgraded)", "install what needs them as a package (NodePackage)"},
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

// aptRepoHint: what to do instead of writing APT's keys.
const aptRepoHint = "declare the APT source with NodeAptRepo (the linux extension): it writes the source and its key, scoped to it"

// aptSourcesHint: what to do instead of writing APT's sources. NodeAptRepo is for a source of one's
// own; the distribution's sources cannot be declared yet (ADR 0016, a limitation).
const aptSourcesHint = "declare a vendor's source (Docker's, a database's) with NodeAptRepo (the linux extension): it writes the source and its key, scoped to it. " +
	"The distribution's own sources (another mirror, a component such as non-free-firmware) cannot be declared yet: NodeAptRepo would name the URIs and suites of the distribution's file, which apt refuses; change them by hand on the node"

// The reasons and hint of the libraries' places.
const (
	libWhy    = "it holds the system's libraries and the modules programs load by name (NSS, PAM, gconv): a file there is loaded into what runs as root, mamelet's programs included"
	moduleWhy = "the interpreters apt and dpkg run as root (debconf is Perl, APT's hooks are Python) load their modules from there"
	libHint   = "install libraries as a package (NodePackage), or keep an application's own under /opt/<app>"
)

// pathDirs are the directories of the programs that run by name (each PATH, in order: root's and
// systemd's services' from /usr/local, mamelet's children's, apt's and dpkg's from /usr/sbin).
var pathDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// packageDirs are the directories of pathDirs that are the packages' (dpkg's): apt, dpkg and the
// packages' scripts run their programs from there as root (PATH=/usr/sbin:/usr/bin:/sbin:/bin, which
// mamelet gives them and APT gives dpkg), so a file there replaces a program they run, or shadows one
// of the same name in a later directory (a new /usr/sbin/tar runs instead of /usr/bin/tar).
var packageDirs = []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// rootRuns are, by name, the programs root runs that a program in /usr/local/sbin or /usr/local/bin
// would shadow (those come first in the PATH of root's shells and of systemd's services): mamelet's
// (mamelet.Programs, added by programNames), apt's and dpkg's and those dpkg needs on its PATH, the
// shells and interpreters of the packages' scripts, and what those run to install services, make
// users and become another user.
//
// It is the graph's check, for the names it knows; mamelet, which knows the node, refuses besides any
// name the node has in a directory of the packages' programs (shadowed).
var rootRuns = []string{
	"add-shell", "addgroup", "adduser", "apt", "apt-cache", "apt-config", "apt-extracttemplates", "apt-get", "apt-key", "apt-mark",
	"awk", "base64", "basename", "bash", "bzip2", "c_rehash", "cat", "chfn", "chgrp", "chmod", "chown", "chpasswd", "chroot", "chsh",
	"cmp", "cp", "cut", "dash", "date", "dd", "deb-systemd-helper", "deb-systemd-invoke", "debconf", "debconf-apt-progress",
	"debconf-communicate", "debconf-copydb", "debconf-escape", "debconf-set-selections", "debconf-show", "delgroup", "deluser",
	"depmod", "df", "diff", "dirname", "dpkg", "dpkg-deb", "dpkg-divert", "dpkg-maintscript-helper", "dpkg-preconfigure",
	"dpkg-query", "dpkg-realpath", "dpkg-reconfigure", "dpkg-split", "dpkg-statoverride", "dpkg-trigger", "echo", "egrep", "env",
	"expr", "false", "fgrep", "find", "findmnt", "flock", "gawk", "getent", "getopt", "gpasswd", "gpg", "gpg-agent", "gpgconf",
	"gpgv", "grep", "groupadd", "groupdel", "groupmod", "grub-install", "grub-mkconfig", "grub-probe", "gunzip", "gzip", "head",
	"hostname", "id", "insserv", "install", "install-info", "installkernel", "invoke-rc.d", "ionice", "ischroot", "journalctl",
	"kill", "kmod", "ldconfig", "ldconfig.real", "ldd", "ln", "logger", "login", "ls", "lsb_release", "mawk", "md5sum", "mkdir",
	"mkinitramfs", "mknod", "mktemp", "modprobe", "mount", "mv", "needrestart", "newgrp", "nologin", "nscd", "openssl", "passwd",
	"perl", "pgrep", "pidof", "pkill", "printf", "ps", "py3clean", "py3compile", "python3", "readlink", "realpath", "remove-shell",
	"rm", "rmdir", "run-parts", "runuser", "savelog", "sed", "seq", "service", "setpriv", "sh", "sha1sum", "sha256sum",
	"sha512sum", "sleep", "sort", "sss_cache", "start-stop-daemon", "stat", "su", "sudo", "sync", "sysctl", "systemctl",
	"systemd-detect-virt", "systemd-escape", "systemd-run", "systemd-sysusers", "systemd-tmpfiles", "tail", "tar", "tee",
	"tempfile", "test", "touch", "tr", "true", "ucf", "ucfr", "udevadm", "umount", "uname", "unattended-upgrade",
	"unattended-upgrades", "uniq", "unxz", "update-alternatives", "update-ca-certificates", "update-grub", "update-initramfs",
	"update-rc.d", "update-shells", "useradd", "userdel", "usermod", "wc", "which", "xargs", "xz", "zcat", "zstd",
}

// programNames are rootRuns and the names of mamelet's programs.
func programNames() []string {
	out := slices.Clone(rootRuns)
	for _, p := range mamelet.Programs() {
		out = append(out, path.Base(p))
	}
	return out
}

// pathProblem says why p, a program that would run by name, is not a file Mamela writes ("" when it
// may be): it is in a directory of the packages' programs, or it shadows a program root runs.
func pathProblem(p string) (why, hint string) {
	dir, base := path.Dir(p), path.Base(p)
	switch {
	case slices.Contains(packageDirs, dir):
		return fmt.Sprintf("%s holds the packages' programs, which apt, dpkg and the packages' scripts run as root by name: a file there replaces one of them, or runs instead of one of the same name in a later directory of their PATH (/usr/sbin:/usr/bin:/sbin:/bin)", dir),
			"install a program of your own under /usr/local/bin or /opt/<app> (NodeArtifact), or as a package (NodePackage)"
	case slices.Contains(pathDirs, dir) && slices.Contains(programNames(), base):
		return fmt.Sprintf("%s comes first in the PATH of root's shells and of systemd's services: a %s there runs instead of the system's %s, which mamelet, apt, dpkg or the packages' scripts run as root", dir, base, base),
			"give the program another name, or install it under /opt/<app>"
	}
	return "", ""
}

// libraryDirs are the directories the dynamic linker looks in (and the multiarch ones below them):
// a shared object there can be loaded by what runs as root (/usr/local/lib is in Debian's and
// Ubuntu's ld.so.conf, and comes first).
var libraryDirs = []string{"/lib", "/usr/lib", "/usr/local/lib"}

// multiarch is a directory of the libraries of one architecture ("x86_64-linux-gnu").
var multiarch = regexp.MustCompile(`^[a-z0-9_]+-linux-gnu[a-z0-9_]*$`)

// libraryProblem says why p, under the system's library directories, is not a file Mamela writes
// ("" when it may be): a multiarch directory, whole (its libraries, the ELF interpreter's links, and
// the modules programs load by name: security/ for PAM, gconv/, perl5/); a shared object anywhere
// below them.
func libraryProblem(p string) string {
	for _, d := range libraryDirs {
		if !strings.HasPrefix(p, d+"/") {
			continue
		}
		rest := strings.Split(p[len(d)+1:], "/")
		if len(rest) > 1 && multiarch.MatchString(rest[0]) {
			return libWhy
		}
		if base := path.Base(p); strings.HasSuffix(base, ".so") || strings.Contains(base, ".so.") {
			return "it is a shared object below " + d + ", where the dynamic linker (or a program loading a module by name) can find it for what runs as root, mamelet's programs included"
		}
	}
	return ""
}

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
		switch parts[0] {
		case "service.d":
			return "it is a drop-in of every service, mamelet's too", "write a drop-in of the one service: " + d + "/<name>.service.d/<file>.conf"
		case "slice.d", "-.slice", "-.slice.d", "system.slice", "system.slice.d":
			// The slices every service runs in (system.slice, below the root slice -.slice): their
			// limits (MemoryMax=, CPUQuota=, IPAddressDeny=) and the drop-ins of every slice.
			return "it sets the slice every service runs in (system.slice, the root slice -.slice) or every slice, mamelet's too: its limits and its network access",
				"set the limits in the unit of the one service (a drop-in <unit>.service.d/*.conf), or give it a slice of its own (Slice=)"
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
// none of the places that are not files to write (the kernel's, mamelet's, k3s's, the users', APT's
// sources and keys, which NodeAptRepo declares, an authorized_keys that NodeUser declares, what the
// programs mamelet runs as root run or trust: forbidden, pathProblem, libraryProblem). The graph
// checks a declaration with it, and mamelet checks it again before it writes, whatever the revision
// says.
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
	if why, hint := pathProblem(p); why != "" {
		return bad(fmt.Sprintf("Mamela does not write %s: %s", p, why), hint)
	}
	if why := libraryProblem(p); why != "" {
		return bad(fmt.Sprintf("Mamela does not write %s: %s", p, why), libHint)
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
