package protocol

import "testing"

// A manifest's scripts have unique, plain names and say what they do.
func TestManifestScripts(t *testing.T) {
	base := Manifest{Schema: Schema, Provider: "p", Version: "1"}
	ok := base
	ok.Scripts = []Script{{Name: "bootstrap", Description: "creates the identities; run as root"}, {Name: "clean-up", Description: "x"}}
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	for name, scripts := range map[string][]Script{
		"duplicate":      {{Name: "a", Description: "x"}, {Name: "a", Description: "y"}},
		"no description": {{Name: "a"}},
		"path in name":   {{Name: "../a", Description: "x"}},
		"uppercase":      {{Name: "Bootstrap", Description: "x"}},
	} {
		m := base
		m.Scripts = scripts
		if m.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
