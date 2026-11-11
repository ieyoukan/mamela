package protocol

import "testing"

// A claim of ownership stands only for an object whose IDs Mamela recorded it set out to create.
func TestVouch(t *testing.T) {
	created := []map[string]string{{"node": "pve1", "vmid": "7007", "storage": "local"}}
	o := Observed{ID: "ProxmoxVM web", Exists: true, IDs: map[string]string{"node": "pve1", "vmid": "7007"}}
	for _, c := range []struct {
		name               string
		owned, unfinished  bool
		ids                map[string]string
		created            []map[string]string
		wantOwned, wantUnf bool
	}{
		{"claimed and recorded", true, false, o.IDs, created, true, false},
		{"unfinished and recorded", false, true, o.IDs, created, true, true},
		{"claimed, not recorded (a copied marker)", true, false, o.IDs, nil, false, false},
		{"claimed, another object recorded", true, false, map[string]string{"node": "pve1", "vmid": "7008"}, created, false, false},
		{"not claimed though recorded", false, false, o.IDs, created, false, false},
		{"claimed without IDs", true, false, nil, created, false, false},
	} {
		in := o
		in.Owned, in.Unfinished, in.IDs = c.owned, c.unfinished, c.ids
		got := Vouch(in, c.created)
		if got.Owned != c.wantOwned || got.Unfinished != c.wantUnf {
			t.Errorf("%s: owned %v unfinished %v", c.name, got.Owned, got.Unfinished)
		}
	}
}
