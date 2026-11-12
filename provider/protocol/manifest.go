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
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"regexp"
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
	// APIs name the APIs of a provider that uses several (AWS: "ec2", "ecs", "logs", "iam"), each
	// with its own endpoint, credentials and catalog on the host; then each call names its API
	// (ADR 0007). A provider sets API or APIs, not both.
	APIs []string `json:"apis,omitempty"`
	// Observe is what every observation needs (each plan and each apply observes).
	Observe []Need `json:"observe,omitempty"`
	// ObserveCalls are the API calls an observation sends (reads only).
	ObserveCalls []Call `json:"observeCalls,omitempty"`
	Kinds        []Kind `json:"kinds"`
	// Guard, when set, is the provider's safety check around every apply of its changes (ADR
	// 0006): the reads it makes. Mamela runs it before applying (a block finding stops the apply)
	// and after (comparing with what it found before).
	Guard *Guard `json:"guard,omitempty"`
	// Scripts are the setup and maintenance scripts the provider writes for a person to review and
	// run with privileges Mamela never holds (ADR 0006): creating the identities and grants Mamela
	// uses, removing what Mamela can no longer remove. The provider writes them from the settings
	// alone (method script), so everyone needs only Mamela and the pinned provider to get them.
	Scripts []Script `json:"scripts,omitempty"`
}

// Script is one script a provider writes.
type Script struct {
	Name        string `json:"name"`        // "bootstrap"
	Description string `json:"description"` // what it does, and where and as whom it runs
}

// Guard declares a provider's safety check.
type Guard struct {
	Calls []Call `json:"calls"` // reads only
}

// Kind is one resource kind a provider handles.
type Kind struct {
	Kind       ResourceKind `json:"kind"`
	Operations []Operation  `json:"operations"`
	// Node: resources of this kind are machines mamelet runs on (ADR 0004). Their observation
	// reports Observed.Node, and node resources (packages, services, k3s) may be declared on them.
	Node bool `json:"node,omitempty"`
	// Spec are the fields of the kind's spec (ADR 0009), when the provider gives them: its Pome's
	// types and functions can then be generated.
	Spec []Field `json:"spec,omitempty"`
	// Doc says what a resource of the kind is, for its generated Pome.
	Doc string `json:"doc,omitempty"`
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
	// Explicit operations (deleting, rebooting) are never planned: a person asks for one by name
	// (mamela act) and confirms it by typing the resource's name (ADR 0006). Diff never returns
	// them; prepare makes their change.
	Explicit bool `json:"explicit,omitempty"`
	// Adopts: the explicit operation takes over an Adoptable object (its only use); once it is done,
	// Mamela records the object as its own.
	Adopts bool `json:"adopts,omitempty"`
}

// Call is one API call: a method and a path template whose "{name}" segments are IDs ("/items/{item}"),
// and every parameter it may send, by where it sends it: in the query or in the form body. A
// name is in one place only. No parameters means it sends none.
type Call struct {
	Method string   `json:"method"`
	Path   string   `json:"path"`
	Query  []string `json:"query,omitempty"`
	Form   []string `json:"form,omitempty"`
	// File is the form parameter that carries an uploaded artifact (multipart), for an upload call.
	File string `json:"file,omitempty"`
	// Fixed are parameters the call always sends with exactly these values, and which tell it
	// apart from other calls on the same method and path: a query API's "Action=RunInstances"
	// (ADR 0005), or an AWS JSON API's "X-Amz-Target" header (ADR 0007). Each is also one of
	// Query, Form or Header, where it is sent.
	Fixed map[string]string `json:"fixed,omitempty"`
	// API is which of the provider's APIs the call goes to, for a provider with several (APIs);
	// empty for a provider with one.
	API string `json:"api,omitempty"`
	// Header are parameters sent as HTTP headers ("X-Amz-Target", ADR 0007).
	Header []string `json:"header,omitempty"`
	// JSON are the members of a JSON object the call sends as its body (an AWS JSON API, ADR
	// 0007). A member's value, as a parameter, is its canonical JSON text ("\"locust\"", "1").
	JSON []string `json:"json,omitempty"`
}

// Same reports whether two calls are the same call: API, method, path and fixed parameters.
func (c Call) Same(o Call) bool {
	return c.API == o.API && c.Method == o.Method && c.Path == o.Path && maps.Equal(c.Fixed, o.Fixed)
}

