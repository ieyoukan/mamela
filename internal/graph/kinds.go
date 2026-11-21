package graph

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The kinds of node extensions (k3s, docs/design/node-extensions.md) are registered: the core
// decodes, checks, orders and encodes them through their KindDef, and keeps their spec, which it
// does not look into, in Resource.Ext. Extensions are compiled in and register in init, before any
// graph is decoded.

// KindDef is a kind of a node extension: a resource on a node, whose spec the extension defines.
type KindDef struct {
	Kind Kind
	// Order is the kind's place in the canonical order. It is part of every graph's digest, so a
	// kind keeps its order for good, and no two kinds share one.
	Order int
	// Decode reads the spec (d.Spec) strictly and returns it, or nil with d's problems.
	Decode func(d *SpecDecoder) any
	// Validate checks the meaning of one resource of the kind (its spec is r.Ext).
	Validate func(v *Validator, r Resource)
	// Canonical is the spec as the canonical encoding writes it: a struct whose members are
	// declared in the byte order of their JSON names.
	Canonical func(spec any) any
	// Node is the machine the resource lives on.
	Node func(spec any) string
	// Fields are the values a provider's template may use ("{version}"), besides name, id and node.
	Fields func(spec any) map[string]string
	// Describe is the resource in a few words, for a person (mamela eval).
	Describe func(spec any) string
	// New is an empty spec (a pointer to a struct with a Node member): tests that cover every kind
	// fill it (spectest).
	New func() any
}

// Extension is what a node extension adds to declarations.
type Extension struct {
	Name  string
	Kinds []KindDef
	// Check checks what concerns more than one resource, once every resource has been validated.
	Check func(g *Graph, v *Validator)
	// Warnings are the warnings of the graph that are the extension's, in the canonical order of
	// the resources they are about.
	Warnings func(g *Graph) []Warning
	// WarningCodes are the codes of those warnings: each is the extension's alone.
	WarningCodes []Code
}

// coreOrder is the canonical order of the core's kinds. The kinds of extensions fill the places
// between them (NodeK3s is 3 and NodeK3sManifest 4, as they were before extensions).
var coreOrder = map[Kind]int{KindNodePackage: 1, KindNodeService: 2, KindNodeMamelet: 5}

// providedOrder is where the kinds of providers come in the canonical order: they sort among
// themselves and with the kinds of order 5 (NodeMamelet) by name. It is fixed: it was the number
// of built-in kinds when there were 5, and every graph's digest depends on it.
const providedOrder = 5

var (
	extKinds       = map[Kind]KindDef{}
	extensions     []Extension
	warningOwners  = map[Code]string{}
	extensionNames = map[string]bool{}
)

// Register adds a node extension's kinds. It panics when a kind, an order or a warning code is
// taken already: a programming error, found when the program starts.
func Register(e Extension) {
	if e.Name == "" || e.Name == "core" || extensionNames[e.Name] {
		panic(fmt.Sprintf("graph: extension %q: a name of its own is needed", e.Name))
	}
	orders := map[int]Kind{}
	for k, o := range coreOrder {
		orders[o] = k
	}
	for _, d := range extKinds {
		orders[d.Order] = d.Kind
	}
	for _, d := range e.Kinds {
		_, taken := extKinds[d.Kind]
		switch {
		case CoreKind(d.Kind) || taken:
			panic(fmt.Sprintf("graph: extension %s: kind %s is taken", e.Name, d.Kind))
		case orders[d.Order] != "" || d.Order <= 0:
			panic(fmt.Sprintf("graph: extension %s: kind %s: order %d is taken", e.Name, d.Kind, d.Order))
		case d.Decode == nil || d.Validate == nil || d.Canonical == nil || d.Node == nil || d.New == nil:
			panic(fmt.Sprintf("graph: extension %s: kind %s needs Decode, Validate, Canonical, Node and New", e.Name, d.Kind))
		}
		orders[d.Order] = d.Kind
	}
	for _, c := range e.WarningCodes {
		if owner, taken := warningOwners[c]; taken {
			panic(fmt.Sprintf("graph: extension %s: warning code %s is %s's", e.Name, c, owner))
		}
	}
	for _, c := range e.WarningCodes {
		warningOwners[c] = e.Name
	}
	for _, d := range e.Kinds {
		extKinds[d.Kind] = d
	}
	extensionNames[e.Name] = true
	extensions = append(extensions, e)
}

