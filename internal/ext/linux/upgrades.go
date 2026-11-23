package linux

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
)

// KindAptUpgrades is the approved upgrades of a node's installed packages from some APT origins (a
// distribution's security archive): never automatic, never more than what the plan showed.
const KindAptUpgrades graph.Kind = "NodeAptUpgrades"

// AptUpgradesSpec says that the pending upgrades of the packages installed on Node whose candidate
// version comes from one of Origins are applied, each time with an approval. An origin is written as
// apt shows it ("<label>:<version>/<suite>", the policy's allowedAptOrigins form), or its beginning:
// "Debian-Security:" is Debian's security archive, "Debian:" its point releases and stable-updates,
// "Ubuntu:24.04/noble-security" Ubuntu's. The resource's name is "<node>/apt-upgrades" (one a node).
//
// mamelet works out the pending upgrades (apt-get -s dist-upgrade: the installed packages whose
// candidate comes from one of the origins), simulates exactly those (apt-get -s install --only-upgrade
// pkg=version ...) and reports the change set; the plan shows every package with its versions and
// origin, and the change set's digest. Approved, mamelet simulates again and upgrades only if the
// change set is still the approved one. It reads the package lists again (apt-get update) when they
// are older than AptListsMaxAge, as Debian's apt-daily does: the one thing it does without an approval.
type AptUpgradesSpec struct {
	Node    string
	Origins []string // sorted, each once
}

// Bounds of a declaration's origins.
const (
	MaxAptOrigins = 8
	maxAptOrigin  = 100
)

// AptUpgradesOf is the upgrades a resource declares (nil for a resource of another kind).
func AptUpgradesOf(r graph.Resource) *AptUpgradesSpec { s, _ := r.Ext.(*AptUpgradesSpec); return s }

// AptUpgradesName is the name of the resource of the upgrades of node.
func AptUpgradesName(node string) string { return node + "/apt-upgrades" }

// aptOriginRe is an APT origin as apt-get -s shows a version's (pkgCache::VerIterator::RelStr): a
// label (letters, digits, . _ + ~ - and inner spaces, "Docker CE"), ":", then the release's version
// and suite ("13/stable-security"); or the beginning of one, ending after the ":" at the earliest. A
// beginning shorter than the label and its ":" ("Deb") would take any label that starts so.
var aptOriginRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9 ._+~-]*[A-Za-z0-9._+~-])?:[A-Za-z0-9._+~:/-]*$`)

// ValidAptOrigin reports whether s can be an origin a declaration names (and mamelet matches).
func ValidAptOrigin(s string) bool { return len(s) <= maxAptOrigin && aptOriginRe.MatchString(s) }

// aptOriginsProblem says what is wrong with a list of origins ("" nothing): 1 to MaxAptOrigins, each
// valid, none twice. The graph checks a declaration with it, and mamelet a revision's.
func aptOriginsProblem(origins []string) (message, hint string) {
	const shape = `an origin as apt-get -s shows it, "<label>:<version>/<suite>", or its beginning up to the ":" at least, such as "Debian-Security:" (Debian's security archive) or "Ubuntu:24.04/noble-security"`
	switch {
	case len(origins) == 0:
		return "there is no origin", "name the origins whose upgrades are applied: " + shape
	case len(origins) > MaxAptOrigins:
		return fmt.Sprintf("%d origins; at most %d", len(origins), MaxAptOrigins), "name fewer, or a shorter beginning of them"
	}
	seen := map[string]bool{}
	for _, o := range origins {
		switch {
		case !ValidAptOrigin(o):
			return fmt.Sprintf("%s is not an APT origin", quote(o)), shape
		case seen[o]:
			return fmt.Sprintf("origin %s is named twice", quote(o)), "name each once"
		}
		seen[o] = true
	}
	return "", ""
}

// matchesOrigin reports whether an origin of a version (apt's "Debian-Security:13/stable-security")
// is one of origins (each the whole origin, or its beginning), as the policy's allowedAptOrigins
// match.
func matchesOrigin(origins []string, o string) bool {
	return slices.ContainsFunc(origins, func(p string) bool { return p != "" && strings.HasPrefix(o, p) })
}

type wireAptUpgrades struct {
	Node    *string   `json:"node"`
	Origins *[]string `json:"origins"`
}

type canonAptUpgrades struct {
	Node    string   `json:"node"`
	Origins []string `json:"origins"`
}

// aptUpgradesKind is NodeAptUpgrades in every layer.
var aptUpgradesKind = kind{
	def: graph.KindDef{
		Kind: KindAptUpgrades, Order: 10,
		New:      func() any { return &AptUpgradesSpec{} },
		Decode:   decodeAptUpgrades,
		Validate: validateAptUpgrades,
		Canonical: func(spec any) any {
			s := spec.(*AptUpgradesSpec)
			return canonAptUpgrades{Node: s.Node, Origins: nonNil(s.Origins)}
		},
		Node: func(spec any) string { return spec.(*AptUpgradesSpec).Node },
		Fields: func(spec any) map[string]string {
			return map[string]string{"origins": strings.Join(spec.(*AptUpgradesSpec).Origins, " ")}
		},
		Describe: func(spec any) string {
			s := spec.(*AptUpgradesSpec)
			return fmt.Sprintf("upgrades of the installed packages from %s on %s", strings.Join(s.Origins, ", "), s.Node)
		},
	},
	codes:    aptUpgradesPlanCodes,
	planner:  aptUpgradesPlanner,
	ops:      []operation{upgradePackagesOp},
	manifest: aptUpgradesManifest,
	publish:  publishAptUpgrades,
	prune:    pruneAptUpgrades,
	protect:  protectAptUpgrades,
	observe:  observeAptUpgrades,
	act:      actAptUpgrades,
	results:  aptUpgradesResults,
}

func decodeAptUpgrades(d *graph.SpecDecoder) any {
	var s wireAptUpgrades
	if !d.Strict(&s) {
		return nil
	}
	d.Need(s.Node != nil, "node")
	d.Need(s.Origins != nil, "origins")
	if !d.OK() {
		return nil
	}
	// The order the declaration names them in means nothing: one set of origins is one set.
	return &AptUpgradesSpec{Node: *s.Node, Origins: slices.Sorted(slices.Values(*s.Origins))}
}

func validateAptUpgrades(v *graph.Validator, r graph.Resource) {
	s := AptUpgradesOf(r)
	v.NodeRef(r, s.Node)
	if m, h := aptOriginsProblem(s.Origins); m != "" {
		v.Problem(r, graph.CodeInvalidValue, "spec.origins", m, h)
	}
	if want := AptUpgradesName(s.Node); r.Name != want {
		v.Problem(r, graph.CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/apt-upgrades)", want), "")
	}
}
