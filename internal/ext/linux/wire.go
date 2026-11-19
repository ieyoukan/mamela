package linux

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
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
//
// What an entry asks mamelet to observe (an owner's existence, a shell, the URIs of a source) are
// observation targets, not permissions: a plan of any declaration (a pull request's too) adds its own
// (Publish), so that the observation of each declaration is in the report whichever was planned last
// and the revision does not change from one plan to the next; the apply of an approved plan sets them
// to that declaration's (Prune), and a bound keeps them few (targets.go). A grant carries every value it
// needs, so what the targets say never changes what mamelet does.
type Part struct {
	// Files are the files mamelet observes, by path (sorted), each with its approved write, if any.
	Files []File `json:"files,omitempty"`

	// Users are the users mamelet observes, by name (sorted), each with its approved change, if any
	// (NodeUser: user_wire.go).
	Users []User `json:"users,omitempty"`

	// Artifacts are the artifacts mamelet observes, by path (sorted), each with its approved
	// installation, if any (NodeArtifact: artifact_wire.go).
	Artifacts []Artifact `json:"artifacts,omitempty"`

	// NodeAptRepo (aptrepo_wire.go): the APT sources mamelet observes, by name (sorted), each with
	// its approved addition, if any.
	AptRepos []AptRepo `json:"aptRepos,omitempty"`
}

// File is a file mamelet observes, and writes once if Write grants it.
type File struct {
	Path string `json:"path"`
	// Owners and Groups are the users and groups whose existence mamelet reports (Observed.Users and
	// Groups): those the declarations name (sorted). They allow nothing.
	Owners []string `json:"owners,omitempty"`
	Groups []string `json:"groups,omitempty"`
	// InForce: the declaration in force has the file (the declaration of the plan applied last, or of
	// an approved write). A plan of a declaration that does not have it shows it as kept (retained);
	// what only another declaration planned is not shown at all. Mamelet does not read it.
	InForce bool `json:"inForce,omitzero"`
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
	// Before is what was at the path when the approved plan was made: mamelet writes only while the
	// file is still so, or once its content is already the approved one (compare and swap).
	Before Before `json:"before"`
	// Grant is the core's one-time permission: its operation and attempt (and its deadline, the
	// Control Plane's bookkeeping, which is never sent).
	Grant noderev.Once `json:"grant"`
}

// Before is what an approved change was planned against: what the observation the plan was made of
// found at a path (nothing, a regular file of that sha256, or one too large to hash, known by its
// stamp). Mamelet changes
// the path only while it is still so, or once it is already as approved; otherwise it refuses, and the
// next plan shows what is there now. A change that was approved for one state of the node never
// overwrites another (a person's edit, another plan's apply).
type Before struct {
	Exists bool   `json:"exists,omitzero"`
	SHA256 string `json:"sha256,omitempty"`
	Large  bool   `json:"large,omitzero"`
	// Stamp is the stamp of a file too large to hash (FileState.Stamp): without it, a large file is
	// never taken for the one the plan saw.
	Stamp string `json:"stamp,omitempty"`
}

// beforeOf is the before-state of what an observation found at a path.
func beforeOf(f FileState) Before {
	return Before{Exists: f.Exists, SHA256: f.SHA256, Large: f.Large, Stamp: f.Stamp}
}

// Is reports whether the path is still as the plan saw it. A file too large to hash is the one the
// plan saw only while it is the same file, unchanged since (the same stamp: its inode, size and the
// times of its last changes); one the plan has no stamp of never is, so that a change approved for
// one large file never overwrites another (they have no sha256 to tell them apart).
func (b Before) Is(f FileState) bool {
	if b.Large || f.Large {
		return b.Exists && f.Exists && b.Large && f.Large && b.Stamp != "" && b.Stamp == f.Stamp
	}
	return b.Exists == f.Exists && b.SHA256 == f.SHA256
}

