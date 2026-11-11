package graph

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// machine is a provider's machine ("TestVM" of provider "test") with a network.
func machine(name, network string) string {
	return `{"kind":"TestVM","name":"` + name + `","provider":"test","spec":{"cpu":1,"network":` + network + `}}`
}

const dhcp = `{"mode":"dhcp"}`

// homelab is a machine and what runs on it, in canonical form.
const homelab = `{"resources":[{"kind":"NodePackage","name":"app-server/nginx","spec":{"node":"app-server","package":"nginx","state":"Present"}},` +
	`{"kind":"NodeService","name":"app-server/nginx","spec":{"enabled":true,"node":"app-server","running":true,"service":"nginx"}},` +
	`{"kind":"TestVM","name":"app-server","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}}],"schema":"mamela-graph/v2"}`

func TestDecodeHomelab(t *testing.T) {
	g, err := Decode([]byte(homelab + "\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(g.Resources) != 3 {
		t.Fatalf("got %d resources, want 3", len(g.Resources))
	}
	m := g.Resources[2]
	if m.Kind != "TestVM" || m.Name != "app-server" || m.Provided == nil || m.Provided.Provider != "test" {
		t.Errorf("unexpected machine: %+v", m)
	}
	svc := g.Resources[1].NodeService
	if svc == nil || !svc.Enabled || !svc.Running || svc.Service != "nginx" {
		t.Errorf("unexpected service: %+v", svc)
	}
}

func TestCanonicalIsStable(t *testing.T) {
	g, err := Decode([]byte(homelab))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(g.Canonical()); got != homelab {
		t.Errorf("canonical form differs\n got: %s\nwant: %s", got, homelab)
	}
}

func TestDigestIgnoresDeclarationOrder(t *testing.T) {
	reordered := `{"schema":"mamela-graph/v2","resources":[` +
		`{"spec":{"running":true,"service":"nginx","node":"app-server","enabled":true},"name":"app-server/nginx","kind":"NodeService"},` +
		`{"provider":"test","kind":"TestVM","name":"app-server","spec":{"network":{"mode":"dhcp"},"cpu":1}},` +
		`{"kind":"NodePackage","name":"app-server/nginx","spec":{"state":"Present","package":"nginx","node":"app-server"}}]}`
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
	const want = "sha256:2dfa6ad59c1523afda3d1fa5a986ca780f485f6cba0269a106cc55e0bec2bb20"
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
	pkg := func(spec string) string {
		return `{"schema":"mamela-graph/v1","resources":[{"kind":"NodePackage","name":"a/nginx","spec":` + spec + `}]}`
	}
	okPkg := `{"node":"a","package":"nginx","state":"Present"}`
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
		{"provider declares a built-in kind", `{"schema":"mamela-graph/v2","resources":[{"kind":"NodePackage","name":"a","provider":"kv","spec":{}}]}`, CodeUnknownKind},
		{"provider kind name", `{"schema":"mamela-graph/v2","resources":[{"kind":"kv item","name":"a","provider":"kv","spec":{}}]}`, CodeInvalidName},
		{"provider name", `{"schema":"mamela-graph/v2","resources":[{"kind":"KVItem","name":"a","provider":"KV/../x","spec":{}}]}`, CodeInvalidName},
		{"provided resource name", `{"schema":"mamela-graph/v2","resources":[{"kind":"KVItem","name":"../a","provider":"kv","spec":{}}]}`, CodeInvalidName},
		{"missing resources", `{"schema":"mamela-graph/v1"}`, CodeMissingField},
		{"unknown top-level member", `{"schema":"mamela-graph/v1","resources":[],"extra":1}`, CodeUnknownField},
		{"duplicate top-level member", `{"schema":"mamela-graph/v1","schema":"mamela-graph/v1","resources":[]}`, CodeDuplicateMember},
		{"unknown resource member", `{"schema":"mamela-graph/v1","resources":[{"kind":"NodePackage","name":"a/nginx","spec":` + okPkg + `,"x":1}]}`, CodeUnknownField},
		{"unknown kind", `{"schema":"mamela-graph/v1","resources":[{"kind":"Shell","name":"a","spec":{}}]}`, CodeUnknownKind},
		{"the removed VirtualMachine kind", `{"schema":"mamela-graph/v1","resources":[{"kind":"VirtualMachine","name":"a","spec":{"cpu":1}}]}`, CodeUnknownKind},
		{"missing spec", `{"schema":"mamela-graph/v1","resources":[{"kind":"NodePackage","name":"a/nginx"}]}`, CodeMissingField},
		{"null spec", `{"schema":"mamela-graph/v1","resources":[{"kind":"NodePackage","name":"a/nginx","spec":null}]}`, CodeMissingField},
		{"unknown spec member", pkg(`{"node":"a","package":"nginx","state":"Present","command":"rm -rf /"}`), CodeUnknownField},
		{"duplicate spec member", pkg(`{"node":"a","node":"b","package":"nginx","state":"Present"}`), CodeDuplicateMember},
		{"number for string", pkg(`{"node":"a","package":1,"state":"Present"}`), CodeWrongType},
		{"missing spec field", pkg(`{"node":"a","package":"nginx"}`), CodeMissingField},
		{"null spec field", pkg(`{"node":null,"package":"nginx","state":"Present"}`), CodeMissingField},
		{"invalid utf-8", "{\"schema\":\"mamela-graph/v1\",\"resources\":[{\"kind\":\"NodePackage\",\"name\":\"\xff\",\"spec\":" + okPkg + "}]}", CodeInvalidJSON},
		{"too large", `{"schema":"` + strings.Repeat("a", MaxDocumentBytes) + `"}`, CodeTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.in))
			assertCode(t, err, tt.code)
		})
	}
}

