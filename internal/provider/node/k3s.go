package node

import (
	"context"
	"slices"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/store"
)

// k3sRevision is what a node's revision says about k3s: the release, the role, the cluster's
// token (made once per cluster, keyed by its server) and, for an agent, its server's address as
// the server's mamelet last reported it, unless the server has a declared static address
// (serverIP). ok is false while an agent's server has no address yet.
func k3sRevision(ctx context.Context, st *store.Store, k graph.NodeK3sSpec, serverIP string) (noderev.K3s, bool, error) {
	kr := noderev.K3s{Version: k.Version, SHA256: k.SHA256, Role: string(k.Role)}
	cluster := k.Node
	if k.Role == graph.K3sAgent && serverIP != "" {
		cluster, kr.Server = k.Server, graph.K3sServerURL(serverIP)
	} else if k.Role == graph.K3sAgent {
		cluster = k.Server
		url, ok, err := K3sServerURL(ctx, st, k.Server)
		if err != nil || !ok {
			return kr, false, err
		}
		kr.Server = url
	}
	token, err := st.K3sToken(ctx, cluster)
	if err != nil {
		return kr, false, err
	}
	kr.Token = token
	return kr, true, nil
}

// K3sServerURL is the address an agent joins a server node at, from the server's last report.
func K3sServerURL(ctx context.Context, st *store.Store, server string) (string, bool, error) {
	nr, ok, err := st.NodeReportOf(ctx, server)
	if err != nil || !ok || nr.Report.K3s == nil || len(nr.Report.K3s.Addresses) == 0 {
		return "", false, err
	}
	return "https://" + slices.Sorted(slices.Values(nr.Report.K3s.Addresses))[0] + ":6443", true, nil
}

// declaresNode is whether the declaration has a k3s node called node that joins server (or is it).
func declaresNode(g *graph.Graph, server, node string) bool {
	for _, r := range g.Sorted() {
		if k := graph.K3sOf(r); k != nil && k.Node == node && (k.Server == server || k.Node == server) {
			return true
		}
	}
	return false
}

func findManifest(r *noderev.Revision, name string) *noderev.Manifest {
	for i := range r.Manifests {
		if r.Manifests[i].Name == name {
			return &r.Manifests[i]
		}
	}
	return nil
}
