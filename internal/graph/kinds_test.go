package graph

import "testing"

// A node extension's kind takes no kind, place in the canonical order or warning code that is
// taken: the order and the codes are in every digest and plan.
func TestRegisterRefusesWhatIsTaken(t *testing.T) {
	def := func(k Kind, order int) KindDef {
		return KindDef{Kind: k, Order: order, Decode: func(*SpecDecoder) any { return nil }, Validate: func(*Validator, Resource) {},
			Canonical: func(any) any { return nil }, Node: func(any) string { return "" }, New: func() any { return nil }}
	}
	for name, e := range map[string]Extension{
		"a core kind":       {Name: "x", Kinds: []KindDef{def(KindNodePackage, 9)}},
		"a registered kind": {Name: "x", Kinds: []KindDef{def(Kind("NodeK3s"), 9)}},
		"a core order":      {Name: "x", Kinds: []KindDef{def("NodeX", 5)}},
		"another's order":   {Name: "x", Kinds: []KindDef{def("NodeX", 3)}},
		"no order":          {Name: "x", Kinds: []KindDef{def("NodeX", 0)}},
		"two of one order":  {Name: "x", Kinds: []KindDef{def("NodeX", 9), def("NodeY", 9)}},
		"another's warning": {Name: "x", WarningCodes: []Code{Code("MAM-GRAPH-0201")}},
		"a registered name": {Name: "k3s"},
		"no name":           {},
		"no decode":         {Name: "x", Kinds: []KindDef{{Kind: "NodeX", Order: 9}}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: registered", name)
				}
			}()
			Register(e)
		}()
	}
	if _, ok := KindOf("NodeX"); ok {
		t.Error("a refused kind is known")
	}
	if Order(Kind("NodeK3s")) != 3 || Order(Kind("NodeK3sManifest")) != 4 || Order("KVItem") != Order(KindNodeMamelet) {
		t.Error("the canonical order of the kinds changed")
	}
}
