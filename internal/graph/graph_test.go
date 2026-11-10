package graph

import (
	"errors"
	"strings"
	"testing"
)

// homelab is exactly what examples/homelab prints (Emela v0.27, WASM target).
const homelab = `{"resources":[{"kind":"VirtualMachine","name":"app-server","spec":{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}},{"kind":"NodePackage","name":"app-server/nginx","spec":{"node":"app-server","package":"nginx","state":"Present"}},{"kind":"NodeService","name":"app-server/nginx","spec":{"enabled":true,"node":"app-server","running":true,"service":"nginx"}}],"schema":"mamela-graph/v1"}`

func TestDecodeHomelab(t *testing.T) {
	g, err := Decode([]byte(homelab + "\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(g.Resources) != 3 {
		t.Fatalf("got %d resources, want 3", len(g.Resources))
	}
	vm := g.Resources[0]
	if vm.Kind != KindVirtualMachine || vm.Name != "app-server" || vm.VirtualMachine.MemoryMiB != 1024 {
		t.Errorf("unexpected VM: %+v %+v", vm, vm.VirtualMachine)
	}
	svc := g.Resources[2].NodeService
	if svc == nil || !svc.Enabled || !svc.Running || svc.Service != "nginx" {
		t.Errorf("unexpected service: %+v", svc)
	}
}

func TestCanonicalMatchesEmelaWriter(t *testing.T) {
	g, err := Decode([]byte(homelab))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(g.Canonical()); got != homelab {
		t.Errorf("canonical form differs from the Emela output\n got: %s\nwant: %s", got, homelab)
	}
}

func TestDigestIgnoresDeclarationOrder(t *testing.T) {
	reordered := `{"schema":"mamela-graph/v1","resources":[` +
		`{"spec":{"running":true,"service":"nginx","node":"app-server","enabled":true},"name":"app-server/nginx","kind":"NodeService"},` +
		`{"kind":"NodePackage","name":"app-server/nginx","spec":{"state":"Present","package":"nginx","node":"app-server"}},` +
		`{"kind":"VirtualMachine","name":"app-server","spec":{"memoryMiB":1024,"image":"ubuntu-24.04","diskGiB":10,"cpu":1}}]}`
	a, err := Decode([]byte(homelab))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Decode([]byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() != b.Digest() {
		t.Errorf("digests differ: %s vs %s", a.Digest(), b.Digest())
	}
	if !strings.HasPrefix(a.Digest(), "sha256:") || len(a.Digest()) != len("sha256:")+64 {
		t.Errorf("malformed digest %q", a.Digest())
	}
}

func TestDigestChangesWithContent(t *testing.T) {
	a, _ := Decode([]byte(homelab))
	b, err := Decode([]byte(strings.Replace(homelab, `"cpu":1`, `"cpu":2`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() == b.Digest() {
		t.Error("changing cpu did not change the digest")
	}
}

func TestDigestIsStable(t *testing.T) {
	// A pinned value: if this changes, every stored revision digest changes. Update only on purpose.
	const want = "sha256:d108bb2c17f95992ec7b6082f65ad9849fc2fd21f39be45b735ce18df41c2ad4"
	for range 3 {
		g, err := Decode([]byte(homelab))
		if err != nil {
			t.Fatal(err)
		}
		if got := g.Digest(); got != want {
			t.Fatalf("digest = %s, want %s", got, want)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	vm := func(spec string) string {
		return `{"schema":"mamela-graph/v1","resources":[{"kind":"VirtualMachine","name":"a","spec":` + spec + `}]}`
	}
	okVM := `{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}`
	tests := []struct {
		name string
		in   string
		code Code
	}{
		{"empty", ``, CodeInvalidJSON},
		{"truncated", homelab[:len(homelab)/2], CodeInvalidJSON},
		{"not json", `hello`, CodeInvalidJSON},
		{"two documents", homelab + "\n" + homelab, CodeTrailingData},
		{"trailing garbage", homelab + "x", CodeTrailingData},
		{"missing schema", `{"resources":[]}`, CodeMissingField},
		{"unknown schema", `{"schema":"mamela-graph/v3","resources":[]}`, CodeUnknownSchema},
		{"provider in v1", `{"schema":"mamela-graph/v1","resources":[{"kind":"KVItem","name":"a","provider":"kv","spec":{}}]}`, CodeNeedsV2},
		{"provider spec not an object", item(`[1]`), CodeWrongType},
		{"provider spec float", item(`{"n":1.5}`), CodeWrongType},
		{"provider spec integer beyond 2^53", item(`{"n":9007199254740993}`), CodeWrongType},
		{"provider spec integer below -2^53", item(`{"n":-9007199254740993}`), CodeWrongType},
		{"provider spec null", item(`{"n":null}`), CodeWrongType},
		{"provider spec duplicate", item(`{"n":1,"n":2}`), CodeDuplicateMember},
		{"provider declares a built-in kind", `{"schema":"mamela-graph/v2","resources":[{"kind":"VirtualMachine","name":"a","provider":"kv","spec":{}}]}`, CodeUnknownKind},
		{"provider kind name", `{"schema":"mamela-graph/v2","resources":[{"kind":"kv item","name":"a","provider":"kv","spec":{}}]}`, CodeInvalidName},
		{"provider name", `{"schema":"mamela-graph/v2","resources":[{"kind":"KVItem","name":"a","provider":"KV/../x","spec":{}}]}`, CodeInvalidName},
		{"provided resource name", `{"schema":"mamela-graph/v2","resources":[{"kind":"KVItem","name":"../a","provider":"kv","spec":{}}]}`, CodeInvalidName},
		{"missing resources", `{"schema":"mamela-graph/v1"}`, CodeMissingField},
		{"unknown top-level member", `{"schema":"mamela-graph/v1","resources":[],"extra":1}`, CodeUnknownField},
		{"duplicate top-level member", `{"schema":"mamela-graph/v1","schema":"mamela-graph/v1","resources":[]}`, CodeDuplicateMember},
		{"unknown resource member", `{"schema":"mamela-graph/v1","resources":[{"kind":"VirtualMachine","name":"a","spec":` + okVM + `,"x":1}]}`, CodeUnknownField},
		{"unknown kind", `{"schema":"mamela-graph/v1","resources":[{"kind":"Shell","name":"a","spec":{}}]}`, CodeUnknownKind},
		{"missing spec", `{"schema":"mamela-graph/v1","resources":[{"kind":"VirtualMachine","name":"a"}]}`, CodeMissingField},
		{"null spec", `{"schema":"mamela-graph/v1","resources":[{"kind":"VirtualMachine","name":"a","spec":null}]}`, CodeMissingField},
		{"unknown spec member", vm(`{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024,"command":"rm -rf /"}`), CodeUnknownField},
		{"duplicate spec member", vm(`{"cpu":1,"cpu":64,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}`), CodeDuplicateMember},
		{"float", vm(`{"cpu":1.5,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}`), CodeWrongType},
		{"exponent", vm(`{"cpu":1,"diskGiB":1e1,"image":"ubuntu-24.04","memoryMiB":1024}`), CodeWrongType},
		{"string for number", vm(`{"cpu":"1","diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}`), CodeWrongType},
		{"missing spec field", vm(`{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04"}`), CodeMissingField},
		{"null spec field", vm(`{"cpu":null,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}`), CodeMissingField},
		{"invalid utf-8", "{\"schema\":\"mamela-graph/v1\",\"resources\":[{\"kind\":\"VirtualMachine\",\"name\":\"\xff\",\"spec\":" + okVM + "}]}", CodeInvalidJSON},
		{"too large", `{"schema":"` + strings.Repeat("a", MaxDocumentBytes) + `"}`, CodeTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.in))
			assertCode(t, err, tt.code)
		})
	}
}

const providedVM = `{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}`

func item(spec string) string {
	return `{"schema":"mamela-graph/v2","resources":[{"kind":"KVItem","name":"web","provider":"kv-example","spec":` + spec + `}]}`
}

// A provider's resource keeps its spec as data, in canonical form; the graph is v2.
func TestProvidedResources(t *testing.T) {
	a, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
		`{"kind":"KVItem","name":"web","provider":"kv-example","spec":{"value":"v1","labels":["b","a"],"n":3}},` +
		`{"kind":"VirtualMachine","name":"vm","spec":` + providedVM + `}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Decode([]byte(`{"resources":[{"spec":` + providedVM + `,"name":"vm","kind":"VirtualMachine"},` +
		`{"spec":{"n":3,"labels":["b","a"],"value":"v1"},"provider":"kv-example","name":"web","kind":"KVItem"}],"schema":"mamela-graph/v2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() != b.Digest() {
		t.Error("member order changed the digest")
	}
	want := `{"resources":[{"kind":"VirtualMachine","name":"vm","spec":{"cpu":1,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}},` +
		`{"kind":"KVItem","name":"web","provider":"kv-example","spec":{"labels":["b","a"],"n":3,"value":"v1"}}],"schema":"mamela-graph/v2"}`
	if got := string(a.Canonical()); got != want {
		t.Errorf("canonical:\n got %s\nwant %s", got, want)
	}
	again, err := Decode(a.Canonical())
	if err != nil || again.Digest() != a.Digest() {
		t.Errorf("the canonical form does not decode to the same graph: %v", err)
	}
	other, _ := Decode([]byte(item(`{"value":"v2"}`)))
	same, _ := Decode([]byte(item(`{"value":"v1"}`)))
	if other.Digest() == same.Digest() {
		t.Error("a different spec has the same digest")
	}
}

func TestValidateRejects(t *testing.T) {
	doc := func(resources ...string) string {
		return `{"schema":"mamela-graph/v1","resources":[` + strings.Join(resources, ",") + `]}`
	}
	vm := func(name string, cpu, mem, disk int, image string) string {
		return `{"kind":"VirtualMachine","name":"` + name + `","spec":{"cpu":` + itoa(cpu) + `,"diskGiB":` + itoa(disk) +
			`,"image":"` + image + `","memoryMiB":` + itoa(mem) + `}}`
	}
	pkg := func(name, node, pkg, state string) string {
		return `{"kind":"NodePackage","name":"` + name + `","spec":{"node":"` + node + `","package":"` + pkg + `","state":"` + state + `"}}`
	}
	svc := func(name, node, service string) string {
		return `{"kind":"NodeService","name":"` + name + `","spec":{"enabled":true,"node":"` + node + `","running":true,"service":"` + service + `"}}`
	}
	good := vm("app", 1, 1024, 10, "ubuntu-24.04")
	tests := []struct {
		name string
		in   string
		code Code
	}{
		{"uppercase vm name", doc(vm("App", 1, 1024, 10, "ubuntu-24.04")), CodeInvalidName},
		{"vm name with space", doc(vm("a b", 1, 1024, 10, "ubuntu-24.04")), CodeInvalidName},
		{"zero cpu", doc(vm("app", 0, 1024, 10, "ubuntu-24.04")), CodeOutOfRange},
		{"too much cpu", doc(vm("app", 65, 1024, 10, "ubuntu-24.04")), CodeOutOfRange},
		{"too little memory", doc(vm("app", 1, 128, 10, "ubuntu-24.04")), CodeOutOfRange},
		{"negative disk", doc(vm("app", 1, 1024, -1, "ubuntu-24.04")), CodeOutOfRange},
		{"bad image", doc(vm("app", 1, 1024, 10, "../../etc/passwd")), CodeInvalidValue},
		{"duplicate vm", doc(good, good), CodeDuplicateResource},
		{"dangling package node", doc(good, pkg("other/nginx", "other", "nginx", "Present")), CodeDanglingNode},
		{"package removal", doc(good, pkg("app/nginx", "app", "nginx", "Absent")), CodeInvalidValue},
		{"bad package name", doc(good, pkg("app/nginx;rm", "app", "nginx;rm", "Present")), CodeInvalidValue},
		{"package name mismatch", doc(good, pkg("app/apache2", "app", "nginx", "Present")), CodeNameMismatch},
		{"dangling service node", doc(good, svc("other/nginx", "other", "nginx")), CodeDanglingNode},
		{"bad service name", doc(good, svc("app/a b", "app", "a b")), CodeInvalidValue},
		{"service with path", doc(good, svc("app/../x", "app", "../x")), CodeInvalidValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.in))
			assertCode(t, err, tt.code)
		})
	}
}

func TestValidateReportsAllProblemsInOrder(t *testing.T) {
	in := `{"schema":"mamela-graph/v1","resources":[` +
		`{"kind":"VirtualMachine","name":"B","spec":{"cpu":0,"diskGiB":10,"image":"ubuntu-24.04","memoryMiB":1024}},` +
		`{"kind":"NodePackage","name":"x/nginx","spec":{"node":"x","package":"nginx","state":"Present"}}]}`
	_, err := Decode([]byte(in))
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %v", err)
	}
	if len(e.Problems) != 3 {
		t.Fatalf("want 3 problems, got %d:\n%v", len(e.Problems), err)
	}
	// Same input, same order and text.
	_, err2 := Decode([]byte(in))
	if err.Error() != err2.Error() {
		t.Errorf("problem order is not deterministic:\n%v\n---\n%v", err, err2)
	}
	for _, p := range e.Problems {
		if p.Path == "" || p.Message == "" {
			t.Errorf("problem without location or message: %+v", p)
		}
	}
}

func assertCode(t *testing.T, err error, code Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted; want %s", code)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if !e.Has(code) {
		t.Fatalf("want %s, got:\n%v", code, err)
	}
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
