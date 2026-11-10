// Package noderev is the wire format between the Control Plane and mamelet, the agent in each VM.
//
// The Control Plane gives each node a signed Revision: what should be on the node, and exactly
// which changes mamelet may make. mamelet verifies the signature, the node, the validity period
// and that revisions never go backwards before it acts on one. It sends back a Report: what it
// observed, what installing the declared packages would change (apt-get -s), and the results of
// the operations it carried out.
//
// Decoding is strict (unknown members are errors) and every name is checked against a narrow
// pattern, so nothing in a revision can reach a shell or an unexpected command-line option.
package noderev

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Schema identifies the revision format.
const Schema = "mamelet-revision/v1"

// Revision is what a node should look like and what mamelet may do about it.
type Revision struct {
	Schema string `json:"schema"`
	Node   string `json:"node"`
	VMID   int    `json:"vmid"`
	// Serial increases whenever the content changes. mamelet never accepts a lower serial than
	// one it has accepted, so an old revision cannot be replayed.
	Serial    int64     `json:"serial"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Packages  []Package `json:"packages"`
	Services  []Service `json:"services"`
	// K3s, when set, is the k3s server the node should run: mamelet observes it, and installs it
	// only with an approved, one-time Install.
	K3s *K3s `json:"k3s,omitempty"`
}

// K3s is a k3s release, pinned by the sha256 of its binary for the node's architecture, and the
// node's place in its cluster.
type K3s struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Role    string `json:"role"` // "server" or "agent"
	// Server is an agent's server ("https://<address>:6443").
	Server string `json:"server,omitempty"`
	// Token is the cluster's join token, made by the Control Plane and given only to the
	// cluster's nodes (in their revisions, over their mutually authenticated connections). It is
	// never in a declaration, a plan or a report.
	Token string `json:"token"`
	// Install is an approved installation of exactly this binary: mamelet downloads the release
	// asset, and installs it (with Mamela's fixed systemd unit) only if its sha256 matches.
	Install *Once `json:"install,omitempty"`
}

// K3sReleases is where k3s releases are downloaded from: <K3sReleases>/<version>/<asset>.
const K3sReleases = "https://github.com/k3s-io/k3s/releases/download"

// K3sTag is a release version as it appears in the release URL ("v1.33.5%2Bk3s1").
func K3sTag(version string) string { return strings.ReplaceAll(version, "+", "%2B") }

// K3s roles.
const (
	K3sServer = "server"
	K3sAgent  = "agent"
)

// K3sTokenFile is where mamelet writes the cluster's join token (0600).
const K3sTokenFile = "/etc/rancher/k3s/cluster-token"

// K3sArgs are the arguments k3s runs with, fixed in Mamela (a declaration cannot change them): a
// server as light as k3s goes (no ingress controller, no metrics server), its kubeconfig readable
// by root only; an agent joins its server. Both take the join token from K3sTokenFile.
func K3sArgs(role, server string) []string {
	if role == K3sAgent {
		return []string{"agent", "--server=" + server, "--token-file=" + K3sTokenFile}
	}
	return []string{"server", "--token-file=" + K3sTokenFile, "--disable=traefik", "--disable=metrics-server", "--write-kubeconfig-mode=0600"}
}

// Package is an APT package that should be installed.
type Package struct {
	Name string `json:"name"`
	// Install, when set, is an approved installation: mamelet installs the package only if a fresh
	// simulation gives exactly this change set, and only once.
	Install *Install `json:"install,omitempty"`
}

// Install is a one-time, approved installation.
type Install struct {
	Operation string `json:"operation"` // the Control Plane's operation ID
	// Attempt counts the Control Plane's tries of the operation: mamelet tries each attempt at
	// most once, so a failure is not repeated every round, and a retry is a new attempt.
	Attempt   int    `json:"attempt"`
	ChangeSet string `json:"changeSet"` // digest of the approved apt-get -s change set
}

// Service is a systemd service and its desired state.
type Service struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
	// Repair lists what mamelet may do whenever it finds the service drifted, without asking
	// (from policy: only "start" exists).
	Repair []string `json:"repair"`
	// Once lists approved one-time operations.
	Once []Once `json:"once"`
}

// Once is a one-time, approved service operation.
type Once struct {
	Operation string `json:"operation"`
	Attempt   int    `json:"attempt"`
	Action    string `json:"action"` // enable, start, disable or stop
}

// Service actions mamelet knows. Anything else makes the whole revision invalid.
const (
	ActionEnable  = "enable"
	ActionStart   = "start"
	ActionDisable = "disable"
	ActionStop    = "stop"
	ActionInstall = "install" // k3s only
)

var (
	nodePattern      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	packagePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,127}$`)
	servicePattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9@._-]{0,127}$`)
	operationPattern = regexp.MustCompile(`^op-[0-9a-f]{24}$`)
	k3sServerURL     = regexp.MustCompile(`^https://(25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])){3}:6443$`)
	k3sVersion       = regexp.MustCompile(`^v1\.[0-9]{1,2}\.[0-9]{1,2}\+k3s[0-9]{1,2}$`)
	hexSHA256        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ValidNode reports whether s can be a node name.
