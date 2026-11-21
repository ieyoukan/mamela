package linux

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/names"
)

// KindArtifact is a file on a node downloaded from a URL and pinned by its sha256 (a release's
// binary), or one member of such a download when it is a gzip-compressed tar.
const KindArtifact graph.Kind = "NodeArtifact"

// ArtifactSpec says a node has at Path the file downloaded from URL whose sha256 is SHA256 (of the
// bytes downloaded, as a release's checksum file lists them) or, when Extract is set, the member
// Extract of that download (a gzip-compressed tar), with Mode, Owner and Group; and that the
// services of Restart are restarted (if they run) after it changes. The resource's name is
// "<node>/artifact<path>". Installing it needs an approval.
type ArtifactSpec struct {
	Node    string
	URL     string // https, without a user or a fragment
	SHA256  string // of the download
	Path    string
	Extract string   // a member of the download, a .tar.gz ("": the download itself)
	Mode    string   // four octal digits, "0755" by default
	Owner   string   // a user's name, "root" by default
	Group   string   // a group's name, "root" by default
	Restart []string // services on the node, restarted after a change (systemctl try-restart)
}

// DefaultArtifactMode is the mode of an artifact that declares none: a release's binary runs.
const DefaultArtifactMode = "0755"

// MaxArtifact bounds a download and the member taken out of one, in bytes.
const MaxArtifact = 512 << 20

// MaxURL bounds an artifact's URL, in bytes.
const MaxURL = 2048

// MaxExtract bounds the name of the member of an archive, in bytes.
const MaxExtract = 1024

// ArtifactOf is the artifact a resource declares (nil for a resource of another kind).
func ArtifactOf(r graph.Resource) *ArtifactSpec { s, _ := r.Ext.(*ArtifactSpec); return s }

// ArtifactName is the name of the resource of the artifact at path on node.
func ArtifactName(node, path string) string { return node + "/artifact" + path }

type wireArtifact struct {
	Node    *string        `json:"node"`
	URL     *string        `json:"url"`
	SHA256  *string        `json:"sha256"`
	Path    *string        `json:"path"`
	Extract jsontext.Value `json:"extract"` // optional
	Mode    jsontext.Value `json:"mode"`    // optional
	Owner   jsontext.Value `json:"owner"`   // optional
	Group   jsontext.Value `json:"group"`   // optional
	Restart jsontext.Value `json:"restart"` // optional
}

type canonArtifact struct {
	Extract string   `json:"extract,omitempty"`
	Group   string   `json:"group"`
	Mode    string   `json:"mode"`
	Node    string   `json:"node"`
	Owner   string   `json:"owner"`
	Path    string   `json:"path"`
	Restart []string `json:"restart,omitempty"`
	SHA256  string   `json:"sha256"`
	URL     string   `json:"url"`
}

// artifactKind is NodeArtifact in every layer.
var artifactKind = kind{
	def: graph.KindDef{
		Kind: KindArtifact, Order: 8,
		New:      func() any { return &ArtifactSpec{} },
		Decode:   decodeArtifact,
		Validate: validateArtifact,
		Canonical: func(spec any) any {
			s := spec.(*ArtifactSpec)
			return canonArtifact{Extract: s.Extract, Group: s.Group, Mode: s.Mode, Node: s.Node, Owner: s.Owner, Path: s.Path, Restart: s.Restart, SHA256: s.SHA256, URL: s.URL}
		},
		Node: func(spec any) string { return spec.(*ArtifactSpec).Node },
		Fields: func(spec any) map[string]string {
			s := spec.(*ArtifactSpec)
			return map[string]string{"path": s.Path, "sha256": s.SHA256, "url": s.URL}
		},
		Describe: func(spec any) string {
			s := spec.(*ArtifactSpec)
			from := s.URL
			if s.Extract != "" {
				from = s.Extract + " of " + s.URL
			}
			return fmt.Sprintf("artifact %s from %s (sha256 %s, %s %s:%s) on %s", s.Path, from, s.SHA256[:min(12, len(s.SHA256))], s.Mode, s.Owner, s.Group, s.Node)
		},
	},
	claims:       func(r graph.Resource) (string, string, bool) { a := ArtifactOf(r); return a.Node, a.Path, true },
	warnings:     artifactWarnings,
	warningCodes: []graph.Code{CodeArtifactRestartUndeclared},
	codes:        artifactPlanCodes,
	planner:      artifactPlanner,
	ops:          []operation{installArtifactOp},
	manifest:     artifactManifest,
	publish:      publishArtifacts,
	prune:        pruneArtifacts,
	protect:      protectArtifacts,
	observe:      observeArtifacts,
	act:          actArtifacts,
	results:      artifactResults,
	startGate:    artifactStartGate,
	undeclared:   undeclaredArtifacts,
}

