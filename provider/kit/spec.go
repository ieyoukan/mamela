package kit

import (
	"reflect"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// SpecOf is the spec schema of a Go struct (ADR 0009), from its exported fields:
//
//   - the json tag names the field; omitempty or a pointer makes it optional;
//   - strings, integers, booleans, slices (lists) and structs (objects, named by their Go type);
//   - `mamela:"ref=AwsVpc"` on a string: the name of a resource of that kind, which the resource
//     needs first (the generated Pome takes the referenced resource and adds it to after);
//   - `values:"a|b"` on a string: the values it may have; `doc:"..."` what it is.
//
// The manifest gives it as the kind's spec; mamela provider pome generates the kind's Emela types
// and functions from it.
func SpecOf(v any) []protocol.Field {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	return fieldsOf(t)
}

func fieldsOf(t reflect.Type) []protocol.Field {
	var out []protocol.Field
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		f := fieldOf(sf.Type)
		f.Name = name
		f.Optional = f.Optional || strings.Contains(","+opts+",", ",omitempty,") || strings.Contains(","+opts+",", ",omitzero,")
		f.Doc = sf.Tag.Get("doc")
		if v := sf.Tag.Get("values"); v != "" {
			f.Values = strings.Split(v, "|")
		}
		if ref, ok := strings.CutPrefix(sf.Tag.Get("mamela"), "ref="); ok {
			switch {
			case f.Type == protocol.FieldString:
				f.Type, f.Ref = protocol.FieldRef, protocol.ResourceKind(ref)
			case f.Type == protocol.FieldList && f.Elem != nil && f.Elem.Type == protocol.FieldString:
				f.Elem = &protocol.Field{Type: protocol.FieldRef, Ref: protocol.ResourceKind(ref)}
			}
		}
		out = append(out, f)
	}
	return out
}

func fieldOf(t reflect.Type) protocol.Field {
	optional := false
	for t.Kind() == reflect.Pointer {
		t, optional = t.Elem(), true
	}
	f := protocol.Field{Optional: optional}
	switch t.Kind() {
	case reflect.String:
		f.Type = protocol.FieldString
	case reflect.Bool:
		f.Type = protocol.FieldBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		f.Type = protocol.FieldInt
	case reflect.Slice, reflect.Array:
		e := fieldOf(t.Elem())
		e.Optional = false
		f.Type, f.Elem = protocol.FieldList, &e
	case reflect.Struct:
		f.Type, f.TypeName, f.Fields = protocol.FieldObject, t.Name(), fieldsOf(t)
	default:
		f.Type = "unsupported:" + t.Kind().String() // refused by the manifest check
	}
	return f
}
