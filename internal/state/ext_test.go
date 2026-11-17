package state

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"
)

// What a node's mamelet reported of an extension is part of the snapshot's digest, the same however
// its JSON was spaced or ordered; a snapshot without it is what it was before the generic wire.
func TestExtObservationsAreCanonical(t *testing.T) {
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	a := Empty(at)
	a.Ext = map[string]map[string]ExtObservation{"web": {"linux": {Accepts: 1, Payload: jsontext.Value(`{"files":[{"path":"/etc/x","exists":true}]}`), ObservedAt: at}}}
	b := Empty(at)
	b.Ext = map[string]map[string]ExtObservation{"web": {"linux": {Accepts: 1, Payload: jsontext.Value(` { "files" : [ { "exists" : true, "path" : "/etc/x" } ] }`), ObservedAt: at.In(time.FixedZone("JST", 9*3600))}}}
	if a.Digest() != b.Digest() {
		t.Errorf("the same observation, two digests:\n%s\n%s", a.Canonical(), b.Canonical())
	}
	c := a
	c.Ext = map[string]map[string]ExtObservation{"web": {"linux": {Accepts: 1, Payload: jsontext.Value(`{"files":[{"path":"/etc/y","exists":true}]}`), ObservedAt: at}}}
	if a.Digest() == c.Digest() {
		t.Error("another observation, the same digest")
	}
	got, err := Decode(a.Canonical())
	if err != nil || got.Digest() != a.Digest() {
		t.Fatalf("round trip: %v", err)
	}
	if plain := Empty(at); strings.Contains(string(plain.Canonical()), "ext") {
		t.Errorf("%s", plain.Canonical())
	}
	if _, err := Decode([]byte(`{"asOf":"2026-11-17T09:00:00Z","packages":{},"services":{},"ext":{"web":{"linux":{"accepts":1,"payload":[1],"observedAt":"2026-11-17T09:00:00Z"}}}}`)); err == nil {
		t.Error("a payload that is not an object was read")
	}
}
