package kit

import "github.com/ieyoukan/mamela/provider/protocol"

// Op is a supported operation that sends these calls and needs these capabilities.
func Op(op protocol.Op, needs []protocol.Need, calls ...protocol.Call) protocol.Operation {
	return protocol.Operation{Op: op, Support: protocol.Supported, Needs: needs, Calls: calls}
}

// ExplicitOp is an explicit operation (ADR 0006): never planned, carried out only when a person
// asks (mamela act). Kinds that have one implement Explicit.
func ExplicitOp(op protocol.Op, needs []protocol.Need, calls ...protocol.Call) protocol.Operation {
	o := Op(op, needs, calls...)
	o.Explicit = true
	return o
}

// AdoptOp is the explicit operation that takes over an object something else made, once it is
// recognized as the declared one (Object.Adoptable): Mamela then records it as its own.
func AdoptOp(op protocol.Op, needs []protocol.Need, calls ...protocol.Call) protocol.Operation {
	o := ExplicitOp(op, needs, calls...)
	o.Adopts = true
	return o
}

// RemoveOp is the explicit operation that deletes an object (a kind has one). Mamela plans it, with
// an approval made on the Control Plane's host, for an object it made for a declaration that no
// longer has the resource (ADR 0013); a kind without one is never deleted by removing it from a
// declaration. It remains an explicit operation: a person may also ask for it (mamela act).
func RemoveOp(op protocol.Op, needs []protocol.Need, calls ...protocol.Call) protocol.Operation {
	o := ExplicitOp(op, needs, calls...)
	o.Removes = true
	return o
}

// Unsupported is a difference the provider never carries out: Mamela shows it, with why, and
// denies it.
func Unsupported(op protocol.Op, why string) protocol.Operation {
	return protocol.Operation{Op: op, Support: protocol.Unsupported, Why: why}
}

// Request is an approved request of a call, with its parameters (fixed ones included).
func Request(call protocol.Call, params map[string][]string) protocol.Approved {
	p := map[string][]string{}
	for k, v := range call.Fixed {
		p[k] = []string{v}
	}
	for k, v := range params {
		p[k] = v
	}
	return protocol.Approved{API: call.API, Method: call.Method, Path: call.Path, Params: p}
}
