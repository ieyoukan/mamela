package state

import (
	"testing"
	"time"
)

func TestDigestIsDeterministicAndTimezoneFree(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	a := Empty(time.Date(2026, 11, 8, 21, 0, 0, 0, jst))
	a.VMs["b"] = VM{Managed: true, CPU: 1, ObservedAt: time.Date(2026, 11, 8, 20, 59, 0, 0, jst)}
	a.VMs["a"] = VM{Managed: true, CPU: 2, ObservedAt: time.Date(2026, 11, 8, 20, 59, 0, 0, jst)}

	b := Empty(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC))
	b.VMs["a"] = VM{Managed: true, CPU: 2, ObservedAt: time.Date(2026, 11, 8, 11, 59, 0, 0, time.UTC)}
	b.VMs["b"] = VM{Managed: true, CPU: 1, ObservedAt: time.Date(2026, 11, 8, 11, 59, 0, 0, time.UTC)}

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
		"unknown member": `{"asOf":"2026-11-08T12:00:00Z","vms":{},"packages":{},"services":{},"extra":1}`,
		"no asOf":        `{"vms":{},"packages":{},"services":{}}`,
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