// checkAPIs: a provider has one API (API) or several (APIs), and each call names one of them
// exactly when there are several.
func (m Manifest) checkAPIs() error {
	if m.API != "" && len(m.APIs) > 0 {
		return errors.New("api and apis are exclusive")
	}
	seen := map[string]bool{}
	for _, a := range m.APIs {
		if !validName(a) || seen[a] {
			return fmt.Errorf("api %q is invalid or named twice", a)
		}
		seen[a] = true
	}
	calls := slices.Clone(m.ObserveCalls)
	if m.Guard != nil {
		calls = append(calls, m.Guard.Calls...)
	}
	for _, k := range m.Kinds {
		for _, o := range k.Operations {
			calls = append(calls, o.Calls...)
		}
	}
	for _, c := range calls {
		switch {
		case len(m.APIs) > 0 && !seen[c.API]:
			return fmt.Errorf("call %s %s names api %q, not one of %v", c.Method, c.Path, c.API, m.APIs)
		case len(m.APIs) == 0 && c.API != "":
			return fmt.Errorf("call %s %s names api %q, and the provider has one api", c.Method, c.Path, c.API)
		}
	}
	return nil
}

// HasAPIs reports whether the provider uses any API (one, or several).
func (m Manifest) HasAPIs() bool { return m.API != "" || len(m.APIs) > 0 }

// Takes reports whether params (one value each) carry every fixed parameter of the call.
func (c Call) Takes(params map[string][]string) bool {
	for k, v := range c.Fixed {
		if got := params[k]; len(got) != 1 || got[0] != v {
			return false
		}
	}
	return true
}

// FindCall is the declared call a request on method and a path template (or a concrete path,
// when match is set) with params is: the one whose method, path and fixed parameters fit. ok is
// false when none does, or more than one (an ambiguous declaration).
func FindCall(calls []Call, api, method, path string, params map[string][]string, concrete bool) (Call, map[string]string, bool) {
	var found Call
	var ids map[string]string
	n := 0
	for _, c := range calls {
		if c.API != api || c.Method != method || !c.Takes(params) {
			continue
		}
		var m map[string]string
		if concrete {
			p, err := ParsePath(c.Path)
			if err != nil {
				continue
			}
			var ok bool
			if m, ok = p.Match(path); !ok {
				continue
			}
		} else if c.Path != path {
			continue
		}
		if n > 0 && found.Same(c) {
			continue // the same call, declared twice (observe's reads and an operation's)
		}
		found, ids = c, m
		n++
	}
	return found, ids, n == 1
}

// CheckAdopts: only an explicit operation adopts.
func (o Operation) checkAdopts() error {
	if o.Adopts && !o.Explicit {
		return fmt.Errorf("%s adopts, so it must be explicit", o.Op)
	}
	return nil
}

// In says where the call sends a parameter: "query", "form", or "" when it does not declare it.
func (c Call) In(param string) string {
	return in(param, c.Query, c.Form, c.Header, c.JSON)
}

func in(param string, query, form, header, json []string) string {
	switch {
	case slices.Contains(query, param):
		return "query"
	case slices.Contains(form, param):
		return "form"
	case slices.Contains(header, param):
		return "header"
	case slices.Contains(json, param):
		return "json"
	}
	return ""
}

