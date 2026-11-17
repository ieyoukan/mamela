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
	CapAptInstall = "apt.install"
	CapMameletUpd = "mamelet.update"
	capSystemd    = "systemd."
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
	KeyMameletSHA   = "mamela:mamelet.sha256" // sha256 of the mamelet binary fetched from the Control Plane
	FactChangeSet   = "changeSet"             // plan fact: the approved change set digest
	FactOrigins     = "allowedOrigins"        // plan fact: the policy's allowed APT origins (prefixes, "Ubuntu:*")
	FactAutoStart   = "autoStartServices"
	FactOperationID = "operationId" // filled when the operation is granted
	FactExpiresAt   = "expiresAt"   // filled when the revision is signed
)

// GrantConditions bind every one-time grant to its node, its operation and the revision's lifetime:
// a node extension's needs start from them.
func GrantConditions() []spec.Condition { return slices.Clone(grantConditions) }

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
	kinds := []spec.Kind{pkg, svc, mameletKind}
	for _, e := range extensions {
		kinds = append(kinds, e.Kinds...)
	}
	// In the canonical order of the kinds: the manifest is part of every plan's ProvidersDigest.
	slices.SortStableFunc(kinds, func(a, b spec.Kind) int { return graph.Order(a.Kind) - graph.Order(b.Kind) })
	return spec.Manifest{Schema: spec.Schema, Provider: "mamelet", Version: Version, Kinds: kinds}
}
