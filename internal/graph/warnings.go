package graph

// Warning is something in a valid graph that is probably not what was meant. Validate refuses what
// cannot be right; a warning is for what can be, but usually is not. It stops and changes nothing:
// the plan shows it with the resource it is about (plan.Change.Warnings).
type Warning struct {
	Code     Code
	Resource string // the resource it is about, as its ID ("NodeK3s web/k3s")
	Message  string
	Hint     string
}

// Warnings are the warnings of the graph, in the order of its resources (canonical order), and for
// one resource in the order of the codes. They depend on the declaration alone, so the same
// declaration has the same warnings. They are the node extensions' (the core has none of its own).
func (g *Graph) Warnings() []Warning { return g.extWarnings() }
