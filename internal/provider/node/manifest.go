package node

import (
	"slices"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/spec"
)

// Version is the version of the node provider's contract (its manifest).
const Version = "0.2.0"

// TargetNode is the target type of mamelet's capabilities: the VM it runs in. They are granted
// in the node's signed revision, one approved operation at a time unless Standing says otherwise.
const TargetNode = "node"

// Capability names: what mamelet does with the grant, by program and action.
const (
	CapAptInstall  = "apt.install"
	CapK3sInstall  = "k3s.install"
	CapK3sManifest = "k3s.manifest"
	CapK3sRemove   = "k3s.remove-node"
	CapMameletUpd  = "mamelet.update"
	capSystemd     = "systemd."
)

// Condition keys mamelet checks before it acts (facts it knows from the signed revision, its
// clock and its own simulation).
const (
	KeyNode         = "mamela:node"          // the node the revision is for
	KeyOperation    = "mamela:operation"     // the approved operation the grant is for
	KeyNow          = "mamela:now"           // mamelet's clock
	KeyChangeSet    = "mamela:apt.changeSet" // digest of a fresh apt-get -s simulation
	KeyAptRemove    = "mamela:apt.remove"    // packages the simulation would remove
	KeyAptOrigin    = "mamela:apt.origin"    // origins of the packages it would install
	KeyAutoStart    = "mamela:policy.autoStartServices"
	KeyK3sSHA256    = "mamela:k3s.sha256"     // sha256 of the downloaded binary
	KeyMameletSHA   = "mamela:mamelet.sha256" // sha256 of the mamelet binary fetched from the Control Plane
	FactChangeSet   = "changeSet"             // plan fact: the approved change set digest
	FactOrigins     = "allowedOrigins"        // plan fact: the policy's allowed APT origins (prefixes, "Ubuntu:*")
	FactAutoStart   = "autoStartServices"
	FactOperationID = "operationId" // filled when the operation is granted
	FactExpiresAt   = "expiresAt"   // filled when the revision is signed
)

// grantConditions bind every one-time grant to its node, its operation and the revision's lifetime.
var grantConditions = []spec.Condition{
	{Key: KeyNode, Test: spec.Equals, Values: []string{"{node}"}},
	{Key: KeyOperation, Test: spec.Equals, Values: []string{"{" + FactOperationID + "}"}},
	{Key: KeyNow, Test: spec.Before, Values: []string{"{" + FactExpiresAt + "}"}},
}

