// Package state holds the Observed State: what Mamela last saw of the real world.
//
// A Snapshot is an input to planning. It carries its own point in time (AsOf), so planning never
// reads the wall clock and the same snapshot always gives the same plan.
package state

import (
	"crypto/sha256"
	"encoding/hex"
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
	// Packages by "<node>/<package>". Absent means "not observed", not "not installed".
	Packages map[string]Package `json:"packages"`
	// Services by "<node>/<service>". Absent means "not observed".
	Services map[string]Service `json:"services"`
	// Provided are resources of providers, by graph ID ("KVItem web"): the declared ones and
	// those a provider found that Mamela created and that are no longer declared.
	Provided map[string]Provided `json:"provided,omitempty"`
	// K3s by "<node>/k3s". Absent means "not observed".
	K3s map[string]K3s `json:"k3s,omitempty"`
	// Nodes are the machines of providers' node kinds, by resource name (ADR 0004).
	Nodes map[string]Node `json:"nodes,omitempty"`
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
	ObservedAt time.Time `json:"observedAt"`
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
	ObservedAt time.Time        `json:"observedAt"`
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
	Exists     bool      `json:"exists"` // the unit is known to systemd (its package is installed)
	Enabled    bool      `json:"enabled"`
	Running    bool      `json:"running"`
	ObservedAt time.Time `json:"observedAt"`
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
	return n
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
	return s.normalized(), nil
}
