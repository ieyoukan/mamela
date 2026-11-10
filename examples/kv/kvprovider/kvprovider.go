// Package kvprovider is an example Mamela provider: it manages items of the example key-value API
// (kvapi). Read it as a template for a provider; cmd/kv-provider builds it for WASI.
//
// Kind KVItem, spec {"value": "...", "labels": ["..."]}. It creates and updates items it owns
// (owner "mamela"), never deletes, and never touches an item someone else created.
package kvprovider

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// API is the API the provider talks to (the example key-value API; examples/kv/kvapi fakes it).
// The provider must not import the fake: a provider program may import nothing that opens files
// or sockets (net/http does), or the host refuses to run it.
const API = "kv-example/v1"

// item is an item as the API returns it.
type item struct {
	Name    string   `json:"name"`
	Value   string   `json:"value"`
	Owner   string   `json:"owner,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	Version int      `json:"version"`
}

// Kind is the resource kind the provider handles.
const Kind protocol.ResourceKind = "KVItem"

// Operations.
const (
	OpCreate protocol.Op = "create-item"
	OpUpdate protocol.Op = "update-item"
	OpDelete protocol.Op = "delete-item"
)

const owner = "mamela"

// Spec is the declared form of an item.
type Spec struct {
	Value  string   `json:"value"`
	Labels []string `json:"labels,omitempty"`
}

// Provider implements sdk.Provider.
type Provider struct{}

var (
	getItem   = protocol.Call{Method: "GET", Path: "/items/{item}"}
	putItem   = protocol.Call{Method: "PUT", Path: "/items/{item}", Params: []string{"value", "owner", "version"}}
	putLabels = protocol.Call{Method: "PUT", Path: "/items/{item}/labels", Params: []string{"labels"}}
	listItems = protocol.Call{Method: "GET", Path: "/items"}
)

// Manifest implements sdk.Provider.
func (Provider) Manifest() protocol.Manifest {
	write := protocol.Need{Capability: "kv:Write", Target: protocol.Target{Type: "kv-namespace", Ref: "items"}, Object: "{name}", Always: true}
	return protocol.Manifest{
		Schema: protocol.Schema, Provider: "kv-example", Version: "0.1.0", API: API,
		Observe:      []protocol.Need{{Capability: "kv:Read", Target: protocol.Target{Type: "kv-namespace", Ref: "items"}, Always: true}},
		ObserveCalls: []protocol.Call{listItems},
		Kinds: []protocol.Kind{{Kind: Kind, Operations: []protocol.Operation{
			{Op: OpCreate, Support: protocol.Supported, Needs: []protocol.Need{write}, Calls: []protocol.Call{getItem, putItem, putLabels}},
			{Op: OpUpdate, Support: protocol.Supported, Needs: []protocol.Need{write}, Calls: []protocol.Call{getItem, putItem, putLabels}},
			{Op: OpDelete, Support: protocol.Unsupported, Why: "removing a declaration never deletes an item"},
		}}},
	}
}

// Observe implements sdk.Provider: one list of all items.
func (Provider) Observe(ctx context.Context, h *sdk.Host, p protocol.ObserveParams) (protocol.ObserveResult, error) {
	items, err := list(ctx, h)
	if err != nil {
		return protocol.ObserveResult{}, err
	}
	var out protocol.ObserveResult
	declared := map[string]bool{}
	for _, r := range p.Resources {
		declared[r.Name] = true
		it, ok := items[r.Name]
		out.Resources = append(out.Resources, observed(r.ID, r.Name, it, ok))
	}
	// Items Mamela created that are no longer declared: reported, so that Mamela keeps them.
	for _, name := range sortedKeys(items) {
		if it := items[name]; !declared[name] && it.Owner == owner {
			out.Resources = append(out.Resources, observed(string(Kind)+"/"+name, name, it, true))
		}
	}
	return out, nil
}

func observed(id, name string, it item, exists bool) protocol.Observed {
	o := protocol.Observed{ID: id, Kind: Kind, Name: name, Exists: exists, IDs: map[string]string{"item": name}}
	if exists {
		o.Owned = it.Owner == owner
		o.Version = strconv.Itoa(it.Version)
		o.State, _ = json.Marshal(Spec{Value: it.Value, Labels: it.Labels}, json.Deterministic(true))
	}
	return o
}

// Diff implements sdk.Provider. Only what is declared and differs; nothing for what is not
// declared.
func (Provider) Diff(ctx context.Context, p protocol.DiffParams) (protocol.DiffResult, error) {
	obs := map[string]protocol.Observed{}
	for _, o := range p.Observed {
		obs[o.ID] = o
	}
	var out protocol.DiffResult
	desired := slices.Clone(p.Desired)
	slices.SortFunc(desired, func(a, b protocol.Resource) int { return strings.Compare(a.ID, b.ID) })
	for _, r := range desired {
		want, err := spec(r.Spec)
		if err != nil {
			return out, fmt.Errorf("%s: %w", r.ID, err)
		}
		o := obs[r.ID]
		c := protocol.Change{Resource: r.ID, Kind: Kind, IDs: map[string]string{"item": r.Name}}
		switch {
		case !o.Exists:
			c.Op, c.Reasons = OpCreate, []string{"the item does not exist"}
			c.Values = pins(want, true)
		case !o.Owned:
			return out, fmt.Errorf("%s: the item exists and Mamela did not create it; Mamela does not take it over", r.ID)
		default:
			got, err := spec(o.State)
			if err != nil {
				return out, fmt.Errorf("%s: observed state: %w", r.ID, err)
			}
			labels := len(want.Labels) > 0 && !slices.Equal(got.Labels, want.Labels) // no labels declared: not managed
			if got.Value == want.Value && !labels {
				continue
			}
			c.Op, c.Precondition = OpUpdate, o.Version
			c.Values = pins(want, got.Value != want.Value)
			if got.Value != want.Value {
				c.Reasons = append(c.Reasons, fmt.Sprintf("value %q -> %q", got.Value, want.Value))
			}
			if labels {
				c.Reasons = append(c.Reasons, fmt.Sprintf("labels %v -> %v", got.Labels, want.Labels))
			}
		}
		out.Changes = append(out.Changes, c)
	}
	return out, nil
}

// pins fixes the values the change sends: what a person approves.
func pins(s Spec, value bool) []protocol.Pin {
	var out []protocol.Pin
	if value {
		out = append(out,
			protocol.Pin{Method: putItem.Method, Path: putItem.Path, Param: "value", Values: []string{s.Value}},
			protocol.Pin{Method: putItem.Method, Path: putItem.Path, Param: "owner", Values: []string{owner}})
	}
	if len(s.Labels) > 0 {
		out = append(out, protocol.Pin{Method: putLabels.Method, Path: putLabels.Path, Param: "labels", Values: s.Labels})
	}
	return out
}

// Apply implements sdk.Provider. It reads the item first and does only what is missing, so that
// an apply after a partial one resumes it.
func (Provider) Apply(ctx context.Context, h *sdk.Host, p protocol.ApplyParams) (protocol.ApplyResult, error) {
	if p.Desired == nil {
		return protocol.ApplyResult{}, errors.New("no desired state")
	}
	want, err := spec(p.Desired.Spec)
	if err != nil {
		return protocol.ApplyResult{}, err
	}
	name := p.Change.IDs["item"]
	it, exists, err := get(ctx, h, name)
	if err != nil {
		return protocol.ApplyResult{}, err
	}
	if exists && it.Owner != owner {
		return protocol.ApplyResult{}, fmt.Errorf("item %s exists and Mamela did not create it", name)
	}
	if !exists || it.Value != want.Value {
		form := map[string][]string{"value": {want.Value}}
		if exists {
			form["version"] = []string{strconv.Itoa(it.Version)}
		} else {
			form["owner"] = []string{owner}
		}
		if it, err = put(ctx, h, "/items/"+name, form); err != nil {
			return protocol.ApplyResult{}, err
		}
	}
	if len(want.Labels) > 0 && !slices.Equal(it.Labels, want.Labels) {
		if _, err := put(ctx, h, "/items/"+name+"/labels", map[string][]string{"labels": want.Labels}); err != nil {
			return protocol.ApplyResult{}, err
		}
	}
	return protocol.ApplyResult{Message: "item " + name + " is as declared"}, nil
}

// Lookup implements sdk.Provider.
func (Provider) Lookup(ctx context.Context, h *sdk.Host, p protocol.LookupParams) (protocol.LookupResult, error) {
	it, exists, err := get(ctx, h, p.Change.IDs["item"])
	if err != nil {
		return protocol.LookupResult{}, err
	}
	steps, done := 0, 0
	for _, pin := range p.Change.Values {
		switch pin.Param {
		case "value":
			steps++
			if exists && slices.Contains(pin.Values, it.Value) {
				done++
			}
		case "labels":
			steps++
			if exists && slices.Equal(it.Labels, slices.Sorted(slices.Values(pin.Values))) {
				done++
			}
		}
	}
	switch {
	case done == steps:
		return protocol.LookupResult{Status: protocol.LookupDone}, nil
	case done == 0:
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	return protocol.LookupResult{Status: protocol.LookupPartly}, nil
}

func spec(v jsontext.Value) (Spec, error) {
	var s Spec
	if len(v) == 0 {
		return s, errors.New("no spec")
	}
	if err := json.Unmarshal(v, &s, json.RejectUnknownMembers(true)); err != nil {
		return s, err
	}
	s.Labels = slices.Sorted(slices.Values(s.Labels))
	return s, nil
}

func list(ctx context.Context, h *sdk.Host) (map[string]item, error) {
	resp, err := h.Request(ctx, protocol.HostRequest{Method: "GET", Path: "/items"})
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("listing items: status %d: %s", resp.Status, resp.Body)
	}
	var items []item
	if err := json.Unmarshal([]byte(resp.Body), &items); err != nil {
		return nil, err
	}
	out := map[string]item{}
	for _, it := range items {
		out[it.Name] = it
	}
	return out, nil
}

func get(ctx context.Context, h *sdk.Host, name string) (item, bool, error) {
	resp, err := h.Request(ctx, protocol.HostRequest{Method: "GET", Path: "/items/" + name})
	if err != nil {
		return item{}, false, err
	}
	switch resp.Status {
	case 404:
		return item{}, false, nil
	case 200:
		var it item
		err := json.Unmarshal([]byte(resp.Body), &it)
		return it, true, err
	}
	return item{}, false, fmt.Errorf("reading item %s: status %d: %s", name, resp.Status, resp.Body)
}

func put(ctx context.Context, h *sdk.Host, path string, form map[string][]string) (item, error) {
	resp, err := h.Request(ctx, protocol.HostRequest{Method: "PUT", Path: path, Form: form})
	if err != nil {
		return item{}, err
	}
	if resp.Status != 200 {
		return item{}, fmt.Errorf("PUT %s: status %d: %s", path, resp.Status, resp.Body)
	}
	var it item
	err = json.Unmarshal([]byte(resp.Body), &it)
	return it, err
}

func sortedKeys(m map[string]item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
