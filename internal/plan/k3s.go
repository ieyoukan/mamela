package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
)

// machineRestartGate says what a restart of a machine that runs a k3s node waits for (nil: nothing,
// or the machine runs none). The machines of a cluster are restarted one at a time (the agents in
// the order of their names, the server last), and only while every Kubernetes node is Ready: a
// plan that restarts them all at once would take the cluster down.
func (b builder) machineRestartGate(r graph.Resource) *Note {
	var mine *graph.NodeK3sSpec
	for _, o := range b.g.Sorted() {
		if o.NodeK3s != nil && o.NodeK3s.Node == r.Name {
			mine = o.NodeK3s
		}
	}
	if mine == nil || !b.snap.K3s[mine.Node+"/k3s"].Running {
		return nil
	}
	server := mine.Server
	if mine.Role == graph.K3sServer {
		server = mine.Node
	}
	for _, o := range b.g.Sorted() {
		k := o.NodeK3s
		if k == nil || !restartsBefore(k, mine) || (k.Node != server && k.Server != server) {
			continue
		}
		for _, m := range b.g.Sorted() {
			if m.Provided != nil && m.Name == k.Node && b.restartsMachine(b.snap.Provided[m.ID()]) && b.snap.Provided[m.ID()].Observed.Exists {
				return &Note{Code: "MAM-PLAN-0118", Message: fmt.Sprintf("waits for machine %s to be restarted before %s (the machines of a cluster are restarted one at a time)", m.Name, r.Name),
					Hint: "apply the plan that restarts it, then plan again"}
			}
		}
	}
	nodes := b.declaredNodes(b.snap.K3s[server+"/k3s"].Nodes, &graph.NodeK3sSpec{Node: server, Role: graph.K3sServer})
	var notReady []string
	for _, n := range nodes {
		if !n.Ready {
			notReady = append(notReady, n.Name)
		}
	}
	switch {
	case len(nodes) == 0:
		return &Note{Code: "MAM-PLAN-0115", Message: fmt.Sprintf("waits for the k3s server to report its Kubernetes nodes before machine %s is restarted", r.Name),
			Hint: "the server reports them while it runs; if it never does, look at k3s on the server"}
	case len(notReady) > 0:
		return &Note{Code: "MAM-PLAN-0115", Message: fmt.Sprintf("waits for every Kubernetes node to be Ready before machine %s is restarted (not Ready: %s)", r.Name, strings.Join(notReady, ", ")),
			Hint: "a restart goes on only on a healthy cluster; find out why the node is not Ready"}
	}
	return nil
}

// restartsBefore is whether the machine of k3s node k is restarted before the machine of node
// mine: the agents come first, in the order of their names, and the server is the last.
func restartsBefore(k, mine *graph.NodeK3sSpec) bool {
	if kServer, mineServer := k.Role == graph.K3sServer, mine.Role == graph.K3sServer; kServer != mineServer {
		return mineServer
	}
	return k.Node < mine.Node
}

