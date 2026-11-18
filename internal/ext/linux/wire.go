package linux

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ieyoukan/mamela/internal/names"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// Name is the extension's name: the key of its part of a revision and of a report.
const Name = "linux"

// Version is the version of the linux part of a revision this Control Plane writes and this mamelet
// reads. A member a released mamelet does not know raises it (the mamelet refuses a part it cannot
// read, and the Control Plane gives a part only to a mamelet that lists this version or a later one).
const Version = 1

// Release is the mamelet release that brought Version: the plan names it to a node whose mamelet is
// older (MAM-PLAN-0122).
const Release = "0.10.0"

// Part is the linux part of a node's revision (Revision.Ext["linux"]): what mamelet observes, and
// the one-time grants of what it writes. The kinds that follow add members of their own.
type Part struct {
	// Files are the files mamelet observes, by path (sorted), each with its approved write, if any.
	Files []File `json:"files,omitempty"`

	// NodeAptRepo (aptrepo_wire.go): the APT sources mamelet observes, by name (sorted), each with
	// its approved addition, if any.
	AptRepos []AptRepo `json:"aptRepos,omitempty"`
}

// File is a file mamelet observes, and writes once if Write grants it.
type File struct {
	Path string `json:"path"`
	// Owner and Group are what mamelet reports the existence of (Observed.Users and Groups): those
	// the declaration names. They allow nothing.
	Owner string `json:"owner"`
	Group string `json:"group"`
	// Write is an approved, one-time write of exactly this content, mode and owner.
	Write *Write `json:"write,omitempty"`
}

// Write is an approved, one-time write of a file: mamelet writes exactly Content (whose sha256 is
// SHA256, or nothing), with Mode, Owner and Group, then restarts the running services of Restart.
type Write struct {
	Content string   `json:"content"`
	SHA256  string   `json:"sha256"`
	Mode    string   `json:"mode"`
	Owner   string   `json:"owner"`
	Group   string   `json:"group"`
	Restart []string `json:"restart,omitempty"`
	// Grant is the core's one-time permission: its operation and attempt (and its deadline, the
	// Control Plane's bookkeeping, which is never sent).
	Grant noderev.Once `json:"grant"`
}

// ActionWrite is the action of a file's grant.
const ActionWrite = "write-file"

// Observed is the linux part of a report (Report.Ext["linux"]): what mamelet found of what the part
// of its revision names. Mamelet never sends a file's content, only its sha256.
type Observed struct {
	Files []FileState `json:"files,omitempty"`
	// Users and Groups say, of each name the part names as an owner or a group, whether the node has
	// that user or group.
	Users  map[string]bool `json:"users,omitempty"`
	Groups map[string]bool `json:"groups,omitempty"`

	// NodeAptRepo (aptrepo_wire.go): what mamelet found of each APT source of the part.
	AptRepos []AptRepoState `json:"aptRepos,omitempty"`
}

// FileState is what is at a file's path.
type FileState struct {
	Path string `json:"path"`
	// Exists: something is at the path (a regular file, or what Unsafe says).
	Exists bool   `json:"exists,omitzero"`
	SHA256 string `json:"sha256,omitempty"` // of a regular file, unless it is Large
	// Large: a regular file larger than any content Mamela writes (MaxContent); it is not hashed.
	Large bool   `json:"large,omitzero"`
	Mode  string `json:"mode,omitempty"`  // of a regular file: "0644" (with the setuid bits)
	Owner string `json:"owner,omitempty"` // the user's name ("#1001" when the node has no name for the id)
	Group string `json:"group,omitempty"`
	// Missing are the directories above it that do not exist, outermost first: writing it creates
	// them (0755, root's).
	Missing []string `json:"missing,omitempty"`
	// Unsafe says why mamelet does not write there: the path is a symbolic link, a directory or a
	// device, or a directory above it is a link or not a directory. Mamela never follows a link.
	Unsafe string `json:"unsafe,omitempty"`
	// Error says why it could not be observed (including a path mamelet refuses: CheckPath).
	Error string `json:"error,omitempty"`
}

// MaxContent bounds a file's content, in bytes.
const MaxContent = 256 << 10

// MaxNodeContent bounds the content of all the files declared on one node: the approved writes ride
// in the node's revision (MAM-GRAPH-0106 beyond it).
const MaxNodeContent = 1 << 20

// ContentSHA256 is the sha256 (hex) of a file's content.
func ContentSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// encode is the JSON of a part or an observation, deterministic.
func encode(v any) jsontext.Value {
	out, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		panic("linux: encoding failed: " + err.Error())
	}
	return out
}

// decodeStrict reads a part or an observation, refusing what it does not know.
func decodeStrict(data jsontext.Value, v any) error {
	return json.Unmarshal(data, v, json.RejectUnknownMembers(true))
}

