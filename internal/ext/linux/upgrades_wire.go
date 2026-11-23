package linux

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/names"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// AptUpgrades is what the part asks mamelet of the upgrades of its node (Part.AptUpgrades: at most
// one), and the upgrade it allows once.
type AptUpgrades struct {
	// Sets are the sets of origins the declarations planned name (observation targets, as a source's
	// Where): for each, mamelet reports the pending upgrades whose candidate comes from one of them,
	// and their change set. They allow nothing.
	Sets [][]string `json:"sets"`
	// InForce: the declaration in force has the upgrades of the node (File.InForce).
	InForce bool `json:"inForce,omitzero"`
	// Held are the origins of the declaration in force (File.Held: never sent to mamelet).
	Held []string `json:"held,omitempty"`
	// Refresh lets mamelet read the package lists again (apt-get update) when they are older than
	// AptListsMaxAge. Every plan of a declaration that has the upgrades of the node sets it; on a node
	// the Control Plane must not change (its own host, observe-only) Protect takes it away, as it
	// takes away every grant: mamelet then changes nothing there, the lists included.
	Refresh bool `json:"refresh,omitzero"`
	// Upgrade is an approved, one-time upgrade of exactly this change set.
	Upgrade *AptUpgrade `json:"upgrade,omitempty"`
}

// AptUpgrade is an approved, one-time upgrade: mamelet simulates apt-get install --only-upgrade of
// Packages ("<package>=<version>") again, and upgrades them only if the change set's digest is
// ChangeSet (and then with the same options and packages).
type AptUpgrade struct {
	// Origins are the origins the change set was worked out for (the declaration's): an apply of a
	// declaration of other origins withdraws it.
	Origins   []string     `json:"origins"`
	Packages  []string     `json:"packages"`
	ChangeSet string       `json:"changeSet"`
	Grant     noderev.Once `json:"grant"`
}

// ActionUpgradePackages is the action of an upgrade's grant.
const ActionUpgradePackages = "upgrade-packages"

// AptUpgradesState is what mamelet found of the upgrades of its node (Observed.AptUpgrades).
type AptUpgradesState struct {
	// ListsAt is when the newest release file of apt's lists (/var/lib/apt/lists/*_InRelease,
	// *_Release) was last modified: apt gives a file it downloads the time the server says it last
	// changed, so this is how new the newest index apt has is (zero: apt has no lists).
	ListsAt time.Time `json:"listsAt,omitzero"`
	// RefreshedAt is when an apt-get update mamelet ran last read every source without an error;
	// RefreshFailed says why its last one did not (what apt said, bounded), at RefreshFailedAt, when
	// that came after it.
	RefreshedAt     time.Time `json:"refreshedAt,omitzero"`
	RefreshFailed   string    `json:"refreshFailed,omitempty"`
	RefreshFailedAt time.Time `json:"refreshFailedAt,omitzero"`
	// DpkgInterrupted: dpkg did not finish what it was doing (/var/lib/dpkg/updates has entries: an
	// upgrade or an installation was cut short), and apt refuses to install until dpkg --configure -a.
	DpkgInterrupted bool `json:"dpkgInterrupted,omitzero"`
	// RebootRequired: the node says it needs a reboot (/run/reboot-required, which the packages of a
	// new kernel or libc write), for the packages of RebootPackages (/run/reboot-required.pkgs).
	RebootRequired bool     `json:"rebootRequired,omitzero"`
	RebootPackages []string `json:"rebootPackages,omitempty"`
	// Sets are the pending upgrades for each set of origins of the part.
	Sets []AptUpgradeSet `json:"sets,omitempty"`
	// Failed is why the last attempt of the upgrade the revision grants did not succeed (its recorded
	// detail), while the grant is in the revision.
	Failed string `json:"failed,omitempty"`
	// Error says why the upgrades could not be worked out (the simulation failed).
	Error string `json:"error,omitempty"`
}