// k3s plans a k3s server on a node from mamelet's report of it.
func (b builder) k3s(r graph.Resource) Change {
	want := r.NodeK3s
	c := newChange(r)
	if ok, note := b.nodeReady(want.Node); !ok {
		c.Status, c.Note = StatusWaiting, note
		return c
	}
	got, observed := b.snap.K3s[r.Name]
	switch {
	case !observed:
		c.Status, c.Note = b.awaitAgent(want.Node, &Note{Code: "MAM-PLAN-0108",
			Message: fmt.Sprintf("waits for the agent on %s to report k3s", want.Node)})
		return c
	case b.stale(got.ObservedAt):
		c.Status, c.Note = StatusBlocked, b.staleNote(got.ObservedAt)
		return c
	case got.Error != "":
		c.Status, c.Note = StatusBlocked, &Note{Code: "MAM-PLAN-0204",
			Message: fmt.Sprintf("the agent on %s cannot observe k3s: %s", want.Node, got.Error),
			Hint:    "fix it on the node, then plan again"}
		return c
	case got.Installed && got.SHA256 == want.SHA256 && !got.Stale:
		if note := b.k3sVersionNote(r, got); note != nil {
			c.Status, c.Note = StatusWaiting, note
			return c
		}
		return b.retireNode(r, got, b.withOps(c))
	}
	// A k3s whose binary is in place is changed in place: an agent only after its server runs the
	// declared release; and, if it runs (it is restarted), the agents one at a time and only while
	// every Kubernetes node is Ready.
	upgrade := got.Running
	if got.SHA256 != "" {
		if note := b.upgradeGate(r, got); note != nil {
			c.Status, c.Note = StatusWaiting, note
			return c
		}
	}
	server := ""
	if want.Role == graph.K3sAgent && b.g.StaticIP(want.Server) != "" {
		// The server's declared address: an agent never waits for, or follows, what is reported.
		server = graph.K3sServerURL(b.g.StaticIP(want.Server))
	} else if want.Role == graph.K3sAgent {
		srv := b.snap.K3s[want.Server+"/k3s"]
		if len(srv.Addresses) == 0 {
			c.Status, c.Note = StatusWaiting, &Note{Code: "MAM-PLAN-0109",
				Message: fmt.Sprintf("waits for the k3s server %s to report its address", want.Server)}
			return c
		}
		server = "https://" + srv.Addresses[0] + ":6443"
	}
	// The diff says exactly what is approved: the release, the binary's sha256, where it comes
	// from, how it runs and, for an agent, the server it joins. The join token is made by the
	// Control Plane and given only to the cluster's nodes; it is never shown.
	c.Diffs = []Diff{{"version", b.k3sRunning(r, got), want.Version}, {"sha256", got.SHA256, want.SHA256},
		{"from", "", noderev.K3sReleases + "/" + noderev.K3sTag(want.Version) + "/k3s (k3s-arm64 on arm64)"},
		{"role", "", string(want.Role)}}
	if server != "" {
		// Always shown, changed or not: the grant checks the server in the revision against this
		// approved value, so it is what the approval says the agent joins.
		c.Diffs = append(c.Diffs, Diff{"server", got.Server, server})
	}
	c.Diffs = append(c.Diffs, Diff{"runs", "", "/usr/local/bin/k3s " + strings.Join(noderev.K3sArgs(string(want.Role), server), " ")},
		Diff{"token", "", "the cluster's join token, made by the Control Plane, in " + noderev.K3sTokenFile + " (0600)"})
	if upgrade {
		c.Diffs = append(c.Diffs, Diff{"restart", "", "k3s on " + want.Node + " restarts into this release; its Kubernetes node is not Ready for a while"})
		return b.withOps(c, policy.Subject{Op: policy.OpUpgradeK3s})
	}
	return b.withOps(c, policy.Subject{Op: policy.OpInstallK3s})
}

