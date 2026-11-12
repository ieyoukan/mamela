// Package kvprovider is an example Mamela provider: it manages items of the example key-value API
// (kvapi). Read it as a template for a provider; cmd/kv-provider builds it for WASI.
//
// Kind KVItem, spec {"value": "...", "labels": ["..."]}. It creates and updates items it owns
// (owner "mamela") and never touches an item someone else created. Removing a declaration never
// deletes an item: deleting one is an explicit operation (delete-item), which a person asks for by
// name (mamela act). Its guard (ADR 0006) limits how many items there may be and checks that the
// items of others are unchanged by an apply.
package kvprovider

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
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
	getItem    = protocol.Call{Method: "GET", Path: "/items/{item}"}
	putItem    = protocol.Call{Method: "PUT", Path: "/items/{item}", Form: []string{"value", "owner", "version"}}
	putLabels  = protocol.Call{Method: "PUT", Path: "/items/{item}/labels", Form: []string{"labels"}}
	listItems  = protocol.Call{Method: "GET", Path: "/items"}
	deleteItem = protocol.Call{Method: "DELETE", Path: "/items/{item}"}
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
			{Op: OpDelete, Support: protocol.Supported, Explicit: true, Needs: []protocol.Need{write}, Calls: []protocol.Call{getItem, deleteItem}},
		}}},
		Guard: &protocol.Guard{Calls: []protocol.Call{listItems}},
	}
}

// GuardSettings are the guard's settings ([provider.kv-example.guard]).
type GuardSettings struct {
	MaxItems int `json:"max_items"` // the most items the API may hold after an apply
}

// baseline is the items of others before an apply: name -> version.
type baseline struct {
	Others map[string]int `json:"others"`
}

// Guard implements sdk.Guarder: before, the items after the creates stay within max_items; after,
// every item someone else owns is as it was.
func (Provider) Guard(ctx context.Context, h *sdk.Host, p protocol.GuardParams) (protocol.GuardResult, error) {
	var s GuardSettings
	if err := json.Unmarshal(p.Settings, &s, json.RejectUnknownMembers(true)); err != nil || s.MaxItems < 1 {
		return protocol.GuardResult{}, fmt.Errorf("[provider.kv-example.guard] needs max_items (1 or more): %v", err)
	}
	items, err := list(ctx, h)
	if err != nil {
		return protocol.GuardResult{}, err
	}
	var out protocol.GuardResult
	switch p.Stage {
	case protocol.GuardBefore:
		creates := 0
		for _, c := range p.Changes {
			if c.Op == OpCreate {
				creates++
			}
		}
		if n := len(items) + creates; n > s.MaxItems {
			out.Findings = append(out.Findings, protocol.Finding{Code: "KV-0001", Severity: protocol.SeverityBlock,
				Message: fmt.Sprintf("%d items and %d creates would exceed max_items %d", len(items), creates, s.MaxItems)})
		} else {
			out.Findings = append(out.Findings, protocol.Finding{Code: "KV-0001", Severity: protocol.SeverityOK,
				Message: fmt.Sprintf("%d items after the changes; max_items is %d", n, s.MaxItems)})
		}
		b := baseline{Others: map[string]int{}}
		for name, it := range items {
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
			if it, ok := items[name]; !ok || it.Version != b.Others[name] {
				out.Findings = append(out.Findings, protocol.Finding{Code: "KV-0101", Severity: protocol.SeverityBlock,
					Message: fmt.Sprintf("item %s of someone else changed or disappeared", name)})
				changed++
			}
		}
		if changed == 0 {
			out.Findings = append(out.Findings, protocol.Finding{Code: "KV-0100", Severity: protocol.SeverityOK,
				Message: fmt.Sprintf("the %d items of others are unchanged", len(b.Others))})
		}
	default:
		return out, fmt.Errorf("guard stage %q", p.Stage)
	}
	return out, nil
}

// Prepare implements sdk.Preparer: delete-item, of an item Mamela owns, based on its version.
func (Provider) Prepare(ctx context.Context, p protocol.PrepareParams) (protocol.PrepareResult, error) {
	o := p.Observed
	if p.Op != OpDelete || !o.Exists || !o.Owned {
		return protocol.PrepareResult{}, fmt.Errorf("%s: only an existing item Mamela owns can be deleted", o.ID)
	}
	return protocol.PrepareResult{Change: protocol.Change{Resource: o.ID, Kind: Kind, Op: OpDelete, IDs: o.IDs,
		Reasons:  []string{fmt.Sprintf("delete item %s (version %s): its value is lost", o.Name, o.Version)},
		Requests: []protocol.Approved{{Method: deleteItem.Method, Path: deleteItem.Path}}}}, nil
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
	// Version "0" is the version of an item that does not exist: a create is based on it.
	o := protocol.Observed{ID: id, Kind: Kind, Name: name, Exists: exists, IDs: map[string]string{"item": name}, Version: "0"}
	if exists {
		o.Owned = it.Owner == owner
		o.Version = strconv.Itoa(it.Version)
		o.State, _ = json.Marshal(Spec{Value: it.Value, Labels: it.Labels}, json.Deterministic(true))
	}
	return o
}

