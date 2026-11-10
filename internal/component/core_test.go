package component

import (
	"slices"
	"testing"
)

// coreModule assembles a core module with one import section of function imports.
func coreModule(imports ...[2]string) []byte {
	body := leb(uint32(len(imports)))
	for _, im := range imports {
		for _, s := range im {
			body = append(append(body, leb(uint32(len(s)))...), s...)
		}
		body = append(body, 0x00, 0x00) // a function of type 0
	}
	out := append([]byte(nil), coreModulePrefix...)
	out = append(out, 0x01, 0x01, 0x00) // a type section the parser skips
	return append(append(append(out, coreSectionImport), leb(uint32(len(body)))...), body...)
}

func TestCoreImports(t *testing.T) {
	got, err := CoreImports(coreModule([2]string{"wasi_snapshot_preview1", "fd_write"}, [2]string{"wasi_snapshot_preview1", "args_get"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []Import{{"wasi_snapshot_preview1.args_get", ExternCoreFunc}, {"wasi_snapshot_preview1.fd_write", ExternCoreFunc}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Anything the parser does not understand is an error, never an empty import list.
func TestCoreImportsFailsClosed(t *testing.T) {
	good := coreModule([2]string{"wasi_snapshot_preview1", "fd_write"})
	cases := map[string][]byte{
		"a component":         build(),
		"truncated":           good[:len(good)-1],
		"unknown import kind": append(good[:len(good)-2:len(good)-2], 0x09, 0x00),
		"unknown section":     append(append([]byte(nil), coreModulePrefix...), 0x20, 0x00),
		"two import sections": append(append([]byte(nil), good...), good[len(coreModulePrefix)+3:]...),
	}
	for name, wasm := range cases {
		if got, err := CoreImports(wasm); err == nil {
			t.Errorf("%s: accepted, imports %v", name, got)
		}
	}
	if IsCoreModule(build()) || !IsComponent(build()) || !IsCoreModule(good) || IsComponent(good) {
		t.Error("format detection")
	}
}
