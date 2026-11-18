package linux

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/names"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// AptRepo is an APT source mamelet observes (Part.AptRepos), and adds once if Add grants it.
type AptRepo struct {
	Name string `json:"name"`
	// URIs and Suites are where apt reads the source from: mamelet reports which of them apt has no
	// release file of yet (no apt-get update has read them), and which other source files of the node
	// name them too. They allow nothing.
	URIs   []string `json:"uris"`
	Suites []string `json:"suites"`
	// Add is an approved, one-time addition (or change) of exactly this source and key.
	Add *AptRepoAdd `json:"add,omitempty"`
}

// AptRepoAdd is an approved, one-time addition of an APT source: mamelet downloads the key from
// KeyURL, keeps it only if its sha256 is KeySHA256, writes it and the source's file (AptSources), and
// runs apt-get update.
type AptRepoAdd struct {
	URIs          []string `json:"uris"`
	Suites        []string `json:"suites"`
	Components    []string `json:"components"`
	Architectures []string `json:"architectures,omitempty"`
	KeyURL        string   `json:"keyUrl"`
	KeySHA256     string   `json:"keySha256"`
	// SourcesSHA256 is the sha256 of the source's file the approved plan showed: mamelet renders the
	// file from the members above, for an ASCII-armored and for a binary key, and refuses unless one of
	// the two is it.
	SourcesSHA256 string       `json:"sourcesSha256"`
	Grant         noderev.Once `json:"grant"`
}

// ActionAddAptRepo is the action of a source's grant.
const ActionAddAptRepo = "add-apt-repo"

// AptRepoState is what mamelet found of an APT source (Observed.AptRepos): its file and the key in
// each of its two forms (only their sha256, never their content).
type AptRepoState struct {
	Name       string    `json:"name"`
	Sources    FileState `json:"sources"`
	KeyArmored FileState `json:"keyAsc"`
	KeyBinary  FileState `json:"keyGpg"`
	// Unread are the URIs and suites ("<uri> <suite>") apt has no release file of in
	// /var/lib/apt/lists: no apt-get update has read them (or it failed).
	Unread []string `json:"unread,omitempty"`
	// Conflicts are the other source files of the node that name one of its URIs and suites: apt
	// refuses one source with two keys, and then reads no source at all.
	Conflicts []string `json:"conflicts,omitempty"`
	// Error says why it could not be observed (a name mamelet refuses included).
	Error string `json:"error,omitempty"`
}

// spec is the source the addition grants, as the graph declares it (no node).
func (a AptRepoAdd) spec(name string) AptRepoSpec {
	return AptRepoSpec{Name: name, URIs: a.URIs, Suites: a.Suites, Components: a.Components, Architectures: a.Architectures,
		KeyURL: a.KeyURL, KeySHA256: a.KeySHA256}
}

func (p *Part) aptRepo(name string) *AptRepo {
	for i := range p.AptRepos {
		if p.AptRepos[i].Name == name {
			return &p.AptRepos[i]
		}
	}
	return nil
}

func sortAptRepos(p *Part) {
	slices.SortFunc(p.AptRepos, func(a, b AptRepo) int { return strings.Compare(a.Name, b.Name) })
}

// aptRepoGrants are the one-time grants of the part's sources.
func aptRepoGrants(p Part) []noderev.Grant {
	var out []noderev.Grant
	for _, r := range p.AptRepos {
		if a := r.Add; a != nil {
			out = append(out, noderev.Grant{Operation: a.Grant.Operation, Attempt: a.Grant.Attempt, Action: a.Grant.Action, Expires: a.Grant.Expires})
		}
	}
	return out
}

// handOutAptRepos: an addition whose grant has ended is not handed out, and no deadline is sent.
func handOutAptRepos(p *Part, now time.Time) {
	for i := range p.AptRepos {
		if a := p.AptRepos[i].Add; a != nil {
			if g := noderev.HandOut(&a.Grant, now); g != nil {
				c := *a
				c.Grant = *g
				p.AptRepos[i].Add = &c
			} else {
				p.AptRepos[i].Add = nil
			}
		}
	}
}

// aptRepoPartProblems are what mamelet refuses of the part's sources (checkPart).
func aptRepoPartProblems(p Part) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range p.AptRepos {
		if seen[r.Name] {
			out = append(out, fmt.Sprintf("APT source %q appears twice", r.Name))
		}
		seen[r.Name] = true
		// What mamelet observes is checked as a declaration is.
		for _, pr := range aptWhereProblems(r.Name, r.URIs, r.Suites) {
			out = append(out, fmt.Sprintf("APT source %q: %s", r.Name, pr.message))
		}
		if a := r.Add; a != nil {
			if err := checkAptRepoAdd(r.Name, *a); err != nil {
				out = append(out, fmt.Sprintf("APT source %q: %v", r.Name, err))
			}
		}
	}
	return out
}

// checkAptRepoAdd checks an approved addition: its grant, the source and key, and that the file
// they render to is one the approved plan showed.
func checkAptRepoAdd(name string, a AptRepoAdd) error {
	if !noderev.ValidOnce(a.Grant, ActionAddAptRepo) {
		return fmt.Errorf("the grant %+v is not valid", a.Grant)
	}
	if ps := aptRepoProblems(a.spec(name)); len(ps) > 0 {
		var ms []string
		for _, p := range ps {
			ms = append(ms, p.message)
		}
		return fmt.Errorf("the source is not one Mamela writes: %s", strings.Join(ms, "; "))
	}
	armored, binary := aptSourcesSHA256(a.spec(name))
	if !names.SHA256(a.SourcesSHA256) || (a.SourcesSHA256 != armored && a.SourcesSHA256 != binary) {
		return fmt.Errorf("the source's file is not the one approved (sha256 %s)", a.SourcesSHA256)
	}
	return nil
}
