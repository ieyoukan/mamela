package kit_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// guests is a kind of the tests: guests that a declaration may name by their VMID to adopt them
// (kit.Adopter), found by it when they carry no mark (kit.Finder). Nothing is sent: List and Find
// answer from memory, so a Client that sends nothing is enough.
type guests struct {
	listed []kit.Object          // what List gives: Mamela's guests, and what it recognizes
	found  map[string]kit.Object // what Find finds, by VMID (unmarked guests)
	finds  []string              // the VMIDs Find was asked for
}

type guestSpec struct {
	VMID   string `json:"vmid,omitempty"`
	Memory int    `json:"memory"`
}

type guestState struct {
	Memory int `json:"memory"`
}

func (*guests) Kind() protocol.ResourceKind      { return "Guest" }
func (*guests) Reads() []protocol.Call           { return nil }
func (*guests) Operations() []protocol.Operation { return nil }
func (g *guests) List(*kit.Client) ([]kit.Object, error) {
	return slices.Clone(g.listed), nil
}

func (*guests) Diff(d kit.Desired) ([]protocol.Change, error) {
	var want guestSpec
	var got guestState
	if err := d.Spec(&want); err != nil {
		return nil, err
	}
	if err := d.State(&got); err != nil {
		return nil, err
	}
	switch {
	case !d.Observed.Exists:
		return []protocol.Change{{Resource: d.Resource.ID, Kind: "Guest", Op: "create-guest", IDs: map[string]string{"vmid": want.VMID},
			Reasons: []string{"it does not exist"}}}, nil
	case got.Memory != want.Memory:
		return []protocol.Change{{Resource: d.Resource.ID, Kind: "Guest", Op: "resize-guest", IDs: d.Observed.IDs, Precondition: d.Observed.Version,
			Reasons: []string{fmt.Sprintf("memory %d -> %d", got.Memory, want.Memory)}}}, nil
	}
	return nil, nil
}

func (*guests) Done(*kit.Client, protocol.Change, protocol.Approved) (bool, error) { return false, nil }

// Adopts: the declaration names a guest by its VMID.
func (*guests) Adopts(r protocol.Resource) (map[string]string, error) {
	var s guestSpec
	if err := json.Unmarshal(r.Spec, &s); err != nil {
		return nil, err
	}
	if s.VMID == "" {
		return nil, nil
	}
	return map[string]string{"vmid": s.VMID}, nil
}

func (g *guests) Find(_ *kit.Client, ids map[string]string) (*kit.Object, error) {
	g.finds = append(g.finds, ids["vmid"])
	x, ok := g.found[ids["vmid"]]
	if !ok {
		return nil, nil
	}
	return &x, nil
}

func guest(name, spec string) protocol.Resource {
	return protocol.Resource{ID: "Guest " + name, Kind: "Guest", Name: name, Spec: []byte(spec)}
}

func object(name, vmid string, memory int) kit.Object {
	return kit.Object{Name: name, IDs: map[string]string{"vmid": vmid}, Version: "v1", State: guestState{Memory: memory}}
}

func observe(t *testing.T, k *guests, adopting []protocol.Adopting, rs ...protocol.Resource) (map[string]protocol.Observed, error) {
	t.Helper()
	p := &kit.Provider{Name: "guests", Version: "0.1.0", API: "guests/v1", Kinds: []kit.Kind{k}}
	res, err := p.Observe(context.Background(), nil, protocol.ObserveParams{Resources: rs, Adopting: adopting})
	out := map[string]protocol.Observed{}
	for _, o := range res.Resources {
		out[o.ID] = o
	}
	return out, err
}