// KindOf is the definition of a node extension's kind.
func KindOf(k Kind) (KindDef, bool) {
	d, ok := extKinds[k]
	return d, ok
}

// ExtKinds are the node extensions' kinds, in canonical order.
func ExtKinds() []KindDef {
	out := slices.Collect(maps.Values(extKinds))
	slices.SortFunc(out, func(a, b KindDef) int { return a.Order - b.Order })
	return out
}

// Order is a kind's place in the canonical order.
func Order(k Kind) int {
	if o, ok := coreOrder[k]; ok {
		return o
	}
	if d, ok := extKinds[k]; ok {
		return d.Order
	}
	return providedOrder
}

// knownKinds names the core's and the extensions' kinds, in canonical order, for a person.
func knownKinds() string {
	ks := slices.Collect(maps.Keys(coreOrder))
	for k := range extKinds {
		ks = append(ks, k)
	}
	slices.SortFunc(ks, func(a, b Kind) int { return Order(a) - Order(b) })
	names := make([]string, len(ks))
	for i, k := range ks {
		names[i] = string(k)
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// SpecDecoder reads the spec of one resource of an extension's kind, and collects what is wrong
// with it, as the core's own kinds are read.
type SpecDecoder struct {
	Spec     jsontext.Value
	kind     Kind
	where    string
	problems []Problem
	files    Files             // the Pome's files (DecodeWith); nil: none
	sources  map[string]string // the texts read from them (Text), by member
}

// Strict decodes the spec into v (a struct of pointer members, to tell a missing member from a
// zero value), refusing unknown members. On failure the problem is recorded and it returns false.
func (d *SpecDecoder) Strict(v any) bool {
	if err := json.Unmarshal(d.Spec, v, strict); err != nil {
		d.problems = append(d.problems, problemsOf(fromJSONError(err, d.where+" spec"))...)
		return false
	}
	return true
}

// Need records a member that is missing.
func (d *SpecDecoder) Need(present bool, field string) {
	if !present {
		d.problems = append(d.problems, Problem{Code: CodeMissingField, Path: d.where + " spec." + field,
			Message: fmt.Sprintf("%s has no spec.%s", d.kind, field)})
	}
}

// OptionalString is an optional string member: nil when it is absent (null is refused).
func (d *SpecDecoder) OptionalString(v jsontext.Value, field string) *string {
	s, ps := optionalString(v, d.where, field)
	d.problems = append(d.problems, ps...)
	return s
}

// Invalid records a member whose value is wrong.
func (d *SpecDecoder) Invalid(code Code, field, message, hint string) {
	d.problems = append(d.problems, Problem{Code: code, Path: d.where + " spec." + field, Message: message, Hint: hint})
}

// OK is whether nothing is wrong so far.
func (d *SpecDecoder) OK() bool { return len(d.problems) == 0 }

// Validator collects the problems of a graph's meaning.
type Validator struct {
	g     *Graph
	nodes map[string]bool
	add   addFunc
	memo  map[string]any
}

// Memo is f(), computed once per validation under key: what an extension works out of the whole
// graph (its servers) is not worked out again for each resource.
func (v *Validator) Memo(key string, f func() any) any {
	if v.memo == nil {
		v.memo = map[string]any{}
	}
	if m, ok := v.memo[key]; ok {
		return m
	}
	m := f()
	v.memo[key] = m
	return m
}

// Graph is the graph being validated.
func (v *Validator) Graph() *Graph { return v.g }

// Problem records a problem of resource r (field: "spec.version", or "" for the resource).
func (v *Validator) Problem(r Resource, code Code, field, message, hint string) {
	v.add(r, code, field, message, hint)
}

// NodeRef checks that node is a machine of the graph (a provider's resource).
func (v *Validator) NodeRef(r Resource, node string) { checkNodeRef(v.add, r, node, v.nodes) }

// UnsafeTextHint is the advice for text that does not show as itself.
const UnsafeTextHint = unsafeTextHint

// extWarnings are the extensions' warnings, in the canonical order of their resources (stable: for
// one resource, in the order each extension gives them).
func (g *Graph) extWarnings() []Warning {
	var out []Warning
	for _, e := range extensions {
		if e.Warnings != nil {
			out = append(out, e.Warnings(g)...)
		}
	}
	at := map[string]int{}
	for i, r := range g.Sorted() {
		if _, ok := at[r.ID()]; !ok {
			at[r.ID()] = i
		}
	}
	slices.SortStableFunc(out, func(a, b Warning) int { return at[a.Resource] - at[b.Resource] })
	return out
}
