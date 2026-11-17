package k3s

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/store"
)

// Capability names of k3s's operations: what mamelet does with the grant.
const (
	CapInstall  = "k3s.install"
	CapManifest = "k3s.manifest"
	CapRemove   = "k3s.remove-node"
)

// KeySHA256 is the condition key of the sha256 of the downloaded binary, which mamelet checks.
const KeySHA256 = "mamela:k3s.sha256" // sha256 of the downloaded binary

func init() {
	node.Register(node.Extension{
		Name:    "k3s",
		Kinds:   manifestKinds(),
		Observe: observe,
		Publish: publish,
		Protect: protect,
		Prune:   prune,
		Grants: map[policy.Op]node.GrantFunc{
			OpInstall:       grantInstall,
			OpUpgrade:       grantInstall,
			OpRemoveNode:    grantRemoveNode,
			OpWriteManifest: grantManifest,
		},
	})
}

// manifestKinds are the k3s kinds in the node provider's manifest.
func manifestKinds() []spec.Kind {
	target := spec.Target{Type: node.TargetNode, Ref: "{node}"}
	installNeed := spec.Need{
		Capability: CapInstall, Target: target, Object: "k3s {version}", Always: true,
		Conditions: append(node.GrantConditions(),
			spec.Condition{Key: KeySHA256, Test: spec.Equals, Values: []string{"{sha256}"}}),
		Note:    "mamelet downloads the release and installs it only if its sha256 is the declared one, with Mamela's fixed unit",
		Reasons: []string{"mamelet writes /usr/local/bin/k3s and its systemd unit once"},
	}
	upgradeNeed := installNeed
	upgradeNeed.Note = "the same grant as an installation, for a k3s that runs: mamelet then restarts it, so that the node runs the declared release"
	upgradeNeed.Reasons = []string{"mamelet writes /usr/local/bin/k3s and its systemd unit once, and restarts k3s"}
	k3s := spec.Kind{Kind: KindK3s, Operations: []spec.Operation{
		{Op: OpInstall, Support: spec.Supported, Needs: []spec.Need{installNeed}},
		{Op: OpUpgrade, Support: spec.Supported, Needs: []spec.Need{upgradeNeed}},
		{Op: OpRemoveNode, Support: spec.Supported, Needs: []spec.Need{{
			Capability: CapRemove, Target: target, Object: "a Kubernetes node the declaration no longer names (the one the approved plan names)", Always: true,
			Conditions: node.GrantConditions(),
			Note:       "mamelet on the server cordons and drains the node (when it is Ready), then deletes it with the server's own kubeconfig; it never removes the server's own node",
			Reasons:    []string{"mamelet runs k3s kubectl cordon, drain and delete node once"},
		}}},
	}}
	manifest := spec.Kind{Kind: KindManifest, Operations: []spec.Operation{{
		Op: OpWriteManifest, Support: spec.Supported, Needs: []spec.Need{{
			Capability: CapManifest, Target: target, Object: "{name}", Always: true,
			Conditions: node.GrantConditions(),
			Note:       "mamelet writes exactly the content of the approved plan to /var/lib/rancher/k3s/server/manifests/mamela-<name>.yaml, which k3s applies with cluster-admin rights",
			Reasons:    []string{"mamelet writes the manifest file once"},
		}},
	}}}
	return []spec.Kind{k3s, manifest}
}

// observe turns a node's report of k3s and of its manifests into observations.
func observe(r noderev.Report, received time.Time, snap *state.Snapshot) {
	for _, m := range r.Manifests {
		if snap.K3sManifests == nil {
			snap.K3sManifests = map[string]state.K3sManifest{}
		}
		snap.K3sManifests[r.Node+"/manifest/"+m.Name] = state.K3sManifest{SHA256: m.SHA256, Error: m.Error, ObservedAt: received}
	}
	if k := r.K3s; k != nil {
		if snap.K3s == nil {
			snap.K3s = map[string]state.K3s{}
		}
		var nodes []state.K3sNode
		for _, n := range k.Nodes {
			nodes = append(nodes, state.K3sNode{Name: n.Name, Version: n.Version, Ready: n.Ready})
		}
		snap.K3s[r.Node+"/k3s"] = state.K3s{Installed: k.Installed, SHA256: k.SHA256, Error: k.Error, Server: k.Server,
			Running: k.Running, Stale: k.Stale, Nodes: nodes,
			Addresses: slices.Sorted(slices.Values(k.Addresses)), ObservedAt: received}
	}
}

