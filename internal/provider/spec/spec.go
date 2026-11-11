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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// The contract's types are in the public package provider/protocol, so that providers outside
// this module can use them; these aliases keep Mamela's own code short.
const Schema = protocol.Schema

type (
	Manifest    = protocol.Manifest
	Kind        = protocol.Kind
	Support     = protocol.Support
	Operation   = protocol.Operation
	Need        = protocol.Need
	Grant       = protocol.Grant
	Condition   = protocol.Condition
	Test        = protocol.Test
	Target      = protocol.Target
	Requirement = protocol.Requirement
)

const (
	Supported   = protocol.Supported
	Unsupported = protocol.Unsupported
	Once        = protocol.Once
	Standing    = protocol.Standing
	Equals      = protocol.Equals
	In          = protocol.In
	Before      = protocol.Before
	Contains    = protocol.Contains
	Like        = protocol.Like
	NotPresent  = protocol.NotPresent
)

// Providers is the set of providers in use. Each kind belongs to exactly one.
type Providers []Manifest

// Digest identifies the contracts: every manifest, in a canonical form. A plan records it, so that
// a change of any provider's contract (a new version, other operations or capabilities) voids
// the plan's approval. External providers will add their executable's digest and protocol
// version to their manifest (ADR 0002).
func (ps Providers) Digest() string {
	sorted := slices.Clone(ps)
	slices.SortFunc(sorted, func(a, b Manifest) int { return strings.Compare(a.Provider, b.Provider) })
	data, err := json.Marshal(sorted, json.Deterministic(true))
	if err != nil {
		panic("spec: encoding manifests: " + err.Error())
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// External identifies a provider that runs as a separate program: its contract, its program and
// the protocol it speaks. A plan made with one program does not apply with another.
type External struct {
	Manifest Manifest `json:"manifest"`
	Program  string   `json:"program"` // "sha256:..." of the program
	Protocol string   `json:"protocol"`
	// Endpoint identifies the API the provider's requests go to: its URL, the credential (a
	// digest of it) and the catalog. Approving a plan approves changes to that API only.
	Endpoint string `json:"endpoint,omitempty"`
}

// DigestWith identifies the built-in providers' contracts and the external providers. Without
// external providers it is Digest.
func (ps Providers) DigestWith(external []External) string {
	if len(external) == 0 {
		return ps.Digest()
	}
	ext := slices.Clone(external)
	slices.SortFunc(ext, func(a, b External) int { return strings.Compare(a.Manifest.Provider, b.Manifest.Provider) })
	data, err := json.Marshal(struct {
		Builtin  string     `json:"builtin"`
		External []External `json:"external"`
	}{ps.Digest(), ext}, json.Deterministic(true))
	if err != nil {
		panic("spec: encoding providers: " + err.Error())
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

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

// fields are the resource values that templates may use.
func fields(r graph.Resource) map[string]string {
	f := map[string]string{"name": r.Name, "id": r.ID()}
	switch {
	case r.NodePackage != nil:
		f["node"], f["package"] = r.NodePackage.Node, r.NodePackage.Package
	case r.NodeService != nil:
		f["node"], f["service"] = r.NodeService.Node, r.NodeService.Service
	case r.NodeK3s != nil:
		f["node"], f["version"], f["sha256"] = r.NodeK3s.Node, r.NodeK3s.Version, r.NodeK3s.SHA256
	case r.Provided != nil:
		// A provider's templates name the spec's top-level strings and integers ("{pool}").
		var spec map[string]jsontext.Value
		if json.Unmarshal(r.Provided.Spec, &spec) == nil {
			for k, v := range spec {
				switch v.Kind() {
				case '"':
					var str string
					if json.Unmarshal(v, &str) == nil {
						f[k] = str
					}
				case '0':
					f[k] = string(v)
				}
			}
		}
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
	grant := n.Grant
	if grant == "" {
		grant = Once
	}
	q := Requirement{Provider: provider, Support: Supported, Capability: n.Capability, TargetType: n.Target.Type,
		Object: expand(n.Object, f), Always: n.Always, Grant: grant, Note: n.Note, Reasons: n.Reasons}
	q.StandingIf, q.Conditions = n.StandingIf, n.Conditions
	facts := map[string][]string{}
	for k, v := range f {
		facts[k] = []string{v}
	}
	q.Fill(facts)
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

// Observation is what observing the world needs: every provider in use observes on every plan
// and apply, whatever the declaration contains (it also looks for resources that are no longer
// declared, which Mamela keeps), so this does not depend on the graph.
func (ps Providers) Observation(res Resolver) []Requirement {
	var out []Requirement
	for _, m := range ps {
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
