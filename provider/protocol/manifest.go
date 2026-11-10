// Package protocol is the contract between Mamela and its providers, as data: what a provider
// declares about itself (Manifest), what Mamela derives from it (Requirement), and the messages a
// provider program and the Mamela host exchange (rpc.go, permit.go).
//
// A provider declares which resource kinds it handles, which operations it carries out on them
// (or why not), and for each operation the capabilities it needs: a provider-native permission
// ("VM.Allocate" for Proxmox, "apt.install" for mamelet, "ec2:RunInstances" for AWS) on a target,
// under typed conditions. A provider that talks to an API through the host also declares, per
// operation, the API calls it sends (Calls); the host sends nothing else.
//
// Nothing a provider declares is trusted for safety: the host checks every request against the
// operation's permit and classifies its effect from its own catalog (Catalog), never from the
// provider's word. See docs/adr/0002-provider-contract.md and docs/provider-authoring.md.
package protocol

import (
	"fmt"
	"slices"
	"strings"
)

// Schema names this version of the manifest and requirement format.
const Schema = "mamela-provider/v1"

// ResourceKind names a resource type ("VirtualMachine", "NodePackage").
type ResourceKind string

// Op names an operation on a resource ("create-vm", "install-package").
type Op string

// Manifest is what a provider declares about itself.
type Manifest struct {
	Schema   string `json:"schema"`
	Provider string `json:"provider"` // a short name: "proxmox", "mamelet"
	Version  string `json:"version"`
	// API names the API the provider sends its requests to through the host (host/request), for
	// a provider that runs as a separate program. The host holds the API's catalog and
	// credentials. Empty for providers built into Mamela.
	API string `json:"api,omitempty"`
	// Observe is what every observation needs (each plan and each apply observes).
	Observe []Need `json:"observe,omitempty"`
	// ObserveCalls are the API calls an observation sends (reads only).
	ObserveCalls []Call `json:"observeCalls,omitempty"`
	Kinds        []Kind `json:"kinds"`
}

// Kind is one resource kind a provider handles.
type Kind struct {
	Kind       ResourceKind `json:"kind"`
	Operations []Operation  `json:"operations"`
}

// Support says whether a provider carries out an operation.
type Support string

const (
	Supported   Support = "supported"
	Unsupported Support = "unsupported" // never carried out; Why says why
)

// Operation is one operation on a kind.
type Operation struct {
	Op      Op      `json:"op"`
	Support Support `json:"support"`
	Why     string  `json:"why,omitempty"` // for unsupported operations
	Needs   []Need  `json:"needs,omitempty"`
	// Calls are the API calls the operation may send through the host, reads included. The
	// host refuses any other.
	Calls []Call `json:"calls,omitempty"`
}

// Call is one API call: a method and a path template whose "{name}" segments are IDs ("/items/{item}"),
// and every parameter it may send, by where it sends it: in the query or in the form body. A
// name is in one place only. No parameters means it sends none.
type Call struct {
	Method string   `json:"method"`
	Path   string   `json:"path"`
	Query  []string `json:"query,omitempty"`
	Form   []string `json:"form,omitempty"`
}

// In says where the call sends a parameter: "query", "form", or "" when it does not declare it.
func (c Call) In(param string) string {
	switch {
	case slices.Contains(c.Query, param):
		return "query"
	case slices.Contains(c.Form, param):
		return "form"
	}
	return ""
}

