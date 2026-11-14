package protocol

import (
	"strings"
	"testing"
)

// manifestWith is a one-kind manifest with the given operations.
func manifestWith(ops ...Operation) Manifest {
	return Manifest{Schema: Schema, Provider: "p", Version: "1", API: "p/v1",
		Kinds: []Kind{{Kind: "Thing", Operations: ops}}}
}

// Only a supported explicit operation removes, a kind has at most one, and none also adopts.
func TestRemovesIsAnExplicitOperationOfWhichThereIsOne(t *testing.T) {
	del := Operation{Op: "delete-thing", Support: Supported, Explicit: true, Removes: true}
	for _, c := range []struct {
		name string
		ops  []Operation
		want string // "" valid
	}{
		{"explicit and supported", []Operation{del}, ""},
		{"not explicit", []Operation{{Op: "delete-thing", Support: Supported, Removes: true}}, "must be an explicit, supported operation"},
		{"unsupported", []Operation{{Op: "delete-thing", Support: Unsupported, Why: "never", Removes: true}}, "must be an explicit, supported operation"},
		{"also adopts", []Operation{{Op: "delete-thing", Support: Supported, Explicit: true, Removes: true, Adopts: true}}, "cannot both adopt and remove"},
		{"two of them", []Operation{del, {Op: "drop-thing", Support: Supported, Explicit: true, Removes: true}}, "more than one removing operation"},
	} {
		err := manifestWith(c.ops...).Check()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	m := manifestWith(Operation{Op: "create-thing", Support: Supported}, del)
	if o, ok := m.RemoveOp("Thing"); !ok || o.Op != "delete-thing" {
		t.Errorf("RemoveOp = %v %v", o, ok)
	}
	if _, ok := m.RemoveOp("Other"); ok {
		t.Error("RemoveOp of a kind that is not there")
	}
}
