// Package component reads the top-level imports of a WebAssembly component.
//
// A component can only reach the host through what it imports at the top level, so the import
// list is the complete set of host capabilities it asks for. Mamela checks that list against an
// allowlist before running an evaluation. The parser fails closed: anything it does not
// understand is an error, never "probably fine".
//
// Format: https://github.com/WebAssembly/component-model/blob/main/design/mvp/Binary.md
package component

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Section ids of the component binary format.
const (
	sectionCustom   = 0
	sectionImport   = 10
	maxSectionID    = 12
	maxImportsCount = 1024
)

var (
	componentPreamble = []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00}
	coreModulePrefix  = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
)

// ExternKind is the sort of an imported item.
type ExternKind string

const (
	ExternCoreModule ExternKind = "core-module"
	ExternFunc       ExternKind = "func"
	ExternValue      ExternKind = "value"
	ExternType       ExternKind = "type"
	ExternComponent  ExternKind = "component"
	ExternInstance   ExternKind = "instance"
)

// Import is one top-level import.
type Import struct {
	Name string
	Kind ExternKind
}

// ErrNotComponent means the bytes are not a WebAssembly component.
var ErrNotComponent = errors.New("not a WebAssembly component")

// Imports returns the top-level imports of the component, sorted by name.
func Imports(wasm []byte) ([]Import, error) {
	if len(wasm) >= len(coreModulePrefix) && string(wasm[:len(coreModulePrefix)]) == string(coreModulePrefix) {
		return nil, fmt.Errorf("%w: it is a core module (WASI preview 1), not a component", ErrNotComponent)
	}
	if len(wasm) < len(componentPreamble) || string(wasm[:len(componentPreamble)]) != string(componentPreamble) {
		return nil, ErrNotComponent
	}
	r := &reader{b: wasm, off: len(componentPreamble)}
	var imports []Import
	for !r.done() {
		id, err := r.byte()
		if err != nil {
			return nil, err
		}
		if id > maxSectionID {
			return nil, fmt.Errorf("unknown section id %d at offset %d", id, r.off-1)
		}
		size, err := r.u32()
		if err != nil {
			return nil, err
		}
		body, err := r.bytes(int(size))
		if err != nil {
			return nil, fmt.Errorf("section %d: %w", id, err)
		}
		if id != sectionImport {
			continue
		}
		got, err := parseImportSection(body)
		if err != nil {
			return nil, fmt.Errorf("import section at offset %d: %w", r.off-int(size), err)
		}
		imports = append(imports, got...)
		if len(imports) > maxImportsCount {
			return nil, fmt.Errorf("more than %d imports", maxImportsCount)
		}
	}
	sort.SliceStable(imports, func(i, j int) bool { return imports[i].Name < imports[j].Name })
	return imports, nil
}

func parseImportSection(body []byte) ([]Import, error) {
	r := &reader{b: body}
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	if n > maxImportsCount {
		return nil, fmt.Errorf("%d imports, more than %d", n, maxImportsCount)
	}
	out := make([]Import, 0, n)
	for i := uint32(0); i < n; i++ {
		name, err := r.importName()
		if err != nil {
			return nil, fmt.Errorf("import %d: %w", i, err)
		}
		kind, err := r.externDesc()
		if err != nil {
			return nil, fmt.Errorf("import %d (%s): %w", i, name, err)
		}
		out = append(out, Import{Name: name, Kind: kind})
	}
	if !r.done() {
		return nil, fmt.Errorf("%d unparsed bytes after the imports", len(r.b)-r.off)
	}
	return out, nil
}

// Policy is an exact allowlist of import names (including the version suffix).
type Policy struct {
	Allowed []string
}

// EvaluationPolicy allows only what a graph evaluation needs: writing stdout/stderr and exiting.
// It deliberately excludes environment, random and clocks so that evaluation is deterministic,
// and filesystem, sockets and HTTP so that it cannot reach anything.
//
// wasi:cli/types holds only the error-code enum that stdout and stderr use (no functions), and
// Emela v0.27 output imports it alongside them.
var EvaluationPolicy = Policy{Allowed: []string{
	"wasi:cli/exit@0.3.0",
	"wasi:cli/stderr@0.3.0",
	"wasi:cli/stdout@0.3.0",
	"wasi:cli/types@0.3.0",
}}

