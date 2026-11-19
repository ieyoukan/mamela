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
	// Where are where the declarations read the source from (its URIs and suites, each declaration's
	// once: the observation targets of wire.go): of each, mamelet reports which apt has no release file
	// of yet (no apt-get update has read them), and which other source files of the node name them too.
	// They allow nothing.
	Where []AptWhere `json:"where,omitempty"`
	// InForce: the declaration in force has the source (File.InForce).
	InForce bool `json:"inForce,omitzero"`
	// Add is an approved, one-time addition (or change) of exactly this source and key.
	Add *AptRepoAdd `json:"add,omitempty"`
}

// AptWhere are the URIs and suites of one declaration of a source.
type AptWhere struct {
	URIs   []string `json:"uris"`
	Suites []string `json:"suites"`
}

// same reports whether two are the same URIs and suites.
func (w AptWhere) same(o AptWhere) bool {
	return slices.Equal(w.URIs, o.URIs) && slices.Equal(w.Suites, o.Suites)
}

// compareWhere orders the URIs and suites of a source's entry.
func compareWhere(a, b AptWhere) int {
	if c := slices.Compare(a.URIs, b.URIs); c != 0 {
		return c
	}
	return slices.Compare(a.Suites, b.Suites)
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
	SourcesSHA256 string `json:"sourcesSha256"`
	// Before is what the approved plan saw of the source's file and of its key's two files: mamelet
	// writes them only while they are still so, or once the source and its key are already as approved
	// (compare and swap).
	Before AptRepoBefore `json:"before"`
	Grant  noderev.Once  `json:"grant"`
}

// AptRepoBefore is what an approved addition of a source was planned against.
type AptRepoBefore struct {
	Sources    Before `json:"sources"`
	KeyArmored Before `json:"keyAsc"`
	KeyBinary  Before `json:"keyGpg"`
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
	// Where are what mamelet found of each URIs and suites of the part's entry (AptRepo.Where): each
	// says which it was computed for, so a plan uses only the one of its own declaration.
	Where []AptWhereState `json:"where,omitempty"`
	// Failed is why the last attempt of the addition the revision grants did not succeed (its
	// recorded detail: what apt-get update said of the source, a key that could not be downloaded),
	// while the grant is in the revision; the plan says it where apt has not read the source.
	Failed string `json:"failed,omitempty"`
	// Error says why it could not be observed (a name mamelet refuses included).
	Error string `json:"error,omitempty"`
}

// AptWhereState is what mamelet found of one URIs and suites of a source.
type AptWhereState struct {
	AptWhere `json:",inline"`
	// Unread are the URIs and suites ("<uri> <suite>") apt has no release file of in
	// /var/lib/apt/lists: no apt-get update has read them (or it failed).
	Unread []string `json:"unread,omitempty"`
	// Conflicts are the other source files of the node that name one of these URIs and suites: apt
	// refuses one source with two keys, and then reads no source at all.
	Conflicts []string `json:"conflicts,omitempty"`
}

// where is what mamelet found of the source for the URIs and suites w (nil: it has not looked at them
// yet).
func (s AptRepoState) where(w AptWhere) *AptWhereState {
	for i := range s.Where {
		if s.Where[i].same(w) {
			return &s.Where[i]
		}
	}
	return nil
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
		// What mamelet observes is checked as a declaration is (past maxTargets, mamelet observes the
		// first ones only: fit).
		for i, w := range r.Where {
			if i > 0 && compareWhere(r.Where[i-1], w) >= 0 {
				out = append(out, fmt.Sprintf("APT source %q: its URIs and suites are not sorted, or one is there twice", r.Name))
			}
			for _, pr := range aptWhereProblems(r.Name, w.URIs, w.Suites) {
				out = append(out, fmt.Sprintf("APT source %q: %s", r.Name, pr.message))
			}
		}
		if !ValidAptRepoName(r.Name) {
			out = append(out, fmt.Sprintf("%q is not the name of an APT source", r.Name))
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
	for _, b := range []struct {
		what string
		b    Before
	}{{"the source's file", a.Before.Sources}, {"the key (.asc)", a.Before.KeyArmored}, {"the key (.gpg)", a.Before.KeyBinary}} {
		if why := b.b.check(); why != "" {
			return fmt.Errorf("what the plan saw of %s: %s", b.what, why)
		}
	}
	return nil
}
