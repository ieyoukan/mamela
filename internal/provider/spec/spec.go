// Package spec is the contract between Mamela and its providers, as data.
//
// A provider declares itself in a Manifest: which resource kinds it handles, which operations it
// carries out on them (or why not), and for each operation the capabilities it needs: a
// provider-native permission ("VM.Allocate" for Proxmox, "apt.install" for mamelet,
// "ec2:RunInstances" for AWS) on a target, under a condition. Mamela analyses a declaration
// against the manifests into Requirements: this resource, through this operation, needs this
// capability on this target. The CLI renders them, `mamela check --json` gives them to editors
// and tools, and the lab derives least-privilege grants from them. Nothing here is prose to be
// parsed: messages are rendered from the fields.
//
// The manifest is also enforced: a provider's operation may only use what it declares (for
// Proxmox, the client refuses undeclared calls; see proxmox.Restrict). See
// docs/adr/0002-provider-contract.md for the protocol external providers will speak.
package spec

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
)

// Schema names this version of the manifest and requirement format.
const Schema = "mamela-provider/v1"

// Manifest is what a provider declares about itself.
type Manifest struct {
	Schema   string `json:"schema"`
	Provider string `json:"provider"` // a short name: "proxmox", "mamelet"
	Version  string `json:"version"`
	// Observe is what every observation needs (each plan and each apply observes).
	Observe []Need `json:"observe,omitempty"`
	Kinds   []Kind `json:"kinds"`
}

// Kind is one resource kind a provider handles.
type Kind struct {
	Kind       graph.Kind  `json:"kind"`
	Operations []Operation `json:"operations"`
}

// Support says whether a provider carries out an operation.
type Support string

const (
	Supported   Support = "supported"
	Unsupported Support = "unsupported" // never carried out; Why says why
)

// Operation is one operation on a kind.
type Operation struct {
	Op      policy.Op `json:"op"`
	Support Support   `json:"support"`
	Why     string    `json:"why,omitempty"` // for unsupported operations
	Needs   []Need    `json:"needs,omitempty"`
}

