package component

import (
	"errors"
	"fmt"
	"sort"
)

// Sections of the core module binary format.
const coreSectionImport = 2

// ErrNotCoreModule means the bytes are not a WebAssembly core module.
var ErrNotCoreModule = errors.New("not a WebAssembly core module")

// IsCoreModule reports whether the bytes start like a core module (a WASI preview 1 program, such
// as Go's GOOS=wasip1 output).
func IsCoreModule(wasm []byte) bool {
	return len(wasm) >= len(coreModulePrefix) && string(wasm[:len(coreModulePrefix)]) == string(coreModulePrefix)
}

// IsComponent reports whether the bytes start like a component.
func IsComponent(wasm []byte) bool {
	return len(wasm) >= len(componentPreamble) && string(wasm[:len(componentPreamble)]) == string(componentPreamble)
}

// CoreImports returns the imports of a core module, named "module.field" ("wasi_snapshot_preview1.fd_read"),
// sorted by name. A core module reaches the host only through its imports. The parser fails
// closed like Imports.
//
// Format: https://webassembly.github.io/spec/core/binary/modules.html
func CoreImports(wasm []byte) ([]Import, error) {
	if !IsCoreModule(wasm) {
		return nil, ErrNotCoreModule
	}
	r := &reader{b: wasm, off: len(coreModulePrefix)}
	var imports []Import
	for !r.done() {
		id, err := r.byte()
		if err != nil {
			return nil, err
		}
		if id > 13 {
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
		if id != coreSectionImport {
			continue
		}
		if imports != nil {
			return nil, errors.New("two import sections")
		}
		imports, err = parseCoreImportSection(body)
		if err != nil {
			return nil, fmt.Errorf("import section at offset %d: %w", r.off-int(size), err)
		}
	}
	sort.SliceStable(imports, func(i, j int) bool { return imports[i].Name < imports[j].Name })
	return imports, nil
}

// Kinds of core imports.
const (
	ExternCoreFunc   ExternKind = "core-func"
	ExternCoreTable  ExternKind = "core-table"
	ExternCoreMemory ExternKind = "core-memory"
	ExternCoreGlobal ExternKind = "core-global"
)

func parseCoreImportSection(body []byte) ([]Import, error) {
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
		module, err := r.name()
		if err != nil {
			return nil, fmt.Errorf("import %d: %w", i, err)
		}
		field, err := r.name()
		if err != nil {
			return nil, fmt.Errorf("import %d: %w", i, err)
		}
		kind, err := r.coreImportDesc()
		if err != nil {
			return nil, fmt.Errorf("import %d (%s.%s): %w", i, module, field, err)
		}
		out = append(out, Import{Name: module + "." + field, Kind: kind})
	}
	if !r.done() {
		return nil, fmt.Errorf("%d unparsed bytes after the imports", len(r.b)-r.off)
	}
	return out, nil
}

func (r *reader) name() (string, error) {
	n, err := r.u32()
	if err != nil {
		return "", err
	}
	b, err := r.bytes(int(n))
	return string(b), err
}

// coreImportDesc reads importdesc: a function type index, a table, a memory or a global.
func (r *reader) coreImportDesc() (ExternKind, error) {
	tag, err := r.byte()
	if err != nil {
		return "", err
	}
	switch tag {
	case 0x00:
		_, err = r.u32()
		return ExternCoreFunc, err
	case 0x01: // tabletype: reftype limits
		ref, err := r.byte()
		if err != nil {
			return "", err
		}
		if ref != 0x70 && ref != 0x6f {
			return "", fmt.Errorf("unsupported table type 0x%02x", ref)
		}
		return ExternCoreTable, r.limits()
	case 0x02:
		return ExternCoreMemory, r.limits()
	case 0x03: // globaltype: a number type and mutability
		vt, err := r.byte()
		if err != nil {
			return "", err
		}
		if vt < 0x7c || vt > 0x7f {
			return "", fmt.Errorf("unsupported global type 0x%02x", vt)
		}
		mut, err := r.byte()
		if err != nil {
			return "", err
		}
		if mut > 1 {
			return "", fmt.Errorf("unexpected mutability 0x%02x", mut)
		}
		return ExternCoreGlobal, nil
	default:
		return "", fmt.Errorf("unknown import kind 0x%02x", tag)
	}
}

// limits reads limits with a 32-bit minimum and optional maximum (shared and 64-bit memories are
// not accepted).
func (r *reader) limits() error {
	flag, err := r.byte()
	if err != nil {
		return err
	}
	if flag > 1 {
		return fmt.Errorf("unsupported limits flag 0x%02x", flag)
	}
	if _, err := r.u32(); err != nil {
		return err
	}
	if flag == 1 {
		_, err = r.u32()
	}
	return err
}
