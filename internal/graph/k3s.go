package graph

import (
	"encoding/json/jsontext"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ieyoukan/mamela/internal/names"
	"github.com/ieyoukan/mamela/internal/safetext"
)

// NodeK3sSpec says a node runs k3s: this exact release, whose binary for the node's architecture
// has this sha256, as a server or as an agent of the server on another node. Mamela writes the
// systemd unit itself; the Control Plane gives the cluster's nodes its join token. Nothing else of
// the node's configuration comes from the declaration.
type NodeK3sSpec struct {
	Node    string
	Role    K3sRole
	Server  string // for an agent: the node of its server
	Version string // "v1.33.5+k3s1"
	SHA256  string // hex, of the release's binary for the node's architecture
}

// NodeK3sManifestSpec says a k3s server has this manifest in its auto-deploy directory
// (/var/lib/rancher/k3s/server/manifests/mamela-<name>.yaml). k3s applies a manifest with
// cluster-admin rights, so the plan shows the whole content, and writing or changing it needs an
// approval. The content is the declaration's: nothing is downloaded.
type NodeK3sManifestSpec struct {
	Node    string // a k3s server
	Name    string // [a-z0-9-]: the file is mamela-<name>.yaml
	Content string // YAML, at most MaxK3sManifest bytes
}

// MaxK3sManifest bounds a manifest's content.
const MaxK3sManifest = names.MaxManifest

// K3sRole is what a node is in its k3s cluster.
type K3sRole string

const (
	K3sServer K3sRole = "server"
	K3sAgent  K3sRole = "agent"
)

// K3sServerURL is the URL a k3s agent joins its server at, for a server with a static address.
func K3sServerURL(ip string) string { return "https://" + ip + ":6443" }

type wireNodeK3s struct {
	Node    *string        `json:"node"`
	Role    *string        `json:"role"`
	Server  jsontext.Value `json:"server"` // optional
	Version *string        `json:"version"`
	SHA256  *string        `json:"sha256"`
}

type wireNodeK3sManifest struct {
	Node    *string `json:"node"`
	Name    *string `json:"name"`
	Content *string `json:"content"`
}

