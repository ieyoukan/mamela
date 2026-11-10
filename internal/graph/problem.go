package graph

import (
	"fmt"
	"sort"
	"strings"
)

// Code is a stable reason code shown to people. Codes are never reused for a different meaning.
type Code string

const (
	// Decoding (the JSON text itself).
	CodeInvalidJSON      Code = "MAM-GRAPH-0001"
	CodeUnknownSchema    Code = "MAM-GRAPH-0002"
	CodeUnknownField     Code = "MAM-GRAPH-0003"
	CodeMissingField     Code = "MAM-GRAPH-0004"
	CodeUnknownKind      Code = "MAM-GRAPH-0005"
	CodeTooLarge         Code = "MAM-GRAPH-0006"
	CodeWrongType        Code = "MAM-GRAPH-0007"
	CodeDuplicateMember  Code = "MAM-GRAPH-0008"
	CodeTrailingData     Code = "MAM-GRAPH-0009"
	CodeTooManyResources Code = "MAM-GRAPH-0010"

	// Validation (the meaning of a decoded graph).
	CodeInvalidName       Code = "MAM-GRAPH-0101"
	CodeDuplicateResource Code = "MAM-GRAPH-0102"
	CodeDanglingNode      Code = "MAM-GRAPH-0103"
	CodeNameMismatch      Code = "MAM-GRAPH-0104"
	CodeOutOfRange        Code = "MAM-GRAPH-0105"
	CodeInvalidValue      Code = "MAM-GRAPH-0106"
)

// Problem is one thing wrong with a graph, written for a person: what, where, why, and what to do.
type Problem struct {
	Code    Code
	Path    string // where: a resource ("VirtualMachine app-server") and field, or a JSON pointer
	Message string // what and why
	Hint    string // what to do; may be empty
}

func (p Problem) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", p.Code, p.Message)
	if p.Path != "" {
		fmt.Fprintf(&b, "\n  at %s", p.Path)
	}
	if p.Hint != "" {
		fmt.Fprintf(&b, "\n  hint: %s", p.Hint)
	}
	return b.String()
}

// Error carries every problem found, in a deterministic order.
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.String()
	}
	return strings.Join(parts, "\n")
}

// Has reports whether any problem has the code.
func (e *Error) Has(code Code) bool {
	for _, p := range e.Problems {
		if p.Code == code {
			return true
		}
	}
	return false
}

func newError(problems []Problem) error {
	if len(problems) == 0 {
		return nil
	}
	sorted := append([]Problem(nil), problems...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		return sorted[i].Code < sorted[j].Code
	})
	return &Error{Problems: sorted}
}

func single(code Code, path, message, hint string) error {
	return &Error{Problems: []Problem{{Code: code, Path: path, Message: message, Hint: hint}}}
}
