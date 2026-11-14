package kit

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// Importer is a kind that turns resources of Terraform and OpenTofu into declarations (ADR 0009),
// so that what they manage can move to Mamela: `mamela import terraform` writes the declarations
// as Emela, and a person adopts each object with the kind's adopt operation (AdoptOp).
type Importer interface {
	// Imports are the Terraform types it imports ("aws_vpc").
	Imports() []string
	// Import makes the declaration of one object: its spec (the kind's Spec type) and the IDs
	// that identify it at the API, which List (or Find) gives it. The declaration's name is
	// o.Name. Return Skip(why) for an object it cannot declare (say what to do instead).
	Import(o protocol.ImportObject, r Resolver) (Imported, error)
}

// Imported is the declaration of one object.
type Imported struct {
	Spec any
	IDs  map[string]string
	// Also are the addresses of other imported objects the declaration includes (a route table's
	// associations): they move to Mamela with it.
	Also []string
}

// Finder is a kind that finds one object by its IDs, though List does not give it (List gives
// what carries Mamela's marks; an imported object carries none until it is adopted).
type Finder interface {
	Find(c *Client, ids map[string]string) (*Object, error)
}

// Resolver names the declarations of the other imported objects, for references (a subnet's
// vpc_id is the name of the VPC's declaration), and gives the objects and the settings.
type Resolver struct {
	names    map[string]map[string]string // type -> id -> name
	objects  []protocol.ImportObject
	settings []byte
}

// Name is the name of the declaration of the imported object of a type with an ID (its "id"
// attribute), if it is imported too.
func (r Resolver) Name(tfType, id string) (string, bool) {
	n, ok := r.names[tfType][id]
	return n, ok
}

// Objects are the imported objects of a type (a route table's associations, say).
func (r Resolver) Objects(tfType string) []protocol.ImportObject {
	var out []protocol.ImportObject
	for _, o := range r.objects {
		if o.Type == tfType {
			out = append(out, o)
		}
	}
	return out
}

// Settings decodes the provider's guard settings ([provider.<name>.guard]) into v; false when
// there are none.
func (r Resolver) Settings(v any) (bool, error) {
	if len(r.settings) == 0 {
		return false, nil
	}
	return true, json.Unmarshal(r.settings, v)
}

// ErrSkip is an object an importer does not declare.
type ErrSkip struct{ Why string }

func (e *ErrSkip) Error() string { return e.Why }

// Skip says an object is not imported, and why.
func Skip(why string) error { return &ErrSkip{Why: why} }

// Values decodes an imported object's attributes into v (unknown attributes are ignored: a
// Terraform resource has many).
func Values(o protocol.ImportObject, v any) error {
	return json.Unmarshal(o.Values, v)
}

// Import implements sdk.Importer.
func (p *Provider) Import(_ context.Context, in protocol.ImportParams) (protocol.ImportResult, error) {
	r := Resolver{names: map[string]map[string]string{}, objects: in.Objects, settings: in.Settings}
	for _, o := range in.Objects {
		var v struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(o.Values, &v) == nil && v.ID != "" {
			if r.names[o.Type] == nil {
				r.names[o.Type] = map[string]string{}
			}
			r.names[o.Type][v.ID] = o.Name
		}
	}
	var out protocol.ImportResult
	for _, o := range in.Objects {
		var im Importer
		var kind protocol.ResourceKind
		for _, k := range p.Kinds {
			if i, ok := k.(Importer); ok && slices.Contains(i.Imports(), o.Type) {
				im, kind = i, k.Kind()
				break
			}
		}
		if im == nil {
			out.Skipped = append(out.Skipped, protocol.ImportSkip{Address: o.Address, Why: fmt.Sprintf("no kind of provider %s imports %s", p.Name, o.Type)})
			continue
		}
		imp, err := im.Import(o, r)
		var skip *ErrSkip
		switch {
		case errors.As(err, &skip):
			out.Skipped = append(out.Skipped, protocol.ImportSkip{Address: o.Address, Why: skip.Why})
			continue
		case err != nil:
			return out, fmt.Errorf("import of %s: %w", o.Address, err)
		}
		b, err := json.Marshal(imp.Spec, json.Deterministic(true))
		if err != nil {
			return out, fmt.Errorf("import of %s: %w", o.Address, err)
		}
		out.Resources = append(out.Resources, protocol.Imported{Address: o.Address, Kind: kind, Name: o.Name, Spec: b, IDs: imp.IDs, Also: imp.Also})
	}
	// What a declaration includes is not skipped (a route table's association moves with it).
	included := map[string]bool{}
	for _, r := range out.Resources {
		for _, a := range r.Also {
			included[a] = true
		}
	}
	out.Skipped = slices.DeleteFunc(out.Skipped, func(s protocol.ImportSkip) bool { return included[s.Address] })
	return out, nil
}

// adoptable finds, among a kind's objects not taken yet, the one an adoption names (its IDs
// include the imported ones).
func adoptable(objs []Object, taken map[int]bool, ids map[string]string) int {
	for i, x := range objs {
		if !taken[i] && protocol.Targets(x.IDs, ids) {
			return i
		}
	}
	return -1
}
