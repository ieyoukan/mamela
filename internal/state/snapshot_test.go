package state

import (
	"strings"
	"testing"
	"time"
)

func TestDigestIsDeterministicAndTimezoneFree(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	a := Empty(time.Date(2026, 11, 8, 21, 0, 0, 0, jst))
	a.Services["b/nginx"] = Service{Exists: true, ObservedAt: time.Date(2026, 11, 8, 20, 59, 0, 0, jst)}
	a.Services["a/nginx"] = Service{Exists: true, Running: true, ObservedAt: time.Date(2026, 11, 8, 20, 59, 0, 0, jst)}

	b := Empty(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC))
	b.Services["a/nginx"] = Service{Exists: true, Running: true, ObservedAt: time.Date(2026, 11, 8, 11, 59, 0, 0, time.UTC)}
	b.Services["b/nginx"] = Service{Exists: true, ObservedAt: time.Date(2026, 11, 8, 11, 59, 0, 0, time.UTC)}

	if a.Digest() != b.Digest() {
		t.Errorf("same instant in different zones or insertion order gave different digests:\n%s\n%s", a.Canonical(), b.Canonical())
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	s := Empty(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC))
	s.Services["app/nginx"] = Service{Enabled: true, Running: false, ObservedAt: s.AsOf}
	got, err := Decode(s.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest() != s.Digest() {
		t.Error("round trip changed the digest")
	}
}

func TestDecodeRejects(t *testing.T) {
	for name, in := range map[string]string{
		"unknown member": `{"asOf":"2026-11-08T12:00:00Z","packages":{},"services":{},"extra":1}`,
		"no asOf":        `{"packages":{},"services":{}}`,
		"VMs (ADR 0006)": `{"asOf":"2026-11-08T12:00:00Z","vms":{},"packages":{},"services":{}}`,
		"duplicate":      `{"asOf":"2026-11-08T12:00:00Z","asOf":"2026-11-08T12:00:00Z"}`,
		"trailing":       `{"asOf":"2026-11-08T12:00:00Z"} {}`,
	} {
		if _, err := Decode([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every part of a snapshot survives storing and reading it: a plan is recomputed from it.
func TestNodesRoundTrip(t *testing.T) {
	s := Empty(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC))
	s.Nodes = map[string]Node{"web": {Provider: "proxmox", Kind: "ProxmoxVM", Managed: true, ID: 7007, Running: true, UptimeSeconds: 60, ObservedAt: s.AsOf}}
	got, err := Decode(s.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest() != s.Digest() || got.Nodes["web"].ID != 7007 {
		t.Fatalf("nodes do not round-trip: %+v", got.Nodes)
	}
}

// A refusal round-trips (a plan is recomputed from the stored snapshot), and a world where no node
// refuses is encoded as it always was: no new member, so no stored plan's digest changes.
func TestNodeRefusalsRoundTripAndAreAbsentWithoutOne(t *testing.T) {
	s := Empty(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC))
	if strings.Contains(string(s.Canonical()), "nodeRefusals") {
		t.Fatalf("a snapshot without a refusal has the member: %s", s.Canonical())
	}
	s.NodeRefusals = map[string]NodeRefusal{"web": {Serial: 7, WorksFrom: 5, Reason: "the revision is older than one already accepted: serial 7 < 9",
		ObservedAt: time.Date(2026, 11, 17, 9, 0, 0, 0, time.FixedZone("JST", 9*3600))}}
	got, err := Decode(s.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	r := got.NodeRefusals["web"]
	if got.Digest() != s.Digest() || r.Serial != 7 || r.WorksFrom != 5 || !r.ObservedAt.Equal(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("a refusal does not round-trip: %+v", got.NodeRefusals)
	}
}
