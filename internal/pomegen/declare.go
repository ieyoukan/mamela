package pomegen

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// Imports are a provider's imported declarations (mamela import terraform, ADR 0009).
type Imports struct {
	Manifest protocol.Manifest
	// Module is the provider Pome's generated Emela module ("Kv", or "Aws.Kinds" when the root
	// module is written by hand), whose functions declare them.
	Module    string
	Resources []protocol.Imported
}

// DeclareOptions name the output.
type DeclareOptions struct {
	// Source says where the declarations come from, for the file's comment ("a Terraform state").
	Source string
}

// Declarations writes an Emela module (src/imported.emel) with one public function, imported,
// that declares the imported resources with their providers' generated Pome functions: each
// resource a binding of its type, a reference the binding it refers to, in an order where what is
// referred to comes first. The output is deterministic.
func Declarations(all []Imports, o DeclareOptions) ([]byte, error) {
	var decls []*importDecl
	byRef := map[string]*importDecl{} // "Kind name"
	uses := map[string]map[string]bool{}
	vars := map[string]bool{}
	called := map[string]string{}
	for _, p := range all {
		if other, ok := called[lastSegment(p.Module)]; ok && other != p.Module {
			return nil, fmt.Errorf("modules %s and %s are both called %s: import them in two files", other, p.Module, lastSegment(p.Module))
		}
		called[lastSegment(p.Module)] = p.Module
		for _, seg := range strings.Split(p.Module, ".") {
			if !validUpper(seg) {
				return nil, fmt.Errorf("provider %s: module %q", p.Manifest.Provider, p.Module)
			}
		}
		for _, r := range p.Resources {
			i := slices.IndexFunc(p.Manifest.Kinds, func(k protocol.Kind) bool { return k.Kind == r.Kind })
			if i < 0 || len(p.Manifest.Kinds[i].Spec) == 0 {
				return nil, fmt.Errorf("%s: kind %s of provider %s gives no spec: its Pome has no function to declare it", r.Address, r.Kind, p.Manifest.Provider)
			}
			_, fn := Names(r.Kind)
			v := fn + "_" + snake(strings.Map(func(c rune) rune {
				if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
					return c
				}
				return '_'
			}, r.Name))
			v = strings.Trim(strings.ReplaceAll(v, "__", "_"), "_")
			base := v
			for n := 2; vars[v] || reserved[v] || !validLower(v); n++ {
				v = base + "_" + strconv.Itoa(n)
			}
			vars[v] = true
			d := &importDecl{imp: r, module: p.Module, kind: p.Manifest.Kinds[i], v: v}
			key := string(r.Kind) + " " + r.Name
			if _, dup := byRef[key]; dup {
				return nil, fmt.Errorf("%s is declared twice", key)
			}
			byRef[key] = d
			decls = append(decls, d)
		}
	}
	slices.SortFunc(decls, func(a, b *importDecl) int { return strings.Compare(a.imp.Address, b.imp.Address) })

	// Each declaration as Emela, and what it refers to.
	exprs := map[*importDecl]string{}
	deps := map[*importDecl][]*importDecl{}
	for _, d := range decls {
		w := &valueWriter{module: d.module, uses: uses, byRef: byRef}
		typeName, _ := Names(d.kind.Kind)
		e, err := w.record(typeName, d.kind.Spec, d.imp.Spec, true, d.imp.Name)
		if err != nil {
			return nil, fmt.Errorf("%s (%s %s): %w", d.imp.Address, d.kind.Kind, d.imp.Name, err)
		}
		w.use(typeName)
		exprs[d] = e
		for _, ref := range w.refs {
			deps[d] = append(deps[d], byRef[ref])
		}
	}
	// What is referred to first; the order of the addresses otherwise.
	var order []*importDecl
	state := map[*importDecl]int{}
	var visit func(d *importDecl, path []string) error
	visit = func(d *importDecl, path []string) error {
		switch state[d] {
		case 1:
			return fmt.Errorf("the imported resources refer to each other in a cycle: %s", strings.Join(append(path, d.imp.Address), " -> "))
		case 2:
			return nil
		}
		state[d] = 1
		for _, x := range deps[d] {
			if err := visit(x, append(path, d.imp.Address)); err != nil {
				return err
			}
		}
		state[d] = 2
		order = append(order, d)
		return nil
	}
	for _, d := range decls {
		if err := visit(d, nil); err != nil {
			return nil, err
		}
	}

	var b strings.Builder
	source := o.Source
	if source == "" {
		source = "another tool"
	}
	fmt.Fprintf(&b, "## The resources imported from %s by `mamela import terraform`, declared as they are.\n", source)
	b.WriteString("## It is yours now: edit it, and add `|> Imported.imported` to the declaration. Each resource\n")
	b.WriteString("## becomes Mamela's only when a person adopts it (`mamela act --confirm <name> <adopt-op> ...`).\n\n")
	b.WriteString("import Mamela\n")
	for _, m := range slices.Sorted(maps.Keys(uses)) {
		names := slices.Sorted(maps.Keys(uses[m]))
		fmt.Fprintf(&b, "import %s.{%s}\n", m, strings.Join(names, ", "))
	}
	b.WriteString("\npub fn imported(graph: Mamela.Graph) -> Mamela.Graph {\n")
	for _, d := range order {
		fmt.Fprintf(&b, "  %s = %s\n", d.v, exprs[d])
	}
	b.WriteString("  graph")
	for _, d := range order {
		_, fn := Names(d.kind.Kind)
		fmt.Fprintf(&b, "\n    |> %s.%s(%s)", lastSegment(d.module), fn, d.v)
	}
	b.WriteString("\n}\n")
	return []byte(b.String()), nil
}