// Params are all the parameters the call may send.
func (c Call) Params() []string {
	return slices.Concat(c.Query, c.Form, c.Header, c.JSON)
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
	if err := m.checkAPIs(); err != nil {
		return fmt.Errorf("manifest %s: %w", m.Provider, err)
	}
	if m.Schema != Schema || m.Provider == "" || m.Version == "" {
		return fmt.Errorf("manifest %q: schema %q, provider and version are required", m.Provider, Schema)
	}
	for _, c := range m.ObserveCalls {
		if c.File != "" {
			return fmt.Errorf("manifest %s: observe call %s %s uploads a file", m.Provider, c.Method, c.Path)
		}
	}
	if err := checkCalls(m.ObserveCalls); err != nil {
		return fmt.Errorf("manifest %s: observe: %w", m.Provider, err)
	}
	if m.Guard != nil {
		if !m.HasAPIs() || len(m.Guard.Calls) == 0 {
			return fmt.Errorf("manifest %s: a guard reads through an api, with calls", m.Provider)
		}
		if err := checkCalls(m.Guard.Calls); err != nil {
			return fmt.Errorf("manifest %s: guard: %w", m.Provider, err)
		}
		for _, c := range m.Guard.Calls {
			if c.File != "" {
				return fmt.Errorf("manifest %s: guard call %s %s uploads a file", m.Provider, c.Method, c.Path)
			}
		}
	}
	seenScripts := map[string]bool{}
	for _, sc := range m.Scripts {
		if !scriptNameRe.MatchString(sc.Name) || sc.Description == "" || seenScripts[sc.Name] {
			return fmt.Errorf("manifest %s: script %q: a unique name (lowercase letters, digits, '-') and a description are required", m.Provider, sc.Name)
		}
		seenScripts[sc.Name] = true
	}
	if len(m.ObserveCalls) > 0 && !m.HasAPIs() {
		return fmt.Errorf("manifest %s: calls without an api", m.Provider)
	}
	for _, k := range m.Kinds {
		if k.Kind == "" {
			return fmt.Errorf("manifest %s: a kind without a name", m.Provider)
		}
		if err := CheckFields(k.Spec); err != nil {
			return fmt.Errorf("manifest %s: %s spec: %w", m.Provider, k.Kind, err)
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
				if o.Why == "" || len(o.Needs) > 0 || len(o.Calls) > 0 || o.Explicit {
					return fmt.Errorf("manifest %s: unsupported %s %s needs a reason and no capabilities, calls or explicit", m.Provider, k.Kind, o.Op)
				}
			default:
				return fmt.Errorf("manifest %s: %s %s: unknown support %q", m.Provider, k.Kind, o.Op, o.Support)
			}
			if err := checkCalls(o.Calls); err != nil {
				return fmt.Errorf("manifest %s: %s %s: %w", m.Provider, k.Kind, o.Op, err)
			}
			if len(o.Calls) > 0 && !m.HasAPIs() {
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
	byPath := map[string][]Call{}
	for _, c := range calls {
		if err := checkCall(c.Method, c.Path, c.Query, c.Form, c.Header, c.JSON, c.Fixed); err != nil {
			return err
		}
		if err := checkFixed(c.Method, c.Path, c.Fixed, c.In); err != nil {
			return err
		}
		byPath[c.API+" "+c.Method+" "+c.Path] = append(byPath[c.API+" "+c.Method+" "+c.Path], c)
		if c.File != "" && (!validName(c.File) || c.In(c.File) != "" || c.Method != "POST") {
			return fmt.Errorf("call %s %s: file %q must be a POST parameter of its own", c.Method, c.Path, c.File)
		}
		key := c.API + " " + c.Method + " " + c.Path + " " + fixedKey(c.Fixed)
		if seen[key] {
			return fmt.Errorf("call %s %s %s is declared twice", c.Method, c.Path, fixedKey(c.Fixed))
		}
		seen[key] = true
	}
	return checkDistinct(byPath)
}

// checkFixed: each fixed parameter is one the call sends, with a value.
func checkFixed(method, path string, fixed map[string]string, in func(string) string) error {
	for k, v := range fixed {
		if in(k) == "" || v == "" {
			return fmt.Errorf("call %s %s: fixed parameter %s=%q is not one of its parameters, or is empty", method, path, k, v)
		}
	}
	return nil
}

// checkDistinct: calls on the same method and path are told apart by their fixed parameters: each
// has some, and no request can take two of them (they differ in a parameter both fix).
func checkDistinct(byPath map[string][]Call) error {
	for key, cs := range byPath {
		if len(cs) < 2 {
			continue
		}
		for i, a := range cs {
			if len(a.Fixed) == 0 {
				return fmt.Errorf("calls %s are told apart only by fixed parameters, and one has none", key)
			}
			for _, b := range cs[i+1:] {
				differ := false
				for k, v := range a.Fixed {
					if w, ok := b.Fixed[k]; ok && w != v {
						differ = true
					}
				}
				if !differ {
					return fmt.Errorf("calls %s %s and %s could be the same request", key, fixedKey(a.Fixed), fixedKey(b.Fixed))
				}
			}
		}
	}
	return nil
}

func fixedKey(f map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(f)) {
		parts = append(parts, k+"="+f[k])
	}
	return strings.Join(parts, "&")
}

func checkCall(method, path string, query, form, header, json []string, fixed map[string]string) error {
	if !slices.Contains(Methods, method) {
		return fmt.Errorf("call %s %s: unknown method", method, path)
	}
	if _, err := ParsePath(path); err != nil {
		return fmt.Errorf("call %s %s: %w", method, path, err)
	}
	names := map[string]bool{}
	for _, n := range slices.Concat(query, form, header, json) {
		if !validName(n) || names[n] {
			return fmt.Errorf("call %s %s: parameter %q is invalid or declared twice (a name is in one place only)", method, path, n)
		}
		names[n] = true
	}
	if len(json) > 0 && len(form) > 0 {
		return fmt.Errorf("call %s %s: a body is a form or a JSON object, not both", method, path)
	}
	for k := range fixed {
		if slices.Contains(json, k) {
			return fmt.Errorf("call %s %s: fixed parameter %s is a JSON member; fix a header (X-Amz-Target) instead", method, path, k)
		}
	}
	return nil
}

// JSONParams are the members of a JSON object as parameters: each one's canonical JSON text
// (RFC 8785), so that what is approved is exactly what is sent (ADR 0007).
func JSONParams(members map[string]jsontext.Value) (map[string][]string, error) {
	out := map[string][]string{}
	for k, v := range members {
		c := slices.Clone(v)
		if err := (*jsontext.Value)(&c).Canonicalize(); err != nil {
			return nil, fmt.Errorf("JSON member %s: %w", k, err)
		}
		out[k] = []string{string(c)}
	}
	return out, nil
}

// ParamText is a parameter's value as text: a JSON member that is a string, without its quotes
// (to compare with an ID, or to read a measure); anything else as it is.
func ParamText(where, value string) string {
	if where != "json" {
		return value
	}
	var s string
	if json.Unmarshal([]byte(value), &s) == nil {
		return s
	}
	return value
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

var scriptNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
