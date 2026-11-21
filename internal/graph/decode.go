package graph

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/internal/safetext"
)

const (
	// MaxDocumentBytes bounds the size of one graph document.
	MaxDocumentBytes = 4 << 20
	// MaxResources bounds the number of resources in one graph.
	MaxResources = 10000
)

// Wire types mirror the JSON exactly. Pointer fields tell a missing member from a zero value.
type wireGraph struct {
	Schema    *string         `json:"schema"`
	Resources *[]wireResource `json:"resources"`
}

type wireResource struct {
	Kind     *string        `json:"kind"`
	Name     *string        `json:"name"`
	Provider *string        `json:"provider"`
	After    *[]string      `json:"after"`
	Spec     jsontext.Value `json:"spec"`
}

type wireNodePackage struct {
	Node    *string `json:"node"`
	Package *string `json:"package"`
	State   *string `json:"state"`
}

type wireNodeMamelet struct {
	Node    *string        `json:"node"`
	Version jsontext.Value `json:"version"` // optional
	SHA256  *string        `json:"sha256"`
}

type wireNodeService struct {
	Node    *string `json:"node"`
	Service *string `json:"service"`
	Enabled *bool   `json:"enabled"`
	Running *bool   `json:"running"`
}

var strict = json.JoinOptions(json.RejectUnknownMembers(true))

// Decode parses one mamela-graph/v1 document strictly and validates it.
// The returned error is an *Error listing every problem found. A spec that names a file of a Pome
// (contentFrom), or leaves a sha256 to its mamela.sum, is refused: only DecodeWith, given the
// Pome's files, reads them.
func Decode(data []byte) (*Graph, error) { return DecodeWith(data, nil) }

// DecodeWith is Decode for the document a Pome printed, with files the Pome's own: a spec that
// names one of them for a text (contentFrom) has the text of that file, and a sha256 a spec leaves
// out is the one the Pome's mamela.sum has (sums.go), both read before the graph is validated,
// exactly as if the spec had written them. The graph, its canonical form and its digest are the
// same either way; only Resource.Sources tells. When mamela.sum lacks a sha256, the error is a
// *MissingSums once the graph is valid.
func DecodeWith(data []byte, files Files) (*Graph, error) {
	var sums *sumState
	if files != nil {
		sums = &sumState{seen: map[Pin]bool{}}
	}
	if len(data) > MaxDocumentBytes {
		return nil, single(CodeTooLarge, "",
			fmt.Sprintf("the graph document is %d bytes, more than the limit of %d", len(data), MaxDocumentBytes),
			"split the declaration or remove resources that are not needed")
	}
	// A single trailing newline is how a line-oriented writer ends its output; anything else is data.
	data = trimFinalNewline(data)

	var w wireGraph
	if err := json.Unmarshal(data, &w, strict); err != nil {
		return nil, fromJSONError(err, "")
	}
	if w.Schema == nil {
		return nil, single(CodeMissingField, "/schema", "the document has no \"schema\" member",
			fmt.Sprintf("the first member should be \"schema\":%q", Schema))
	}
	if *w.Schema != Schema && *w.Schema != SchemaV2 {
		return nil, single(CodeUnknownSchema, "/schema",
			fmt.Sprintf("schema %q is not supported; this Mamela reads %q and %q", *w.Schema, Schema, SchemaV2),
			"use a Mamela Pome that matches this version of Mamela")
	}
	if w.Resources == nil {
		return nil, single(CodeMissingField, "/resources", "the document has no \"resources\" member", "")
	}
	if n := len(*w.Resources); n > MaxResources {
		return nil, single(CodeTooManyResources, "/resources",
			fmt.Sprintf("the graph has %d resources, more than the limit of %d", n, MaxResources), "")
	}

	var problems []Problem
	g := &Graph{Resources: make([]Resource, 0, len(*w.Resources))}
	first := map[string]int{} // resource ID -> the index in g.Resources of the first that has it
	for i, wr := range *w.Resources {
		if wr.Provider != nil && *w.Schema == Schema {
			problems = append(problems, Problem{Code: CodeNeedsV2, Path: fmt.Sprintf("/resources/%d/provider", i),
				Message: "a resource of a provider needs schema " + SchemaV2,
				Hint:    "use a Mamela Pome that writes " + SchemaV2})
			continue
		}
		r, ps := decodeResource(i, wr, files, sums)
		problems = append(problems, ps...)
		if r == nil {
			continue
		}
		// A resource declared twice the same way is one resource (a provider's Pome declares, for
		// each VM, the template it is cloned from). Declared twice differently, it stays twice, and
		// Validate says so (MAM-GRAPH-0102).
		if at, dup := first[r.ID()]; dup && g.sameAs(at, *r) {
			continue
		} else if !dup {
			first[r.ID()] = len(g.Resources)
		}
		g.Resources = append(g.Resources, *r)
	}
	if err := newError(problems); err != nil {
		return nil, err
	}
	if err := Validate(g); err != nil {
		return nil, err
	}
	if sums != nil && len(sums.missing) > 0 {
		return nil, &MissingSums{Pins: sums.missing}
	}
	return g, nil
}