// partOf is the linux part of a revision (empty when it has none). A part this Control Plane cannot
// read (one a newer one wrote) is taken as empty: what replaces it then has no grant of the old one.
func partOf(r *noderev.Revision) Part {
	var p Part
	if raw, ok := r.Ext[Name]; ok && decodeStrict(raw, &p) != nil {
		return Part{}
	}
	return p
}

// setPart puts the part into the revision (and takes the member out when the part is empty).
func setPart(r *noderev.Revision, p Part) {
	ext := map[string]jsontext.Value{}
	for k, v := range r.Ext {
		ext[k] = v
	}
	if len(p.Files) == 0 && len(p.AptRepos) == 0 {
		delete(ext, Name)
	} else {
		slices.SortFunc(p.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
		sortAptRepos(&p) // NodeAptRepo
		ext[Name] = encode(p)
	}
	r.Ext = ext
	if len(ext) == 0 {
		r.Ext = nil
	}
}

func (p *Part) file(path string) *File {
	for i := range p.Files {
		if p.Files[i].Path == path {
			return &p.Files[i]
		}
	}
	return nil
}

// partGrants are the one-time grants of a part (noderev.Part.Grants).
func partGrants(raw jsontext.Value) ([]noderev.Grant, error) {
	var p Part
	if err := decodeStrict(raw, &p); err != nil {
		return nil, err
	}
	var out []noderev.Grant
	for _, f := range p.Files {
		if w := f.Write; w != nil {
			out = append(out, noderev.Grant{Operation: w.Grant.Operation, Attempt: w.Grant.Attempt, Action: w.Grant.Action, Expires: w.Grant.Expires})
		}
	}
	out = append(out, aptRepoGrants(p)...) // NodeAptRepo
	return out, nil
}

// partForMamelet is the part as mamelet gets it at now (noderev.Part.ForMamelet): a write whose
// grant has ended is not handed out (nor its content), and no deadline is sent.
func partForMamelet(raw jsontext.Value, now time.Time) (jsontext.Value, error) {
	var p Part
	if err := decodeStrict(raw, &p); err != nil {
		return nil, err
	}
	for i := range p.Files {
		if w := p.Files[i].Write; w != nil {
			if g := noderev.HandOut(&w.Grant, now); g != nil {
				c := *w
				c.Grant = *g
				p.Files[i].Write = &c
			} else {
				p.Files[i].Write = nil
			}
		}
	}
	handOutAptRepos(&p, now) // NodeAptRepo
	return encode(p), nil
}

// checkPart is what mamelet checks of the part of a verified revision before it uses it
// (mamelet.Extension.Check): its shape, names and grants. A path is checked by itself when it is
// observed and written (CheckPath), so that one path mamelet refuses does not stop the others.
func checkPart(raw jsontext.Value) error {
	var p Part
	if err := decodeStrict(raw, &p); err != nil {
		return fmt.Errorf("the linux part of the revision cannot be read: %w", err)
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	seen := map[string]bool{}
	for _, f := range p.Files {
		if seen[f.Path] {
			add("file %q appears twice", f.Path)
		}
		seen[f.Path] = true
		if !ValidAccount(f.Owner) || !ValidAccount(f.Group) {
			add("file %q: owner %q / group %q are not names of a user and a group", f.Path, f.Owner, f.Group)
		}
		if w := f.Write; w != nil {
			if err := checkWrite(*w); err != nil {
				add("file %q: %v", f.Path, err)
			}
		}
	}
	problems = append(problems, aptRepoPartProblems(p)...) // NodeAptRepo
	if len(problems) > 0 {
		return fmt.Errorf("invalid linux part: %s", strings.Join(problems, "; "))
	}
	return nil
}

// checkWrite checks an approved write: its grant, its content against its sha256, its mode, owner
// and group, and the services it restarts.
func checkWrite(w Write) error {
	switch {
	case !noderev.ValidOnce(w.Grant, ActionWrite):
		return fmt.Errorf("the write grant %+v is not valid", w.Grant)
	case !names.SHA256(w.SHA256) || ContentSHA256(w.Content) != w.SHA256:
		return fmt.Errorf("the content is not the one approved (sha256 %s)", w.SHA256)
	case len(w.Content) > MaxContent || !utf8.ValidString(w.Content) || strings.ContainsRune(w.Content, 0):
		return fmt.Errorf("the content is too large or not text")
	case !ValidAccount(w.Owner) || !ValidAccount(w.Group):
		return fmt.Errorf("owner %q / group %q are not names of a user and a group", w.Owner, w.Group)
	}
	if _, err := ParseMode(w.Mode); err != nil {
		return err
	}
	for _, s := range w.Restart {
		if !names.Service(s) {
			return fmt.Errorf("%q is not a service name", s)
		}
	}
	return nil
}
