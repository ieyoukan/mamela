package graph

import (
	"fmt"
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