// String is the before-state for a person ("absent", "sha256 0123456789ab", "a file too large to
// hash (inode 1234, 300000 bytes)").
func (b Before) String() string {
	switch {
	case !b.Exists:
		return "absent"
	case b.Large:
		if ino, size, ok := stampFields(b.Stamp); ok {
			return fmt.Sprintf("a file too large to hash (inode %s, %s bytes)", ino, size)
		}
		return "a file too large to hash"
	case b.SHA256 == "":
		return "not a regular file"
	}
	return "sha256 " + b.SHA256[:min(12, len(b.SHA256))]
}

// check says what is wrong with a before-state in a revision ("" nothing).
func (b Before) check() string {
	switch {
	case !b.Exists && (b.SHA256 != "" || b.Large || b.Stamp != ""):
		return "a path that does not exist has no content"
	case b.Exists && b.Large && b.SHA256 != "":
		return "a file too large to hash has no sha256"
	case b.Exists && !b.Large && !names.SHA256(b.SHA256):
		return fmt.Sprintf("%q is not a sha256", b.SHA256)
	case b.Stamp != "" && (!b.Large || !stampRe.MatchString(b.Stamp)):
		return fmt.Sprintf("%q is not the stamp of a file too large to hash", b.Stamp)
	}
	return ""
}

// stampRe is a stamp as stampText writes it.
var stampRe = regexp.MustCompile(`^[0-9]{1,20}:[0-9]{1,20}:[0-9]{1,20}:-?[0-9]{1,20}:-?[0-9]{1,20}$`)