// Manifest is the node provider's declaration of itself. The service capabilities are named after
// the actions mamelet is granted (serviceActions), so they cannot differ from what Apply does.
func Manifest() spec.Manifest {
	node := spec.Target{Type: TargetNode, Ref: "{node}"}
	pkg := spec.Kind{Kind: graph.KindNodePackage, Operations: []spec.Operation{{
		Op: policy.OpInstallPackage, Support: spec.Supported, Needs: []spec.Need{{
			Capability: CapAptInstall, Target: node, Object: "{package}", Always: true,
			Conditions: append(slices.Clone(grantConditions),
				spec.Condition{Key: KeyChangeSet, Test: spec.Equals, Values: []string{"{" + FactChangeSet + "}"}},
				spec.Condition{Key: KeyAptRemove, Test: spec.NotPresent},
				spec.Condition{Key: KeyAptOrigin, Test: spec.Like, Values: []string{"{" + FactOrigins + "}"}}),
			Note:    "mamelet simulates the install again just before and installs only if it is exactly the approved change set",
			Reasons: []string{"mamelet runs apt-get install once"},
		}},
	}}}
	k3sNeed := spec.Need{
		Capability: CapK3sInstall, Target: node, Object: "k3s {version}", Always: true,
		Conditions: append(slices.Clone(grantConditions),
			spec.Condition{Key: KeyK3sSHA256, Test: spec.Equals, Values: []string{"{sha256}"}}),
		Note:    "mamelet downloads the release and installs it only if its sha256 is the declared one, with Mamela's fixed unit",
		Reasons: []string{"mamelet writes /usr/local/bin/k3s and its systemd unit once"},
	}
	k3sUpgradeNeed := k3sNeed
	k3sUpgradeNeed.Note = "the same grant as an installation, for a k3s that runs: mamelet then restarts it, so that the node runs the declared release"
	k3sUpgradeNeed.Reasons = []string{"mamelet writes /usr/local/bin/k3s and its systemd unit once, and restarts k3s"}
	k3s := spec.Kind{Kind: graph.KindNodeK3s, Operations: []spec.Operation{
		{Op: policy.OpInstallK3s, Support: spec.Supported, Needs: []spec.Need{k3sNeed}},
		{Op: policy.OpUpgradeK3s, Support: spec.Supported, Needs: []spec.Need{k3sUpgradeNeed}},
		{Op: policy.OpRemoveK3sNode, Support: spec.Supported, Needs: []spec.Need{{
			Capability: CapK3sRemove, Target: node, Object: "a Kubernetes node the declaration no longer names (the one the approved plan names)", Always: true,
			Conditions: slices.Clone(grantConditions),
			Note:       "mamelet on the server cordons and drains the node (when it is Ready), then deletes it with the server's own kubeconfig; it never removes the server's own node",
			Reasons:    []string{"mamelet runs k3s kubectl cordon, drain and delete node once"},
		}}},
	}}
	manifest := spec.Kind{Kind: graph.KindNodeK3sManifest, Operations: []spec.Operation{{
		Op: policy.OpWriteK3sManifest, Support: spec.Supported, Needs: []spec.Need{{
			Capability: CapK3sManifest, Target: node, Object: "{name}", Always: true,
			Conditions: slices.Clone(grantConditions),
			Note:       "mamelet writes exactly the content of the approved plan to /var/lib/rancher/k3s/server/manifests/mamela-<name>.yaml, which k3s applies with cluster-admin rights",
			Reasons:    []string{"mamelet writes the manifest file once"},
		}},
	}}}
	mameletKind := spec.Kind{Kind: graph.KindNodeMamelet, Operations: []spec.Operation{{
		Op: policy.OpUpdateMamelet, Support: spec.Supported, Needs: []spec.Need{{
			Capability: CapMameletUpd, Target: node, Object: "mamelet {sha256}", Always: true,
			Conditions: append(slices.Clone(grantConditions),
				spec.Condition{Key: KeyMameletSHA, Test: spec.Equals, Values: []string{"{sha256}"}}),
			Note:    "mamelet fetches the binary from the Control Plane over its pinned TLS, replaces itself only if the sha256 is the declared one and the new binary runs (-version), keeps the previous binary, and goes back to it if the new one does not report within 10 minutes",
			Reasons: []string{"mamelet replaces /usr/local/sbin/mamelet once and restarts"},
		}},
	}}}
	svc := spec.Kind{Kind: graph.KindNodeService}
	svc.Operations = append(svc.Operations, spec.Operation{Op: policy.OpConfigureService, Support: spec.Supported,
		Needs: []spec.Need{{Capability: "revision.service-repair", Target: node, Object: "{service}", Always: true,
			Reasons: []string{"apply activates or revokes the policy's standing start permission"}}}})
	for _, op := range []policy.Op{policy.OpEnableService, policy.OpStartService, policy.OpDisableService, policy.OpStopService} {
		action := serviceActions[op]
		n := spec.Need{Capability: capSystemd + action, Target: node, Object: "{service}", Always: true,
			Conditions: slices.Clone(grantConditions),
			Reasons:    []string{"mamelet runs systemctl " + action + " once"}}
		if op == policy.OpStartService {
			n.StandingIf = []spec.Condition{{Key: KeyAutoStart, Test: spec.Contains, Values: []string{"{service}"}}}
			n.Note = "a standing grant lets mamelet start it whenever it finds it stopped (drift repair)"
		}
		svc.Operations = append(svc.Operations, spec.Operation{Op: op, Support: spec.Supported, Needs: []spec.Need{n}})
	}
	return spec.Manifest{Schema: spec.Schema, Provider: "mamelet", Version: Version, Kinds: []spec.Kind{pkg, svc, k3s, manifest, mameletKind}}
}
