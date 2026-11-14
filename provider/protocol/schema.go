package protocol

import (
	"errors"
	"fmt"
	"slices"
)

// Field is one field of a kind's spec (ADR 0009): what a declaration writes, so that the kind's
// Emela functions can be generated (mamela provider pome) and a spec checked before the provider
// sees it. A provider that gives no fields keeps writing its Pome by hand.
type Field struct {
	Name string `json:"name"`
	// Type is "string", "int", "bool", "list" (of Elem), "object" (of Fields), or "ref": the name
	// of a resource of the kind Ref, which the resource then needs first (after).
	Type     string       `json:"type"`
	Elem     *Field       `json:"elem,omitempty"`
	Fields   []Field      `json:"fields,omitempty"`
	Ref      ResourceKind `json:"ref,omitempty"`
	Optional bool         `json:"optional,omitempty"`
	// TypeName names an object type in the generated Pome ("Ingress"); Values lists the strings a
	// field may be (an enum in the generated Pome); Doc says what it is.
	TypeName string   `json:"typeName,omitempty"`
	Values   []string `json:"values,omitempty"`
	Doc      string   `json:"doc,omitempty"`
}

// Field types.
const (
	FieldString = "string"
	FieldInt    = "int"
	FieldBool   = "bool"
	FieldList   = "list"
	FieldObject = "object"
	FieldRef    = "ref"
)

// CheckFields checks a spec's fields: names valid and unique, types known, lists with their
// element, objects with fields, references with a kind.
func CheckFields(fields []Field) error {
	seen := map[string]bool{}
	for _, f := range fields {
		if !validName(f.Name) || seen[f.Name] || f.Name == "name" {
			return fmt.Errorf("field %q: a name of letters, digits, _ and -, once, and not \"name\" (every resource has one)", f.Name)
		}
		seen[f.Name] = true
		if err := checkField(f); err != nil {
			return fmt.Errorf("field %s: %w", f.Name, err)
		}
	}
	return nil
}

func checkField(f Field) error {
	switch f.Type {
	case FieldString:
	case FieldInt, FieldBool:
		if len(f.Values) > 0 {
			return errors.New("values are for strings")
		}
	case FieldList:
		if f.Elem == nil || f.Elem.Type == FieldList {
			return errors.New("a list has an element, which is not a list")
		}
		return checkField(*f.Elem)
	case FieldObject:
		if len(f.Fields) == 0 || f.TypeName == "" {
			return errors.New("an object has fields and a type name")
		}
		seen := map[string]bool{}
		for _, g := range f.Fields {
			if !validName(g.Name) || seen[g.Name] {
				return fmt.Errorf("field %q", g.Name)
			}
			seen[g.Name] = true
			if err := checkField(g); err != nil {
				return fmt.Errorf("%s: %w", g.Name, err)
			}
		}
	case FieldRef:
		if f.Ref == "" {
			return errors.New("a reference names a kind")
		}
	default:
		return fmt.Errorf("type %q is not one of string, int, bool, list, object, ref", f.Type)
	}
	if slices.Contains(f.Values, "") {
		return errors.New("an empty value")
	}
	return nil
}
