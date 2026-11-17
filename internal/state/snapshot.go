// Package state holds the Observed State: what Mamela last saw of the real world.
//
// A Snapshot is an input to planning. It carries its own point in time (AsOf), so planning never
// reads the wall clock and the same snapshot always gives the same plan.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Snapshot is a time-stamped view of the observed world.
type Snapshot struct {
	// AsOf is "now" for planning: observation ages are measured against it.
	AsOf time.Time `json:"asOf"`
	// Stack is the declaration (ADR 0013) the world was observed for: the objects it made, and
	// only those, may be deleted when it no longer has them. Empty: none is named.
	Stack string `json:"stack,omitempty"`
	// Packages by "<node>/<package>". Absent means "not observed", not "not installed".
	Packages map[string]Package `json:"packages"`
	// Services by "<node>/<service>". Absent means "not observed".
	Services map[string]Service `json:"services"`
	// Provided are resources of providers, by graph ID ("KVItem web"): the declared ones and
	// those a provider found that Mamela created and that are no longer declared.
	Provided map[string]Provided `json:"provided,omitempty"`
	// K3s by "<node>/k3s". Absent means "not observed".
	K3s map[string]K3s `json:"k3s,omitempty"`
	// Mamelets by resource name ("<node>/mamelet"): the mamelet each node's agent reported running.
	Mamelets map[string]Mamelet `json:"mamelets,omitempty"`
	// K3sManifests by resource name ("<node>/manifest/<name>"). Absent means "not observed".
	K3sManifests map[string]K3sManifest `json:"k3sManifests,omitempty"`
	// Nodes are the machines of providers' node kinds, by resource name (ADR 0004).
	Nodes map[string]Node `json:"nodes,omitempty"`
	// AgentRegistrations binds node operations to the enrolled instance, even if a VMID is reused.
	AgentRegistrations map[string]string `json:"agentRegistrations,omitempty"`
	// NodeRefusals are the nodes whose mamelet did not accept the revision the Control Plane offers
	// (by node name): whatever is approved, no permission reaches them. Only a node that refuses is
	// in it, so a snapshot of a world where every node accepts is what it always was.
	NodeRefusals map[string]NodeRefusal `json:"nodeRefusals,omitempty"`
	// Ext are what each node's mamelet reported of the node extensions that use the generic wire
	// (noderev's ext.go), by node and then by extension name. A node is in it only when its mamelet
	// takes such an extension (one older than the generic wire takes none), so a snapshot of nodes
	// that do not is what it always was.
	Ext map[string]map[string]ExtObservation `json:"ext,omitempty"`
}

