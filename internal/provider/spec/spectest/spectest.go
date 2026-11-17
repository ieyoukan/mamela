// Package spectest is for tests that must cover every built-in kind that lives on a node. It finds
// the kinds from graph.Resource itself, so a kind that is added later is covered without anyone
// remembering to add it to a list; a test that says what a kind needs (its node, its description,
// its target) then fails for the one that was left out.
package spectest

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

// Node is the node every resource of NodeResources is on.
const Node = "n"

// NodeResources returns one resource of each built-in kind that is on a node: the kinds whose spec
// is a pointer in graph.Resource (named as the kind, with a Node field), each on Node, every other
// string of the spec set to a distinct, long enough value (a sha256 is sliced for display).
func NodeResources(t testing.TB) []graph.Resource {
	t.Helper()
	var out []graph.Resource
	rt := reflect.TypeFor[graph.Resource]()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.Pointer || f.Type.Elem().Kind() != reflect.Struct || f.Name == "Provided" {
			continue
		}
		kind := graph.Kind(f.Name)
		if !graph.Builtin(kind) {
			t.Errorf("graph.Resource.%s is a spec, but %s is not a built-in kind: name the field as its kind", f.Name, kind)
			continue
		}
		spec := reflect.New(f.Type.Elem())
		for j := range spec.Elem().NumField() {
			if sf := spec.Elem().Field(j); sf.Kind() == reflect.String {
				name := f.Type.Elem().Field(j).Name
				sf.SetString(strings.ToLower(name) + "-" + strings.Repeat("0", 64))
				if name == "Node" {
					sf.SetString(Node)
				}
			}
		}
		if n := spec.Elem().FieldByName("Node"); !n.IsValid() || n.String() != Node {
			t.Errorf("graph.Resource.%s has no Node: is %s a kind that lives on a node?", f.Name, kind)
			continue
		}
		r := graph.Resource{Kind: kind, Name: Node + "/" + strings.ToLower(string(kind))}
		reflect.ValueOf(&r).Elem().Field(i).Set(spec)
		out = append(out, r)
	}
	if len(out) == 0 {
		t.Fatal("found no node kinds in graph.Resource")
	}
	return out
}
