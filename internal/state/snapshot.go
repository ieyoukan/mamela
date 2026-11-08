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
)

// Snapshot is a time-stamped view of the observed world.
type Snapshot struct {
	// AsOf is "now" for planning: observation ages are measured against it.
	AsOf time.Time `json:"asOf"`
	// VMs by name.
	VMs map[string]VM `json:"vms"`
	// Packages by "<node>/<package>". Absent means "not observed", not "not installed".
	Packages map[string]Package `json:"packages"`
	// Services by "<node>/<service>". Absent means "not observed".
	Services map[string]Service `json:"services"`
}

// VM is an observed virtual machine.
type VM struct {
	// Managed is true only when every ownership marker matches (pool, VMID range, tag, marker).
	// Anything else is Foreign and is never changed.
	Managed    bool      `json:"managed"`
	CPU        int64     `json:"cpu"`
	MemoryMiB  int64     `json:"memoryMiB"`
	DiskGiB    int64     `json:"diskGiB"`
	Image      string    `json:"image"`
	ObservedAt time.Time `json:"observedAt"`
}

// Package is an observed APT package on a node.
type Package struct {
	Installed  bool      `json:"installed"`
	Version    string    `json:"version"`
	ObservedAt time.Time `json:"observedAt"`
}

// Service is an observed systemd service on a node.
type Service struct {
	Enabled    bool      `json:"enabled"`
	Running    bool      `json:"running"`
	ObservedAt time.Time `json:"observedAt"`
}

// Empty is the snapshot of a world where nothing exists yet.
func Empty(asOf time.Time) Snapshot {
	return Snapshot{AsOf: asOf.UTC(), VMs: map[string]VM{}, Packages: map[string]Package{}, Services: map[string]Service{}}
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
	for k, v := range s.VMs {
		v.ObservedAt = v.ObservedAt.UTC()
		n.VMs[k] = v
	}
	for k, v := range s.Packages {
		v.ObservedAt = v.ObservedAt.UTC()
		n.Packages[k] = v
	}
	for k, v := range s.Services {
		v.ObservedAt = v.ObservedAt.UTC()
		n.Services[k] = v
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