func decodeResource(i int, wr wireResource, files Files, sums *sumState) (*Resource, []Problem) {
	at := fmt.Sprintf("/resources/%d", i)
	var problems []Problem
	missing := func(field string) {
		problems = append(problems, Problem{Code: CodeMissingField, Path: at + "/" + field,
			Message: fmt.Sprintf("the resource has no %q member", field)})
	}
	if wr.Kind == nil {
		missing("kind")
	}
	if wr.Name == nil {
		missing("name")
	}
	if wr.Spec == nil || string(wr.Spec) == "null" {
		missing("spec")
	}
	if len(problems) > 0 {
		return nil, problems
	}

	r := &Resource{Kind: Kind(*wr.Kind), Name: *wr.Name}
	where := fmt.Sprintf("%s (%s)", at, r.ID())
	need := func(present bool, field string) {
		if !present {
			problems = append(problems, Problem{Code: CodeMissingField, Path: where + " spec." + field,
				Message: fmt.Sprintf("%s has no spec.%s", r.Kind, field)})
		}
	}

	if wr.After != nil && wr.Provider == nil {
		return nil, []Problem{{Code: CodeUnknownField, Path: where + " after",
			Message: "only a resource of a provider has after", Hint: "a resource on a node waits for its node already"}}
	}
	if wr.Provider != nil {
		spec, serr := providedSpec(wr.Spec)
		if serr != nil {
			return nil, []Problem{{Code: serr.code, Path: where + " spec" + serr.at, Message: serr.msg, Hint: serr.hint}}
		}
		if Builtin(r.Kind) {
			return nil, []Problem{{Code: CodeUnknownKind, Path: at + "/kind",
				Message: fmt.Sprintf("%s is a built-in kind; a provider cannot declare it", r.Kind)}}
		}
		r.Provided = &ProvidedSpec{Provider: *wr.Provider, Spec: spec}
		if wr.After != nil {
			seen := map[string]bool{}
			for _, a := range *wr.After {
				if !seen[a] {
					r.After = append(r.After, a)
					seen[a] = true
				}
			}
			slices.Sort(r.After)
		}
		return r, nil
	}
	switch r.Kind {
	case "VirtualMachine":
		problems = append(problems, Problem{Code: CodeUnknownKind, Path: at + "/kind",
			Message: "the built-in kind VirtualMachine was removed: machines come from providers (ADR 0006)",
			Hint:    "declare the machine with its provider's Pome (for Proxmox, github.com/ieyoukan/mamela-proxmox/pome: Proxmox.vm) and use the Mamela Pome 0.3.0"})
	case KindNodePackage:
		var s wireNodePackage
		if err := json.Unmarshal(wr.Spec, &s, strict); err != nil {
			return nil, problemsOf(fromJSONError(err, where+" spec"))
		}
		need(s.Node != nil, "node")
		need(s.Package != nil, "package")
		need(s.State != nil, "state")
		if len(problems) == 0 {
			r.NodePackage = &NodePackageSpec{Node: *s.Node, Package: *s.Package, State: PackageState(*s.State)}
		}
	case KindNodeService:
		var s wireNodeService
		if err := json.Unmarshal(wr.Spec, &s, strict); err != nil {
			return nil, problemsOf(fromJSONError(err, where+" spec"))
		}
		need(s.Node != nil, "node")
		need(s.Service != nil, "service")
		need(s.Enabled != nil, "enabled")
		need(s.Running != nil, "running")
		if len(problems) == 0 {
			r.NodeService = &NodeServiceSpec{Node: *s.Node, Service: *s.Service, Enabled: *s.Enabled, Running: *s.Running}
		}
	case KindNodeMamelet:
		var s wireNodeMamelet
		if err := json.Unmarshal(wr.Spec, &s, strict); err != nil {
			return nil, problemsOf(fromJSONError(err, where+" spec"))
		}
		need(s.Node != nil, "node")
		need(s.SHA256 != nil, "sha256")
		version, ps := optionalString(s.Version, where, "version")
		problems = append(problems, ps...)
		if len(problems) == 0 {
			r.NodeMamelet = &NodeMameletSpec{Node: *s.Node, SHA256: *s.SHA256}
			if version != nil {
				r.NodeMamelet.Version = *version
			}
		}
	default:
		if def, ok := extKinds[r.Kind]; ok {
			d := &SpecDecoder{Spec: wr.Spec, kind: r.Kind, where: where, files: files, id: r.ID(), sums: sums}
			spec := def.Decode(d)
			if !d.OK() {
				return nil, d.problems
			}
			r.Ext, r.Sources = spec, d.sources
			break
		}
		problems = append(problems, Problem{Code: CodeUnknownKind, Path: at + "/kind",
			Message: fmt.Sprintf("kind %q is not known", r.Kind),
			Hint:    "known kinds are " + knownKinds() + "; a provider's kind needs its \"provider\""})
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return r, nil
}

// maxExactInt is the largest integer a JSON number (a double) holds exactly: 2^53 - 1.
const maxExactInt = 1<<53 - 1

// maxSpecDepth bounds the nesting of a provider's spec.
const maxSpecDepth = 32

// specError is why a provider's spec is refused: the code, what and where, and what to do about it.
type specError struct {
	code Code
	at   string // the member, as a JSON pointer prefixed with a space; "" for the spec itself
	msg  string
	hint string
}

func wrongType(msg, hint string) *specError {
	return &specError{code: CodeWrongType, msg: msg, hint: hint}
}

// providedSpec checks a provider's spec (a JSON object of strings, integers, booleans, arrays and
// objects, no null, no duplicate member, no text that does not show as itself) and returns its
// canonical form.
func providedSpec(v jsontext.Value) ([]byte, *specError) {
	dec := jsontext.NewDecoder(strings.NewReader(string(v)))
	depth := 0
	for first := true; ; first = false {
		tok, err := dec.ReadToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, wrongType(err.Error(), "the spec of a provider's resource is one JSON object")
		}
		if first && tok.Kind() != '{' {
			return nil, wrongType("the spec is not a JSON object", "the spec of a provider's resource is a JSON object")
		}
		switch tok.Kind() {
		case '{', '[':
			if depth++; depth > maxSpecDepth {
				return nil, wrongType(fmt.Sprintf("the spec is nested deeper than %d", maxSpecDepth),
					fmt.Sprintf("flatten it: a spec is at most %d levels deep", maxSpecDepth))
			}
		case '}', ']':
			depth--
		case 'n':
			return nil, &specError{code: CodeWrongType, at: " " + string(dec.StackPointer()), msg: "the spec has a null",
				hint: "leave the member out instead of writing null: a spec has no null"}
		case '"':
			if f, bad := safetext.Find(tok.String(), true); bad {
				return nil, &specError{code: CodeUnsafeText, at: " " + string(dec.StackPointer()),
					msg:  "the spec has a text with " + f.String(),
					hint: unsafeTextHint}
			}
		case '0':
			if strings.ContainsAny(tok.String(), ".eE") {
				return nil, &specError{code: CodeWrongType, at: " " + string(dec.StackPointer()),
					msg:  fmt.Sprintf("the spec has a number %s that is not an integer", tok.String()),
					hint: "write whole numbers only; a decimal number goes in as a string"}
			}
			// The canonical form (RFC 8785) writes numbers as IEEE 754 doubles: an integer beyond
			// 2^53 would reach the provider as another number. Refuse it rather than change it.
			if n, err := strconv.ParseInt(tok.String(), 10, 64); err != nil || n > maxExactInt || n < -maxExactInt {
				return nil, &specError{code: CodeWrongType, at: " " + string(dec.StackPointer()),
					msg:  fmt.Sprintf("the spec has an integer %s beyond ±%d, which JSON cannot carry exactly (write it as a string)", tok.String(), maxExactInt),
					hint: "write a number that large as a string"}
			}
		}
	}
	out := jsontext.Value(append([]byte(nil), v...))
	if err := out.Canonicalize(); err != nil {
		return nil, wrongType(err.Error(), "the spec of a provider's resource is one JSON object")
	}
	return out, nil
}