func decodeArtifact(d *graph.SpecDecoder) any {
	var s wireArtifact
	if !d.Strict(&s) {
		return nil
	}
	d.Need(s.Node != nil, "node")
	d.Need(s.URL != nil, "url")
	// Left out, the sha256 is mamela.sum's for the URL (docs/design/declaration-inputs.md, 2).
	d.Need(s.SHA256 != nil || d.Summed(), "sha256")
	d.Need(s.Path != nil, "path")
	extract := d.OptionalString(s.Extract, "extract")
	mode := d.OptionalString(s.Mode, "mode")
	owner := d.OptionalString(s.Owner, "owner")
	group := d.OptionalString(s.Group, "group")
	var restart []string
	if s.Restart != nil {
		if string(s.Restart) == "null" || json.Unmarshal(s.Restart, &restart) != nil {
			d.Invalid(graph.CodeWrongType, "restart", "restart is not a list of service names",
				`leave the member out (nothing is restarted), or write a list such as ["node_exporter"]`)
		}
	}
	if !d.OK() {
		return nil
	}
	a := &ArtifactSpec{Node: *s.Node, URL: *s.URL, Path: *s.Path, Mode: DefaultArtifactMode, Owner: DefaultOwner, Group: DefaultGroup, Restart: restart}
	if s.SHA256 != nil {
		a.SHA256 = *s.SHA256
	} else {
		d.FromSum("sha256", a.URL, false, func(h string) { a.SHA256 = h })
	}
	if extract != nil {
		a.Extract = *extract
	}
	if mode != nil {
		a.Mode = *mode
	}
	if owner != nil {
		a.Owner = *owner
	}
	if group != nil {
		a.Group = *group
	}
	return a
}

func validateArtifact(v *graph.Validator, r graph.Resource) {
	s := ArtifactOf(r)
	v.NodeRef(r, s.Node)
	if err := CheckPath(s.Path); err != nil {
		v.Problem(r, graph.CodeInvalidValue, "spec.path", err.Message, err.Hint)
	} else if want := ArtifactName(s.Node, s.Path); r.Name != want {
		v.Problem(r, graph.CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/artifact/path)", want), "")
	}
	if err := CheckURL(s.URL); err != nil {
		v.Problem(r, graph.CodeInvalidValue, "spec.url", err.Error(),
			`write the https URL of the release's file, such as "https://github.com/prometheus/node_exporter/releases/download/v1.8.2/node_exporter-1.8.2.linux-amd64.tar.gz"`)
	}
	if !names.SHA256(s.SHA256) {
		v.Problem(r, graph.CodeInvalidValue, "spec.sha256", fmt.Sprintf("%q is not a sha256 (64 lowercase hex digits)", s.SHA256),
			"copy the sha256 of the download from the release's checksum file (sha256sums.txt), in lowercase")
	}
	if s.Extract != "" {
		if err := CheckExtract(s.Extract); err != nil {
			v.Problem(r, graph.CodeInvalidValue, "spec.extract", err.Error(),
				`name the member as the archive lists it (tar -tzf), without a leading "/" or "./", such as "node_exporter-1.8.2.linux-amd64/node_exporter"`)
		}
	}
	if _, err := ParseMode(s.Mode); err != nil {
		v.Problem(r, graph.CodeInvalidValue, "spec.mode", err.Error(), `write four octal digits without the special bits, such as "0755" or "0750"`)
	}
	if !ValidAccount(s.Owner) {
		v.Problem(r, graph.CodeInvalidValue, "spec.owner", fmt.Sprintf("%q is not a user's name", s.Owner), `a user's name, such as "root"`)
	}
	if !ValidAccount(s.Group) {
		v.Problem(r, graph.CodeInvalidValue, "spec.group", fmt.Sprintf("%q is not a group's name", s.Group), `a group's name, such as "root"`)
	}
	seen := map[string]bool{}
	for _, svc := range s.Restart {
		unit := strings.TrimSuffix(svc, ".service")
		switch {
		case !names.Service(svc):
			v.Problem(r, graph.CodeInvalidValue, "spec.restart", fmt.Sprintf("%q is not a valid systemd service name", svc),
				`write the unit name without ".service", for example "node_exporter"`)
		case seen[unit]:
			v.Problem(r, graph.CodeInvalidValue, "spec.restart", fmt.Sprintf("service %s is named twice", svc), "name each service once")
		case CheckRestart(svc) != "":
			v.Problem(r, graph.CodeInvalidValue, "spec.restart", CheckRestart(svc),
				"take mamelet out of restart: mamelet is updated with NodeMamelet (Mamela.mamelet), and reads its configuration when it starts")
		}
		seen[unit] = true
	}
}

