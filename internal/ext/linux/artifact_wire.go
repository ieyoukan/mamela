package linux

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/names"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// NodeArtifact's members of the linux part of a revision (Part.Artifacts) and of a report
// (Observed.Artifacts).

// Artifact is an artifact mamelet observes, and installs once if Install grants it.
type Artifact struct {
	Path string `json:"path"`
	// Owner and Group are what mamelet reports the existence of (Observed.Users and Groups): those
	// the declaration names. They allow nothing.
	Owner string `json:"owner"`
	Group string `json:"group"`
	// Install is an approved, one-time installation of exactly this download.
	Install *Install `json:"install,omitempty"`
}

// Install is an approved, one-time installation of an artifact: mamelet downloads URL (https), checks
// that its sha256 is SHA256 (or installs nothing), takes the member Extract of it when Extract is
// set (a gzip-compressed tar), installs it with Mode, Owner and Group, then restarts the running
// services of Restart.
type Install struct {
	URL     string   `json:"url"`
	SHA256  string   `json:"sha256"`
	Extract string   `json:"extract,omitempty"`
	Mode    string   `json:"mode"`
	Owner   string   `json:"owner"`
	Group   string   `json:"group"`
	Restart []string `json:"restart,omitempty"`
	// Grant is the core's one-time permission (its deadline is the Control Plane's, never sent).
	Grant noderev.Once `json:"grant"`
}

// ActionInstallArtifact is the action of an artifact's grant.
const ActionInstallArtifact = "install-artifact"

// ArtifactState is what is at an artifact's path (FileState: Large is a file larger than
// MaxArtifact, which is not hashed), and, when it is still the file mamelet installed there, where
// that came from.
type ArtifactState struct {
	FileState `json:",inline"`
	// From is the download mamelet installed at the path, when the file there is exactly the one it
	// installed (its sha256 is the one mamelet recorded); nil when mamelet did not install it, or it
	// changed since (by hand, a package, another program).
	From *Source `json:"from,omitempty"`
}

// Source is a download mamelet installed: its URL, its sha256, and the member it took out of it.
type Source struct {
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Extract string `json:"extract,omitempty"`
}

func (p *Part) artifact(path string) *Artifact {
	for i := range p.Artifacts {
		if p.Artifacts[i].Path == path {
			return &p.Artifacts[i]
		}
	}
	return nil
}

func (o Observed) artifact(path string) *ArtifactState {
	for i := range o.Artifacts {
		if o.Artifacts[i].Path == path {
			return &o.Artifacts[i]
		}
	}
	return nil
}

// artifactGrants are the one-time grants of the part's installations (noderev.Part.Grants).
func artifactGrants(p Part) []noderev.Grant {
	var out []noderev.Grant
	for _, a := range p.Artifacts {
		if in := a.Install; in != nil {
			out = append(out, noderev.Grant{Operation: in.Grant.Operation, Attempt: in.Grant.Attempt, Action: in.Grant.Action, Expires: in.Grant.Expires})
		}
	}
	return out
}

// handOutArtifacts is the part's installations as mamelet gets them at now: one whose grant has
// ended is not handed out, and no deadline is sent.
func handOutArtifacts(p *Part, now time.Time) {
	for i := range p.Artifacts {
		if in := p.Artifacts[i].Install; in != nil {
			if g := noderev.HandOut(&in.Grant, now); g != nil {
				c := *in
				c.Grant = *g
				p.Artifacts[i].Install = &c
			} else {
				p.Artifacts[i].Install = nil
			}
		}
	}
}

// checkArtifacts is what mamelet checks of the part's artifacts (checkPart): each path once, and
// not a file's too; the names; and each installation.
func checkArtifacts(p Part, add func(format string, args ...any)) {
	seen := map[string]bool{}
	for _, f := range p.Files {
		seen[f.Path] = true
	}
	for _, a := range p.Artifacts {
		if seen[a.Path] {
			add("artifact %q appears twice, or is a file's path too", a.Path)
		}
		seen[a.Path] = true
		if !ValidAccount(a.Owner) || !ValidAccount(a.Group) {
			add("artifact %q: owner %q / group %q are not names of a user and a group", a.Path, a.Owner, a.Group)
		}
		if in := a.Install; in != nil {
			if err := checkInstall(*in); err != nil {
				add("artifact %q: %v", a.Path, err)
			}
		}
	}
}

// checkInstall checks an approved installation: its grant, its URL, sha256 and member, its mode,
// owner and group, and the services it restarts.
func checkInstall(in Install) error {
	switch {
	case !noderev.ValidOnce(in.Grant, ActionInstallArtifact):
		return fmt.Errorf("the install grant %+v is not valid", in.Grant)
	case !names.SHA256(in.SHA256):
		return fmt.Errorf("%q is not a sha256", in.SHA256)
	case !ValidAccount(in.Owner) || !ValidAccount(in.Group):
		return fmt.Errorf("owner %q / group %q are not names of a user and a group", in.Owner, in.Group)
	}
	if err := CheckURL(in.URL); err != nil {
		return err
	}
	if in.Extract != "" {
		if err := CheckExtract(in.Extract); err != nil {
			return err
		}
	}
	if _, err := ParseMode(in.Mode); err != nil {
		return err
	}
	for _, s := range in.Restart {
		if !names.Service(s) {
			return fmt.Errorf("%q is not a service name", s)
		}
		if why := CheckRestart(s); why != "" {
			return errors.New(why)
		}
	}
	return nil
}

// sortArtifacts puts the part's artifacts in the order of their paths (setPart).
func sortArtifacts(p *Part) {
	slices.SortFunc(p.Artifacts, func(a, b Artifact) int { return strings.Compare(a.Path, b.Path) })
}