// retireNode adds to the plan of a k3s server that is in sync the removal of a Kubernetes node
// that no declaration of this cluster names any more: a node whose machine this plan deletes (it
// is drained first, while its machine still runs), or a Not Ready node whose machine is gone and on
// which a mamelet once enrolled (a left-over of a machine Mamela made: a node joined by hand, on a
// machine no provider observes, looks the same but for that). One node per plan; the server's own
// node, a node whose machine still exists, and a cluster whose node names do not include the
// server's own are never touched. It is a deletion:
// the policy's removal = retain keeps it, and only the Control Plane's host approves it.
func (b builder) retireNode(r graph.Resource, got state.K3s, c Change) Change {
	want := r.NodeK3s
	if want.Role != graph.K3sServer || !got.Running || !b.pol.Deletes() ||
		!slices.ContainsFunc(got.Nodes, func(n state.K3sNode) bool { return n.Name == want.Node }) {
		return c
	}
	declared := map[string]bool{}
	for _, o := range b.g.Sorted() {
		if k := o.NodeK3s; k != nil && (k.Node == want.Node || k.Server == want.Node) {
			declared[k.Node] = true
		}
	}
	byName := slices.SortedFunc(slices.Values(got.Nodes), func(x, y state.K3sNode) int { return strings.Compare(x.Name, y.Name) })
	for _, n := range byName {
		if declared[n.Name] || b.pol.Protected(n.Name) {
			continue
		}
		var why string
		switch {
		case b.machineLeaves(n.Name):
			why = "its machine is deleted by this plan; its pods are moved to other nodes first, then the node is deleted"
			if !n.Ready {
				why = "its machine is deleted by this plan and the node is not Ready (it cannot be drained); the node is deleted"
			}
		case !n.Ready && !b.machineExists(n.Name) && b.snap.AgentRegistrations[n.Name] != "":
			why = "the node is not Ready and no machine of that name exists any more, and a mamelet once enrolled there (a left-over of a machine Mamela made); the node is deleted"
		default:
			continue
		}
		// The approval names the node: the grant takes it from this diff, and no other.
		c.Diffs = append(c.Diffs, Diff{"remove-node", "", n.Name}, Diff{"remove-node because", "", why})
		d := b.pol.Decide(policy.Subject{Op: policy.OpRemoveK3sNode})
		c.Status = StatusChange
		c.Verdict = policy.Stricter(c.Verdict, d.Verdict)
		c.Operations = append(c.Operations, Operation{Op: policy.OpRemoveK3sNode, Decision: d, Late: true, Deletes: true})
		return c
	}
	return c
}

// k3sManifest plans a manifest file in a k3s server's auto-deploy directory. Its whole content is
// in the diff: that is what is approved, and what k3s then applies with cluster-admin rights.
func (b builder) k3sManifest(r graph.Resource) Change {
	want := r.NodeK3sManifest
	c := newChange(r)
	if ok, note := b.nodeReady(want.Node); !ok {
		c.Status, c.Note = StatusWaiting, note
		return c
	}
	got, observed := b.snap.K3sManifests[r.Name]
	switch {
	case !observed:
		c.Status, c.Note = b.awaitAgent(want.Node, &Note{Code: "MAM-PLAN-0117",
			Message: fmt.Sprintf("waits for the agent on %s to report the manifest %s", want.Node, want.Name)})
		return c
	case b.stale(got.ObservedAt):
		c.Status, c.Note = StatusBlocked, b.staleNote(got.ObservedAt)
		return c
	case got.Error != "":
		c.Status, c.Note = StatusBlocked, &Note{Code: "MAM-PLAN-0204",
			Message: fmt.Sprintf("the agent on %s cannot observe the manifest %s: %s", want.Node, want.Name, got.Error),
			Hint:    "fix it on the node, then plan again"}
		return c
	}
	sum := noderev.ManifestSHA256(want.Content)
	if got.SHA256 == sum {
		return b.withOps(c)
	}
	c.Diffs = []Diff{{"file", "", noderev.K3sManifestDir + "/" + noderev.K3sManifestFile(want.Name)},
		{"sha256", got.SHA256, sum}, {"content", "", want.Content}}
	return b.withOps(c, policy.Subject{Op: policy.OpWriteK3sManifest})
}

// rollbackWarnMinors is how many minor releases back a declaration may take a running k3s before
// the plan warns: Kubernetes does not support a downgrade (what a newer release stored, an older
// one may not read), and a release that far back is more likely a typo than a plan.
const rollbackWarnMinors = 7

// rollbackWarning says that a declared release is a rollback of rollbackWarnMinors minor releases
// or more from the release that runs (nil when it is not, or when it is not known).
func rollbackWarning(want *graph.NodeK3sSpec, running string) *Note {
	d, okD := graph.ParseRelease(want.Version)
	n, okN := graph.ParseRelease(running)
	if !okD || !okN || (n.Major == d.Major && n.Minor-d.Minor < rollbackWarnMinors) || n.Major < d.Major {
		return nil
	}
	return &Note{Code: "MAM-PLAN-0401",
		Message: fmt.Sprintf("k3s on %s runs %s and the declaration takes it back to %s, a rollback of %s: Kubernetes does not support downgrades, and what the newer release stored the older may not read",
			want.Node, running, want.Version, rollbackSize(n, d)),
		Hint: "if this is a typo, fix the version and the sha256; to really go back, restore the cluster from a snapshot taken at that release (Mamela does not roll a cluster back)"}
}

