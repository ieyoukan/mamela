package graph

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
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
	Spec     jsontext.Value `json:"spec"`
}

type wireVirtualMachine struct {
	CPU       *int64  `json:"cpu"`
	MemoryMiB *int64  `json:"memoryMiB"`
	DiskGiB   *int64  `json:"diskGiB"`
	Image     *string `json:"image"`
}

type wireNodePackage struct {
	Node    *string `json:"node"`
	Package *string `json:"package"`
	State   *string `json:"state"`
}

type wireNodeK3s struct {
	Node    *string `json:"node"`
	Role    *string `json:"role"`
	Server  *string `json:"server"`
	Version *string `json:"version"`
	SHA256  *string `json:"sha256"`
}

type wireNodeService struct {
	Node    *string `json:"node"`
	Service *string `json:"service"`
	Enabled *bool   `json:"enabled"`
	Running *bool   `json:"running"`
}

var strict = json.JoinOptions(json.RejectUnknownMembers(true))

// Decode parses one mamela-graph/v1 document strictly and validates it.
// The returned error is an *Error listing every problem found.
func Decode(data []byte) (*Graph, error) {
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
	for i, wr := range *w.Resources {
		if wr.Provider != nil && *w.Schema == Schema {
			problems = append(problems, Problem{Code: CodeNeedsV2, Path: fmt.Sprintf("/resources/%d/provider", i),
				Message: "a resource of a provider needs schema " + SchemaV2,
				Hint:    "use a Mamela Pome that writes " + SchemaV2})
			continue
		}
		r, ps := decodeResource(i, wr)
		problems = append(problems, ps...)
		if r != nil {
			g.Resources = append(g.Resources, *r)
		}
	}
	if err := newError(problems); err != nil {
		return nil, err
	}
	if err := Validate(g); err != nil {
		return nil, err
	}
	return g, nil
}

func decodeResource(i int, wr wireResource) (*Resource, []Problem) {
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

	if wr.Provider != nil {
		spec, err := providedSpec(wr.Spec)
		if err != nil {
			return nil, []Problem{{Code: CodeWrongType, Path: where + " spec", Message: err.Error(),
				Hint: "the spec of a provider's resource is a JSON object; numbers must be integers"}}
		}
		if _, builtin := kindOrder[r.Kind]; builtin {
			return nil, []Problem{{Code: CodeUnknownKind, Path: at + "/kind",
				Message: fmt.Sprintf("%s is a built-in kind; a provider cannot declare it", r.Kind)}}
		}
		r.Provided = &ProvidedSpec{Provider: *wr.Provider, Spec: spec}
		return r, nil
	}
	switch r.Kind {
	case KindVirtualMachine:
		var s wireVirtualMachine
		if err := json.Unmarshal(wr.Spec, &s, strict); err != nil {
			return nil, problemsOf(fromJSONError(err, where+" spec"))
		}
		need(s.CPU != nil, "cpu")
		need(s.MemoryMiB != nil, "memoryMiB")
		need(s.DiskGiB != nil, "diskGiB")
		need(s.Image != nil, "image")
		if len(problems) == 0 {
			r.VirtualMachine = &VirtualMachineSpec{CPU: *s.CPU, MemoryMiB: *s.MemoryMiB, DiskGiB: *s.DiskGiB, Image: *s.Image}
		}
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
	case KindNodeK3s:
		var s wireNodeK3s
		if err := json.Unmarshal(wr.Spec, &s, strict); err != nil {
			return nil, problemsOf(fromJSONError(err, where+" spec"))
		}
		need(s.Node != nil, "node")
		need(s.Role != nil, "role")
		need(s.Version != nil, "version")
		need(s.SHA256 != nil, "sha256")
		if len(problems) == 0 {
			r.NodeK3s = &NodeK3sSpec{Node: *s.Node, Role: K3sRole(*s.Role), Version: *s.Version, SHA256: *s.SHA256}
			if s.Server != nil {
				r.NodeK3s.Server = *s.Server
			}
		}
	default:
		problems = append(problems, Problem{Code: CodeUnknownKind, Path: at + "/kind",
			Message: fmt.Sprintf("kind %q is not known", r.Kind),
			Hint:    "known kinds are VirtualMachine, NodePackage, NodeService and NodeK3s; a provider's kind needs its \"provider\""})
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

// providedSpec checks a provider's spec (a JSON object of strings, integers, booleans, arrays and
// objects, no null, no duplicate member) and returns its canonical form.
func providedSpec(v jsontext.Value) ([]byte, error) {
	dec := jsontext.NewDecoder(strings.NewReader(string(v)))
	depth := 0
	for first := true; ; first = false {
		tok, err := dec.ReadToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if first && tok.Kind() != '{' {
			return nil, errors.New("the spec is not a JSON object")
		}
		switch tok.Kind() {
		case '{', '[':
			if depth++; depth > maxSpecDepth {
				return nil, fmt.Errorf("the spec is nested deeper than %d", maxSpecDepth)
			}
		case '}', ']':
			depth--
		case 'n':
			return nil, errors.New("the spec has a null")
		case '0':
			if strings.ContainsAny(tok.String(), ".eE") {
				return nil, fmt.Errorf("the spec has a number %s that is not an integer", tok.String())
			}
			// The canonical form (RFC 8785) writes numbers as IEEE 754 doubles: an integer beyond
			// 2^53 would reach the provider as another number. Refuse it rather than change it.
			if n, err := strconv.ParseInt(tok.String(), 10, 64); err != nil || n > maxExactInt || n < -maxExactInt {
				return nil, fmt.Errorf("the spec has an integer %s beyond ±%d, which JSON cannot carry exactly (write it as a string)", tok.String(), maxExactInt)
			}
		}
	}
	out := jsontext.Value(append([]byte(nil), v...))
	if err := out.Canonicalize(); err != nil {
		return nil, err
	}
	return out, nil
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
			"numbers must be integers written without a fraction or exponent")
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