// publish adds to a node's revision the manifests and the k3s g declares on it that it does not
// have yet (with no permission), and takes away an old removal of a node that g declares again.
func publish(ctx context.Context, st *store.Store, g *graph.Graph, name string) (func(r *noderev.Revision), error) {
	var k3s *noderev.K3s
	var manifests []noderev.Manifest
	for _, r := range g.Sorted() {
		switch {
		case ManifestOf(r) != nil && ManifestOf(r).Node == name:
			manifests = append(manifests, noderev.Manifest{Name: ManifestOf(r).Name, Content: ManifestOf(r).Content})
		case SpecOf(r) != nil && SpecOf(r).Node == name:
			kr, ok, err := revisionOf(ctx, st, *SpecOf(r), g.StaticIP(SpecOf(r).Server))
			if err != nil {
				return nil, err
			}
			if ok {
				k3s = &kr
			}
		}
	}
	return func(r *noderev.Revision) {
		r.Manifests = slices.Clone(r.Manifests)
		for _, m := range manifests {
			if findManifest(r, m.Name) == nil {
				r.Manifests = append(r.Manifests, m)
			}
		}
		if r.K3s == nil {
			r.K3s = k3s
		}
		if rm := r.K3s; rm != nil && rm.Remove != nil && declaresNode(g, name, rm.Remove.Node) {
			// A node that is declared again is never removed by an old grant.
			k := *rm
			k.Remove = nil
			r.K3s = &k
		}
	}, nil
}

// protect takes every k3s grant out of a protected node's revision.
func protect(r *noderev.Revision) {
	if r.K3s != nil {
		k := *r.K3s
		k.Install, k.Remove = nil, nil
		r.K3s = &k
	}
	r.Manifests = slices.Clone(r.Manifests)
	for i := range r.Manifests {
		r.Manifests[i].Write = nil
	}
}

// prune takes out of a node's revision the manifests and the k3s g no longer declares on it.
func prune(g *graph.Graph, name string, r *noderev.Revision) {
	manifests, k3s := map[string]bool{}, false
	for _, res := range g.Resources {
		switch {
		case ManifestOf(res) != nil && ManifestOf(res).Node == name:
			manifests[ManifestOf(res).Name] = true
		case SpecOf(res) != nil && SpecOf(res).Node == name:
			k3s = true
		}
	}
	r.Manifests = slices.DeleteFunc(slices.Clone(r.Manifests), func(m noderev.Manifest) bool { return !manifests[m.Name] })
	if !k3s {
		r.K3s = nil // with its join token and its grants
	}
}

// grantInstall grants the installation or the upgrade of the declared k3s.
func grantInstall(ctx context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := SpecOf(q.Step.Resource)
	if want == nil {
		return node.Grant{}, false, nil
	}
	kr, ok, err := revisionOf(ctx, q.Store, *want, q.Declaration.StaticIP(want.Server))
	if err != nil {
		return node.Grant{}, true, err
	}
	if !ok {
		return node.Grant{}, true, fmt.Errorf("k3s server address is not observed; plan again")
	}
	g := node.Grant{Node: want.Node, What: "install k3s " + want.Version}
	if q.Step.Op == OpUpgrade {
		g.What = "upgrade to k3s " + want.Version
	}
	g.Put = func(rev *noderev.Revision, once func(string) noderev.Once) error {
		// An agent joins the server the approved plan named, or nothing happens.
		if approved := q.Approved("server"); kr.Server != approved {
			return fmt.Errorf("the revision of %s names server %q, the approved plan %q (plan again)", g.Node, kr.Server, approved)
		}
		k := kr
		in := once(noderev.ActionInstall)
		k.Install = &in
		rev.K3s = &k
		return nil
	}
	return g, true, nil
}