func ValidNode(s string) bool { return nodePattern.MatchString(s) }

// ValidPackage reports whether s can be a Debian package name (and is not an option).
func ValidPackage(s string) bool { return packagePattern.MatchString(s) }

// ValidService reports whether s can be a systemd service name (and is not an option).
func ValidService(s string) bool { return servicePattern.MatchString(s) }

// Check validates the content of a revision (not its signature or validity period).
func (r *Revision) Check() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if r.Schema != Schema {
		add("schema is %q, want %q", r.Schema, Schema)
	}
	if !ValidNode(r.Node) {
		add("node %q is not a valid node name", r.Node)
	}
	if r.VMID <= 0 {
		add("vmid %d is not valid", r.VMID)
	}
	if r.Serial <= 0 {
		add("serial %d is not valid", r.Serial)
	}
	if !r.ExpiresAt.After(r.IssuedAt) {
		add("expiresAt is not after issuedAt")
	}
	seen := map[string]bool{}
	for _, p := range r.Packages {
		if !ValidPackage(p.Name) {
			add("package %q is not a valid package name", p.Name)
		}
		if seen["p/"+p.Name] {
			add("package %s appears twice", p.Name)
		}
		seen["p/"+p.Name] = true
		if p.Install != nil {
			if !operationPattern.MatchString(p.Install.Operation) {
				add("package %s: operation %q is not valid", p.Name, p.Install.Operation)
			}
			if p.Install.Attempt < 1 {
				add("package %s: attempt %d is not valid", p.Name, p.Install.Attempt)
			}
			if !digestPattern.MatchString(p.Install.ChangeSet) {
				add("package %s: change set %q is not a digest", p.Name, p.Install.ChangeSet)
			}
		}
	}
	for _, s := range r.Services {
		if !ValidService(s.Name) {
			add("service %q is not a valid service name", s.Name)
		}
		if seen["s/"+s.Name] {
			add("service %s appears twice", s.Name)
		}
		seen["s/"+s.Name] = true
		for _, a := range s.Repair {
			if a != ActionStart {
				add("service %s: repair action %q is not allowed (only %q)", s.Name, a, ActionStart)
			}
		}
		for _, o := range s.Once {
			if !operationPattern.MatchString(o.Operation) {
				add("service %s: operation %q is not valid", s.Name, o.Operation)
			}
			if o.Attempt < 1 {
				add("service %s: attempt %d is not valid", s.Name, o.Attempt)
			}
			if o.Action != ActionEnable && o.Action != ActionStart && o.Action != ActionDisable && o.Action != ActionStop {
				add("service %s: action %q is not known", s.Name, o.Action)
			}
		}
	}
	if k := r.K3s; k != nil {
		if !k3sVersion.MatchString(k.Version) || !hexSHA256.MatchString(k.SHA256) {
			add("k3s %q / sha256 %q are not valid", k.Version, k.SHA256)
		}
		switch {
		case k.Role == K3sServer && k.Server != "":
			add("k3s: a server has no server")
		case k.Role == K3sAgent && !k3sServerURL.MatchString(k.Server):
			add("k3s: server %q is not https://<IPv4>:6443", k.Server)
		case k.Role != K3sServer && k.Role != K3sAgent:
			add("k3s: role %q", k.Role)
		}
		if !hexSHA256.MatchString(k.Token) {
			add("k3s: the token is not 64 hex digits")
		}
		if in := k.Install; in != nil && (!operationPattern.MatchString(in.Operation) || in.Attempt < 1 || in.Action != ActionInstall) {
			add("k3s: install grant %+v is not valid", *in)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid revision: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Content is the revision without its validity period, canonically encoded: two revisions with
// the same content and serial differ only in when they were issued and until when they are valid.
func (r Revision) Content() []byte {
	r.IssuedAt, r.ExpiresAt = time.Time{}, time.Time{}
	r.Packages = slices.Clone(r.Packages)
	r.Services = slices.Clone(r.Services)
	slices.SortFunc(r.Packages, func(a, b Package) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(r.Services, func(a, b Service) int { return strings.Compare(a.Name, b.Name) })
	if r.Packages == nil {
		r.Packages = []Package{}
	}
	if r.Services == nil {
		r.Services = []Service{}
	}
	for i := range r.Services {
		if r.Services[i].Repair == nil {
			r.Services[i].Repair = []string{}
		}
		if r.Services[i].Once == nil {
			r.Services[i].Once = []Once{}
		}
	}
	out, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		panic("noderev: encoding failed: " + err.Error())
	}
	return out
}

// Signed is a revision with the Control Plane's Ed25519 signature over the exact payload bytes.
type Signed struct {
	Payload   []byte `json:"payload"`   // the revision as JSON (base64 on the wire)
	Signature []byte `json:"signature"` // Ed25519 over Payload
}

// Sign encodes and signs a revision.
func Sign(key ed25519.PrivateKey, r Revision) (Signed, error) {
	if err := r.Check(); err != nil {
		return Signed{}, err
	}
	payload, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		return Signed{}, err
	}
	return Signed{Payload: payload, Signature: ed25519.Sign(key, payload)}, nil
}

// Expectation is what mamelet knows about itself and its history.
type Expectation struct {
	Node       string
	VMID       int
	Now        time.Time
	LastSerial int64         // the highest serial accepted so far (0: none)
	MaxSkew    time.Duration // tolerated clock difference for issuedAt (default 5 minutes)
}

// Errors from Verify. Each names a reason a revision is refused.
var (
	ErrBadSignature = errors.New("the signature does not match the Control Plane's key")
	ErrOtherNode    = errors.New("the revision is for another node")
	ErrExpired      = errors.New("the revision has expired")
	ErrFromFuture   = errors.New("the revision was issued in the future (clock skew?)")
	ErrRolledBack   = errors.New("the revision is older than one already accepted")
)

// Verify checks a signed revision and returns it. Nothing in the payload is looked at before the
// signature has been verified.
func Verify(key ed25519.PublicKey, s Signed, want Expectation) (Revision, error) {
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, s.Payload, s.Signature) {
		return Revision{}, ErrBadSignature
	}
	var r Revision
	if err := json.Unmarshal(s.Payload, &r, json.RejectUnknownMembers(true)); err != nil {
		return Revision{}, fmt.Errorf("decode revision: %w", err)
	}
	if err := r.Check(); err != nil {
		return Revision{}, err
	}
	skew := want.MaxSkew
	if skew == 0 {
		skew = 5 * time.Minute
	}
	switch {
	case r.Node != want.Node || r.VMID != want.VMID:
		return Revision{}, fmt.Errorf("%w: %s (VMID %d), this is %s (VMID %d)", ErrOtherNode, r.Node, r.VMID, want.Node, want.VMID)
	case !want.Now.Before(r.ExpiresAt):
		return Revision{}, fmt.Errorf("%w at %s", ErrExpired, r.ExpiresAt.UTC().Format(time.RFC3339))
	case r.IssuedAt.After(want.Now.Add(skew)):
		return Revision{}, fmt.Errorf("%w: %s", ErrFromFuture, r.IssuedAt.UTC().Format(time.RFC3339))
	case r.Serial < want.LastSerial:
		return Revision{}, fmt.Errorf("%w: serial %d < %d", ErrRolledBack, r.Serial, want.LastSerial)
	}
	return r, nil
}