// urlRe: what an artifact's URL may have, the characters of RFC 3986 but "#" (no space, no
// control character), so that the plan shows it as it is fetched.
var urlRe = regexp.MustCompile(`^https://[A-Za-z0-9._~:/?\[\]@!$&'()*+,;=%-]+$`)

// CheckURL says why Mamela does not download from u (nil when it may): an https URL of at most
// MaxURL bytes, with a host, no user or password, and no fragment. The graph checks a declaration with
// it, and mamelet checks it again before it downloads.
func CheckURL(u string) error {
	switch {
	case len(u) > MaxURL:
		return fmt.Errorf("the URL is %d bytes; at most %d", len(u), MaxURL)
	case strings.HasPrefix(strings.ToLower(u), "http://"):
		return fmt.Errorf("%q is not https: a download over http can be changed on the way (the sha256 would refuse it, but nothing should be fetched that way)", u)
	case strings.Contains(u, "#"):
		return fmt.Errorf("%q has a fragment (#...), which is not part of what is downloaded", u)
	case !urlRe.MatchString(u):
		return fmt.Errorf("%q is not an https URL of the characters a URL has (no space, no fragment)", u)
	}
	p, err := url.Parse(u)
	switch {
	case err != nil:
		return fmt.Errorf("%q is not a URL: %v", u, err)
	case p.Scheme != "https" || p.Opaque != "" || p.Hostname() == "":
		return fmt.Errorf("%q is not an https URL with a host", u)
	case p.User != nil:
		return fmt.Errorf("%q has a user or a password: Mamela keeps no credentials in a declaration", u)
	}
	return nil
}

// CheckExtract says why e cannot name a member of an archive (nil when it can): a relative, clean
// path of at most MaxExtract bytes, its components of letters, digits and . _ + @ = : , - only.
func CheckExtract(e string) error {
	switch {
	case e == "":
		return fmt.Errorf("the member's name is empty")
	case len(e) > MaxExtract:
		return fmt.Errorf("the member's name is %d bytes; at most %d", len(e), MaxExtract)
	case strings.HasPrefix(e, "/"):
		return fmt.Errorf("%q starts with \"/\": a member is named relative to the archive", e)
	case strings.HasSuffix(e, "/"):
		return fmt.Errorf("%q is a directory, not a file", e)
	case path.Clean(e) != e:
		return fmt.Errorf("%q is not a clean path (it would be %q)", e, path.Clean(e))
	}
	for _, c := range strings.Split(e, "/") {
		switch {
		case c == "." || c == "..":
			return fmt.Errorf("%q has a %q component", e, c)
		case !componentRe.MatchString(c):
			return fmt.Errorf("%q has a character other than letters, digits and . _ + @ = : , - in %q", e, c)
		}
	}
	return nil
}

// CodeArtifactRestartUndeclared: an artifact restarts a service that the declaration does not have
// on the node (NodeArtifact's warning: doc.go).
const CodeArtifactRestartUndeclared graph.Code = "MAM-GRAPH-0215"

// artifactWarnings are NodeArtifact's warnings: an artifact that restarts a service the declaration
// does not have on its node.
func artifactWarnings(g *graph.Graph) []graph.Warning {
	services := map[string]bool{} // node + "\x00" + unit
	for _, r := range g.Resources {
		if s := r.NodeService; s != nil {
			services[s.Node+"\x00"+strings.TrimSuffix(s.Service, ".service")] = true
		}
	}
	var out []graph.Warning
	for _, r := range g.Sorted() {
		a := ArtifactOf(r)
		if a == nil {
			continue
		}
		for _, svc := range a.Restart {
			if !services[a.Node+"\x00"+strings.TrimSuffix(svc, ".service")] {
				out = append(out, graph.Warning{Code: CodeArtifactRestartUndeclared, Resource: r.ID(),
					Message: fmt.Sprintf("%s restarts service %s on %s, which the declaration does not have there: it is restarted only if it exists and runs", a.Path, svc, a.Node),
					Hint:    fmt.Sprintf("declare the service: Mamela.service(graph, node, %q, True, True), or check its name", strings.TrimSuffix(svc, ".service"))})
			}
		}
	}
	return out
}
