package k3s

import "github.com/ieyoukan/mamela/internal/policy"

// The operations of k3s, registered as a node extension's.
const (
	OpInstall       policy.Op = "install-k3s"
	OpUpgrade       policy.Op = "upgrade-k3s"
	OpWriteManifest policy.Op = "write-k3s-manifest"
	OpRemoveNode    policy.Op = "remove-k3s-node"
)

func init() {
	policy.RegisterOp("k3s", OpInstall, decision("MAM-POL-0210", "installs Kubernetes (k3s, a server or an agent) downloaded from GitHub, pinned by sha256, which runs as root",
		"check the version and sha256 against the k3s release page before approving"))
	policy.RegisterOp("k3s", OpUpgrade, decision("MAM-POL-0212", "replaces the k3s binary (a release pinned by sha256) and restarts k3s on this node: a server's API is unavailable while it restarts, and an agent's node is not Ready",
		"check the release against the k3s release page and that agents are not newer than the server; back up the server's data first"))
	policy.RegisterOp("k3s", OpWriteManifest, decision("MAM-POL-0213", "puts a manifest in k3s's auto-deploy directory: k3s applies it with cluster-admin rights, and again whenever it changes",
		"read the whole manifest in this plan; a HelmChart or a workload in it runs with the rights its YAML asks for"))
	policy.RegisterOp("k3s", OpRemoveNode, decision("MAM-POL-0214", "deletes a Kubernetes node from the cluster (its pods are drained first when the node is Ready): the node is not declared any more",
		"check the node's name in this plan; only the Control Plane's host can approve it (mamela approve), as with every deletion"))
}

// decision is an operation's decision: always an approval (the policy refuses an extension's Auto).
func decision(code, message, hint string) policy.Decision {
	return policy.Decision{Verdict: policy.ApprovalRequired, Code: policy.Code(code), Message: message, Hint: hint}
}
