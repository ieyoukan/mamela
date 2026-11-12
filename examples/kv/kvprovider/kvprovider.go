// Package kvprovider is an example Mamela provider, written with provider/kit (ADR 0009): it
// manages items of the example key-value API (kvapi). Read it as a template for a provider;
// cmd/kv-provider builds it for WASI.
//
// Kind KVItem, spec {"value": "...", "labels": ["..."]}. It creates and updates items it owns
// (owner "mamela") and never touches an item someone else created. Removing a declaration never
// deletes an item: deleting one is an explicit operation (delete-item), which a person asks for by
// name (mamela act). Its guard (ADR 0006) limits how many items there may be and checks that the
// items of others are unchanged by an apply.
//
// What the author writes is the kind: how to list items, what changes a declaration needs, and
// whether a request is in effect. kit does the protocol.
package kvprovider

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// API is the API the provider talks to (the example key-value API; examples/kv/kvapi fakes it).
// The provider must not import the fake: a provider program may import nothing that opens files
// or sockets (net/http does), or the host refuses to run it.
const API = "kv-example/v1"

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
	Value  string   `json:"value" doc:"the item's value"`
	Labels []string `json:"labels,omitempty" doc:"its labels (none declared: not managed)"`
}

// item is an item as the API returns it.
type item struct {
	Name    string   `json:"name"`
	Value   string   `json:"value"`
	Owner   string   `json:"owner,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	Version int      `json:"version"`
}

var (
	listItems  = protocol.Call{Method: "GET", Path: "/items"}
	putItem    = protocol.Call{Method: "PUT", Path: "/items/{item}", Form: []string{"value", "owner", "version"}}
	putLabels  = protocol.Call{Method: "PUT", Path: "/items/{item}/labels", Form: []string{"labels"}}
	deleteItem = protocol.Call{Method: "DELETE", Path: "/items/{item}"}
)

// Provider is the provider.
var Provider = &kit.Provider{
	Name: "kv-example", Version: "0.1.0", API: API,
	ObserveNeeds: []protocol.Need{{Capability: "kv:Read", Target: protocol.Target{Type: "kv-namespace", Ref: "items"}, Always: true}},
	Kinds:        []kit.Kind{items{}},
	GuardFunc:    guard,
	GuardCalls:   []protocol.Call{listItems},
}

// items is the kind KVItem.
type items struct{}

var write = []protocol.Need{{Capability: "kv:Write", Target: protocol.Target{Type: "kv-namespace", Ref: "items"}, Object: "{name}", Always: true}}

func (items) Kind() protocol.ResourceKind { return Kind }
func (items) Spec() any                   { return Spec{} }
func (items) Doc() string                 { return "An item of the key-value API: its value, and its labels." }
func (items) Reads() []protocol.Call      { return []protocol.Call{listItems} }
func (items) Operations() []protocol.Operation {
	return []protocol.Operation{
		kit.Op(OpCreate, write, putItem, putLabels),
		kit.Op(OpUpdate, write, putItem, putLabels),
		kit.ExplicitOp(OpDelete, write, deleteItem),
	}
}

// Absent: version "0" is the version of an item that does not exist (a create is based on it: the
// API creates the item only if it still does not exist).
func (items) Absent(r protocol.Resource) (string, map[string]string) {
	return "0", map[string]string{"item": r.Name}
}

// List lists the items: those of others are Foreign (Mamela refuses a declaration of one).
func (items) List(c *kit.Client) ([]kit.Object, error) {
	all, err := list(c)
	if err != nil {
		return nil, err
	}
	var out []kit.Object
	for _, name := range slices.Sorted(maps.Keys(all)) {
		it := all[name]
		out = append(out, kit.Object{Name: name, IDs: map[string]string{"item": name}, Version: strconv.Itoa(it.Version),
			State: Spec{Value: it.Value, Labels: it.Labels}, Foreign: it.Owner != owner})
	}
	return out, nil
}

// Diff: only what differs. No labels declared: labels are not managed.
func (items) Diff(d kit.Desired) ([]protocol.Change, error) {
	var want, got Spec
	if err := d.Spec(&want); err != nil {
		return nil, err
	}
	if err := d.State(&got); err != nil {
		return nil, err
	}
	want.Labels = slices.Sorted(slices.Values(want.Labels))
	got.Labels = slices.Sorted(slices.Values(got.Labels))
	c := protocol.Change{Resource: d.Resource.ID, Kind: Kind, IDs: map[string]string{"item": d.Resource.Name}}
	labels := len(want.Labels) > 0
	if !d.Observed.Exists {
		c.Op, c.Reasons, c.Precondition = OpCreate, []string{"the item does not exist"}, d.Observed.Version
		c.Requests = append(c.Requests, kit.Request(putItem, map[string][]string{"value": {want.Value}, "owner": {owner}, "version": {d.Observed.Version}}))
	} else {
		labels = labels && !slices.Equal(got.Labels, want.Labels)
		if got.Value == want.Value && !labels {
			return nil, nil
		}
		c.Op, c.Precondition = OpUpdate, d.Observed.Version
		if got.Value != want.Value {
			c.Reasons = append(c.Reasons, fmt.Sprintf("value %q -> %q", got.Value, want.Value))
			c.Requests = append(c.Requests, kit.Request(putItem, map[string][]string{"value": {want.Value}, "version": {d.Observed.Version}}))
		}
		if labels {
			c.Reasons = append(c.Reasons, fmt.Sprintf("labels %v -> %v", got.Labels, want.Labels))
		}
	}
	if labels {
		c.Requests = append(c.Requests, kit.Request(putLabels, map[string][]string{"labels": want.Labels}))
	}
	return []protocol.Change{c}, nil
}

// Done: the item already is what the request makes it. An item of someone else's is refused.
func (items) Done(c *kit.Client, ch protocol.Change, a protocol.Approved) (bool, error) {
	all, err := list(c)
	if err != nil {
		return false, err
	}
	it, exists := all[ch.IDs["item"]]
	deleting := a.Method == deleteItem.Method && a.Path == deleteItem.Path
	switch {
	case !exists:
		return deleting, nil
	case it.Owner != owner:
		if deleting {
			return true, nil // not Mamela's item any more: nothing of Mamela's to delete
		}
		return false, fmt.Errorf("item %s exists and Mamela did not create it", it.Name)
	}
	switch {
	case deleting:
		return false, nil
	case a.Path == putItem.Path:
		return slices.Equal(a.Params["value"], []string{it.Value}), nil
	case a.Path == putLabels.Path:
		return slices.Equal(slices.Sorted(slices.Values(a.Params["labels"])), it.Labels), nil
	}
	return false, nil
}

// Prepare: delete-item, of an item Mamela owns, based on its version.
func (items) Prepare(op protocol.Op, o protocol.Observed, _ *protocol.Resource) (protocol.Change, error) {
	if op != OpDelete || !o.Owned {
		return protocol.Change{}, fmt.Errorf("%s: only an existing item Mamela owns can be deleted", o.ID)
	}
	return protocol.Change{Resource: o.ID, Kind: Kind, Op: OpDelete, IDs: o.IDs,
		Reasons:  []string{fmt.Sprintf("delete item %s (version %s): its value is lost", o.Name, o.Version)},
		Requests: []protocol.Approved{kit.Request(deleteItem, nil)}}, nil
}

// GuardSettings are the guard's settings ([provider.kv-example.guard]).
type GuardSettings struct {
	MaxItems int `json:"max_items"` // the most items the API may hold after an apply
}

// baseline is the items of others before an apply: name -> version.
type baseline struct {
	Others map[string]int `json:"others"`
}

// guard: before, the items after the creates stay within max_items; after, every item someone else
// owns is as it was.
func guard(c *kit.Client, p protocol.GuardParams) (protocol.GuardResult, error) {
	var s GuardSettings
	if err := json.Unmarshal(p.Settings, &s, json.RejectUnknownMembers(true)); err != nil || s.MaxItems < 1 {
		return protocol.GuardResult{}, fmt.Errorf("[provider.kv-example.guard] needs max_items (1 or more): %v", err)
	}
	all, err := list(c)
	if err != nil {
		return protocol.GuardResult{}, err
	}
	var out protocol.GuardResult
	add := func(code, severity, format string, args ...any) {
		out.Findings = append(out.Findings, protocol.Finding{Code: code, Severity: severity, Message: fmt.Sprintf(format, args...)})
	}
	switch p.Stage {
	case protocol.GuardBefore:
		creates := 0
		for _, ch := range p.Changes {
			if ch.Op == OpCreate {
				creates++
			}
		}
		if n := len(all) + creates; n > s.MaxItems {
			add("KV-0001", protocol.SeverityBlock, "%d items and %d creates would exceed max_items %d", len(all), creates, s.MaxItems)
		} else {
			add("KV-0001", protocol.SeverityOK, "%d items after the changes; max_items is %d", n, s.MaxItems)
		}
		b := baseline{Others: map[string]int{}}
		for name, it := range all {
			if it.Owner != owner {
				b.Others[name] = it.Version
			}
		}
		out.Baseline, _ = json.Marshal(b, json.Deterministic(true))
	case protocol.GuardAfter:
		var b baseline
		if err := json.Unmarshal(p.Baseline, &b, json.RejectUnknownMembers(true)); err != nil {
			return out, fmt.Errorf("baseline: %w", err)
		}
		changed := 0
		for _, name := range slices.Sorted(maps.Keys(b.Others)) {
			if it, ok := all[name]; !ok || it.Version != b.Others[name] {
				add("KV-0101", protocol.SeverityBlock, "item %s of someone else changed or disappeared", name)
				changed++
			}
		}
		if changed == 0 {
			add("KV-0100", protocol.SeverityOK, "the %d items of others are unchanged", len(b.Others))
		}
	default:
		return out, fmt.Errorf("guard stage %q", p.Stage)
	}
	return out, nil
}

func list(c *kit.Client) (map[string]item, error) {
	var all []item
	if err := c.ReadJSON(listItems, nil, nil, &all); err != nil {
		return nil, fmt.Errorf("listing items: %w", err)
	}
	out := map[string]item{}
	for _, it := range all {
		out[it.Name] = it
	}
	return out, nil
}