// stampText is a file's stamp as FileState and Before carry it: "<device>:<inode>:<size>:<mtime
// ns>:<ctime ns>" ("" when the system gave none).
func stampText(s fileStamp) string {
	if !s.OK {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", s.Dev, s.Ino, s.Size, s.Mtime, s.Ctime)
}

// stampFields are the inode and size of a stamp, for a person.
func stampFields(stamp string) (ino, size string, ok bool) {
	f := strings.Split(stamp, ":")
	if len(f) != 5 {
		return "", "", false
	}
	return f[1], f[2], true
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

	// UserStates are what mamelet found of the part's users (NodeUser: user_wire.go).
	UserStates []UserState `json:"userStates,omitempty"`

	// Artifacts are what is at each artifact's path (NodeArtifact: artifact_wire.go).
	Artifacts []ArtifactState `json:"artifacts,omitempty"`

	// NodeAptRepo (aptrepo_wire.go): what mamelet found of each APT source of the part.
	AptRepos []AptRepoState `json:"aptRepos,omitempty"`

	// Error says why the details were left out of the report (it would have been larger than
	// MaxObserved): each entry then has only its path or name, and its own Error.
	Error string `json:"error,omitempty"`

	// SetAside are the records of mamelet's state directory it could not read and set aside, while
	// they are there ("linux-artifacts.json.corrupt"): what they said is lost (the artifacts mamelet
	// installed are reported as ones it did not install, until they are installed again; what was
	// pending after a write, as not known).
	SetAside []string `json:"setAside,omitempty"`

	// InForce is the Control Plane's, never mamelet's: it is set from the node's revision when the
	// Control Plane observes (annotate, targets.go), whatever a report says. It names what the
	// declaration in force has (the entries of the part that are InForce), which a plan of a
	// declaration that does not have them shows as kept.
	InForce *InForce `json:"inForce,omitempty"`
}

// InForce names, by kind, what the declaration in force has on a node: files and artifacts by path,
// users and APT sources by name (each sorted).
type InForce struct {
	Files     []string `json:"files,omitempty"`
	Users     []string `json:"users,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	AptRepos  []string `json:"aptRepos,omitempty"`
}

// FileState is what is at a file's path.
type FileState struct {
	Path string `json:"path"`
	// Exists: something is at the path (a regular file, or what Unsafe says).
	Exists bool   `json:"exists,omitzero"`
	SHA256 string `json:"sha256,omitempty"` // of a regular file, unless it is Large
	// Large: a regular file larger than any content Mamela writes (MaxContent); it is not hashed.
	Large bool `json:"large,omitzero"`
	// Stamp is what tells a Large file apart, instead of its sha256: its device and inode, its size,
	// and the times of its last change of content and of anything (ctime, which no user can set back),
	// "<device>:<inode>:<size>:<mtime ns>:<ctime ns>"; it changes whenever the content does.
	Stamp string `json:"stamp,omitempty"`
	Mode  string `json:"mode,omitempty"`  // of a regular file: "0644" (with the setuid bits)
	Owner string `json:"owner,omitempty"` // the user's name ("#1001" when the node has no name for the id)
	Group string `json:"group,omitempty"`
	// Missing are the directories above it that do not exist, outermost first: writing it creates
	// them (0755, root's).
	Missing []string `json:"missing,omitempty"`
	// Unsafe says why mamelet does not write there: the path is a symbolic link, a directory or a
	// device, or a directory above it is a link or not a directory. Mamela never follows a link.
	Unsafe string `json:"unsafe,omitempty"`
	// Exposed are the directories above the path that a user other than root can change (one it
	// owns, or one its group or everyone may write): that user can replace what is at the path, so
	// a file root puts there is in its hands (the plan warns: MAM-PLAN-0144).
	Exposed []DirOwner `json:"exposed,omitempty"`
	// Pending is what did not finish after mamelet last wrote the file (its daemon-reload, the
	// restarts of its services): the file is as approved, but what runs may not use it yet. Reported
	// while the file is still the one written (MAM-PLAN-0218).
	Pending *Pending `json:"pending,omitempty"`
	// Error says why it could not be observed (including a path mamelet refuses: CheckPath).
	Error string `json:"error,omitempty"`
}

// DirOwner is a directory's mode and owner (FileState.Exposed).
type DirOwner struct {
	Dir   string `json:"dir"`
	Mode  string `json:"mode"`
	Owner string `json:"owner"`
	Group string `json:"group"`
}

// Pending is what is left to do after a file was written (FileState.Pending): systemctl
// daemon-reload, and the services to restart (try-restart), in the order the write names them.
type Pending struct {
	SHA256  string   `json:"sha256"` // of the file written
	Reload  bool     `json:"reload,omitzero"`
	Restart []string `json:"restart,omitempty"`
	// Unknown: mamelet lost its record of what was left after its writes (it could not be read, and
	// was set aside): whether what follows the write of this file (its daemon-reload, the restarts its
	// declaration names) finished is not known. Restart is then empty.
	Unknown bool `json:"unknown,omitzero"`
}

// MaxContent bounds a file's content, in bytes.
const MaxContent = 256 << 10

// MaxNodeContent bounds the content of all the files declared on one node: the approved writes ride
// in the node's revision (MAM-GRAPH-0106 beyond it).
const MaxNodeContent = 1 << 20

// The most of each kind one node has (the graph refuses more, and mamelet refuses a part with more):
// they bound what mamelet observes and hashes each round, and its report.
const (
	MaxNodeFiles     = 256
	MaxNodeUsers     = 64
	MaxNodeAptRepos  = 32
	MaxNodeArtifacts = 64
)

// MaxObserved bounds the linux part of a report, encoded (the Control Plane takes a report of at most
// 1 MiB, the packages and services included): mamelet sends, past it, only what each entry is and
// why its details were left out (Observed.Error), so that the node is not taken for dead.
const MaxObserved = 512 << 10

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

// encodeObserved is the JSON of an observation, of at most MaxObserved bytes: past it, each entry
// keeps only its path or name, with an Error that says why its details were left out (the plan then
// blocks it as not observed, and says why), and the whole says so (Observed.Error).
func encodeObserved(o Observed) jsontext.Value {
	out := encode(o)
	if len(out) <= MaxObserved {
		return out
	}
	why := fmt.Sprintf("mamelet left the details out of its report: they would have been %d KiB, more than the %d KiB it sends of the linux part (a file of very many keys or lines?)",
		len(out)>>10, MaxObserved>>10)
	d := Observed{Users: o.Users, Groups: o.Groups, Error: why, SetAside: o.SetAside}
	for _, f := range o.Files {
		d.Files = append(d.Files, FileState{Path: f.Path, Error: why})
	}
	for _, u := range o.UserStates {
		d.UserStates = append(d.UserStates, UserState{Name: u.Name, Error: why})
	}
	for _, a := range o.Artifacts {
		d.Artifacts = append(d.Artifacts, ArtifactState{FileState: FileState{Path: a.Path, Error: why}})
	}
	for _, r := range o.AptRepos {
		d.AptRepos = append(d.AptRepos, AptRepoState{Name: r.Name, Error: why})
	}
	if out = encode(d); len(out) > MaxObserved {
		out = encode(Observed{Error: why})
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
	// Empty is what encodes to an empty object: every member of Part is omitted when it is empty, so a
	// kind's member counts without being named here (wire_test.go checks that each is).
	if string(encode(p)) == "{}" {
		delete(ext, Name)
	} else {
		slices.SortFunc(p.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
		sortUsers(&p)
		sortArtifacts(&p)
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
	out = append(out, userGrants(p)...)
	out = append(out, artifactGrants(p)...)
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
	handOutUsers(&p, now)
	handOutArtifacts(&p, now)
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
	for _, n := range []struct {
		what      string
		got, most int
	}{{"files", len(p.Files), MaxNodeFiles}, {"users", len(p.Users), MaxNodeUsers}, {"APT sources", len(p.AptRepos), MaxNodeAptRepos}, {"artifacts", len(p.Artifacts), MaxNodeArtifacts}} {
		if n.got > n.most {
			add("it names %d %s; mamelet observes at most %d on one node (declare fewer)", n.got, n.what, n.most)
		}
	}
	seen := map[string]bool{}
	for _, f := range p.Files {
		if seen[f.Path] {
			add("file %q appears twice", f.Path)
		}
		seen[f.Path] = true
		if why := accountsProblem(f.Owners, f.Groups); why != "" {
			add("file %q: %s", f.Path, why)
		}
		if w := f.Write; w != nil {
			if err := checkWrite(*w); err != nil {
				add("file %q: %v", f.Path, err)
			}
		}
	}
	problems = append(problems, checkUsers(p.Users)...)
	checkArtifacts(p, add)
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
	if why := w.Before.check(); why != "" {
		return fmt.Errorf("what the write was planned against: %s", why)
	}
	if _, err := ParseMode(w.Mode); err != nil {
		return err
	}
	for _, s := range w.Restart {
		if !names.Service(s) {
			return fmt.Errorf("%q is not a service name", s)
		}
		if why := CheckRestart(s); why != "" {
			return errors.New(why)
		}
	}
	return nil
}

// accountsProblem says what is wrong with the users and groups an entry asks about ("" nothing): each
// a name, sorted, none twice, at most maxTargets.
func accountsProblem(users, groups []string) string {
	for _, l := range []struct {
		what  string
		names []string
	}{{"owners", users}, {"groups", groups}} {
		if why := targetsProblem(l.names, func(n string) bool { return ValidAccount(n) }); why != "" {
			return l.what + ": " + why
		}
	}
	return ""
}

// targetsProblem says what is wrong with a list of observation targets ("" nothing): each valid,
// sorted, none twice, at most maxTargets.
func targetsProblem[T cmp.Ordered](l []T, valid func(T) bool) string {
	if len(l) > maxTargets {
		return fmt.Sprintf("%d values; at most %d", len(l), maxTargets)
	}
	for i, v := range l {
		if !valid(v) {
			return fmt.Sprintf("%v is not valid", v)
		}
		if i > 0 && l[i-1] >= v {
			return "not sorted, or a value twice"
		}
	}
	return ""
}
