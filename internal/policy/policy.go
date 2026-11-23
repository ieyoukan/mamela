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

	"github.com/ieyoukan/mamela/internal/names"
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
	// OpChangeForeign is any change to a resource Mamela did not create: always denied.
	OpChangeForeign    Op = "change-foreign"
	OpInstallPackage   Op = "install-package"
	OpEnableService    Op = "enable-service"
	OpDisableService   Op = "disable-service"
	OpStartService     Op = "start-service"
	OpStopService      Op = "stop-service"
	OpConfigureService Op = "configure-service-repair"
	OpUpdateMamelet    Op = "update-mamelet"
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
	// ProtectedNodes are machines that this Control Plane never changes: its own host (site
	// controlplane.host). Every change to them, or to what runs on them, is denied (ADR 0003).
	ProtectedNodes []string `json:"protectedNodes,omitempty"`
	// ObserveOnly are names of machines (nodes) and of resources of providers that Mamela observes
	// and plans, and never changes: a change of one, or of what runs on a machine of one, is shown
	// and denied (MAM-POL-0312); nothing is created under such a name, nothing of it is deleted, and
	// a node's mamelet gets no grant. It is how something that already exists comes under Mamela
	// with nothing at risk: the plan shows how it differs from the declaration until a person lifts
	// it on the Control Plane's host. The site's observe_only adds to it (cmd/mamela currentPolicy).
	ObserveOnly []string `json:"observeOnly,omitempty"`
	// Removal says what becomes of an object Mamela made for a declaration once the declaration no
	// longer has it (ADR 0013): "delete" (the default) plans its deletion, which needs an approval
	// made on the Control Plane's host; "retain" keeps it, as before.
	Removal string `json:"removal,omitempty"`
}

// Deletes reports whether removing a resource from its declaration plans its deletion.
func (p Policy) Deletes() bool { return p.Removal != "retain" }

// Protected reports whether node is one the Control Plane never changes.
func (p Policy) Protected(node string) bool {
	return node != "" && slices.Contains(p.ProtectedNodes, node)
}

// ProtectedDecision is the decision for any operation on a protected node.
var ProtectedDecision = Decision{Denied, "MAM-POL-0308", "this machine is the Control Plane's own host; a Control Plane never changes the host it runs on (it could stop itself in the middle of an apply)",
	"change it from another Control Plane, or by hand while this one is stopped (ADR 0003)"}

// ObservesOnly reports whether name is observe-only: Mamela shows how it differs from the
// declaration and changes nothing of it or on it.
func (p Policy) ObservesOnly(name string) bool {
	return name != "" && slices.Contains(p.ObserveOnly, name)
}

// ObserveOnlyDecision is the decision for any operation of an observe-only name, or on it.
var ObserveOnlyDecision = Decision{Denied, "MAM-POL-0312",
	"observe-only (observeOnly in the policy, or observe_only in mamela.site.toml): Mamela shows how it differs from the declaration and changes nothing of it or on it",
	"when the plan shows what you expect, remove the name from observeOnly in the policy file, or from observe_only in mamela.site.toml, on the Control Plane's host, and plan again"}

// Held says what holds every change of name, or of what runs on it: the Control Plane's own host
// (MAM-POL-0308, ADR 0003) before observe-only (MAM-POL-0312). ok is false when nothing does.
func (p Policy) Held(name string) (d Decision, ok bool) {
	switch {
	case p.Protected(name):
		return ProtectedDecision, true
	case p.ObservesOnly(name):
		return ObserveOnlyDecision, true
	}
	return Decision{}, false
}