type importDecl struct {
	imp    protocol.Imported
	module string
	kind   protocol.Kind
	v      string // the binding
}

type valueWriter struct {
	module string
	uses   map[string]map[string]bool
	byRef  map[string]*importDecl
	refs   []string
}

func (w *valueWriter) use(name string) {
	if w.uses[w.module] == nil {
		w.uses[w.module] = map[string]bool{}
	}
	w.uses[w.module][name] = true
}

// record writes a record of the fields from a JSON object: Type(name: "...", field: value, ...).
func (w *valueWriter) record(typeName string, fields []protocol.Field, raw jsontext.Value, withName bool, name string) (string, error) {
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return "", fmt.Errorf("spec: %w", err)
	}
	known := map[string]bool{}
	var parts []string
	if withName {
		parts = append(parts, "name: "+literal(name))
	}
	for _, f := range fields {
		known[f.Name] = true
		v, present := members[f.Name]
		if present && string(v) == "null" {
			present = false
		}
		var e string
		switch {
		case !present && f.Type == protocol.FieldList:
			e = "[]"
		case !present && f.Optional:
			e = "None"
		case !present:
			return "", fmt.Errorf("the spec has no %s, which the kind requires", f.Name)
		default:
			var err error
			if e, err = w.value(typeName, f, v); err != nil {
				return "", fmt.Errorf("%s: %w", f.Name, err)
			}
			if f.Optional && f.Type != protocol.FieldList {
				e = "Some(" + e + ")"
			}
		}
		parts = append(parts, emelaField(f.Name)+": "+e)
	}
	for _, k := range slices.Sorted(maps.Keys(members)) {
		if !known[k] {
			return "", fmt.Errorf("the spec has %s, which the kind does not declare", k)
		}
	}
	return typeName + "(" + strings.Join(parts, ", ") + ")", nil
}

func (w *valueWriter) value(owner string, f protocol.Field, raw jsontext.Value) (string, error) {
	switch f.Type {
	case protocol.FieldString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("not a string: %s", raw)
		}
		if len(f.Values) == 0 {
			return literal(s), nil
		}
		i := slices.Index(f.Values, s)
		if i < 0 {
			return "", fmt.Errorf("%q is not one of %s", s, strings.Join(f.Values, ", "))
		}
		vs, err := variants(f.Values)
		if err != nil {
			return "", err
		}
		w.use(vs[i])
		return vs[i], nil
	case protocol.FieldInt:
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n != math.Trunc(n) || n > math.MaxInt32 || n < -math.MaxInt32 {
			return "", fmt.Errorf("not an integer of Emela's Int: %s", raw)
		}
		return strconv.FormatInt(int64(n), 10), nil
	case protocol.FieldBool:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("not a boolean: %s", raw)
		}
		if v {
			return "True", nil
		}
		return "False", nil
	case protocol.FieldRef:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("not the name of a %s: %s", f.Ref, raw)
		}
		key := string(f.Ref) + " " + s
		d, ok := w.byRef[key]
		if !ok {
			return "", fmt.Errorf("refers to %s, which is not imported: import it too (or declare it and refer to it by hand)", key)
		}
		w.refs = append(w.refs, key)
		return d.v, nil
	case protocol.FieldList:
		var items []jsontext.Value
		if err := json.Unmarshal(raw, &items); err != nil {
			return "", fmt.Errorf("not a list: %s", raw)
		}
		e := *f.Elem
		e.Name, e.Optional = f.Name, false
		var out []string
		for _, it := range items {
			s, err := w.value(owner, e, it)
			if err != nil {
				return "", err
			}
			out = append(out, s)
		}
		return "[" + strings.Join(out, ", ") + "]", nil
	case protocol.FieldObject:
		name := upper(f.TypeName)
		w.use(name)
		return w.record(name, f.Fields, raw, false, "")
	}
	return "", fmt.Errorf("type %s", f.Type)
}

// literal is an Emela string literal of s.
func literal(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '#':
			b.WriteString(`\#`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u{%x}`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// lastSegment is the name a module is called by once imported ("Aws.Kinds": Kinds).
func lastSegment(module string) string {
	return module[strings.LastIndex(module, ".")+1:]
}