// AptUpgradeSet is what is pending for one set of origins: the packages to upgrade ("<package>=
// <version>", the candidate) and what upgrading them changes (Changes, whose digest is Digest).
type AptUpgradeSet struct {
	Origins  []string    `json:"origins"`
	Packages []string    `json:"packages,omitempty"`
	Changes  []AptChange `json:"changes,omitempty"`
	Digest   string      `json:"digest,omitempty"`
	Error    string      `json:"error,omitempty"`
}

// AptChange is one line of an upgrade's change set: a package upgraded (From the installed version
// to Version), installed (a new dependency) or removed.
type AptChange struct {
	Action  string   `json:"action"` // noderev.ChangeUpgrade, ChangeInstall, ChangeRemove
	Package string   `json:"package"`
	From    string   `json:"from,omitempty"`
	Version string   `json:"version,omitempty"`
	Origins []string `json:"origins,omitempty"`
}

// Line is the change for a person: "upgrade bind9-libs 1:9.20.11-4 -> 1:9.20.15-1~deb13u1
// (Debian-Security:13/stable-security)".
func (c AptChange) Line() string {
	line := c.Action + " " + c.Package
	switch {
	case c.From != "" && c.Version != "":
		line += " " + c.From + " -> " + c.Version
	case c.Version != "":
		line += " " + c.Version
	case c.From != "":
		line += " " + c.From
	}
	if len(c.Origins) > 0 {
		line += " (" + strings.Join(c.Origins, ", ") + ")"
	}
	return line
}

// aptChangeSet sorts the changes (by package, then action; each one's origins sorted) and their digest:
// the sha256 of their deterministic JSON. The Control Plane and mamelet compute it alike.
func aptChangeSet(changes []AptChange) ([]AptChange, string) {
	out := slices.Clone(changes)
	if out == nil {
		out = []AptChange{}
	}
	for i := range out {
		out[i].Origins = slices.Sorted(slices.Values(out[i].Origins))
	}
	slices.SortFunc(out, func(a, b AptChange) int {
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		return strings.Compare(a.Action, b.Action)
	})
	data, err := json.Marshal(out, json.Deterministic(true))
	if err != nil {
		panic("linux: encoding failed: " + err.Error())
	}
	sum := sha256.Sum256(data)
	return out, "sha256:" + hex.EncodeToString(sum[:])
}

// Removes reports whether the set removes a package.
func (s AptUpgradeSet) Removes() bool {
	return slices.ContainsFunc(s.Changes, func(c AptChange) bool { return c.Action == noderev.ChangeRemove })
}

// set is what mamelet found for the origins (nil: it has not worked them out yet).
func (s AptUpgradesState) set(origins []string) *AptUpgradeSet {
	for i := range s.Sets {
		if slices.Equal(s.Sets[i].Origins, origins) {
			return &s.Sets[i]
		}
	}
	return nil
}

// The shapes of an upgrade's packages: a Debian package name with its architecture, if apt names one
// ("libc6:i386"), and a Debian version ("1:9.20.15-1~deb13u1").
var (
	aptQualifiedRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{1,127}(:[a-z0-9][a-z0-9-]{0,31})?$`)
	aptVersionRe   = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+~:-]{0,127}$`)
)

// splitPin is the package and version of "<package>=<version>", if it is one mamelet passes to
// apt-get (ok false otherwise).
func splitPin(s string) (pkg, version string, ok bool) {
	pkg, version, ok = strings.Cut(s, "=")
	return pkg, version, ok && aptQualifiedRe.MatchString(pkg) && aptVersionRe.MatchString(version)
}

// MaxAptUpgrades bounds the packages of one upgrade (more is a release upgrade, done by hand).
const MaxAptUpgrades = 1000

// maxAptUpgradeSets bounds the sets of origins mamelet works out (one simulation each, every round).
const maxAptUpgradeSets = 4

