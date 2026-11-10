// Package policy decides, deterministically, whether an operation may run automatically, needs a
// person's approval, or must never run. Unknown operations are denied.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// Verdict is the outcome of a decision. Ordered from least to most restrictive.
type Verdict string

const (
	Auto             Verdict = "Auto"
	ApprovalRequired Verdict = "ApprovalRequired"
	Denied           Verdict = "Denied"
)

var rank = map[Verdict]int{Auto: 0, ApprovalRequired: 1, Denied: 2}

// Stricter returns the more restrictive of two verdicts.
func Stricter(a, b Verdict) Verdict {
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Op is a kind of real-world change. It is the provider protocol's Op, so that manifests name
// operations with the same type.
type Op = protocol.Op

const (
	OpCreateVM       Op = "create-vm"
	OpResizeVMCPU    Op = "resize-vm-cpu"
	OpResizeVMMemory Op = "resize-vm-memory"
	OpGrowVMDisk     Op = "grow-vm-disk"
	OpShrinkVMDisk   Op = "shrink-vm-disk"
	OpReplaceVMImage Op = "replace-vm-image"
	OpChangeForeign  Op = "change-foreign"
	OpInstallPackage Op = "install-package"
	OpEnableService  Op = "enable-service"
	OpDisableService Op = "disable-service"
	OpStartService   Op = "start-service"
	OpStopService    Op = "stop-service"
	OpInstallK3s     Op = "install-k3s"
)

// Code is a stable reason code for a decision.
type Code string

// Decision is a verdict with the reason, written for a person.
type Decision struct {
	Verdict Verdict `json:"verdict"`
	Code    Code    `json:"code"`
	Message string  `json:"message"`
	Hint    string  `json:"hint,omitempty"`
}

// Policy is versioned configuration for decisions. Its digest is part of every plan's inputs.
type Policy struct {
	Version string `json:"version"`
	// MaxObservationAgeSeconds: older observations block planning for that resource.
	MaxObservationAgeSeconds int64 `json:"maxObservationAgeSeconds"`
	// AutoStartServices may be started without approval (drift repair), by service name.
	AutoStartServices []string `json:"autoStartServices"`
	// AllowedAptOrigins are the APT origins (prefixes, e.g. "Ubuntu:") packages may be installed
	// from. A change set with any other origin is denied.
	AllowedAptOrigins []string `json:"allowedAptOrigins"`
	// AutoProviderEffects are the effects (from the host's catalog) that a provider's operation
	// may have and still run without approval. Empty: every provider operation needs approval.
	AutoProviderEffects []protocol.Effect `json:"autoProviderEffects,omitempty"`
}

// Default is the conservative policy used when nothing else is configured.
var Default = Policy{
	Version:                  "default/1",
	MaxObservationAgeSeconds: 600,
	AutoStartServices:        nil,
	AllowedAptOrigins:        []string{"Ubuntu:"},
}

// MaxObservationAge is MaxObservationAgeSeconds as a duration.
func (p Policy) MaxObservationAge() time.Duration {
	return time.Duration(p.MaxObservationAgeSeconds) * time.Second
}

// Digest identifies the policy content.
func (p Policy) Digest() string {
	sum := sha256.Sum256(p.Canonical())
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Subject is what an operation acts on, as far as the policy cares.
type Subject struct {
	Op      Op
	Service string // for service operations
	// For install-package: whether the change set removes packages, and its origins.
	Removes bool
	Origins []string
}

// DecideProvided decides an operation of a provider from what the host's catalog verifies about
// the requests it would send, never from what the provider says about them.
func (p Policy) DecideProvided(v protocol.Verified) Decision {
	switch {
	case v.MayLoseData():
		return Decision{Denied, "MAM-POL-0306", "the host's catalog says a request of this change can destroy data; Mamela never sends it",
			"change the declaration so that the provider does not need it, or do it by hand"}
	case len(v.Unclassified) > 0:
		return Decision{ApprovalRequired, "MAM-POL-0208",
			fmt.Sprintf("%d of its requests are calls the host's catalog does not know; review the requests before approving", len(v.Unclassified)),
			"add the API's catalog to the provider's configuration to have them classified"}
	case len(p.AutoProviderEffects) > 0 && v.AutoApprovable(p.AutoProviderEffects):
		return Decision{Auto, "MAM-POL-0102", "the policy lets these effects run automatically (autoProviderEffects)", ""}
	}
	var es []string
	for _, e := range v.Effects {
		es = append(es, string(e))
	}
	return Decision{ApprovalRequired, "MAM-POL-0209", "a change through a provider (" + strings.Join(es, ", ") + ")", ""}
}

// Decide returns the decision for one operation. It is a pure function of the policy and subject.
func (p Policy) Decide(s Subject) Decision {
	switch s.Op {
	case OpInstallK3s:
		return Decision{ApprovalRequired, "MAM-POL-0210", "installs Kubernetes (k3s, a server or an agent) downloaded from GitHub, pinned by sha256, which runs as root",
			"check the version and sha256 against the k3s release page before approving"}
	case OpCreateVM:
		return Decision{ApprovalRequired, "MAM-POL-0201", "creating a VM reserves host CPU, memory and disk",
			"approve the plan if the host has room for it"}
	case OpResizeVMCPU, OpResizeVMMemory:
		return Decision{ApprovalRequired, "MAM-POL-0202", "changing vCPUs or memory restarts the VM if it is running (Proxmox applies it at the next start)",
			"approve when a short outage of the VM is acceptable"}
	case OpGrowVMDisk:
		return Decision{ApprovalRequired, "MAM-POL-0203", "growing a disk uses shared storage and cannot be undone", ""}
	case OpInstallPackage:
		if s.Removes {
			return Decision{Denied, "MAM-POL-0304", "installing would remove other packages; Mamela never removes packages",
				"find out why apt wants to remove them, and resolve it by hand"}
		}
		for _, o := range s.Origins {
			if !p.allowedOrigin(o) {
				return Decision{Denied, "MAM-POL-0305", "a package would come from APT origin " + o + ", which the policy does not allow",
					"install only from allowed origins (allowedAptOrigins), or add this origin to the policy after review"}
			}
		}
		return Decision{ApprovalRequired, "MAM-POL-0204", "installing a package runs its maintainer scripts as root",
			"approve after checking the package and its source"}
	case OpEnableService:
		return Decision{ApprovalRequired, "MAM-POL-0205", "enabling a service changes what runs after a reboot", ""}
	case OpStartService:
		if slices.Contains(p.AutoStartServices, s.Service) {
			return Decision{Auto, "MAM-POL-0101", "this service may be started automatically (policy autoStartServices)", ""}
		}
		return Decision{ApprovalRequired, "MAM-POL-0206", "starting a service has side effects",
			"add it to autoStartServices to repair this drift without approval"}
	case OpStopService, OpDisableService:
		return Decision{ApprovalRequired, "MAM-POL-0207", "stopping or disabling a service can cause an outage", ""}
	case OpShrinkVMDisk:
		return Decision{Denied, "MAM-POL-0301", "shrinking a disk can destroy data; Mamela never does it",
			"shrink it by hand after a backup, or restore the declared size"}
	case OpReplaceVMImage:
		return Decision{Denied, "MAM-POL-0302", "changing the image would mean replacing the VM and its data; Mamela never does it",
			"declare a new VM with the new image, move the workload, then retire the old VM by hand"}
	case OpChangeForeign:
		return Decision{Denied, "MAM-POL-0303", "the resource exists but Mamela did not create it (Foreign); Mamela never changes it",
			"rename the declaration, or adopt the resource by hand"}
	default:
		return Decision{Denied, "MAM-POL-0399", "the operation is not known to the policy",
			"this is a bug in Mamela; unknown operations are always denied"}
	}
}

func (p Policy) allowedOrigin(o string) bool {
	for _, a := range p.AllowedAptOrigins {
		if a != "" && strings.HasPrefix(o, a) {
			return true
		}
	}
	return false
}

// Canonical is the deterministic JSON of the policy.
func (p Policy) Canonical() []byte {
	q := p
	q.AutoStartServices = slices.Sorted(slices.Values(p.AutoStartServices))
	q.AllowedAptOrigins = slices.Sorted(slices.Values(p.AllowedAptOrigins))
	out, err := json.Marshal(q, json.Deterministic(true))
	if err != nil {
		panic("policy: encoding failed: " + err.Error())
	}
	return out
}

// Decode reads a policy strictly.
func Decode(data []byte) (Policy, error) {
	var p Policy
	if err := json.Unmarshal(data, &p, json.RejectUnknownMembers(true)); err != nil {
		return Policy{}, err
	}
	return p, nil
}
