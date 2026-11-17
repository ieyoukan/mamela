package graph

import (
	"encoding/json/jsontext"
	"fmt"
	"regexp"
	"strconv"

	"github.com/ieyoukan/mamela/internal/names"
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