// grantRemoveNode grants a server the removal of the Kubernetes node the approved plan names.
func grantRemoveNode(_ context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := SpecOf(q.Step.Resource)
	if want == nil || want.Role != RoleServer {
		return node.Grant{}, false, nil
	}
	// The node to remove is the one the approved plan named, and no other.
	target := q.Approved("remove-node")
	if !noderev.ValidNode(target) {
		return node.Grant{}, true, fmt.Errorf("the approved plan names no Kubernetes node to remove (plan again)")
	}
	g := node.Grant{Node: want.Node, What: "remove Kubernetes node " + target}
	g.Put = func(rev *noderev.Revision, once func(string) noderev.Once) error {
		if rev.K3s == nil || rev.K3s.Role != noderev.K3sServer {
			return fmt.Errorf("%s has no k3s server in its revision (plan again)", g.Node)
		}
		k := *rev.K3s
		k.Remove = &noderev.NodeRemoval{Node: target, Once: once(noderev.ActionRemove)}
		rev.K3s = &k
		return nil
	}
	return g, true, nil
}

// grantManifest grants the writing of exactly the manifest the approved plan showed.
func grantManifest(_ context.Context, q node.GrantRequest) (node.Grant, bool, error) {
	want := ManifestOf(q.Step.Resource)
	if want == nil {
		return node.Grant{}, false, nil
	}
	m := *want
	g := node.Grant{Node: m.Node, What: "write k3s manifest " + m.Name}
	g.Put = func(rev *noderev.Revision, once func(string) noderev.Once) error {
		// Exactly the content the approved plan showed, or nothing happens.
		if approved := q.Approved("sha256"); approved != noderev.ManifestSHA256(m.Content) {
			return fmt.Errorf("the manifest %s is not the one the approved plan showed (plan again)", m.Name)
		}
		w := once(noderev.ActionWrite)
		rev.Manifests = slices.Clone(rev.Manifests)
		if cur := findManifest(rev, m.Name); cur != nil {
			cur.Content, cur.Write = m.Content, &w
		} else {
			rev.Manifests = append(rev.Manifests, noderev.Manifest{Name: m.Name, Content: m.Content, Write: &w})
		}
		return nil
	}
	return g, true, nil
}

// revisionOf is what a node's revision says about k3s: the release, the role, the cluster's
// token (made once per cluster, keyed by its server) and, for an agent, its server's address as
// the server's mamelet last reported it, unless the server has a declared static address
// (serverIP). ok is false while an agent's server has no address yet.
func revisionOf(ctx context.Context, st *store.Store, k Spec, serverIP string) (noderev.K3s, bool, error) {
	kr := noderev.K3s{Version: k.Version, SHA256: k.SHA256, Role: string(k.Role)}
	cluster := k.Node
	if k.Role == RoleAgent && serverIP != "" {
		cluster, kr.Server = k.Server, ServerURL(serverIP)
	} else if k.Role == RoleAgent {
		cluster = k.Server
		url, ok, err := ReportedServerURL(ctx, st, k.Server)
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

// ReportedServerURL is the address an agent joins a server node at, from the server's last report.
func ReportedServerURL(ctx context.Context, st *store.Store, server string) (string, bool, error) {
	nr, ok, err := st.NodeReportOf(ctx, server)
	if err != nil || !ok || nr.Report.K3s == nil || len(nr.Report.K3s.Addresses) == 0 {
		return "", false, err
	}
	return "https://" + slices.Sorted(slices.Values(nr.Report.K3s.Addresses))[0] + ":6443", true, nil
}

// declaresNode is whether the declaration has a k3s node called node that joins server (or is it).
func declaresNode(g *graph.Graph, server, node string) bool {
	for _, r := range g.Sorted() {
		if k := SpecOf(r); k != nil && k.Node == node && (k.Server == server || k.Node == server) {
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
