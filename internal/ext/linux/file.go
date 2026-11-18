package linux

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/names"
	"github.com/ieyoukan/mamela/internal/safetext"
)

// KindFile is a file on a node, with exactly the declared content, mode, owner and group.
const KindFile graph.Kind = "NodeFile"

// FileSpec says a node has a file at Path with exactly Content (text), Mode, Owner and Group, and
// that the services of Restart are restarted (if they run) after it changes. The resource's name is
// "<node>/file<path>". The plan shows the whole content; writing it needs an approval.
type FileSpec struct {
	Node    string
	Path    string
	Content string
	Mode    string   // four octal digits, "0644" by default
	Owner   string   // a user's name, "root" by default
	Group   string   // a group's name, "root" by default
	Restart []string // services on the node, restarted after a change (systemctl try-restart)
}

// Defaults of a file's optional members.
const (
	DefaultMode  = "0644"
	DefaultOwner = "root"
	DefaultGroup = "root"
)

// FileOf is the file a resource declares (nil for a resource of another kind).
func FileOf(r graph.Resource) *FileSpec { s, _ := r.Ext.(*FileSpec); return s }

// FileName is the name of the resource of the file at path on node.
func FileName(node, path string) string { return node + "/file" + path }

type wireFile struct {
	Node    *string        `json:"node"`
	Path    *string        `json:"path"`
	Content *string        `json:"content"`
	Mode    jsontext.Value `json:"mode"`    // optional
	Owner   jsontext.Value `json:"owner"`   // optional
	Group   jsontext.Value `json:"group"`   // optional
	Restart jsontext.Value `json:"restart"` // optional
}

type canonFile struct {
	Content string   `json:"content"`
	Group   string   `json:"group"`
	Mode    string   `json:"mode"`
	Node    string   `json:"node"`
	Owner   string   `json:"owner"`
	Path    string   `json:"path"`
	Restart []string `json:"restart,omitempty"`
}

// fileKind is NodeFile in every layer.
var fileKind = kind{
	def: graph.KindDef{
		Kind: KindFile, Order: 9,
		New:      func() any { return &FileSpec{} },
		Decode:   decodeFile,
		Validate: validateFile,
		Canonical: func(spec any) any {
			s := spec.(*FileSpec)
			return canonFile{Content: s.Content, Group: s.Group, Mode: s.Mode, Node: s.Node, Owner: s.Owner, Path: s.Path, Restart: s.Restart}
		},
		Node: func(spec any) string { return spec.(*FileSpec).Node },
		Fields: func(spec any) map[string]string {
			s := spec.(*FileSpec)
			return map[string]string{"path": s.Path, "sha256": ContentSHA256(s.Content)}
		},
		Describe: func(spec any) string {
			s := spec.(*FileSpec)
			return fmt.Sprintf("file %s (%d bytes, %s %s:%s) on %s", s.Path, len(s.Content), s.Mode, s.Owner, s.Group, s.Node)
		},
	},
	claims:     func(r graph.Resource) (string, string, bool) { f := FileOf(r); return f.Node, f.Path, true },
	content:    func(r graph.Resource) int { return len(FileOf(r).Content) },
	planner:    filePlanner,
	ops:        []operation{writeFileOp},
	manifest:   fileManifest,
	publish:    publishFiles,
	prune:      pruneFiles,
	protect:    protectFiles,
	observe:    observeFiles,
	act:        actFiles,
	results:    fileResults,
	startGate:  fileStartGate,
	undeclared: undeclaredFiles,
}

func decodeFile(d *graph.SpecDecoder) any {
	var s wireFile
	if !d.Strict(&s) {
		return nil
	}
	d.Need(s.Node != nil, "node")
	d.Need(s.Path != nil, "path")
	d.Need(s.Content != nil, "content")
	mode := d.OptionalString(s.Mode, "mode")
	owner := d.OptionalString(s.Owner, "owner")
	group := d.OptionalString(s.Group, "group")
	var restart []string
	if s.Restart != nil {
		if string(s.Restart) == "null" || json.Unmarshal(s.Restart, &restart) != nil {
			d.Invalid(graph.CodeWrongType, "restart", "restart is not a list of service names",
				`leave the member out (nothing is restarted), or write a list such as ["nginx"]`)
		}
	}
	if !d.OK() {
		return nil
	}
	f := &FileSpec{Node: *s.Node, Path: *s.Path, Content: *s.Content, Mode: DefaultMode, Owner: DefaultOwner, Group: DefaultGroup, Restart: restart}
	if mode != nil {
		f.Mode = *mode
	}
	if owner != nil {
		f.Owner = *owner
	}
	if group != nil {
		f.Group = *group
	}
	return f
}

func validateFile(v *graph.Validator, r graph.Resource) {
	s := FileOf(r)
	v.NodeRef(r, s.Node)
	if err := CheckPath(s.Path); err != nil {
		v.Problem(r, graph.CodeInvalidValue, "spec.path", err.Message, err.Hint)
	} else if want := FileName(s.Node, s.Path); r.Name != want {
		v.Problem(r, graph.CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/file/path)", want), "")
	}
	switch {
	case len(s.Content) > MaxContent:
		v.Problem(r, graph.CodeInvalidValue, "spec.content", fmt.Sprintf("the content is %d bytes; at most %d", len(s.Content), MaxContent),
			"a larger file is a download pinned by its sha256 (NodeArtifact), not text in the declaration")
	case !utf8.ValidString(s.Content) || strings.ContainsRune(s.Content, 0):
		v.Problem(r, graph.CodeInvalidValue, "spec.content", "the content is not text (UTF-8 without NUL)",
			"a binary file is a download pinned by its sha256 (NodeArtifact)")
	default:
		// The plan shows the whole content, and it is approved as shown: it must show as what it is.
		if f, bad := safetext.Find(s.Content, true); bad {
			v.Problem(r, graph.CodeUnsafeText, "spec.content", "the content has "+f.String(), graph.UnsafeTextHint)
		}
	}
	if _, err := ParseMode(s.Mode); err != nil {
		v.Problem(r, graph.CodeInvalidValue, "spec.mode", err.Error(), `write four octal digits without the special bits, such as "0644" or "0640"`)
	}
	if !ValidAccount(s.Owner) {
		v.Problem(r, graph.CodeInvalidValue, "spec.owner", fmt.Sprintf("%q is not a user's name", s.Owner), `a user's name, such as "root" or "www-data"`)
	}
	if !ValidAccount(s.Group) {
		v.Problem(r, graph.CodeInvalidValue, "spec.group", fmt.Sprintf("%q is not a group's name", s.Group), `a group's name, such as "root" or "www-data"`)
	}
	seen := map[string]bool{}
	for _, svc := range s.Restart {
		unit := strings.TrimSuffix(svc, ".service")
		switch {
		case !names.Service(svc):
			v.Problem(r, graph.CodeInvalidValue, "spec.restart", fmt.Sprintf("%q is not a valid systemd service name", svc),
				`write the unit name without ".service", for example "nginx"`)
		case seen[unit]:
			v.Problem(r, graph.CodeInvalidValue, "spec.restart", fmt.Sprintf("service %s is named twice", svc), "name each service once")
		case CheckRestart(svc) != "":
			v.Problem(r, graph.CodeInvalidValue, "spec.restart", CheckRestart(svc),
				"take mamelet out of restart: mamelet is updated with NodeMamelet (Mamela.mamelet), and reads its configuration when it starts")
		}
		seen[unit] = true
	}
}