// unsafeTextHint is the advice for text that does not show as itself.
const unsafeTextHint = "a text may have newlines and tabs, but no other control character (a carriage return, an escape), " +
	"bidirectional control or zero-width character: they hide what a person reads in the plan. " +
	"Leave it out, or write it as an escape where the format has one (\\u202e in a double-quoted YAML string)"

// optionalString is an optional string member: nil when the member is absent. null is not another
// way to write absent (the schema refuses null everywhere): leave the member out.
func optionalString(v jsontext.Value, where, field string) (*string, []Problem) {
	if v == nil {
		return nil, nil
	}
	if string(v) == "null" {
		return nil, []Problem{{Code: CodeWrongType, Path: where + " spec." + field,
			Message: "a value has the wrong type: " + field + " is null", Hint: "leave the member out instead of writing null"}}
	}
	var s string
	if err := json.Unmarshal(v, &s, strict); err != nil {
		return nil, problemsOf(fromJSONError(err, where+" spec."+field))
	}
	return &s, nil
}

func problemsOf(err error) []Problem {
	var e *Error
	if errors.As(err, &e) {
		return e.Problems
	}
	return []Problem{{Code: CodeInvalidJSON, Message: err.Error()}}
}

// fromJSONError turns a json/v2 error into a Problem with a stable code. The original
// message is kept in the text, since it names the exact position.
func fromJSONError(err error, prefix string) error {
	var syn *jsontext.SyntacticError
	var sem *json.SemanticError
	switch {
	case errors.Is(err, jsontext.ErrDuplicateName):
		return single(CodeDuplicateMember, joinPath(prefix, pointerOf(err)),
			"an object has the same member twice: "+err.Error(),
			"each member may appear once; this usually means the Graph was written by hand or by a broken writer")
	case errors.Is(err, json.ErrUnknownName):
		return single(CodeUnknownField, joinPath(prefix, pointerOf(err)),
			"an object has a member this schema does not define: "+err.Error(),
			"check the spelling, or update Mamela if the Pome is newer")
	case errors.As(err, &syn):
		if strings.Contains(syn.Err.Error(), "after top-level value") {
			return single(CodeTrailingData, prefix,
				"there is more output after the graph document: "+err.Error(),
				"print exactly one graph; send diagnostics to standard error (dbg or print_error_line)")
		}
		return single(CodeInvalidJSON, joinPath(prefix, string(syn.JSONPointer)),
			"the output is not valid JSON: "+err.Error(),
			"the evaluation may have been cut off, or printed something other than the graph to standard output")
	case errors.As(err, &sem):
		return single(CodeWrongType, joinPath(prefix, string(sem.JSONPointer)),
			"a value has the wrong type: "+err.Error(),
			"check the type of the member in docs/graph-schema.md (strings are strings, flags are true or false, numbers are integers written without a fraction or exponent)")
	default:
		return single(CodeInvalidJSON, prefix, err.Error(), "")
	}
}

func pointerOf(err error) string {
	var syn *jsontext.SyntacticError
	if errors.As(err, &syn) {
		return string(syn.JSONPointer)
	}
	var sem *json.SemanticError
	if errors.As(err, &sem) {
		return string(sem.JSONPointer)
	}
	return ""
}

func joinPath(prefix, pointer string) string {
	switch {
	case prefix == "":
		return pointer
	case pointer == "":
		return prefix
	default:
		return prefix + " " + pointer
	}
}

func trimFinalNewline(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		return b[:n-1]
	}
	return b
}

// sameAs says whether the resource at index i is exactly r (in canonical form).
func (g *Graph) sameAs(i int, r Resource) bool {
	one, other := (&Graph{Resources: []Resource{g.Resources[i]}}).Canonical(), (&Graph{Resources: []Resource{r}}).Canonical()
	return bytes.Equal(one, other)
}
