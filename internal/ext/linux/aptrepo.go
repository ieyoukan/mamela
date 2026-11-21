package linux

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/names"
)

// KindAptRepo is an APT source on a node, with the key its packages are signed with.
const KindAptRepo graph.Kind = "NodeAptRepo"

// AptRepoSpec says a node has the APT source Name: apt reads the Suites (and Components) of each of
// URIs, of the Architectures named (all the node's when none), and trusts only the key at KeyURL,
// whose sha256 is KeySHA256, to sign them. The resource's name is "<node>/apt-repo/<name>". mamelet
// writes the key to /etc/apt/keyrings/mamela-<name>.asc (.gpg for a binary key) and the source to
// /etc/apt/sources.list.d/mamela-<name>.sources (AptSources), then runs apt-get update. The plan
// shows the whole file; adding or changing it needs an approval.
type AptRepoSpec struct {
	Node          string
	Name          string
	URIs          []string
	Suites        []string
	Components    []string // none for a flat repository (every suite ends with "/")
	Architectures []string // none: every architecture of the node
	KeyURL        string
	KeySHA256     string
}

// Bounds of a source's lists and values.
const (
	MaxAptURIs   = 4
	MaxAptSuites = 8
	MaxAptComps  = 16
	MaxAptArchs  = 8
	maxAptURI    = 255
	maxAptKeyURL = 512
	maxAptSuite  = 100
	maxAptComp   = 64
)

// MaxAptKey bounds the key mamelet downloads, in bytes.
const MaxAptKey = 64 << 10

// AptRepoOf is the APT source a resource declares (nil for a resource of another kind).
func AptRepoOf(r graph.Resource) *AptRepoSpec { s, _ := r.Ext.(*AptRepoSpec); return s }

// AptRepoName is the name of the resource of the APT source name on node.
func AptRepoName(node, name string) string { return node + "/apt-repo/" + name }

// AptSourcesPath is where the source's file is (a prefix CheckPath keeps NodeFile out of).
func AptSourcesPath(name string) string { return "/etc/apt/sources.list.d/mamela-" + name + ".sources" }

// AptKeyPath is where the source's key is: .asc for an ASCII-armored key, .gpg for a binary one (apt
// tells them apart by the extension).
func AptKeyPath(name string, armored bool) string {
	if armored {
		return "/etc/apt/keyrings/mamela-" + name + ".asc"
	}
	return "/etc/apt/keyrings/mamela-" + name + ".gpg"
}

// AptSources is the deb822 file of the source, its key at keyPath: the one rendering the Control
// Plane shows in the plan (and compares with what mamelet reports) and mamelet writes. Every value
// was checked with aptRepoProblems first: none has a space, a newline or a control character, so
// none can add a line or a field.
func AptSources(s AptRepoSpec, keyPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Mamela's APT source %s (NodeAptRepo): change the declaration, not this file.\n", s.Name)
	b.WriteString("Types: deb\n")
	b.WriteString("URIs: " + strings.Join(s.URIs, " ") + "\n")
	b.WriteString("Suites: " + strings.Join(s.Suites, " ") + "\n")
	if len(s.Components) > 0 {
		b.WriteString("Components: " + strings.Join(s.Components, " ") + "\n")
	}
	if len(s.Architectures) > 0 {
		b.WriteString("Architectures: " + strings.Join(s.Architectures, " ") + "\n")
	}
	b.WriteString("Signed-By: " + keyPath + "\n")
	return b.String()
}

// aptSourcesSHA256 are the sha256 of the source's file with an ASCII-armored key and with a binary
// one: the key, pinned by its sha256, decides which mamelet writes (Signed-By names its file).
func aptSourcesSHA256(s AptRepoSpec) (armored, binary string) {
	return ContentSHA256(AptSources(s, AptKeyPath(s.Name, true))), ContentSHA256(AptSources(s, AptKeyPath(s.Name, false)))
}