// DeniedImportError lists the imports outside the allowlist.
type DeniedImportError struct {
	Denied  []Import
	Allowed []string
}

func (e *DeniedImportError) Error() string {
	names := make([]string, len(e.Denied))
	for i, d := range e.Denied {
		names[i] = d.Name
	}
	return fmt.Sprintf("the component imports %s, which evaluation does not allow (allowed: %s)",
		strings.Join(names, ", "), strings.Join(e.Allowed, ", "))
}

// Check returns a *DeniedImportError if any import is not in the allowlist.
func (p Policy) Check(imports []Import) error {
	allowed := make(map[string]bool, len(p.Allowed))
	for _, a := range p.Allowed {
		allowed[a] = true
	}
	var denied []Import
	for _, im := range imports {
		if !allowed[im.Name] {
			denied = append(denied, im)
		}
	}
	if len(denied) > 0 {
		return &DeniedImportError{Denied: denied, Allowed: append([]string(nil), p.Allowed...)}
	}
	return nil
}

type reader struct {
	b   []byte
	off int
}

func (r *reader) done() bool { return r.off >= len(r.b) }

var errTruncated = errors.New("truncated")

func (r *reader) byte() (byte, error) {
	if r.off >= len(r.b) {
		return 0, errTruncated
	}
	c := r.b[r.off]
	r.off++
	return c, nil
}

func (r *reader) bytes(n int) ([]byte, error) {
	if n < 0 || r.off+n > len(r.b) {
		return nil, errTruncated
	}
	out := r.b[r.off : r.off+n]
	r.off += n
	return out, nil
}

// u32 reads an unsigned LEB128 value of at most 5 bytes.
func (r *reader) u32() (uint32, error) {
	var v uint64
	for shift := 0; shift < 35; shift += 7 {
		c, err := r.byte()
		if err != nil {
			return 0, err
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			if v > 0xffffffff {
				return 0, errors.New("u32 out of range")
			}
			return uint32(v), nil
		}
	}
	return 0, errors.New("u32 too long")
}

// importName reads importname'. Only the plain forms are accepted.
func (r *reader) importName() (string, error) {
	tag, err := r.byte()
	if err != nil {
		return "", err
	}
	if tag != 0x00 && tag != 0x01 {
		return "", fmt.Errorf("unsupported import name form 0x%02x", tag)
	}
	n, err := r.u32()
	if err != nil {
		return "", err
	}
	b, err := r.bytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// externDesc reads externdesc and returns its kind.
func (r *reader) externDesc() (ExternKind, error) {
	tag, err := r.byte()
	if err != nil {
		return "", err
	}
	switch tag {
	case 0x00: // core module: 0x00 0x11 typeidx
		sub, err := r.byte()
		if err != nil {
			return "", err
		}
		if sub != 0x11 {
			return "", fmt.Errorf("unexpected core sort 0x%02x", sub)
		}
		_, err = r.u32()
		return ExternCoreModule, err
	case 0x01:
		_, err = r.u32()
		return ExternFunc, err
	case 0x02: // value bound: 0x00 valueidx | 0x01 valtype
		b, err := r.byte()
		if err != nil {
			return "", err
		}
		if b != 0x00 && b != 0x01 {
			return "", fmt.Errorf("unexpected value bound 0x%02x", b)
		}
		_, err = r.u32() // valueidx, or valtype (a primitive byte or a type index)
		return ExternValue, err
	case 0x03: // type bound: 0x00 typeidx | 0x01 (sub resource)
		b, err := r.byte()
		if err != nil {
			return "", err
		}
		switch b {
		case 0x00:
			_, err = r.u32()
			return ExternType, err
		case 0x01:
			return ExternType, nil
		default:
			return "", fmt.Errorf("unexpected type bound 0x%02x", b)
		}
	case 0x04:
		_, err = r.u32()
		return ExternComponent, err
	case 0x05:
		_, err = r.u32()
		return ExternInstance, err
	default:
		return "", fmt.Errorf("unknown extern kind 0x%02x", tag)
	}
}