// Report is what mamelet sends after each round.
type Report struct {
	Node     string         `json:"node"`
	VMID     int            `json:"vmid"`
	Serial   int64          `json:"serial"` // the revision the round worked from (0: none)
	Version  string         `json:"version"`
	Packages []PackageState `json:"packages"`
	Services []ServiceState `json:"services"`
	K3s      *K3sState      `json:"k3s,omitempty"`
	Results  []Result       `json:"results"`
	// Refused explains why the last revision received was not accepted, if it was not.
	Refused string `json:"refused,omitempty"`
}

// PackageState is an observed package. ChangeSet is what installing it would do, when it is not
// installed and the simulation ran.
type PackageState struct {
	Name      string     `json:"name"`
	Installed bool       `json:"installed"`
	Version   string     `json:"version,omitempty"`
	ChangeSet *ChangeSet `json:"changeSet,omitempty"`
	Error     string     `json:"error,omitempty"` // why it could not be observed or simulated
}

// K3sState is the observed k3s: whether Mamela's unit and a binary are installed, and the binary's
// sha256.
type K3sState struct {
	Installed bool   `json:"installed"` // the binary, Mamela's unit and the token, as the revision says
	SHA256    string `json:"sha256,omitempty"`
	Error     string `json:"error,omitempty"`
	// Addresses are the node's IPv4 addresses (an agent's revision names its server by one).
	Addresses []string `json:"addresses,omitempty"`
	// Server is the server the installed agent's unit joins ("" for a server or when not installed).
	Server string `json:"server,omitempty"`
}