// The shapes of a source's values: narrow, without a space, a newline, a control character or a
// character a deb822 file or apt gives a meaning to ('#', '$', '[').
var (
	aptNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	aptHost   = `[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*(:[0-9]{1,5})?`
	aptPath   = `(/([A-Za-z0-9._~+-]|%[0-9A-Fa-f]{2})*)*`
	// A source's URI: http or https, a host (and port), a path; nothing else.
	aptURIRe = regexp.MustCompile(`^https?://` + aptHost + aptPath + `$`)
	// The key's URL: https, a host, a path and a query (a key server's lookup has one).
	aptKeyURLRe = regexp.MustCompile(`^https://` + aptHost + aptPath + `(\?([A-Za-z0-9._~+=&,:/-]|%[0-9A-Fa-f]{2})*)?$`)
	// A suite: "noble", "stable/updates", or an exact path of a flat repository ending with "/"
	// ("./", "/", "stable/").
	aptSuiteRe = regexp.MustCompile(`^(/|([A-Za-z0-9._+~-]+/)*[A-Za-z0-9._+~-]+/?)$`)
	aptCompRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*(/[A-Za-z0-9][A-Za-z0-9._+-]*)*$`)
	// A dpkg architecture ("amd64", "arm64", "armhf", "musl-linux-amd64").
	aptArchRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// ValidAptRepoName reports whether s can be the name of an APT source (and so of its files).
func ValidAptRepoName(s string) bool { return aptNameRe.MatchString(s) }

// aptProblem is what is wrong with one value of a source, and what to write instead.
type aptProblem struct {
	field, message, hint string
}

// aptRepoProblems are what is wrong with a source (its node aside): the graph checks a declaration
// with it, and mamelet checks the source of a grant again before it writes anything.
func aptRepoProblems(s AptRepoSpec) []aptProblem {
	out := aptWhereProblems(s.Name, s.URIs, s.Suites)
	add := func(field, message, hint string) { out = append(out, aptProblem{field, message, hint}) }
	list := func(field string, values []string, min, max int, what string) {
		out = append(out, aptListProblems(field, values, min, max, what)...)
	}
	flat := 0
	for _, su := range s.Suites {
		if strings.HasSuffix(su, "/") {
			flat++
		}
	}
	list("spec.components", s.Components, 0, MaxAptComps, "component")
	for _, c := range s.Components {
		if len(c) > maxAptComp || !aptCompRe.MatchString(c) {
			add("spec.components", fmt.Sprintf("%s is not a component", quote(c)), `a component such as "main" or "stable"`)
		}
	}
	switch {
	case flat > 0 && flat < len(s.Suites):
		add("spec.suites", "some suites end with \"/\" (a flat repository's path) and some do not",
			"a flat repository's suites are all paths ending with \"/\", without components; declare the other suites as a source of their own")
	case flat > 0 && len(s.Components) > 0:
		add("spec.components", "a flat repository (its suites end with \"/\") has no components",
			"leave the components out (an empty list)")
	case flat == 0 && len(s.Components) == 0 && len(s.Suites) > 0:
		add("spec.components", "there is no component",
			`name the components, such as ["main"]; only a flat repository, whose suites end with "/", has none`)
	}
	list("spec.architectures", s.Architectures, 0, MaxAptArchs, "architecture")
	for _, a := range s.Architectures {
		if !aptArchRe.MatchString(a) {
			add("spec.architectures", fmt.Sprintf("%s is not a dpkg architecture", quote(a)), `such as "amd64" or "arm64"`)
		}
	}
	if m, h := aptKeyURLProblem(s.KeyURL); m != "" {
		add("spec.keyUrl", m, h)
	}
	if !names.SHA256(s.KeySHA256) {
		add("spec.keySha256", fmt.Sprintf("%s is not a sha256", quote(s.KeySHA256)),
			"the sha256 of the key file as it is downloaded, 64 lowercase hex digits (sha256sum of the downloaded file)")
	}
	return out
}

// aptWhereProblems are what is wrong with a source's name, URIs and suites: what mamelet observes
// of it, and checks again in a revision before it looks.
func aptWhereProblems(name string, uris, suites []string) []aptProblem {
	var out []aptProblem
	add := func(field, message, hint string) { out = append(out, aptProblem{field, message, hint}) }
	if !ValidAptRepoName(name) {
		add("spec.name", fmt.Sprintf("%s is not the name of an APT source", quote(name)),
			`1 to 40 lowercase letters, digits and "-", starting with a letter or digit, such as "docker": the files are mamela-<name>.sources and mamela-<name>.asc`)
	}
	out = append(out, aptListProblems("spec.uris", uris, 1, MaxAptURIs, "URI")...)
	for _, u := range uris {
		if m, h := aptURIProblem(u); m != "" {
			add("spec.uris", m, h)
		}
	}
	out = append(out, aptListProblems("spec.suites", suites, 1, MaxAptSuites, "suite")...)
	for _, su := range suites {
		if len(su) > maxAptSuite || !aptSuiteRe.MatchString(su) || (su != "./" && hasDotParts(su)) {
			add("spec.suites", fmt.Sprintf("%s is not a suite", quote(su)),
				`a suite such as "noble" or "bookworm-updates", or the path of a flat repository ending with "/" (such as "./"); letters, digits and . _ + ~ - only, no "." or ".." parts`)
		}
	}
	return out
}

// aptListProblems: a list of min to max values, none twice.
func aptListProblems(field string, values []string, min, max int, what string) []aptProblem {
	var out []aptProblem
	switch {
	case len(values) < min:
		out = append(out, aptProblem{field, fmt.Sprintf("there is no %s", what), ""})
	case len(values) > max:
		out = append(out, aptProblem{field, fmt.Sprintf("%d %ss; at most %d", len(values), what, max), ""})
	}
	seen := map[string]bool{}
	for _, v := range values {
		if seen[v] {
			out = append(out, aptProblem{field, fmt.Sprintf("%s %s is named twice", what, quote(v)), "name each once"})
		}
		seen[v] = true
	}
	return out
}

// aptURIProblem says what is wrong with a source's URI ("" when nothing is), and what to write.
func aptURIProblem(u string) (message, hint string) {
	const shape = `write http:// or https://, a host and a path, such as "https://download.docker.com/linux/ubuntu"`
	switch {
	case len(u) > maxAptURI:
		return fmt.Sprintf("the URI is %d bytes; at most %d", len(u), maxAptURI), ""
	case !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://"):
		return fmt.Sprintf("%s is not an http or https URI", quote(u)),
			"apt's other methods (file, cdrom, mirror+, tor+) are not supported; " + shape
	case strings.ContainsAny(u, "@"):
		return fmt.Sprintf("%s has a user name or a password", quote(u)),
			"a credential would be in clear text in the declaration and the plan: give it to apt on the node (/etc/apt/auth.conf.d), and write the URI without it"
	case strings.ContainsAny(u, "?#"):
		return fmt.Sprintf("%s has a query or a fragment", quote(u)), "an APT source's URI is a host and a path; " + shape
	case !aptURIRe.MatchString(u) || hasDotParts(strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")) || !validPort(u):
		return fmt.Sprintf("%s is not a URI Mamela writes into a source", quote(u)),
			"a host of letters, digits, \".\" and \"-\" (no IPv6 address), an optional port, and a path of letters, digits and . _ ~ + - %XX, without \".\" or \"..\" parts and without a space; " + shape
	}
	return "", ""
}

// aptKeyURLProblem says what is wrong with a key's URL ("" when nothing is).
func aptKeyURLProblem(u string) (message, hint string) {
	switch {
	case len(u) > maxAptKeyURL:
		return fmt.Sprintf("the key's URL is %d bytes; at most %d", len(u), maxAptKeyURL), ""
	case !strings.HasPrefix(u, "https://"):
		return fmt.Sprintf("%s is not an https URL", quote(u)),
			"the key is downloaded over https only (its sha256 is checked too); write the vendor's https URL of the key"
	case strings.ContainsAny(u, "@"):
		return fmt.Sprintf("%s has a user name or a password", quote(u)), "write the URL without it"
	case strings.Contains(u, "#"):
		return fmt.Sprintf("%s has a fragment", quote(u)), "write the URL without the part after \"#\""
	case !aptKeyURLRe.MatchString(u) || !validPort(u):
		return fmt.Sprintf("%s is not a URL Mamela downloads a key from", quote(u)),
			"https://, a host of letters, digits, \".\" and \"-\", an optional port, a path and a query of letters, digits and . _ ~ + = & , : / - %XX, without a space"
	}
	return "", ""
}

// validPort: a URI's port, if it has one, is 1 to 65535.
func validPort(u string) bool {
	rest := u[strings.Index(u, "://")+3:]
	host, _, _ := strings.Cut(rest, "/")
	host, _, _ = strings.Cut(host, "?")
	_, port, ok := strings.Cut(host, ":")
	if !ok {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// hasDotParts: a path with a "." or ".." part.
func hasDotParts(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}

// quote shows a value in a message, its control characters written out.
func quote(s string) string { return strconv.Quote(s) }

type wireAptRepo struct {
	Node          *string        `json:"node"`
	Name          *string        `json:"name"`
	URIs          *[]string      `json:"uris"`
	Suites        *[]string      `json:"suites"`
	Components    *[]string      `json:"components"`
	Architectures jsontext.Value `json:"architectures"` // optional
	KeyURL        *string        `json:"keyUrl"`
	KeySHA256     *string        `json:"keySha256"`
}

type canonAptRepo struct {
	Architectures []string `json:"architectures,omitempty"`
	Components    []string `json:"components"`
	KeySHA256     string   `json:"keySha256"`
	KeyURL        string   `json:"keyUrl"`
	Name          string   `json:"name"`
	Node          string   `json:"node"`
	Suites        []string `json:"suites"`
	URIs          []string `json:"uris"`
}

// aptRepoKind is NodeAptRepo in every layer.
var aptRepoKind = kind{
	def: graph.KindDef{
		Kind: KindAptRepo, Order: 7,
		New:      func() any { return &AptRepoSpec{} },
		Decode:   decodeAptRepo,
		Validate: validateAptRepo,
		Canonical: func(spec any) any {
			s := spec.(*AptRepoSpec)
			return canonAptRepo{Architectures: s.Architectures, Components: nonNil(s.Components), KeySHA256: s.KeySHA256, KeyURL: s.KeyURL,
				Name: s.Name, Node: s.Node, Suites: s.Suites, URIs: s.URIs}
		},
		Node: func(spec any) string { return spec.(*AptRepoSpec).Node },
		Fields: func(spec any) map[string]string {
			s := spec.(*AptRepoSpec)
			return map[string]string{"path": AptSourcesPath(s.Name), "keySha256": s.KeySHA256}
		},
		Describe: func(spec any) string {
			s := spec.(*AptRepoSpec)
			return fmt.Sprintf("APT source %s (%s %s) on %s", s.Name, strings.Join(s.URIs, " "), strings.Join(s.Suites, " "), s.Node)
		},
	},
	codes:       aptRepoPlanCodes,
	planner:     aptRepoPlanner,
	ops:         []operation{addAptRepoOp},
	manifest:    aptRepoManifest,
	publish:     publishAptRepos,
	prune:       pruneAptRepos,
	protect:     protectAptRepos,
	observe:     observeAptRepos,
	act:         actAptRepos,
	results:     aptRepoResults,
	packageGate: aptRepoPackageGate,
	undeclared:  undeclaredAptRepos,
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func decodeAptRepo(d *graph.SpecDecoder) any {
	var s wireAptRepo
	if !d.Strict(&s) {
		return nil
	}
	d.Need(s.Node != nil, "node")
	d.Need(s.Name != nil, "name")
	d.Need(s.URIs != nil, "uris")
	d.Need(s.Suites != nil, "suites")
	d.Need(s.Components != nil, "components")
	d.Need(s.KeyURL != nil, "keyUrl")
	// Left out, the key's sha256 is mamela.sum's for its URL (docs/design/declaration-inputs.md, 2).
	d.Need(s.KeySHA256 != nil || d.Summed(), "keySha256")
	var archs []string
	if s.Architectures != nil {
		if string(s.Architectures) == "null" || json.Unmarshal(s.Architectures, &archs) != nil {
			d.Invalid(graph.CodeWrongType, "architectures", "architectures is not a list of dpkg architectures",
				`leave the member out (every architecture of the node), or write a list such as ["amd64"]`)
		}
	}
	if !d.OK() {
		return nil
	}
	if len(archs) == 0 {
		archs = nil
	}
	r := &AptRepoSpec{Node: *s.Node, Name: *s.Name, URIs: *s.URIs, Suites: *s.Suites, Components: slices.Clip(*s.Components),
		Architectures: archs, KeyURL: *s.KeyURL}
	if s.KeySHA256 != nil {
		r.KeySHA256 = *s.KeySHA256
	} else {
		d.FromSum("keySha256", r.KeyURL, true, func(h string) { r.KeySHA256 = h })
	}
	return r
}

func validateAptRepo(v *graph.Validator, r graph.Resource) {
	s := AptRepoOf(r)
	v.NodeRef(r, s.Node)
	problems := aptRepoProblems(*s)
	for _, p := range problems {
		v.Problem(r, graph.CodeInvalidValue, p.field, p.message, p.hint)
	}
	if ValidAptRepoName(s.Name) {
		if want := AptRepoName(s.Node, s.Name); r.Name != want {
			v.Problem(r, graph.CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/apt-repo/name)", want), "")
		}
	}
}

// checkAptRepos checks the sources of each node together: two of them must not name one URI and
// suite. apt refuses one source with two keys (Signed-By), and stops reading every source then.
func checkAptRepos(g *graph.Graph, v *graph.Validator) {
	seen := map[string]string{} // node, URI and suite -> the resource that names them
	for _, r := range g.Sorted() {
		s := AptRepoOf(r)
		if s == nil {
			continue
		}
		for _, u := range s.URIs {
			for _, su := range s.Suites {
				key := s.Node + "\x00" + strings.TrimSuffix(u, "/") + "\x00" + su
				if other, ok := seen[key]; ok && other != r.ID() {
					v.Problem(r, graph.CodeConflict, "spec.uris", fmt.Sprintf("%s %s is also a source of %s on %s: apt refuses one source with two keys, and then reads no source at all", u, su, other, s.Node),
						"declare each URI and suite in one source")
					continue
				}
				seen[key] = r.ID()
			}
		}
	}
	// No file of the declaration can name one too: NodeFile does not write apt's source files
	// (CheckPath). Those of the node are mamelet's to find (aptConflicts).
}