// rollbackWarnings are the warnings of a k3s that runs, whose declared release is another binary
// (an upgrade in place that takes it far back), whether the upgrade is planned or waits.
func (b builder) rollbackWarnings(r graph.Resource) []Note {
	got, ok := b.snap.K3s[r.Name]
	if !ok || !got.Running || got.SHA256 == r.NodeK3s.SHA256 {
		return nil
	}
	if w := rollbackWarning(r.NodeK3s, b.k3sRunning(r, got)); w != nil {
		return []Note{*w}
	}
	return nil
}

func rollbackSize(n, d graph.Release) string {
	if n.Major != d.Major {
		return fmt.Sprintf("%d major releases", n.Major-d.Major)
	}
	return fmt.Sprintf("%d minor releases", n.Minor-d.Minor)
}

// k3sVersionNote says what a k3s waits for when its binary is the declared one (by its sha256) and
// runs, and its Kubernetes node reports another release than the declared version (nil when they
// agree or nothing is known). The binary is right by its sha256, so either the node has not
// reported the release it restarted into yet (the next plan agrees), or the version and the
// sha256 of the declaration are of different releases (a version changed, the sha256 forgotten).
// The kubelet may report "v1.37.2" for "v1.37.2+k3s1": the k3s revision counts only when it is
// reported.
func (b builder) k3sVersionNote(r graph.Resource, got state.K3s) *Note {
	want := r.NodeK3s
	if !got.Running {
		return nil // what the node last said is from before it stopped
	}
	if want.Role == graph.K3sAgent {
		if srv, ok := b.snap.K3s[want.Server+"/k3s"]; !ok || b.stale(srv.ObservedAt) {
			return nil // the server's view of the nodes is not current
		}
	}
	reported := b.k3sRunning(r, got)
	d, okD := graph.ParseRelease(want.Version)
	n, okN := graph.ParseRelease(reported)
	if !okD || !okN || d.Compare(n) == 0 {
		return nil
	}
	return &Note{Code: "MAM-PLAN-0121",
		Message: fmt.Sprintf("the declaration says k3s on %s is %s, and the binary on it is the declared one (sha256 %s), but its Kubernetes node reports %s",
			want.Node, want.Version, want.SHA256[:12], reported),
		Hint: "if the node has only just restarted, plan again in a minute; if it stays, the version and the sha256 do not belong together: " +
			"put the sha256 of the binary of " + want.Version + " (the release's sha256sum-<arch>.txt) with it, or the version of the binary the sha256 is of"}
}

// k3sCurrent is whether a node's k3s is what its declaration says: the declared binary, running.
func k3sCurrent(got state.K3s, want *graph.NodeK3sSpec) bool {
	return got.Installed && got.SHA256 == want.SHA256 && !got.Stale
}

// k3sRunning is the release a node's kubelet runs, as the cluster's server reports it ("" when
// unknown).
func (b builder) k3sRunning(r graph.Resource, got state.K3s) string {
	nodes := got.Nodes
	if r.NodeK3s.Role == graph.K3sAgent {
		nodes = b.snap.K3s[r.NodeK3s.Server+"/k3s"].Nodes
	}
	for _, n := range nodes {
		if n.Name == r.NodeK3s.Node {
			return n.Version
		}
	}
	return ""
}

