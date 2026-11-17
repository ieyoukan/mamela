package graph

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Warning is something in a valid graph that is probably not what was meant. Validate refuses what
// cannot be right; a warning is for what can be, but usually is not. It stops and changes nothing:
// the plan shows it with the resource it is about (plan.Change.Warnings).
type Warning struct {
	Code     Code
	Resource string // the resource it is about, as its ID ("NodeK3s web/k3s")
	Message  string
	Hint     string
}

const (
	// CodeAgentNewer: a k3s agent of a release newer than its server's.
	CodeAgentNewer Code = "MAM-GRAPH-0201"
	// CodeK3sNotStarted: k3s is declared on a node, and nothing declares its service.
	CodeK3sNotStarted Code = "MAM-GRAPH-0202"
)

// Warnings are the warnings of the graph, in the order of its resources (canonical order), and for
// one resource in the order of the codes. They depend on the declaration alone, so the same
// declaration has the same warnings.
func (g *Graph) Warnings() []Warning {
	servers := map[string]*NodeK3sSpec{} // by node
	services := map[string]bool{}        // node + "\x00" + unit, of the declared NodeService
	for _, r := range g.Resources {
		switch {
		case r.NodeK3s != nil && r.NodeK3s.Role == K3sServer:
			servers[r.NodeK3s.Node] = r.NodeK3s
		case r.NodeService != nil:
			services[r.NodeService.Node+"\x00"+strings.TrimSuffix(r.NodeService.Service, ".service")] = true
		}
	}
	var out []Warning
	for _, r := range g.Sorted() {
		k := r.NodeK3s
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
