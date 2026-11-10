package node

import (
	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/spec"
)

// Version is the version of the node provider's contract (its manifest).
const Version = "0.1.0"

// TargetNode is the target type of mamelet's capabilities: the VM it runs in. They are granted
// in the node's signed revision, one approved operation at a time unless Standing says otherwise.
const TargetNode = "node"

// Capability names: what mamelet does with the grant, by program and action.
const (
	CapAptInstall = "apt.install"
	capSystemd    = "systemd."
)

// Manifest is the node provider's declaration of itself. The service capabilities are named after
// the actions mamelet is granted (serviceActions), so they cannot differ from what Apply does.
func Manifest() spec.Manifest {
	node := spec.Target{Type: TargetNode, Ref: "{node}"}
	pkg := spec.Kind{Kind: graph.KindNodePackage, Operations: []spec.Operation{{
		Op: policy.OpInstallPackage, Support: spec.Supported, Needs: []spec.Need{{
			Capability: CapAptInstall, Target: node, Object: "{package}", Always: true,
			Limit: "only the apt change set approved in the plan, simulated again just before; never a removal, " +
				"only from allowed APT origins",
			Reasons: []string{"mamelet runs apt-get install once"},
		}},
	}}}
	svc := spec.Kind{Kind: graph.KindNodeService}
	for _, op := range []policy.Op{policy.OpEnableService, policy.OpStartService, policy.OpDisableService, policy.OpStopService} {
		action := serviceActions[op]
		n := spec.Need{Capability: capSystemd + action, Target: node, Object: "{service}", Always: true,
			Reasons: []string{"mamelet runs systemctl " + action + " once"}}
		if op == policy.OpStartService {
			n.Standing = "with the policy autoStartServices, mamelet starts it whenever it finds it stopped"
		}
		svc.Operations = append(svc.Operations, spec.Operation{Op: op, Support: spec.Supported, Needs: []spec.Need{n}})
	}
	return spec.Manifest{Schema: spec.Schema, Provider: "mamelet", Version: Version, Kinds: []spec.Kind{pkg, svc}}
}