func diff(t *testing.T, rs []protocol.Resource, observed map[string]protocol.Observed) protocol.DiffResult {
	t.Helper()
	p := &kit.Provider{Name: "guests", Version: "0.1.0", API: "guests/v1", Kinds: []kit.Kind{&guests{}}}
	var obs []protocol.Observed
	for _, o := range observed {
		obs = append(obs, o)
	}
	res, err := p.Diff(context.Background(), protocol.DiffParams{Desired: rs, Observed: obs})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A declaration that names a guest by its VMID adopts the guest its Finder finds: observe reports it
// as adoptable (not Mamela's), and diff returns what it would need once adopted as previews, never as
// changes. A guest that matches the declaration has no preview.
func TestAdopterFindsTheNamedObject(t *testing.T) {
	k := &guests{found: map[string]kit.Object{"101": object("", "101", 2048)}}
	web := guest("web", `{"vmid":"101","memory":4096}`)
	got, err := observe(t, k, nil, web)
	if err != nil {
		t.Fatal(err)
	}
	o := got["Guest web"]
	if !o.Exists || o.Owned || !o.Adoptable || o.Unfinished || o.IDs["vmid"] != "101" || !slices.Equal(k.finds, []string{"101"}) {
		t.Fatalf("observed %+v (finds %v)", o, k.finds)
	}
	res := diff(t, []protocol.Resource{web}, got)
	if len(res.Changes) != 0 || len(res.Previews) != 1 || res.Previews[0].Op != "resize-guest" ||
		res.Previews[0].Reasons[0] != "memory 2048 -> 4096" || res.Previews[0].IDs["vmid"] != "101" || res.Previews[0].Precondition != "v1" {
		t.Fatalf("diff %+v", res)
	}
	// As declared: nothing would change once adopted.
	k = &guests{found: map[string]kit.Object{"101": object("", "101", 4096)}}
	got, _ = observe(t, k, nil, web)
	if res := diff(t, []protocol.Resource{web}, got); len(res.Changes) != 0 || len(res.Previews) != 0 {
		t.Fatalf("as declared: %+v", res)
	}
	// What List itself gives as adoptable is taken without asking Find.
	adoptable := object("legacy-web", "101", 2048)
	adoptable.Adoptable = true
	k = &guests{listed: []kit.Object{adoptable}}
	if got, err := observe(t, k, nil, web); err != nil || !got["Guest web"].Adoptable || len(k.finds) != 0 {
		t.Fatalf("listed as adoptable: %+v %v (finds %v)", got["Guest web"], err, k.finds)
	}
	// Named but not there: one to create, as any other.
	k = &guests{}
	got, err = observe(t, k, nil, web)
	if err != nil || got["Guest web"].Exists || got["Guest web"].Adoptable {
		t.Fatalf("not there: %+v %v", got["Guest web"], err)
	}
	if res := diff(t, []protocol.Resource{web}, got); len(res.Changes) != 1 || res.Changes[0].Op != "create-guest" || len(res.Previews) != 0 {
		t.Fatalf("not there: %+v", res)
	}
}

// An object that carries Mamela's mark (listed, neither adoptable nor Foreign) is never adopted for a
// declaration that names it by its IDs: observe says so, and nothing is found or reported adoptable.
func TestAdopterNeverTakesAMarkedObject(t *testing.T) {
	k := &guests{listed: []kit.Object{object("other", "101", 2048)}, found: map[string]kit.Object{"101": object("", "101", 2048)}}
	_, err := observe(t, k, nil, guest("other", `{"memory":2048}`), guest("web", `{"vmid":"101","memory":2048}`))
	if err == nil || !strings.Contains(err.Error(), "Guest web names the object vmid=101 to adopt, which is Mamela's already (Guest other)") || len(k.finds) != 0 {
		t.Fatalf("a marked object named to adopt: %v (finds %v)", err, k.finds)
	}
	// Not declared at all, the marked object is still never taken.
	k.finds = nil
	if _, err := observe(t, k, nil, guest("web", `{"vmid":"101","memory":2048}`)); err == nil || len(k.finds) != 0 {
		t.Fatalf("undeclared marked object: %v (finds %v)", err, k.finds)
	}
	// Its own declaration, by its name, is Mamela's as always (after an adoption the object has the
	// mark, with the declared name).
	got, err := observe(t, k, nil, guest("other", `{"vmid":"101","memory":2048}`))
	if o := got["Guest other"]; err != nil || !o.Exists || !o.Owned || o.Adoptable {
		t.Fatalf("by its name: %+v %v", o, err)
	}
}

// An object is adopted for one declaration only: a second declaration that names it is an error, and
// Find is not asked again (it would be the same object twice).
func TestAdopterTakesAnObjectOnce(t *testing.T) {
	k := &guests{found: map[string]kit.Object{"101": object("", "101", 2048)}}
	_, err := observe(t, k, nil, guest("a", `{"vmid":"101","memory":2048}`), guest("b", `{"vmid":"101","memory":2048}`))
	if err == nil || !strings.Contains(err.Error(), "Guest b names the object vmid=101 to adopt, and so does Guest a") || len(k.finds) != 1 {
		t.Fatalf("named twice: %v (finds %v)", err, k.finds)
	}
	// An import of the same object, then a declaration that names it: the second is refused too.
	k = &guests{found: map[string]kit.Object{"101": object("", "101", 2048)}}
	_, err = observe(t, k, []protocol.Adopting{{Resource: "Guest a", IDs: map[string]string{"vmid": "101"}}},
		guest("a", `{"memory":2048}`), guest("b", `{"vmid":"101","memory":2048}`))
	if err == nil || !strings.Contains(err.Error(), "and so does Guest a") || len(k.finds) != 1 {
		t.Fatalf("imported, then named: %v (finds %v)", err, k.finds)
	}
}

// A record of an import (ADR 0009) comes before what the declaration names: its IDs are the ones the
// object is found by.
func TestImportRecordComesFirst(t *testing.T) {
	k := &guests{found: map[string]kit.Object{"101": object("", "101", 2048), "202": object("", "202", 1024)}}
	got, err := observe(t, k, []protocol.Adopting{{Resource: "Guest web", IDs: map[string]string{"vmid": "202"}}}, guest("web", `{"vmid":"101","memory":2048}`))
	if o := got["Guest web"]; err != nil || !o.Adoptable || o.IDs["vmid"] != "202" || !slices.Equal(k.finds, []string{"202"}) {
		t.Fatalf("imported: %+v %v (finds %v)", o, err, k.finds)
	}
}

// Diff never changes an object to adopt: only a preview, even when the kind's diff has a change.
// Something that exists and is neither Mamela's nor adoptable is refused, as before.
func TestDiffPreviewsOnlyAdoptable(t *testing.T) {
	web := guest("web", `{"memory":4096}`)
	o := protocol.Observed{ID: "Guest web", Kind: "Guest", Name: "web", Exists: true, IDs: map[string]string{"vmid": "101"}, Version: "v1",
		State: []byte(`{"memory":2048}`)}
	adoptable, owned := o, o
	adoptable.Adoptable = true
	owned.Owned = true
	if res := diff(t, []protocol.Resource{web}, map[string]protocol.Observed{web.ID: adoptable}); len(res.Changes) != 0 || len(res.Previews) != 1 {
		t.Fatalf("adoptable: %+v", res)
	}
	if res := diff(t, []protocol.Resource{web}, map[string]protocol.Observed{web.ID: owned}); len(res.Changes) != 1 || len(res.Previews) != 0 {
		t.Fatalf("owned: %+v", res)
	}
	p := &kit.Provider{Name: "guests", Version: "0.1.0", API: "guests/v1", Kinds: []kit.Kind{&guests{}}}
	if _, err := p.Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{web}, Observed: []protocol.Observed{o}}); err == nil ||
		!strings.Contains(err.Error(), "Mamela did not create it") {
		t.Fatalf("Foreign: %v", err)
	}
}