// Holds reports whether every change of name, or of what runs on it, is denied (Held).
func (p Policy) Holds(name string) bool {
	_, ok := p.Held(name)
	return ok
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
	// For an operation that changes APT packages (install-package, and an extension's such as
	// linux.upgrade-packages): whether the change set removes packages, and its origins. Every
	// operation that says so is held to the same rules (MAM-POL-0304, MAM-POL-0305).
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

// DecideRemoval decides the deletion of an object that no declaration has any more. It always needs
// an approval, which only the Control Plane's host can give (ADR 0013); a request the host's
// catalog does not know is never sent.
func (p Policy) DecideRemoval(v protocol.Verified) Decision {
	if len(v.Unclassified) > 0 {
		return Decision{Denied, "MAM-POL-0311", fmt.Sprintf("%d of the deletion's requests are calls the host's catalog does not know; Mamela deletes only through calls it can classify", len(v.Unclassified)),
			"add the API's catalog to the provider's configuration"}
	}
	return Decision{ApprovalRequired, "MAM-POL-0310", "deletes an object Mamela made for this declaration, which no longer has it",
		"review what is deleted, then approve the plan on the Control Plane's host (mamela approve): a pull request's comment cannot approve a deletion"}
}

// Decide returns the decision for one operation. It is a pure function of the policy and subject.
func (p Policy) Decide(s Subject) Decision {
	switch s.Op {
	case OpConfigureService:
		return Decision{Auto, "MAM-POL-0103", "apply activates the service repair policy shown in this plan", ""}
	case OpUpdateMamelet:
		return Decision{ApprovalRequired, "MAM-POL-0215", "replaces mamelet (the node's agent, which runs as root) with the binary of the declared sha256, which the Control Plane serves, and restarts it; the previous binary is kept, and mamelet goes back to it if the new one does not report",
			"check the sha256 against the binary you built and put in controlplane.mamelet_binary; nodes are updated one at a time"}
	case OpInstallPackage:
		if s.Removes {
			return Decision{Denied, "MAM-POL-0304", "installing would remove other packages; Mamela never removes packages",
				"find out why apt wants to remove them, and resolve it by hand"}
		}
		if d, denied := p.originDenied(s.Origins); denied {
			return d
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
	case OpChangeForeign:
		return Decision{Denied, "MAM-POL-0303", "the resource exists but Mamela did not create it (Foreign); Mamela never changes it",
			"rename the declaration, or adopt the resource by hand"}
	default:
		if d, ok := extOps[s.Op]; ok {
			// An extension's operation that changes APT packages (linux.upgrade-packages) is held to the
			// core's rules of an apt change set first, whatever its own decision: it never removes a
			// package, and every package comes from an origin the policy allows.
			if s.Removes {
				return Decision{Denied, "MAM-POL-0304", "apt would remove packages to do it; Mamela never removes packages",
					"find out why apt wants to remove them (the change set above names them), and resolve it by hand"}
			}
			if d, denied := p.originDenied(s.Origins); denied {
				return d
			}
			return d.decision
		}
		return Decision{Denied, "MAM-POL-0399", "the operation is not known to the policy",
			"this is a bug in Mamela; unknown operations are always denied"}
	}
}

// originDenied is the decision for packages from origins one of which the policy does not allow
// (denied false when it allows them all).
func (p Policy) originDenied(origins []string) (Decision, bool) {
	for _, o := range origins {
		if !p.allowedOrigin(o) {
			return Decision{Denied, "MAM-POL-0305", "a package would come from APT origin " + o + ", which the policy does not allow",
				"install only from allowed origins (allowedAptOrigins), or add this origin to the policy after review"}, true
		}
	}
	return Decision{}, false
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
	q.ObserveOnly = NormalNames(p.ObserveOnly)
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
	if p.Removal != "" && p.Removal != "delete" && p.Removal != "retain" {
		return Policy{}, fmt.Errorf("policy: removal %q: \"delete\" or \"retain\"", p.Removal)
	}
	if err := CheckNames("policy: observeOnly", p.ObserveOnly); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// CheckNames checks a list of names of machines and resources (observeOnly): each a name a
// declaration can give a machine or a provider's resource.
func CheckNames(what string, list []string) error {
	for _, n := range list {
		if !names.Resource(n) {
			return fmt.Errorf("%s: %q is not the name of a machine or a resource (letters, digits and '.', '_', ':', '@', '-', starting with a letter or digit)", what, n)
		}
	}
	return nil
}

// NormalNames is a list of names sorted, each once (nil when empty): the same names in another
// order, or twice, are the same policy.
func NormalNames(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	return slices.Compact(slices.Sorted(slices.Values(list)))
}

// The operations of node extensions (k3s, docs/design/node-extensions.md) are registered, each with
// its one decision. An extension's operation always needs an approval or is denied: an extension
// cannot make something run without a person's approval (Auto is the core's to give, from the
// policy's own settings).

type extOp struct {
	owner    string
	decision Decision
}

var (
	extOps = map[Op]extOp{}
	// coreOps are the core's operations; coreCodes the codes of the core's decisions. A code is in
	// the plan's JSON, so it never changes meaning, and no extension may take one.
	coreOps   = []Op{OpChangeForeign, OpInstallPackage, OpEnableService, OpDisableService, OpStartService, OpStopService, OpConfigureService, OpUpdateMamelet}
	coreCodes = []Code{"MAM-POL-0101", "MAM-POL-0102", "MAM-POL-0103", "MAM-POL-0204", "MAM-POL-0205", "MAM-POL-0206", "MAM-POL-0207",
		"MAM-POL-0208", "MAM-POL-0209", "MAM-POL-0215", "MAM-POL-0303", "MAM-POL-0304", "MAM-POL-0305", "MAM-POL-0306", "MAM-POL-0308",
		"MAM-POL-0309", "MAM-POL-0310", "MAM-POL-0311", "MAM-POL-0312", "MAM-POL-0399"}
)

// RegisterOp adds an operation of the node extension owner, with its decision. It panics when the
// decision would let the operation run without an approval, and when the operation or the code is
// taken already: programming errors, found when the program starts.
func RegisterOp(owner string, op Op, d Decision) {
	switch {
	case owner == "" || owner == "core":
		panic("policy: an extension needs a name of its own")
	case d.Verdict != ApprovalRequired && d.Verdict != Denied:
		panic(fmt.Sprintf("policy: extension %s: operation %s: verdict %q; an extension's operation needs an approval or is denied", owner, op, d.Verdict))
	case op == "" || slices.Contains(coreOps, op):
		panic(fmt.Sprintf("policy: extension %s: operation %q is the core's", owner, op))
	case d.Code == "" || d.Message == "" || slices.Contains(coreCodes, d.Code):
		panic(fmt.Sprintf("policy: extension %s: operation %s needs a code of its own and a message", owner, op))
	}
	if other, taken := extOps[op]; taken {
		panic(fmt.Sprintf("policy: extension %s: operation %s is %s's", owner, op, other.owner))
	}
	for o, e := range extOps {
		if e.decision.Code == d.Code {
			panic(fmt.Sprintf("policy: extension %s: code %s is the code of %s (%s)", owner, d.Code, o, e.owner))
		}
	}
	extOps[op] = extOp{owner: owner, decision: d}
}

// Ops are every operation the policy knows, the core's and the extensions', sorted.
func Ops() []Op {
	ops := slices.Clone(coreOps)
	for op := range extOps {
		ops = append(ops, op)
	}
	slices.Sort(ops)
	return ops
}