// Params are all the parameters the call may send.
func (c Call) Params() []string { return append(slices.Clone(c.Query), c.Form...) }

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
	// Grant is how long the capability is given: once (for one approved operation, the default)
	// or standing (until the declaration changes), the latter only when StandingIf holds.
	Grant      Grant       `json:"grant,omitempty"`
	StandingIf []Condition `json:"standingIf,omitempty"`
	// Conditions must hold for every use of the capability. They are data, checked by whoever
	// enforces the grant (mamelet for its revision, the host for a provider's requests, IAM for
	// AWS), and rendered for people from their fields.
	Conditions []Condition `json:"conditions,omitempty"`
	// Note explains the need to people. Nothing checks it.
	Note    string   `json:"note,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
}

// Grant is how long a capability is given.
type Grant string

const (
	Once     Grant = "once"
	Standing Grant = "standing"
)

// Condition is one typed condition on a capability: Key Test Values. Keys are namespaced:
// "mamela:..." for what Mamela and mamelet check, "aws:..." / "ec2:..." for IAM condition keys.
// Values are literals or templates: "{field}" of the resource, or a plan fact ("{changeSet}")
// that is filled in when a plan fixes it.
type Condition struct {
	Key    string   `json:"key"`
	Test   Test     `json:"test"`
	Values []string `json:"values"`
}

// Test is how a condition compares (named after the IAM operators where they exist).
type Test string

const (
	Equals     Test = "equals"     // the fact is exactly the one value
	In         Test = "in"         // the fact is one of the values (each fact, for a list of facts)
	Before     Test = "before"     // the fact (an RFC 3339 time) is before the value
	Contains   Test = "contains"   // the facts contain the value
	Like       Test = "like"       // each fact matches one of the values; a trailing "*" matches any rest
	NotPresent Test = "notPresent" // there is no fact for the key
)

// Resolved reports whether every value is a literal (no template left).
func (c Condition) Resolved() bool {
	return !slices.ContainsFunc(c.Values, func(v string) bool { return strings.Contains(v, "{") })
}

// Holds checks the condition against facts (key -> values). An unresolved condition never holds.
func (c Condition) Holds(facts map[string][]string) bool {
	if !c.Resolved() {
		return false
	}
	got, present := facts[c.Key]
	switch c.Test {
	case NotPresent:
		return !present || len(got) == 0
	case Equals:
		return len(c.Values) == 1 && len(got) == 1 && got[0] == c.Values[0]
	case In:
		return present && len(got) > 0 && !slices.ContainsFunc(got, func(g string) bool { return !slices.Contains(c.Values, g) })
	case Contains:
		return len(c.Values) == 1 && slices.Contains(got, c.Values[0])
	case Like:
		return present && len(got) > 0 && !slices.ContainsFunc(got, func(g string) bool {
			return !slices.ContainsFunc(c.Values, func(p string) bool {
				if prefix, ok := strings.CutSuffix(p, "*"); ok {
					return strings.HasPrefix(g, prefix)
				}
				return g == p
			})
		})
	case Before:
		return len(c.Values) == 1 && len(got) == 1 && got[0] < c.Values[0] // RFC 3339 UTC compares as text
	}
	return false
}

// Target is where a capability is held.
type Target struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

// Check verifies a manifest's own consistency.
func (m Manifest) Check() error {
	if m.Schema != Schema || m.Provider == "" || m.Version == "" {
		return fmt.Errorf("manifest %q: schema %q, provider and version are required", m.Provider, Schema)
	}
	if err := checkCalls(m.ObserveCalls); err != nil {
		return fmt.Errorf("manifest %s: observe: %w", m.Provider, err)
	}
	if len(m.ObserveCalls) > 0 && m.API == "" {
		return fmt.Errorf("manifest %s: calls without an api", m.Provider)
	}
	for _, k := range m.Kinds {
		if k.Kind == "" {
			return fmt.Errorf("manifest %s: a kind without a name", m.Provider)
		}
		seen := map[Op]bool{}
		for _, o := range k.Operations {
			if o.Op == "" || seen[o.Op] {
				return fmt.Errorf("manifest %s: %s %q is empty or declared twice", m.Provider, k.Kind, o.Op)
			}
			seen[o.Op] = true
			switch o.Support {
			case Supported:
				if o.Why != "" {
					return fmt.Errorf("manifest %s: %s %s is supported but says why not", m.Provider, k.Kind, o.Op)
				}
			case Unsupported:
				if o.Why == "" || len(o.Needs) > 0 || len(o.Calls) > 0 {
					return fmt.Errorf("manifest %s: unsupported %s %s needs a reason and no capabilities or calls", m.Provider, k.Kind, o.Op)
				}
			default:
				return fmt.Errorf("manifest %s: %s %s: unknown support %q", m.Provider, k.Kind, o.Op, o.Support)
			}
			if err := checkCalls(o.Calls); err != nil {
				return fmt.Errorf("manifest %s: %s %s: %w", m.Provider, k.Kind, o.Op, err)
			}
			if len(o.Calls) > 0 && m.API == "" {
				return fmt.Errorf("manifest %s: calls without an api", m.Provider)
			}
			for _, n := range o.Needs {
				if n.Capability == "" || n.Target.Type == "" || n.Target.Ref == "" {
					return fmt.Errorf("manifest %s: %s %s: a need without capability or target", m.Provider, k.Kind, o.Op)
				}
				if n.Grant == Standing {
					return fmt.Errorf("manifest %s: %s %s: a need is never standing by itself; say when with standingIf", m.Provider, k.Kind, o.Op)
				}
				for _, c := range append(slices.Clone(n.Conditions), n.StandingIf...) {
					if c.Key == "" || !slices.Contains([]Test{Equals, In, Like, Before, Contains, NotPresent}, c.Test) {
						return fmt.Errorf("manifest %s: %s %s: condition %+v has no key or an unknown test", m.Provider, k.Kind, o.Op, c)
					}
				}
			}
		}
	}
	return nil
}

// Methods are the HTTP methods a call may use.
var Methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

func checkCalls(calls []Call) error {
	seen := map[string]bool{}
	for _, c := range calls {
		if err := checkCall(c.Method, c.Path, c.Query, c.Form); err != nil {
			return err
		}
		if seen[c.Method+" "+c.Path] {
			return fmt.Errorf("call %s %s is declared twice", c.Method, c.Path)
		}
		seen[c.Method+" "+c.Path] = true
	}
	return nil
}

func checkCall(method, path string, query, form []string) error {
	if !slices.Contains(Methods, method) {
		return fmt.Errorf("call %s %s: unknown method", method, path)
	}
	if _, err := ParsePath(path); err != nil {
		return fmt.Errorf("call %s %s: %w", method, path, err)
	}
	names := map[string]bool{}
	for _, n := range append(slices.Clone(query), form...) {
		if !validName(n) || names[n] {
			return fmt.Errorf("call %s %s: parameter %q is invalid or declared twice (a name is in the query or the form, not both)", method, path, n)
		}
		names[n] = true
	}
	return nil
}

// Operation returns the declaration of an operation on a kind.
func (m Manifest) Operation(kind ResourceKind, op Op) (Operation, bool) {
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

// Requirement is one resolved need: this resource, through this operation, needs this capability
// on this target, given for this long, under these conditions.
type Requirement struct {
	Resource   string      `json:"resource,omitempty"` // graph ID; empty for observation
	Op         Op          `json:"op,omitempty"`       // empty for observation
	Provider   string      `json:"provider"`
	Support    Support     `json:"support"`
	Why        string      `json:"why,omitempty"` // for unsupported operations
	Capability string      `json:"capability,omitempty"`
	TargetType string      `json:"targetType,omitempty"`
	Target     string      `json:"target,omitempty"`
	Resolved   bool        `json:"resolved,omitempty"` // Target is the real name, not a placeholder
	Object     string      `json:"object,omitempty"`
	Always     bool        `json:"always,omitempty"`
	Grant      Grant       `json:"grant,omitempty"`
	StandingIf []Condition `json:"standingIf,omitempty"`
	Conditions []Condition `json:"conditions,omitempty"`
	Note       string      `json:"note,omitempty"`
	Reasons    []string    `json:"reasons,omitempty"`
}

// Fill replaces facts in the conditions: a value that is exactly "{name}" becomes all the values
// of that fact (a list, such as the allowed origins); "{name}" inside a value becomes its first.
func (q *Requirement) Fill(facts map[string][]string) {
	fill := func(cs []Condition) []Condition {
		out := slices.Clone(cs)
		for i := range out {
			var vals []string
			for _, v := range out[i].Values {
				if name, ok := strings.CutPrefix(v, "{"); ok && strings.HasSuffix(name, "}") && !strings.Contains(name[:len(name)-1], "{") {
					if f, ok := facts[strings.TrimSuffix(name, "}")]; ok {
						vals = append(vals, f...)
						continue
					}
				}
				for k, f := range facts {
					if len(f) > 0 {
						v = strings.ReplaceAll(v, "{"+k+"}", f[0])
					}
				}
				vals = append(vals, v)
			}
			out[i].Values = vals
		}
		return out
	}
	q.Conditions, q.StandingIf = fill(q.Conditions), fill(q.StandingIf)
}