// Need is one capability an operation needs.
type Need struct {
	// Capability is the provider-native permission.
	Capability string `json:"capability"`
	// Target is where the capability must be held. Type is provider-defined ("proxmox-acl",
	// "node"); Ref names it: a site setting ("pool", "disk-storage") that a Resolver turns into
	// the real name, or a template over the resource's fields ("{node}").
	Target Target `json:"target"`
	// Object is what the capability is used on, as a template over the resource ("{package}"),
	// when that is narrower than the target.
	Object string `json:"object,omitempty"`
	// Always is false when the operation needs it only in some cases (Reasons say which).
	Always bool `json:"always"`
	// Standing is set when the capability can be granted for good rather than for one approved
	// operation, and says on what condition.
	Standing string `json:"standing,omitempty"`
	// Limit narrows what the capability may do ("only the approved apt change set").
	Limit   string   `json:"limit,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
}

// Target is where a capability is held.
type Target struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

// Check verifies a manifest's own consistency.
func (m Manifest) Check() error {
	if m.Schema != Schema || m.Provider == "" || m.Version == "" {
		return fmt.Errorf("manifest %q: schema, provider and version are required", m.Provider)
	}
	for _, k := range m.Kinds {
		seen := map[policy.Op]bool{}
		for _, o := range k.Operations {
			if seen[o.Op] {
				return fmt.Errorf("manifest %s: %s %s is declared twice", m.Provider, k.Kind, o.Op)
			}
			seen[o.Op] = true
			switch o.Support {
			case Supported:
				if o.Why != "" {
					return fmt.Errorf("manifest %s: %s %s is supported but says why not", m.Provider, k.Kind, o.Op)
				}
			case Unsupported:
				if o.Why == "" || len(o.Needs) > 0 {
					return fmt.Errorf("manifest %s: unsupported %s %s needs a reason and no capabilities", m.Provider, k.Kind, o.Op)
				}
			default:
				return fmt.Errorf("manifest %s: %s %s: unknown support %q", m.Provider, k.Kind, o.Op, o.Support)
			}
			for _, n := range o.Needs {
				if n.Capability == "" || n.Target.Type == "" || n.Target.Ref == "" {
					return fmt.Errorf("manifest %s: %s %s: a need without capability or target", m.Provider, k.Kind, o.Op)
				}
			}
		}
	}
	return nil
}

// Operation returns the declaration of an operation on a kind.
func (m Manifest) Operation(kind graph.Kind, op policy.Op) (Operation, bool) {
	for _, k := range m.Kinds {
		if k.Kind == kind {
			for _, o := range k.Operations {
				if o.Op == op {
					return o, true
				}
			}
		}
	}
	return Operation{}, false
}

// Providers is the set of providers in use. Each kind belongs to exactly one.
type Providers []Manifest

// For returns the provider of a kind.
func (ps Providers) For(kind graph.Kind) (Manifest, bool) {
	for _, m := range ps {
		if slices.ContainsFunc(m.Kinds, func(k Kind) bool { return k.Kind == kind }) {
			return m, true
		}
	}
	return Manifest{}, false
}

// Check verifies every manifest and that no kind has two providers.
func (ps Providers) Check() error {
	owner := map[graph.Kind]string{}
	for _, m := range ps {
		if err := m.Check(); err != nil {
			return err
		}
		for _, k := range m.Kinds {
			if o, ok := owner[k.Kind]; ok {
				return fmt.Errorf("kind %s is provided by both %s and %s", k.Kind, o, m.Provider)
			}
			owner[k.Kind] = m.Provider
		}
	}
	return nil
}

// Resolver names a site-level target ("pool" -> "/pool/mamela-lab"). ok is false when it does
// not know the target; a placeholder is shown instead.
type Resolver interface {
	ResolveTarget(provider string, t Target) (string, bool)
}

// Requirement is one resolved need: this resource, through this operation, needs this capability
// on this target.
type Requirement struct {
	Resource   string    `json:"resource,omitempty"` // graph ID; empty for observation
	Op         policy.Op `json:"op,omitempty"`       // empty for observation
	Provider   string    `json:"provider"`
	Support    Support   `json:"support"`
	Why        string    `json:"why,omitempty"` // for unsupported operations
	Capability string    `json:"capability,omitempty"`
	TargetType string    `json:"targetType,omitempty"`
	Target     string    `json:"target,omitempty"`
	Resolved   bool      `json:"resolved,omitempty"` // Target is the real name, not a placeholder
	Object     string    `json:"object,omitempty"`
	Always     bool      `json:"always,omitempty"`
	Standing   string    `json:"standing,omitempty"`
	Limit      string    `json:"limit,omitempty"`
	Reasons    []string  `json:"reasons,omitempty"`
}

// fields are the resource values that templates may use.
func fields(r graph.Resource) map[string]string {
	f := map[string]string{"name": r.Name, "id": r.ID()}
	switch {
	case r.NodePackage != nil:
		f["node"], f["package"] = r.NodePackage.Node, r.NodePackage.Package
	case r.NodeService != nil:
		f["node"], f["service"] = r.NodeService.Node, r.NodeService.Service
	}
	return f
}

func expand(tmpl string, f map[string]string) string {
	if f == nil {
		return tmpl
	}
	for k, v := range f {
		tmpl = strings.ReplaceAll(tmpl, "{"+k+"}", v)
	}
	return tmpl
}

func resolve(provider string, n Need, r *graph.Resource, res Resolver) Requirement {
	var f map[string]string
	if r != nil {
		f = fields(*r)
	}
	q := Requirement{Provider: provider, Support: Supported, Capability: n.Capability, TargetType: n.Target.Type,
		Object: expand(n.Object, f), Always: n.Always, Standing: n.Standing, Limit: n.Limit, Reasons: n.Reasons}
	if strings.Contains(n.Target.Ref, "{") {
		q.Target = expand(n.Target.Ref, f)
		q.Resolved = !strings.Contains(q.Target, "{")
	} else if res != nil {
		q.Target, q.Resolved = res.ResolveTarget(provider, n.Target)
	}
	if q.Target == "" {
		q.Target, q.Resolved = "<"+n.Target.Ref+">", false
	}
	return q
}

// Observation is what observing the world needs, by provider (only providers whose kinds appear
// in the graph observe).
func (ps Providers) Observation(g *graph.Graph, res Resolver) []Requirement {
	var out []Requirement
	for _, m := range ps {
		if !slices.ContainsFunc(g.Resources, func(r graph.Resource) bool {
			return slices.ContainsFunc(m.Kinds, func(k Kind) bool { return k.Kind == r.Kind })
		}) {
			continue
		}
		for _, n := range m.Observe {
			out = append(out, resolve(m.Provider, n, nil, res))
		}
	}
	return out
}

// Analyze is what each resource of a declaration may need, whatever the world is like: for every
// resource, every operation its provider declares for its kind. A resource of a kind no provider
// handles gets one requirement saying so.
func (ps Providers) Analyze(g *graph.Graph, res Resolver) []Requirement {
	var out []Requirement
	for _, r := range g.Resources {
		m, ok := ps.For(r.Kind)
		if !ok {
			out = append(out, Requirement{Resource: r.ID(), Support: Unsupported, Why: "no provider handles " + string(r.Kind)})
			continue
		}
		for _, k := range m.Kinds {
			if k.Kind != r.Kind {
				continue
			}
			for _, o := range k.Operations {
				out = append(out, ps.opRequirements(m, r, o, res)...)
			}
		}
	}
	return out
}

// OperationRequirements is what one operation on one resource needs.
func (ps Providers) OperationRequirements(r graph.Resource, op policy.Op, res Resolver) []Requirement {
	m, ok := ps.For(r.Kind)
	if !ok {
		return []Requirement{{Resource: r.ID(), Op: op, Support: Unsupported, Why: "no provider handles " + string(r.Kind)}}
	}
	o, ok := m.Operation(r.Kind, op)
	if !ok {
		return []Requirement{{Resource: r.ID(), Op: op, Provider: m.Provider, Support: Unsupported,
			Why: fmt.Sprintf("the %s provider does not declare %s", m.Provider, op)}}
	}
	return ps.opRequirements(m, r, o, res)
}

func (ps Providers) opRequirements(m Manifest, r graph.Resource, o Operation, res Resolver) []Requirement {
	if o.Support != Supported {
		return []Requirement{{Resource: r.ID(), Op: o.Op, Provider: m.Provider, Support: o.Support, Why: o.Why}}
	}
	var out []Requirement
	for _, n := range o.Needs {
		q := resolve(m.Provider, n, &r, res)
		q.Resource, q.Op = r.ID(), o.Op
		out = append(out, q)
	}
	return out
}
