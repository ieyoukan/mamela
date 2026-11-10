package component

import (
	"errors"
	"testing"
)

// build assembles a component with the given sections (id, body).
func build(sections ...[]byte) []byte {
	out := append([]byte(nil), componentPreamble...)
	for _, s := range sections {
		out = append(out, s...)
	}
	return out
}

func section(id byte, body []byte) []byte {
	return append(append([]byte{id}, leb(uint32(len(body)))...), body...)
}

func leb(v uint32) []byte {
	var out []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, c|0x80)
			continue
		}
		return append(out, c)
	}
}

// instanceImport encodes `(import "name" (instance (type idx)))`.
func instanceImport(name string, idx uint32) []byte {
	out := append([]byte{0x00}, leb(uint32(len(name)))...)
	out = append(out, name...)
	out = append(out, 0x05)
	return append(out, leb(idx)...)
}

func importSection(entries ...[]byte) []byte {
	body := leb(uint32(len(entries)))
	for _, e := range entries {
		body = append(body, e...)
	}
	return section(sectionImport, body)
}

func TestImportsReadsTopLevelImports(t *testing.T) {
	wasm := build(
		section(7, []byte{0x00}), // a type section the parser skips
		importSection(instanceImport("wasi:cli/stdout@0.3.0", 0), instanceImport("wasi:cli/exit@0.3.0", 1)),
		section(sectionCustom, []byte{0x01, 'x'}),
	)
	got, err := Imports(wasm)
	if err != nil {
		t.Fatal(err)
	}
	want := []Import{{"wasi:cli/exit@0.3.0", ExternInstance}, {"wasi:cli/stdout@0.3.0", ExternInstance}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("import %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestImportsSkipsNestedComponents(t *testing.T) {
	// A nested component's own imports are satisfied inside the outer component, not by the host.
	nested := build(importSection(instanceImport("wasi:sockets/tcp@0.3.0", 0)))
	wasm := build(section(4, nested), importSection(instanceImport("wasi:cli/stdout@0.3.0", 0)))
	got, err := Imports(wasm)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "wasi:cli/stdout@0.3.0" {
		t.Fatalf("got %v", got)
	}
}

func TestImportsRejectsMalformed(t *testing.T) {
	valid := importSection(instanceImport("wasi:cli/stdout@0.3.0", 0))
	tests := []struct {
		name string
		wasm []byte
	}{
		{"empty", nil},
		{"core module", []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}},
		{"wrong magic", []byte("not wasm at all")},
		{"truncated section", build(valid[:len(valid)-3])},
		{"section size past end", build([]byte{sectionImport, 0x7f, 0x00})},
		{"unknown section id", build(section(13, nil))},
		{"unknown import name form", build(section(sectionImport, append(leb(1), 0x02, 0x01, 'a', 0x05, 0x00)))},
		{"unknown extern kind", build(section(sectionImport, append(leb(1), 0x00, 0x01, 'a', 0x09, 0x00)))},
		{"bad core sort", build(section(sectionImport, append(leb(1), 0x00, 0x01, 'a', 0x00, 0x10, 0x00)))},
		{"leftover bytes in section", build(section(sectionImport, append(leb(0), 0xff)))},
		{"too many imports", build(section(sectionImport, leb(maxImportsCount+1)))},
		{"overlong leb", build([]byte{sectionImport, 0x80, 0x80, 0x80, 0x80, 0x80, 0x00})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := Imports(tt.wasm); err == nil {
				t.Fatalf("accepted malformed input, imports %v", got)
			}
		})
	}
}

func TestNotComponentError(t *testing.T) {
	_, err := Imports([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00})
	if !errors.Is(err, ErrNotComponent) {
		t.Fatalf("got %v, want ErrNotComponent", err)
	}
}

func TestEvaluationPolicy(t *testing.T) {
	ok := []Import{
		{"wasi:cli/exit@0.3.0", ExternInstance}, {"wasi:cli/stderr@0.3.0", ExternInstance},
		{"wasi:cli/stdout@0.3.0", ExternInstance}, {"wasi:cli/types@0.3.0", ExternInstance},
	}
	if err := EvaluationPolicy.Check(ok); err != nil {
		t.Fatalf("rejected the minimal set: %v", err)
	}
	for _, name := range []string{
		"wasi:cli/environment@0.3.0",     // non-deterministic input
		"wasi:random/random@0.3.0",       // non-deterministic input
		"wasi:clocks/system-clock@0.3.0", // non-deterministic input
		"wasi:filesystem/types@0.3.0",    // host files
		"wasi:sockets/types@0.3.0",       // network
		"wasi:http/types@0.3.0",          // network
		"wasi:cli/stdout@0.2.0",          // a different version is a different interface
		"mamela:host/proxmox@0.1.0",      // anything invented
	} {
		err := EvaluationPolicy.Check(append(ok, Import{name, ExternInstance}))
		var denied *DeniedImportError
		if !errors.As(err, &denied) || len(denied.Denied) != 1 || denied.Denied[0].Name != name {
			t.Errorf("%s: got %v, want it denied", name, err)
		}
	}
}