// upgradeGate says what a change of a k3s whose binary is in place waits for, or nil when it may go
// on. The server goes first, for an agent running or not (an agent never runs a newer release than
// its server: a stopped agent given a newer binary would start on it). A k3s that runs is restarted:
// the agents one at a time (in the order of their names), and each step only while every
// Kubernetes node is Ready, as the server sees it: a restart on top of an unhealthy cluster would
// take more of it down.
func (b builder) upgradeGate(r graph.Resource, got state.K3s) *Note {
	want := r.NodeK3s
	nodes := got.Nodes
	if want.Role == graph.K3sAgent {
		srv, ok := b.snap.K3s[want.Server+"/k3s"]
		for _, other := range b.g.Sorted() {
			o := other.NodeK3s
			if o == nil || o.Node != want.Server {
				continue
			}
			if !ok || b.stale(srv.ObservedAt) || !k3sCurrent(srv, o) {
				return &Note{Code: "MAM-PLAN-0116", Message: fmt.Sprintf("waits for the k3s server %s to run its declared release before %s is upgraded (an agent never runs a newer release than its server)", want.Server, want.Node),
					Hint: "the server's upgrade comes first in this or an earlier plan"}
			}
		}
		for _, other := range b.g.Sorted() {
			o := other.NodeK3s
			if !got.Running || o == nil || o.Role != graph.K3sAgent || o.Server != want.Server || o.Node >= want.Node {
				continue
			}
			if og, seen := b.snap.K3s[other.Name]; seen && og.Running && !k3sCurrent(og, o) {
				return &Note{Code: "MAM-PLAN-0116", Message: fmt.Sprintf("waits for the k3s agent %s to be upgraded before %s (agents are upgraded one at a time)", o.Node, want.Node)}
			}
		}
		nodes = srv.Nodes
	}
	if !got.Running {
		return nil // nothing restarts
	}
	nodes = b.declaredNodes(nodes, want)
	var notReady []string
	for _, n := range nodes {
		if !n.Ready {
			notReady = append(notReady, n.Name)
		}
	}
	switch {
	case len(nodes) == 0:
		return &Note{Code: "MAM-PLAN-0115", Message: fmt.Sprintf("waits for the k3s server to report its Kubernetes nodes before k3s on %s is restarted", want.Node),
			Hint: "the server reports them while it runs; if it never does, look at k3s on the server"}
	case len(notReady) > 0:
		return &Note{Code: "MAM-PLAN-0115", Message: fmt.Sprintf("waits for every Kubernetes node to be Ready before k3s on %s is restarted (not Ready: %s)", want.Node, strings.Join(notReady, ", ")),
			Hint: "an upgrade goes on only on a healthy cluster; find out why the node is not Ready"}
	}
	return nil
}

// declaredNodes are the Kubernetes nodes the declaration is about: those named like a k3s node of
// this cluster. A node the declaration does not name (a machine whose declaration was removed and
// left its node behind, or one joined by hand) is not Mamela's to wait for: it would stop every
// upgrade for good. When no name matches at all (the Kubernetes node names are not the declared
// ones), nothing is dropped: the gate stays as careful as it can be.
func (b builder) declaredNodes(nodes []state.K3sNode, want *graph.NodeK3sSpec) []state.K3sNode {
	server := want.Server
	if want.Role == graph.K3sServer {
		server = want.Node
	}
	declared := map[string]bool{}
	for _, r := range b.g.Sorted() {
		if o := r.NodeK3s; o != nil && (o.Node == server || o.Server == server) {
			declared[o.Node] = true
		}
	}
	var mine []state.K3sNode
	for _, n := range nodes {
		if declared[n.Name] {
			mine = append(mine, n)
		}
	}
	if len(mine) == 0 {
		return nodes
	}
	return mine
}

// k3sServerOf is the server of node's k3s agent, when service is that agent's k3s service.
func (b builder) k3sServerOf(node, service string) string {
	if service != "k3s" || b.g == nil {
		return ""
	}
	for _, r := range b.g.Resources {
		if r.NodeK3s != nil && r.NodeK3s.Node == node && r.NodeK3s.Role == graph.K3sAgent {
			return r.NodeK3s.Server
		}
	}
	return ""
}