func item(spec string) string {
	return `{"schema":"mamela-graph/v2","resources":[{"kind":"KVItem","name":"web","provider":"kv-example","spec":` + spec + `}]}`
}

// A provider's resource keeps its spec as data, in canonical form; the graph is v2.
func TestProvidedResources(t *testing.T) {
	a, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` +
		`{"kind":"KVItem","name":"web","provider":"kv-example","spec":{"value":"v1","labels":["b","a"],"n":3}},` +
		`{"kind":"NodePackage","name":"vm/nginx","spec":{"node":"vm","package":"nginx","state":"Present"}},` + machine("vm", dhcp) + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Decode([]byte(`{"resources":[{"spec":{"state":"Present","package":"nginx","node":"vm"},"name":"vm/nginx","kind":"NodePackage"},` +
		machine("vm", dhcp) + `,{"spec":{"n":3,"labels":["b","a"],"value":"v1"},"provider":"kv-example","name":"web","kind":"KVItem"}],"schema":"mamela-graph/v2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() != b.Digest() {
		t.Error("member order changed the digest")
	}
	want := `{"resources":[{"kind":"NodePackage","name":"vm/nginx","spec":{"node":"vm","package":"nginx","state":"Present"}},` +
		`{"kind":"KVItem","name":"web","provider":"kv-example","spec":{"labels":["b","a"],"n":3,"value":"v1"}},` +
		`{"kind":"TestVM","name":"vm","provider":"test","spec":{"cpu":1,"network":{"mode":"dhcp"}}}],"schema":"mamela-graph/v2"}`
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

func TestNodeK3s(t *testing.T) {
	doc := func(name, version, sha string) string {
		return `{"schema":"mamela-graph/v2","resources":[` + machine("web", dhcp) + `,` +
			`{"kind":"NodeK3s","name":"` + name + `","spec":{"node":"web","role":"server","sha256":"` + sha + `","version":"` + version + `"}}]}`
	}
	sha := strings.Repeat("ab", 32)
	g, err := Decode([]byte(doc("web/k3s", "v1.37.2+k3s1", sha)))
	if err != nil {
		t.Fatal(err)
	}
	if k := g.Sorted()[0].NodeK3s; k == nil || k.SHA256 != sha {
		t.Fatalf("%+v", g)
	}
	for name, d := range map[string]string{
		"not a release":     doc("web/k3s", "latest", sha),
		"an option":         doc("web/k3s", "--help", sha),
		"a path":            doc("web/k3s", "v1.37.2+k3s1/../../x", sha),
		"a short sha256":    doc("web/k3s", "v1.37.2+k3s1", "abc"),
		"an uppercase hash": doc("web/k3s", "v1.37.2+k3s1", strings.ToUpper(sha)),
		"another name":      doc("web/kube", "v1.37.2+k3s1", sha),
	} {
		if _, err := Decode([]byte(d)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Decode([]byte(strings.Replace(doc("other/k3s", "v1.37.2+k3s1", sha), `"node":"web","role"`, `"node":"other","role"`, 1))); err == nil {
		t.Error("k3s on an undeclared node: accepted")
	}
	vm := func(n string) string { return machine(n, dhcp) }
	k := func(node, role, server string) string {
		srv := ""
		if server != "" {
			srv = `"server":"` + server + `",`
		}
		return `{"kind":"NodeK3s","name":"` + node + `/k3s","spec":{"node":"` + node + `","role":"` + role + `",` + srv + `"sha256":"` + sha + `","version":"v1.37.2+k3s1"}}`
	}
	cluster := func(rs ...string) string {
		return `{"schema":"mamela-graph/v2","resources":[` + strings.Join(rs, ",") + `]}`
	}
	if g, err := Decode([]byte(cluster(vm("s"), vm("a"), k("s", "server", ""), k("a", "agent", "s")))); err != nil || g.Sorted()[0].NodeK3s.Server != "s" {
		t.Errorf("a server and an agent: %v", err)
	}
	for name, d := range map[string]string{
		"an agent of no server":     cluster(vm("s"), vm("a"), k("a", "agent", "s")),
		"an agent of an agent":      cluster(vm("s"), vm("a"), vm("b"), k("s", "server", ""), k("a", "agent", "s"), k("b", "agent", "a")),
		"a server with a server":    cluster(vm("s"), vm("a"), k("s", "server", "a")),
		"an unknown role":           cluster(vm("s"), k("s", "master", "")),
		"an agent of an undeclared": cluster(vm("a"), k("a", "agent", "x")),
	} {
		if _, err := Decode([]byte(d)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	doc := func(resources ...string) string {
		return `{"schema":"mamela-graph/v2","resources":[` + strings.Join(resources, ",") + `]}`
	}
	pkg := func(name, node, pkg, state string) string {
		return `{"kind":"NodePackage","name":"` + name + `","spec":{"node":"` + node + `","package":"` + pkg + `","state":"` + state + `"}}`
	}
	svc := func(name, node, service string) string {
		return `{"kind":"NodeService","name":"` + name + `","spec":{"enabled":true,"node":"` + node + `","running":true,"service":"` + service + `"}}`
	}
	good := machine("app", dhcp)
	tests := []struct {
		name string
		in   string
		code Code
	}{
		{"duplicate machine", doc(good, good), CodeDuplicateResource},
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
	in := `{"schema":"mamela-graph/v2","resources":[` + machine("b", `{"mode":"slaac"}`) + `,` +
		`{"kind":"NodePackage","name":"x/nginx","spec":{"node":"x","package":"nginx","state":"Absent"}}]}`
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

// A machine's network (spec.network of a provider's machine) is DHCP or a complete, coherent
// static address, and each static address is declared once.
func TestNetworks(t *testing.T) {
	doc := func(networks ...string) string {
		var rs []string
		for i, n := range networks {
			rs = append(rs, machine("vm"+itoa(i), n))
		}
		return `{"schema":"mamela-graph/v2","resources":[` + strings.Join(rs, ",") + `]}`
	}
	static := func(addr, gw string, dns ...string) string {
		return `{"mode":"static","address":"` + addr + `","gateway":"` + gw + `","dns":["` + strings.Join(dns, `","`) + `"]}`
	}
	g, err := Decode([]byte(doc(static("192.0.2.10/24", "192.0.2.1", "192.0.2.1", "198.51.100.1"), dhcp)))
	if err != nil {
		t.Fatal(err)
	}
	n, ok := g.Sorted()[0].Network()
	if !ok || n.String() != "static 192.0.2.10/24 via 192.0.2.1 dns 192.0.2.1,198.51.100.1" || g.StaticIP("vm0") != "192.0.2.10" || g.StaticIP("vm1") != "" {
		t.Fatalf("network %q, static IPs %q %q", n, g.StaticIP("vm0"), g.StaticIP("vm1"))
	}
	for _, c := range []struct {
		name, in string
		code     Code
	}{
		{"no mode", doc(`{}`), CodeInvalidValue},
		{"unknown mode", doc(`{"mode":"slaac"}`), CodeInvalidValue},
		{"dhcp with an address", doc(`{"mode":"dhcp","address":"192.0.2.10/24"}`), CodeInvalidValue},
		{"static without dns", doc(`{"mode":"static","address":"192.0.2.10/24","gateway":"192.0.2.1"}`), CodeInvalidValue},
		{"no prefix", doc(static("192.0.2.10", "192.0.2.1", "192.0.2.1")), CodeInvalidValue},
		{"ipv6", doc(static("2001:db8::10/64", "2001:db8::1", "2001:db8::1")), CodeInvalidValue},
		{"network address", doc(static("192.0.2.0/24", "192.0.2.1", "192.0.2.1")), CodeInvalidValue},
		{"broadcast address", doc(static("192.0.2.255/24", "192.0.2.1", "192.0.2.1")), CodeInvalidValue},
		{"gateway in another subnet", doc(static("192.0.2.10/24", "198.51.100.1", "192.0.2.1")), CodeInvalidValue},
		{"gateway is the machine", doc(static("192.0.2.10/24", "192.0.2.10", "192.0.2.1")), CodeInvalidValue},
		{"bad dns", doc(static("192.0.2.10/24", "192.0.2.1", "dns.example")), CodeInvalidValue},
		{"too many dns", doc(static("192.0.2.10/24", "192.0.2.1", "192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4")), CodeInvalidValue},
		{"/31", doc(static("192.0.2.10/31", "192.0.2.11", "192.0.2.1")), CodeInvalidValue},
		{"same address twice", doc(static("192.0.2.10/24", "192.0.2.1", "192.0.2.1"), static("192.0.2.10/24", "192.0.2.1", "192.0.2.1")), CodeDuplicateAddress},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode([]byte(c.in))
			assertCode(t, err, c.code)
		})
	}
	// A site's range.
	first, last := netip.MustParseAddr("192.0.2.40"), netip.MustParseAddr("192.0.2.49")
	in := Network{Mode: NetworkStatic, Address: "192.0.2.49/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.1"}}
	out := Network{Mode: NetworkStatic, Address: "192.0.2.50/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.1"}}
	if in.Allowed(false, first, last) != nil || out.Allowed(true, first, last) == nil ||
		(Network{Mode: NetworkDHCP}).Allowed(false, first, last) == nil || in.Allowed(true, netip.Addr{}, netip.Addr{}) == nil {
		t.Fatal("site range not enforced")
	}
}

// A node resource is on a provider's machine (ADR 0004); a node's name must name one resource.
func TestNodeOnAProvidersMachine(t *testing.T) {
	m := `{"kind":"ProxmoxVM","name":"web","provider":"proxmox","spec":{"cpu":1,"network":{"mode":"static","address":"192.0.2.40/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}}}`
	pkg := `{"kind":"NodePackage","name":"web/nginx","spec":{"node":"web","package":"nginx","state":"Present"}}`
	g, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + m + `,` + pkg + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	if g.StaticIP("web") != "192.0.2.40" {
		t.Fatalf("static IP of a provider's machine: %q", g.StaticIP("web"))
	}
	_, err = Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + m + `,` + machine("web", dhcp) + `,` + pkg + `]}`))
	assertCode(t, err, CodeDanglingNode)
}