// ExtObservation is what a node's mamelet reported of one node extension.
type ExtObservation struct {
	// Accepts is the version of the extension's part of a revision the mamelet reads
	// (Report.Extensions).
	Accepts int `json:"accepts"`
	// Payload is what the extension observed, in its own JSON (Report.Ext), canonical (RFC 8785);
	// absent when it reported nothing.
	Payload jsontext.Value `json:"payload,omitzero"`
	// Refused says why the mamelet did not take the extension's part of its revision
	// (Report.ExtRefused), in its words made printable.
	Refused    string    `json:"refused,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

// NodeRefusal is a mamelet saying that it refused the Control Plane's revision (its report's
// Refused): an unknown member (an older mamelet), a serial lower than one it accepted, a clock that
// differs, a signature that is not the Control Plane's.
type NodeRefusal struct {
	Serial    int64  `json:"serial"`              // the revision the Control Plane offers (its newest)
	WorksFrom int64  `json:"worksFrom,omitempty"` // the revision the node still works from (0: none)
	Reason    string `json:"reason"`              // mamelet's words, made printable
	// ObservedAt is when the report arrived.
	ObservedAt time.Time `json:"observedAt"`
}

// AddProvided records providers' observations, and from them the machines of node kinds.
func (s *Snapshot) AddProvided(provided map[string]Provided) {
	if len(provided) == 0 {
		return
	}
	s.Provided = provided
	for _, p := range provided {
		o := p.Observed
		if o.Node == nil || !o.Exists {
			continue
		}
		if s.Nodes == nil {
			s.Nodes = map[string]Node{}
		}
		s.Nodes[p.Name] = Node{Provider: p.Provider, Kind: string(p.Kind), Managed: o.Owned, ID: o.Node.ID,
			Running: o.Node.Running, UptimeSeconds: o.Node.UptimeSeconds, ObservedAt: p.ObservedAt}
	}
}

// Node is a provider's machine that mamelet runs on.
type Node struct {
	Provider      string    `json:"provider"`
	Kind          string    `json:"kind"`
	Managed       bool      `json:"managed"` // the provider's marker says Mamela created it
	ID            int64     `json:"id"`      // in revisions and enrollment
	Running       bool      `json:"running"`
	UptimeSeconds int64     `json:"uptimeSeconds,omitempty"`
	ObservedAt    time.Time `json:"observedAt"`
}

// K3s is an observed k3s on a node: Mamela's unit installed, and the sha256 of the binary.
type K3s struct {
	Installed  bool      `json:"installed"`
	SHA256     string    `json:"sha256,omitempty"`
	Error      string    `json:"error,omitempty"`
	Addresses  []string  `json:"addresses,omitempty"` // the node's IPv4 addresses, sorted
	Server     string    `json:"server,omitempty"`    // the server an installed agent joins
	Running    bool      `json:"running,omitempty"`   // the k3s service is active
	Stale      bool      `json:"stale,omitempty"`     // it started before the binary in place was written
	Nodes      []K3sNode `json:"nodes,omitempty"`     // a running server's view of the cluster
	ObservedAt time.Time `json:"observedAt"`
}

// Mamelet is an observed mamelet: the sha256 of the binary that runs, and the release it says it is.
type Mamelet struct {
	SHA256     string    `json:"sha256,omitempty"`
	Version    string    `json:"version,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

// K3sManifest is an observed manifest file of a k3s server: the sha256 of its content ("" when the
// file is not there).
type K3sManifest struct {
	SHA256     string    `json:"sha256,omitempty"`
	Error      string    `json:"error,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

// K3sNode is a Kubernetes node as its server sees it.
type K3sNode struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Ready   bool   `json:"ready"`
}

// Provided is what a provider observed of one resource, and the changes its diff computed from
// that observation for the declared spec. Like an apt simulation of a package, the changes are
// part of the observation: a plan is computed from them, and approving the plan approves exactly
// these requests.
type Provided struct {
	Provider string                `json:"provider"`
	Kind     protocol.ResourceKind `json:"kind"`
	Name     string                `json:"name"`
	Declared bool                  `json:"declared"`
	Observed protocol.Observed     `json:"observed"`
	// SpecDigest identifies the declared spec the changes were computed for (SpecDigest()).
	SpecDigest string           `json:"specDigest,omitempty"`
	Changes    []ProvidedChange `json:"changes,omitempty"`
	// Waiting says which resource it needs first does not exist yet or is not Mamela's (ADR
	// 0007): its changes are not computed until then.
	Waiting    string    `json:"waiting,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
	// An object that exists, is Mamela's and is not declared (ADR 0013) has either Removal, the
	// provider's change that deletes it (it was made for this declaration, and its kind has a
	// removing operation), or Kept, the reason it is left as it is.
	Removal *ProvidedChange `json:"removal,omitempty"`
	// RemovalOrder: the later the object was created, the higher; objects are deleted from the
	// highest down, so that what depends on an object goes before it.
	RemovalOrder int   `json:"removalOrder,omitempty"`
	Kept         *Kept `json:"kept,omitempty"`
}

// Kept is why an object that no declaration has any more is left as it is.
type Kept struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// ProvidedChange is one change a provider's diff computed, with what the host's catalog verifies
// about its requests (computed by Mamela, not by the provider).
type ProvidedChange struct {
	Change   protocol.Change   `json:"change"`
	Verified protocol.Verified `json:"verified"`
	// Unsupported says why the provider does not carry the change out (its manifest's Why): the
	// world differs from the declaration in a way it never changes. The plan denies it.
	Unsupported string `json:"unsupported,omitempty"`
}

// SpecDigest identifies a provider's spec (canonical JSON).
func SpecDigest(spec []byte) string {
	sum := sha256.Sum256(spec)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Package is an observed APT package on a node.
type Package struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	// ChangeSet is what installing the package would change (apt-get -s on the node), when it is
	// not installed. Approving an installation approves exactly this set.
	ChangeSet *noderev.ChangeSet `json:"changeSet,omitempty"`
	// Error says why the package could not be observed or simulated.
	Error      string    `json:"error,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

// Service is an observed systemd service on a node.
type Service struct {
	RepairStart bool      `json:"repairStart,omitempty"` // active standing permission, read from the CP's revision
	Exists      bool      `json:"exists"`                // the unit is known to systemd (its package is installed)
	Enabled     bool      `json:"enabled"`
	Running     bool      `json:"running"`
	ObservedAt  time.Time `json:"observedAt"`
}

// Empty is the snapshot of a world where nothing exists yet.
func Empty(asOf time.Time) Snapshot {
	return Snapshot{AsOf: asOf.UTC(), Packages: map[string]Package{}, Services: map[string]Service{}}
}

// Canonical is the deterministic JSON encoding (map keys sorted, times in UTC).
func (s Snapshot) Canonical() []byte {
	out, err := json.Marshal(s.normalized(), json.Deterministic(true))
	if err != nil {
		panic("state: canonical encoding failed: " + err.Error())
	}
	return out
}

// Digest is "sha256:" followed by the hex SHA-256 of the canonical encoding.
func (s Snapshot) Digest() string {
	sum := sha256.Sum256(s.Canonical())
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s Snapshot) normalized() Snapshot {
	n := Empty(s.AsOf)
	n.Stack = s.Stack
	n.AgentRegistrations = s.AgentRegistrations
	for k, v := range s.Packages {
		v.ObservedAt = v.ObservedAt.UTC()
		n.Packages[k] = v
	}
	for k, v := range s.Services {
		v.ObservedAt = v.ObservedAt.UTC()
		n.Services[k] = v
	}
	if len(s.K3s) > 0 {
		n.K3s = map[string]K3s{}
		for k, v := range s.K3s {
			v.ObservedAt = v.ObservedAt.UTC()
			n.K3s[k] = v
		}
	}
	if len(s.Mamelets) > 0 {
		n.Mamelets = map[string]Mamelet{}
		for k, v := range s.Mamelets {
			v.ObservedAt = v.ObservedAt.UTC()
			n.Mamelets[k] = v
		}
	}
	if len(s.K3sManifests) > 0 {
		n.K3sManifests = map[string]K3sManifest{}
		for k, v := range s.K3sManifests {
			v.ObservedAt = v.ObservedAt.UTC()
			n.K3sManifests[k] = v
		}
	}
	if len(s.Provided) > 0 {
		n.Provided = map[string]Provided{}
		for k, v := range s.Provided {
			v.ObservedAt = v.ObservedAt.UTC()
			n.Provided[k] = v
		}
	}
	if len(s.Nodes) > 0 {
		n.Nodes = map[string]Node{}
		for k, v := range s.Nodes {
			v.ObservedAt = v.ObservedAt.UTC()
			n.Nodes[k] = v
		}
	}
	if len(s.NodeRefusals) > 0 {
		n.NodeRefusals = map[string]NodeRefusal{}
		for k, v := range s.NodeRefusals {
			v.ObservedAt = v.ObservedAt.UTC()
			n.NodeRefusals[k] = v
		}
	}
	if len(s.Ext) > 0 {
		n.Ext = map[string]map[string]ExtObservation{}
		for node, exts := range s.Ext {
			n.Ext[node] = map[string]ExtObservation{}
			for name, v := range exts {
				v.ObservedAt = v.ObservedAt.UTC()
				if len(v.Payload) > 0 {
					// The same observation is the same bytes, whatever whitespace or member order it came in.
					p := v.Payload.Clone()
					if p.Canonicalize() == nil {
						v.Payload = p
					}
				}
				n.Ext[node][name] = v
			}
		}
	}
	return n
}

// checkExt says what is wrong with the extensions' observations of a decoded snapshot.
func checkExt(ext map[string]map[string]ExtObservation) error {
	for node, exts := range ext {
		for name, v := range exts {
			if len(v.Payload) > 0 && (!v.Payload.IsValid() || v.Payload.Kind() != '{') {
				return fmt.Errorf("snapshot: ext %s of node %s: the payload is not a JSON object", name, node)
			}
		}
	}
	return nil
}

// Decode reads a snapshot strictly (unknown members, duplicates and trailing data are errors).
func Decode(data []byte) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s, json.RejectUnknownMembers(true)); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: %w", err)
	}
	if s.AsOf.IsZero() {
		return Snapshot{}, fmt.Errorf("snapshot: asOf is required")
	}
	if err := checkExt(s.Ext); err != nil {
		return Snapshot{}, err
	}
	return s.normalized(), nil
}