type canonNodeK3s struct {
	Node    string `json:"node"`
	Role    string `json:"role"`
	Server  string `json:"server,omitempty"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}

type canonNodeK3sManifest struct {
	Content string `json:"content"`
	Name    string `json:"name"`
	Node    string `json:"node"`
}

// Release is a k3s release ("v1.37.2+k3s1"), or the version a kubelet reports for it, which may
// leave the k3s revision out ("v1.37.2"): Kubernetes' version and, when there is one, k3s's
// revision of it.
type Release struct {
	Major, Minor, Patch int
	K3s                 int  // the revision after "+k3s"
	HasK3s              bool // whether there is one
}

var releaseRe = regexp.MustCompile(`^v([0-9]{1,4})\.([0-9]{1,4})\.([0-9]{1,4})(?:\+k3s([0-9]{1,4}))?$`)

// ParseRelease reads "v1.37.2+k3s1" or "v1.37.2".
func ParseRelease(s string) (Release, bool) {
	m := releaseRe.FindStringSubmatch(s)
	if m == nil {
		return Release{}, false
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	r := Release{Major: n(1), Minor: n(2), Patch: n(3)}
	if m[4] != "" {
		r.K3s, r.HasK3s = n(4), true
	}
	return r, true
}

func (r Release) String() string {
	s := fmt.Sprintf("v%d.%d.%d", r.Major, r.Minor, r.Patch)
	if r.HasK3s {
		s += fmt.Sprintf("+k3s%d", r.K3s)
	}
	return s
}

// Compare is -1, 0 or 1 as r is older than, the same as, or newer than o. The k3s revision counts
// only when both have one: a kubelet that does not report it is not older than the declaration.
func (r Release) Compare(o Release) int {
	for _, d := range [][2]int{{r.Major, o.Major}, {r.Minor, o.Minor}, {r.Patch, o.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	if r.HasK3s && o.HasK3s && r.K3s != o.K3s {
		if r.K3s < o.K3s {
			return -1
		}
		return 1
	}
	return 0
}

// The k3s kinds, registered as a node extension's.
const (
	KindNodeK3s Kind = "NodeK3s"
	// KindNodeK3sManifest is a manifest in the auto-deploy directory of a k3s server: k3s applies
	// what is in it (a Helm chart, a workload) and applies it again whenever it changes.
	KindNodeK3sManifest Kind = "NodeK3sManifest"
)

const (
	// CodeAgentNewer: a k3s agent of a release newer than its server's.
	CodeAgentNewer Code = "MAM-GRAPH-0201"
	// CodeK3sNotStarted: k3s is declared on a node, and nothing declares its service.
	CodeK3sNotStarted Code = "MAM-GRAPH-0202"
)

// K3sOf is the k3s a resource declares (nil for a resource of another kind).
func K3sOf(r Resource) *NodeK3sSpec { s, _ := r.Ext.(*NodeK3sSpec); return s }

// K3sManifestOf is the manifest a resource declares (nil for a resource of another kind).
func K3sManifestOf(r Resource) *NodeK3sManifestSpec { s, _ := r.Ext.(*NodeK3sManifestSpec); return s }

func init() {
	Register(Extension{
		Name: "k3s",
		Kinds: []KindDef{{
			Kind: KindNodeK3s, Order: 3,
			Decode:   decodeK3s,
			Validate: validateK3s,
			Canonical: func(spec any) any {
				s := spec.(*NodeK3sSpec)
				return canonNodeK3s{Node: s.Node, Role: string(s.Role), Server: s.Server, SHA256: s.SHA256, Version: s.Version}
			},
			Node: func(spec any) string { return spec.(*NodeK3sSpec).Node },
			Fields: func(spec any) map[string]string {
				s := spec.(*NodeK3sSpec)
				return map[string]string{"version": s.Version, "sha256": s.SHA256}
			},
			Describe: func(spec any) string {
				k := spec.(*NodeK3sSpec)
				return fmt.Sprintf("k3s %s (sha256 %s…) on %s", k.Version, k.SHA256[:12], k.Node)
			},
		}, {
			Kind: KindNodeK3sManifest, Order: 4,
			Decode:   decodeK3sManifest,
			Validate: validateK3sManifest,
			Canonical: func(spec any) any {
				s := spec.(*NodeK3sManifestSpec)
				return canonNodeK3sManifest{Content: s.Content, Name: s.Name, Node: s.Node}
			},
			Node: func(spec any) string { return spec.(*NodeK3sManifestSpec).Node },
			// The manifest's own name, not the resource's ("app/manifest/argocd").
			Fields: func(spec any) map[string]string { return map[string]string{"name": spec.(*NodeK3sManifestSpec).Name} },
			Describe: func(spec any) string {
				m := spec.(*NodeK3sManifestSpec)
				return fmt.Sprintf("k3s manifest %s (%d bytes) on %s", m.Name, len(m.Content), m.Node)
			},
		}},
		Check:        checkK3sService,
		Warnings:     k3sWarnings,
		WarningCodes: []Code{CodeAgentNewer, CodeK3sNotStarted},
	})
}

func decodeK3s(d *SpecDecoder) any {
	var s wireNodeK3s
	if !d.Strict(&s) {
		return nil
	}
	d.Need(s.Node != nil, "node")
	d.Need(s.Role != nil, "role")
	d.Need(s.Version != nil, "version")
	d.Need(s.SHA256 != nil, "sha256")
	server := d.OptionalString(s.Server, "server")
	if server != nil && *server == "" {
		d.Invalid(CodeInvalidValue, "server", "the server is an empty string", "leave the member out (a k3s server has no server), or name the node of the server (an agent)")
	}
	if !d.OK() {
		return nil
	}
	k := &NodeK3sSpec{Node: *s.Node, Role: K3sRole(*s.Role), Version: *s.Version, SHA256: *s.SHA256}
	if server != nil {
		k.Server = *server
	}
	return k
}

func decodeK3sManifest(d *SpecDecoder) any {
	var s wireNodeK3sManifest
	if !d.Strict(&s) {
		return nil
	}
	d.Need(s.Node != nil, "node")
	d.Need(s.Name != nil, "name")
	d.Need(s.Content != nil, "content")
	if !d.OK() {
		return nil
	}
	return &NodeK3sManifestSpec{Node: *s.Node, Name: *s.Name, Content: *s.Content}
}

// k3sServers are the nodes the graph declares as k3s servers.
func k3sServers(g *Graph) map[string]bool {
	servers := map[string]bool{}
	for _, r := range g.Resources {
		if k := K3sOf(r); k != nil && k.Role == K3sServer {
			servers[k.Node] = true
		}
	}
	return servers
}

func validateK3s(v *Validator, r Resource) {
	s := K3sOf(r)
	v.NodeRef(r, s.Node)
	if !names.K3sVersion(s.Version) {
		v.Problem(r, CodeInvalidValue, "spec.version", fmt.Sprintf("%q is not a k3s release", s.Version),
			"write the release tag, such as \"v1.33.5+k3s1\"")
	}
	if !names.SHA256(s.SHA256) {
		v.Problem(r, CodeInvalidValue, "spec.sha256", "sha256 is not 64 lowercase hex digits",
			"copy it from the release's sha256sum file for the node's architecture")
	}
	if want := s.Node + "/k3s"; r.Name != want {
		v.Problem(r, CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/k3s)", want), "")
	}
	switch {
	case s.Role == K3sServer && s.Server != "":
		v.Problem(r, CodeInvalidValue, "spec.server", "a k3s server has no server", "")
	case s.Role == K3sAgent && !v.Memo("k3s.servers", func() any { return k3sServers(v.Graph()) }).(map[string]bool)[s.Server]:
		v.Problem(r, CodeDanglingNode, "spec.server", fmt.Sprintf("%q is not a node declared as a k3s server in this graph", s.Server),
			"declare the server with K3s.server on that node")
	case s.Role == K3sAgent && s.Server == s.Node:
		v.Problem(r, CodeInvalidValue, "spec.server", "a node cannot be an agent of itself", "")
	case s.Role != K3sServer && s.Role != K3sAgent:
		v.Problem(r, CodeInvalidValue, "spec.role", fmt.Sprintf("role %q is not server or agent", s.Role), "")
	}
}

func validateK3sManifest(v *Validator, r Resource) {
	s := K3sManifestOf(r)
	v.NodeRef(r, s.Node)
	if !v.Memo("k3s.servers", func() any { return k3sServers(v.Graph()) }).(map[string]bool)[s.Node] {
		v.Problem(r, CodeDanglingNode, "spec.node", fmt.Sprintf("%q is not a node declared as a k3s server in this graph", s.Node),
			"k3s applies manifests on a server: declare the server with K3s.server on that node")
	}
	if !names.Manifest(s.Name) {
		v.Problem(r, CodeInvalidValue, "spec.name", fmt.Sprintf("%q is not a valid manifest name", s.Name),
			"use lowercase letters, digits and '-' (the file is mamela-<name>.yaml)")
	}
	switch {
	case s.Content == "" || strings.TrimSpace(s.Content) == "":
		v.Problem(r, CodeInvalidValue, "spec.content", "the manifest is empty", "")
	case len(s.Content) > MaxK3sManifest:
		v.Problem(r, CodeInvalidValue, "spec.content", fmt.Sprintf("the manifest is %d bytes; at most %d", len(s.Content), MaxK3sManifest),
			"a manifest that needs more should install a Helm chart (a HelmChart manifest) instead of holding it")
	case !utf8.ValidString(s.Content) || strings.ContainsRune(s.Content, 0):
		v.Problem(r, CodeInvalidValue, "spec.content", "the manifest is not text (UTF-8 without NUL)", "")
	default:
		// k3s applies the manifest with cluster-admin rights and the plan shows it in full: it
		// must show as what it is.
		if f, bad := safetext.Find(s.Content, true); bad {
			v.Problem(r, CodeUnsafeText, "spec.content", "the manifest has "+f.String(), UnsafeTextHint)
		}
	}
	if want := s.Node + "/manifest/" + s.Name; r.Name != want {
		v.Problem(r, CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/manifest/name)", want), "")
	}
}

// checkK3sService refuses the k3s service declared not running on a node that is declared to run
// k3s.
func checkK3sService(g *Graph, v *Validator) {
	k3sNodes := map[string]bool{}
	for _, r := range g.Resources {
		if k := K3sOf(r); k != nil {
			k3sNodes[k.Node] = true
		}
	}
	for _, r := range g.Resources {
		s := r.NodeService
		if s == nil {
			continue
		}
		if unit := strings.TrimSuffix(s.Service, ".service"); unit == "k3s" && !s.Running && k3sNodes[s.Node] {
			v.Problem(r, CodeConflict, "spec.running", fmt.Sprintf("%s says k3s does not run on %s, but %s/k3s declares that it runs k3s there", r.ID(), s.Node, s.Node),
				"declare the service running (k3s is only installed by NodeK3s; starting it is this service's job), or take NodeK3s out")
		}
	}
}

// k3sWarnings: an agent newer than its server, and k3s whose service nothing declares.
func k3sWarnings(g *Graph) []Warning {
	servers := map[string]*NodeK3sSpec{} // by node
	services := map[string]bool{}        // node + "\x00" + unit, of the declared NodeService
	for _, r := range g.Resources {
		switch k := K3sOf(r); {
		case k != nil && k.Role == K3sServer:
			servers[k.Node] = k
		case r.NodeService != nil:
			services[r.NodeService.Node+"\x00"+strings.TrimSuffix(r.NodeService.Service, ".service")] = true
		}
	}
	var out []Warning
	for _, r := range g.Sorted() {
		k := K3sOf(r)
		if k == nil {
			continue
		}
		if srv := servers[k.Server]; k.Role == K3sAgent && srv != nil {
			agent, okA := ParseRelease(k.Version)
			server, okS := ParseRelease(srv.Version)
			if okA && okS && agent.Compare(server) > 0 {
				out = append(out, Warning{Code: CodeAgentNewer, Resource: r.ID(),
					Message: fmt.Sprintf("the agent on %s is declared at %s, newer than its server %s at %s: Kubernetes does not support a kubelet newer than its API server", k.Node, k.Version, srv.Node, srv.Version),
					Hint:    "declare the server's release first (the server is upgraded before its agents), or the agent's at the server's release at most"})
			}
		}
		if !services[k.Node+"\x00k3s"] {
			out = append(out, Warning{Code: CodeK3sNotStarted, Resource: r.ID(),
				Message: fmt.Sprintf("k3s is declared on %s, but nothing declares its service: the binary is installed and k3s is not started or kept running (the plan says in sync)", k.Node),
				Hint:    "declare the service of k3s on the node: Mamela.service(graph, node, \"k3s\", enabled: true, running: true)"})
		}
	}
	return out
}