// ServiceState is an observed service.
type ServiceState struct {
	Name    string `json:"name"`
	Exists  bool   `json:"exists"`
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
}

// Result is the outcome of an operation mamelet carried out or refused.
type Result struct {
	Operation string `json:"operation"` // an operation ID, or "repair" for a standing repair
	Attempt   int    `json:"attempt,omitempty"`
	Outcome   string `json:"outcome"` // done, failed, refused
	Detail    string `json:"detail,omitempty"`
}

// Result outcomes.
const (
	OutcomeDone    = "done"
	OutcomeFailed  = "failed"
	OutcomeRefused = "refused"
)

// ChangeSet is what apt would do to install a package: every package installed, upgraded or
// removed, with versions and origins.
type ChangeSet struct {
	Digest  string   `json:"digest"`
	Changes []Change `json:"changes"`
}

// Change is one line of a change set.
type Change struct {
	Action  string   `json:"action"` // install, upgrade, remove
	Package string   `json:"package"`
	Version string   `json:"version,omitempty"`
	Origins []string `json:"origins,omitempty"` // e.g. "Ubuntu:24.04/noble-updates"
}

// Change actions.
const (
	ChangeInstall = "install"
	ChangeUpgrade = "upgrade"
	ChangeRemove  = "remove"
)

// NewChangeSet sorts the changes and computes the digest.
func NewChangeSet(changes []Change) ChangeSet {
	cs := ChangeSet{Changes: slices.Clone(changes)}
	if cs.Changes == nil {
		cs.Changes = []Change{}
	}
	for i := range cs.Changes {
		cs.Changes[i].Origins = slices.Sorted(slices.Values(cs.Changes[i].Origins))
	}
	slices.SortFunc(cs.Changes, func(a, b Change) int {
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		return strings.Compare(a.Action, b.Action)
	})
	cs.Digest = changeDigest(cs.Changes)
	return cs
}

func changeDigest(changes []Change) string {
	out, err := json.Marshal(changes, json.Deterministic(true))
	if err != nil {
		panic("noderev: encoding failed: " + err.Error())
	}
	sum := sha256.Sum256(out)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Check verifies the digest against the changes (a report is not trusted as sent).
func (c ChangeSet) Check() error {
	if got := NewChangeSet(c.Changes).Digest; got != c.Digest {
		return fmt.Errorf("change set digest %s does not match its changes (%s)", c.Digest, got)
	}
	return nil
}

// Removes reports whether the change set removes any package.
func (c ChangeSet) Removes() bool {
	return slices.ContainsFunc(c.Changes, func(ch Change) bool { return ch.Action == ChangeRemove })
}