// aptUpgradesGrants are the one-time grants of the part's upgrades.
func aptUpgradesGrants(p Part) []noderev.Grant {
	if p.AptUpgrades == nil || p.AptUpgrades.Upgrade == nil {
		return nil
	}
	g := p.AptUpgrades.Upgrade.Grant
	return []noderev.Grant{{Operation: g.Operation, Attempt: g.Attempt, Action: g.Action, Expires: g.Expires}}
}

// handOutAptUpgrades: an upgrade whose grant has ended is not handed out, and no deadline is sent.
func handOutAptUpgrades(p *Part, now time.Time) {
	u := p.AptUpgrades
	if u == nil {
		return
	}
	c := *u
	if c.Upgrade != nil {
		if g := noderev.HandOut(&c.Upgrade.Grant, now); g != nil {
			up := *c.Upgrade
			up.Grant = *g
			c.Upgrade = &up
		} else {
			c.Upgrade = nil
		}
	}
	p.AptUpgrades = &c
}

// aptUpgradesPartProblems are what mamelet refuses of the part's upgrades (checkPart).
func aptUpgradesPartProblems(p Part) []string {
	u := p.AptUpgrades
	if u == nil {
		return nil
	}
	// Past maxAptUpgradeSets, mamelet works out the first ones only (observeAptUpgrades).
	var out []string
	for i, s := range u.Sets {
		if m, _ := aptOriginsProblem(s); m != "" {
			out = append(out, "the upgrades' origins: "+m)
		}
		if !slices.IsSorted(s) || i > 0 && slices.Compare(u.Sets[i-1], s) >= 0 {
			out = append(out, "the upgrades' sets of origins are not sorted, or one is there twice")
		}
	}
	if up := u.Upgrade; up != nil {
		if err := checkAptUpgrade(*up); err != nil {
			out = append(out, "the upgrade: "+err.Error())
		}
	}
	return out
}

// checkAptUpgrade checks an approved upgrade: its grant, its origins, packages and change set.
func checkAptUpgrade(up AptUpgrade) error {
	if !noderev.ValidOnce(up.Grant, ActionUpgradePackages) {
		return fmt.Errorf("the grant %+v is not valid", up.Grant)
	}
	return checkUpgradeOf(up)
}

// checkUpgradeOf checks what an upgrade upgrades: its origins, packages (each once) and change set.
func checkUpgradeOf(up AptUpgrade) error {
	switch {
	case !strings.HasPrefix(up.ChangeSet, "sha256:") || !names.SHA256(strings.TrimPrefix(up.ChangeSet, "sha256:")):
		return fmt.Errorf("the change set %q is not a digest", up.ChangeSet)
	case len(up.Packages) == 0 || len(up.Packages) > MaxAptUpgrades:
		return fmt.Errorf("%d packages; 1 to %d", len(up.Packages), MaxAptUpgrades)
	}
	if m, _ := aptOriginsProblem(up.Origins); m != "" {
		return fmt.Errorf("its origins: %s", m)
	}
	seen := map[string]bool{}
	for _, pin := range up.Packages {
		pkg, _, ok := splitPin(pin)
		switch {
		case !ok:
			return fmt.Errorf("%q is not a package and version mamelet upgrades", pin)
		case seen[pkg]:
			return fmt.Errorf("package %s is named twice", pkg)
		}
		seen[pkg] = true
	}
	return nil
}

// trimmed is the state with only its error and what says why its details were left out (why):
// encodeObserved past MaxObserved.
func (s AptUpgradesState) trimmed(why string) *AptUpgradesState {
	t := &AptUpgradesState{ListsAt: s.ListsAt, RefreshedAt: s.RefreshedAt, RefreshFailedAt: s.RefreshFailedAt,
		DpkgInterrupted: s.DpkgInterrupted, RebootRequired: s.RebootRequired, Error: why}
	for _, set := range s.Sets {
		t.Sets = append(t.Sets, AptUpgradeSet{Origins: set.Origins, Error: why})
	}
	return t
}
