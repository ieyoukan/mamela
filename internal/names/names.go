// Package names defines the shape of every name that a declaration and a signed revision both
// carry: a node, a Debian package, a systemd service, a k3s manifest, a k3s release, a sha256.
//
// internal/graph checks the declaration with these, and internal/noderev checks the revision the
// Control Plane signs from it with the very same ones. They used to keep a copy each, and the
// copies drifted: a declaration the graph accepted (a service "a:b", a package of 150
// characters) could not be signed, and since a node's revisions only ever grow, fixing the
// declaration did not bring the node back. One definition cannot drift. It depends on nothing, so
// both can import it.
package names

import "regexp"

var (
	// A node name is a DNS label: it becomes a hostname.
	nodeRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// The name of a resource of a provider (a machine's is a node name as well).
	resourceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,252}$`)
	// Debian package names (Debian Policy 5.6.1: at least two characters), at most 128.
	packageRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{1,127}$`)
	// A systemd unit name without the ".service" suffix: no path separators, no whitespace, no
	// ':' (which systemd allows in a unit name, but no unit Mamela manages needs).
	serviceRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9@._-]{0,127}$`)
	// The name of a k3s manifest: the file is mamela-<name>.yaml.
	manifestRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// A k3s release ("v1.33.5+k3s1").
	k3sVersionRe = regexp.MustCompile(`^v1\.[0-9]{1,2}\.[0-9]{1,2}\+k3s[0-9]{1,2}$`)
	// A sha256 in lowercase hex.
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// The name of a node extension ("k3s", "linux"): a key of a revision's and a report's ext.
	extensionRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// MaxManifest bounds a k3s manifest's content, in bytes.
const MaxManifest = 64 << 10

// Node reports whether s can be a node's name (and so its hostname).
func Node(s string) bool { return nodeRe.MatchString(s) }

// Resource reports whether s can be the name of a resource of a provider (every node name can).
func Resource(s string) bool { return resourceRe.MatchString(s) }

// Package reports whether s can be a Debian package name (and is not an option).
func Package(s string) bool { return packageRe.MatchString(s) }

// Service reports whether s can be a systemd service name (and is not an option).
func Service(s string) bool { return serviceRe.MatchString(s) }

// Manifest reports whether s can be the name of a k3s manifest.
func Manifest(s string) bool { return manifestRe.MatchString(s) }

// K3sVersion reports whether s is a k3s release tag.
func K3sVersion(s string) bool { return k3sVersionRe.MatchString(s) }

// SHA256 reports whether s is a sha256 in lowercase hex (64 digits).
func SHA256(s string) bool { return sha256Re.MatchString(s) }

// Extension reports whether s can be the name of a node extension (a key of a revision's ext).
func Extension(s string) bool { return extensionRe.MatchString(s) }