// Diff implements sdk.Provider. Only what is declared and differs; nothing for what is not
// declared. Each change lists exactly the requests it sends: that is what is approved.
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
		labels := len(want.Labels) > 0 // no labels declared: labels are not managed
		switch {
		case !o.Exists:
			c.Op, c.Reasons, c.Precondition = OpCreate, []string{"the item does not exist"}, o.Version
			c.Requests = append(c.Requests, protocol.Approved{Method: putItem.Method, Path: putItem.Path,
				Params: map[string][]string{"value": {want.Value}, "owner": {owner}, "version": {o.Version}}})
		case !o.Owned:
			return out, fmt.Errorf("%s: the item exists and Mamela did not create it; Mamela does not take it over", r.ID)
		default:
			got, err := spec(o.State)
			if err != nil {
				return out, fmt.Errorf("%s: observed state: %w", r.ID, err)
			}
			labels = labels && !slices.Equal(got.Labels, want.Labels)
			if got.Value == want.Value && !labels {
				continue
			}
			c.Op, c.Precondition = OpUpdate, o.Version
			if got.Value != want.Value {
				c.Reasons = append(c.Reasons, fmt.Sprintf("value %q -> %q", got.Value, want.Value))
				c.Requests = append(c.Requests, protocol.Approved{Method: putItem.Method, Path: putItem.Path,
					Params: map[string][]string{"value": {want.Value}, "version": {o.Version}}})
			}
			if labels {
				c.Reasons = append(c.Reasons, fmt.Sprintf("labels %v -> %v", got.Labels, want.Labels))
			}
		}
		if labels {
			c.Requests = append(c.Requests, protocol.Approved{Method: putLabels.Method, Path: putLabels.Path,
				Params: map[string][]string{"labels": want.Labels}})
		}
		out.Changes = append(out.Changes, c)
	}
	return out, nil
}

// Apply implements sdk.Provider. It sends the approved requests as they are, in order, and skips
// those already in effect, so that an apply after a partial one resumes it.
func (Provider) Apply(ctx context.Context, h *sdk.Host, p protocol.ApplyParams) (protocol.ApplyResult, error) {
	name := p.Change.IDs["item"]
	it, exists, err := get(ctx, h, name)
	if err != nil {
		return protocol.ApplyResult{}, err
	}
	if exists && it.Owner != owner {
		return protocol.ApplyResult{}, fmt.Errorf("item %s exists and Mamela did not create it", name)
	}
	if p.Change.Op == OpDelete {
		if !exists {
			return protocol.ApplyResult{Message: "item " + name + " is gone"}, nil
		}
		resp, err := h.Request(ctx, protocol.HostRequest{Method: "DELETE", Path: "/items/" + name})
		if err != nil {
			return protocol.ApplyResult{}, err
		}
		if resp.Status != 200 {
			return protocol.ApplyResult{}, fmt.Errorf("DELETE /items/%s: status %d: %s", name, resp.Status, resp.Body)
		}
		return protocol.ApplyResult{Message: "item " + name + " deleted"}, nil
	}
	for _, a := range p.Change.Requests {
		if exists && inEffect(it, a) {
			continue
		}
		path := "/items/" + name
		if a.Path == putLabels.Path {
			path += "/labels"
		}
		if it, err = put(ctx, h, path, a.Params); err != nil {
			return protocol.ApplyResult{}, err // a refused, failed or uncertain request ends the apply
		}
		exists = true
	}
	return protocol.ApplyResult{Message: "item " + name + " is as declared"}, nil
}

// inEffect reports whether the item already is what the approved request would make it.
func inEffect(it item, a protocol.Approved) bool {
	switch a.Path {
	case putItem.Path:
		return slices.Equal(a.Params["value"], []string{it.Value})
	case putLabels.Path:
		return slices.Equal(slices.Sorted(slices.Values(a.Params["labels"])), it.Labels)
	}
	return false
}

// Lookup implements sdk.Provider: how many of the approved requests are in effect.
func (Provider) Lookup(ctx context.Context, h *sdk.Host, p protocol.LookupParams) (protocol.LookupResult, error) {
	it, exists, err := get(ctx, h, p.Change.IDs["item"])
	if err != nil {
		return protocol.LookupResult{}, err
	}
	if p.Change.Op == OpDelete {
		if !exists || it.Owner != owner {
			return protocol.LookupResult{Status: protocol.LookupDone}, nil
		}
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	done := 0
	for _, a := range p.Change.Requests {
		if exists && it.Owner == owner && inEffect(it, a) {
			done++
		}
	}
	switch {
	case done == len(p.Change.Requests):
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
