package policy

import "testing"

// An extension's operation needs an approval or is denied (never Auto), and takes neither an
// operation nor a code that is another's.
func TestRegisterOpRefuses(t *testing.T) {
	ok := Decision{ApprovalRequired, "MAM-POL-0900", "does something", ""}
	for name, f := range map[string]func(){
		"Auto":             func() { RegisterOp("x", "x-op", Decision{Auto, "MAM-POL-0901", "runs", ""}) },
		"no verdict":       func() { RegisterOp("x", "x-op", Decision{"", "MAM-POL-0902", "runs", ""}) },
		"a core operation": func() { RegisterOp("x", OpStartService, ok) },
		"a core code":      func() { RegisterOp("x", "x-op", Decision{ApprovalRequired, "MAM-POL-0204", "x", ""}) },
		"another's op":     func() { RegisterOp("x", Op("install-k3s"), ok) },
		"another's code":   func() { RegisterOp("x", "x-op", Decision{Denied, "MAM-POL-0210", "x", ""}) },
		"no owner":         func() { RegisterOp("", "x-op", ok) },
		"no code":          func() { RegisterOp("x", "x-op", Decision{ApprovalRequired, "", "x", ""}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: registered", name)
				}
			}()
			f()
		}()
	}
	if d := Default.Decide(Subject{Op: "x-op"}); d.Code != "MAM-POL-0399" {
		t.Errorf("a refused registration is known: %+v", d)
	}
	for _, op := range []Op{Op("install-k3s"), Op("remove-k3s-node"), OpUpdateMamelet} {
		if !contains(Ops(), op) {
			t.Errorf("Ops lacks %s", op)
		}
	}
}

func contains(ops []Op, op Op) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}
