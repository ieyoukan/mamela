package graph

import (
	"encoding/json/v2"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
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
		{"a machine declared twice differently", doc(good, machine("app", `{"mode":"static","address":"192.0.2.10/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}`)), CodeDuplicateResource},
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
	// Which resources of a name are machines is for the providers' manifests to say (a machine
	// "web" may have a disk "web"): the graph accepts several of one name, and CheckGraph refuses
	// a name that is two machines once the providers run (B14 of the 0.3.2 audit).
	g, err = Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + m + `,` + `{"kind":"Disk","name":"web","provider":"proxmox","spec":{"gib":10}}` + `,` + pkg + `]}`))
	if err != nil {
		t.Fatalf("a machine and a disk of one name: %v", err)
	}
	if g.StaticIP("web") != "192.0.2.40" {
		t.Errorf("static IP of the machine that has a disk of its name: %q", g.StaticIP("web"))
	}
	// A node that no resource of a provider has is still refused.
	_, err = Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + m + `,` + strings.Replace(pkg, "web", "db", -1) + `]}`))
	assertCode(t, err, CodeDanglingNode)
}

// A resource of a provider may need others first (ADR 0007): sorted and unique in the canonical
// form; each must be a resource of a provider in the graph; no cycle; only providers' resources.
func TestAfter(t *testing.T) {
	r := func(name, after string) string {
		a := ""
		if after != "" {
			a = `,"after":[` + after + `]`
		}
		return `{"kind":"Thing","name":"` + name + `","provider":"test","spec":{}` + a + `}`
	}
	doc := func(rs ...string) string {
		return `{"schema":"mamela-graph/v2","resources":[` + strings.Join(rs, ",") + `]}`
	}
	g, err := Decode([]byte(doc(r("a", ""), r("b", `"Thing a"`), r("c", `"Thing b","Thing a","Thing b"`))))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(g.Canonical()); !strings.Contains(got, `{"after":["Thing a","Thing b"],"kind":"Thing","name":"c","provider":"test","spec":{}}`) {
		t.Fatalf("canonical: %s", got)
	}
	again, _ := Decode(g.Canonical())
	without, _ := Decode([]byte(doc(r("a", ""), r("b", ""), r("c", ""))))
	if again.Digest() != g.Digest() || without.Digest() == g.Digest() {
		t.Error("after is not part of the digest, or does not round-trip")
	}
	for name, c := range map[string]struct {
		in   string
		code Code
	}{
		"an undeclared resource": {doc(r("a", `"Thing x"`)), CodeDanglingNode},
		"itself":                 {doc(r("a", `"Thing a"`)), CodeCycle},
		"a cycle":                {doc(r("a", `"Thing b"`), r("b", `"Thing c"`), r("c", `"Thing a"`)), CodeCycle},
		"a built-in resource": {`{"schema":"mamela-graph/v2","resources":[` + machine("web", dhcp) +
			`,{"kind":"NodePackage","name":"web/nginx","after":["TestVM web"],"spec":{"node":"web","package":"nginx","state":"Present"}}]}`, CodeUnknownField},
		"a built-in reference": {`{"schema":"mamela-graph/v2","resources":[` + machine("web", dhcp) + `,` + r("a", `"NodePackage web/nginx"`) +
			`,{"kind":"NodePackage","name":"web/nginx","spec":{"node":"web","package":"nginx","state":"Present"}}]}`, CodeDanglingNode},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(c.in))
			assertCode(t, err, c.code)
		})
	}
}

// A resource declared twice the same way is one resource (a provider's Pome declares, with each VM,
// the template it is cloned from); declared twice differently, it is refused.
func TestSameDeclarationTwiceIsOne(t *testing.T) {
	m := machine("app", `{"mode":"dhcp"}`)
	g, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + m + `,` + m + `]}`))
	if err != nil || len(g.Resources) != 1 {
		t.Fatalf("the same declaration twice: %v %v", g, err)
	}
	other := machine("app", `{"mode":"static","address":"192.0.2.10/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}`)
	if _, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + m + `,` + other + `]}`)); err == nil || !strings.Contains(err.Error(), "MAM-GRAPH-0102") {
		t.Fatalf("two different declarations: %v", err)
	}
}

// A manifest is a file of a k3s server's auto-deploy directory: the name is a file stem, the
// content is bounded text, and the node is a k3s server of the graph.
func TestNodeK3sManifest(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	server := `{"kind":"NodeK3s","name":"web/k3s","spec":{"node":"web","role":"server","sha256":"` + sha + `","version":"v1.37.2+k3s1"}}`
	doc := func(node, name, content string) string {
		c, _ := json.Marshal(content)
		return `{"schema":"mamela-graph/v2","resources":[` + machine("web", dhcp) + `,` + machine("other", dhcp) + `,` + server + `,` +
			`{"kind":"NodeK3sManifest","name":"` + node + `/manifest/` + name + `","spec":{"node":"` + node + `","name":"` + name + `","content":` + string(c) + `}}]}`
	}
	g, err := Decode([]byte(doc("web", "argocd", "kind: Namespace\n")))
	if err != nil {
		t.Fatal(err)
	}
	var m *NodeK3sManifestSpec
	for _, r := range g.Sorted() {
		if r.NodeK3sManifest != nil {
			m = r.NodeK3sManifest
		}
	}
	if m == nil || m.Node != "web" || m.Name != "argocd" || m.Content != "kind: Namespace\n" {
		t.Fatalf("manifest: %+v", m)
	}
	// The content is part of the declaration: another content is another digest.
	g2, _ := Decode([]byte(doc("web", "argocd", "kind: Namespace\n# changed\n")))
	if g.Digest() == g2.Digest() {
		t.Error("the digest ignores the content")
	}
	for name, d := range map[string]string{
		"a node that is no server": doc("other", "argocd", "x: 1"),
		"a path as the name":       doc("web", "../argocd", "x: 1"),
		"an uppercase name":        doc("web", "ArgoCD", "x: 1"),
		"an empty content":         doc("web", "argocd", "  \n"),
		"too large":                doc("web", "argocd", strings.Repeat("a", MaxK3sManifest+1)),
		"a NUL":                    doc("web", "argocd", "a\x00b"),
	} {
		if _, err := Decode([]byte(d)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The mamelet a node runs is declared by its sha256 (a label for people is optional), on a node of
// the graph, under the name "<node>/mamelet".
func TestNodeMamelet(t *testing.T) {
	sha := strings.Repeat("cd", 32)
	doc := func(node, name, spec string) string {
		return `{"schema":"mamela-graph/v2","resources":[` + machine("web", dhcp) + `,` +
			`{"kind":"NodeMamelet","name":"` + name + `","spec":` + strings.ReplaceAll(spec, "NODE", node) + `}]}`
	}
	g, err := Decode([]byte(doc("web", "web/mamelet", `{"node":"NODE","version":"0.9.1","sha256":"`+sha+`"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var m *NodeMameletSpec
	for _, r := range g.Sorted() {
		if r.NodeMamelet != nil {
			m = r.NodeMamelet
		}
	}
	if m == nil || m.Node != "web" || m.SHA256 != sha || m.Version != "0.9.1" {
		t.Fatalf("mamelet: %+v", m)
	}
	// No label is fine; another sha256 is another digest.
	g2, err := Decode([]byte(doc("web", "web/mamelet", `{"node":"NODE","sha256":"`+sha+`"}`)))
	if err != nil {
		t.Fatalf("without a label: %v", err)
	}
	g3, _ := Decode([]byte(doc("web", "web/mamelet", `{"node":"NODE","sha256":"`+strings.Repeat("ef", 32)+`"}`)))
	if g2.Digest() == g3.Digest() || g.Digest() == g2.Digest() {
		t.Error("the digest ignores the sha256 or the label")
	}
	for name, d := range map[string]string{
		"an unknown node":       doc("nope", "nope/mamelet", `{"node":"NODE","sha256":"`+sha+`"}`),
		"a short sha256":        doc("web", "web/mamelet", `{"node":"NODE","sha256":"abcd"}`),
		"an uppercase sha256":   doc("web", "web/mamelet", `{"node":"NODE","sha256":"`+strings.ToUpper(sha)+`"}`),
		"no sha256":             doc("web", "web/mamelet", `{"node":"NODE"}`),
		"a name that is not it": doc("web", "web/agent", `{"node":"NODE","sha256":"`+sha+`"}`),
		"a label with a space":  doc("web", "web/mamelet", `{"node":"NODE","version":"a b","sha256":"`+sha+`"}`),
		"an unknown field":      doc("web", "web/mamelet", `{"node":"NODE","sha256":"`+sha+`","url":"https://x"}`),
	} {
		if _, err := Decode([]byte(d)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// What a person reads in a plan must be what is applied (A9 of the 0.3.2 audit). A carriage
// return, an escape, a bidirectional control or a zero-width character in a manifest (which k3s
// applies with cluster-admin rights) or in a provider's text can make a plan show something other
// than what runs; the graph refuses them, and says where.
func TestTextThatHidesWhatItIsIsRefused(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	server := `{"kind":"NodeK3s","name":"web/k3s","spec":{"node":"web","role":"server","sha256":"` + sha + `","version":"v1.37.2+k3s1"}}`
	manifest := func(content string) string {
		c, _ := json.Marshal(content)
		return `{"schema":"mamela-graph/v2","resources":[` + machine("web", dhcp) + `,` + server + `,` +
			`{"kind":"NodeK3sManifest","name":"web/manifest/m","spec":{"node":"web","name":"m","content":` + string(c) + `}}]}`
	}
	if _, err := Decode([]byte(manifest("kind: Namespace\n\tname: a\n"))); err != nil {
		t.Fatalf("a manifest with newlines and tabs: %v", err)
	}
	if _, err := Decode([]byte(manifest("metadata:\n  name: é日本語 \U0001F600\n"))); err != nil {
		t.Fatalf("a manifest with ordinary non-ASCII text: %v", err)
	}
	for name, content := range map[string]string{
		"a carriage return":        "image: safe\r      image: evil\n",
		"an escape":                "a: b\x1b[2K\n",
		"a backspace":              "a: b\x08c\n",
		"a bell":                   "a: b\x07\n",
		"a delete":                 "a: b\x7f\n",
		"a C1 control":             "a: b\u009b2K\n",
		"a next line":              "a: b\u0085c\n",
		"a line separator":         "a: b\u2028c\n",
		"a right-to-left override": "a: \u202eevil\n",
		"a bidi isolate":           "a: \u2066evil\u2069\n",
		"a left-to-right mark":     "a: \u200eb\n",
		"a zero-width space":       "a: b\u200bc\n",
		"a zero-width joiner":      "a: b\u200dc\n",
		"a word joiner":            "a: b\u2060c\n",
		"a byte order mark":        "a: b\ufeffc\n",
		"a form feed":              "a: b\fc\n",
		"a tag character":          "a: b\U000e0041\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(manifest(content)))
			assertCode(t, err, Code("MAM-GRAPH-0109"))
			if !strings.Contains(err.Error(), "line ") || !strings.Contains(err.Error(), "U+") {
				t.Errorf("the problem does not say where and what:\n%v", err)
			}
		})
	}
	// A provider's spec is text a person reads in the plan too; its member names as well.
	for name, spec := range map[string]string{
		"an escape in a value":       `{"value":"a\u001b[2Kb"}`,
		"a bidi override in a value": `{"value":"a\u202eb"}`,
		"a zero-width space":         `{"labels":["a\u200bb"]}`,
		"an escape in a member":      `{"a\u001b":"b"}`,
		"a carriage return":          `{"value":"a\rb"}`,
	} {
		t.Run("spec: "+name, func(t *testing.T) {
			_, err := Decode([]byte(item(spec)))
			assertCode(t, err, Code("MAM-GRAPH-0109"))
		})
	}
	if _, err := Decode([]byte(item(`{"value":"line 1\nline 2\tcolumn","name":"é日本語"}`))); err != nil {
		t.Fatalf("a spec with a newline, a tab and non-ASCII text: %v", err)
	}
}

// The error is read in a terminal and in a pull request: what it quotes is shown as escapes, so
// that it cannot rewrite what is on the screen either.
func TestProblemTextIsEscaped(t *testing.T) {
	in := `{"schema":"mamela-graph/v2","resources":[` +
		`{"kind":"NodePackage","name":"x\u001b[2K\u202e/y","spec":{"node":"x\u001b[2K","package":"nginx","state":"Present"}}]}`
	_, err := Decode([]byte(in))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, r := range err.Error() {
		if r == '\x1b' || r == '\u202e' || (r < ' ' && r != '\n') {
			t.Fatalf("the error has a raw control character %U:\n%q", r, err.Error())
		}
	}
	if !strings.Contains(err.Error(), `\x1b`) {
		t.Errorf("the escape is not shown as one:\n%v", err)
	}
}

// A spec.network that is not a network (B14 of the 0.3.2 audit) used to make the whole network
// check pass as "no network": a string, a list, a member of the wrong type or an unknown member.
func TestNetworkOfTheWrongShapeIsRefused(t *testing.T) {
	doc := func(network string) string {
		return `{"schema":"mamela-graph/v2","resources":[` + machine("web", network) + `]}`
	}
	for name, network := range map[string]string{
		"a string":                  `"dhcp"`,
		"a number":                  `7`,
		"a list":                    `["dhcp"]`,
		"a bool":                    `true`,
		"an unknown member":         `{"mode":"dhcp","extra":1}`,
		"an unknown member only":    `{"kind":"dhcp"}`,
		"a mode that is a number":   `{"mode":1}`,
		"dns that is a string":      `{"mode":"static","address":"192.0.2.10/24","gateway":"192.0.2.1","dns":"192.0.2.1"}`,
		"an address that is a list": `{"mode":"static","address":["192.0.2.10/24"],"gateway":"192.0.2.1","dns":["192.0.2.1"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(doc(network)))
			assertCode(t, err, CodeInvalidValue)
			if !strings.Contains(err.Error(), "spec.network") || !strings.Contains(err.Error(), "hint:") {
				t.Errorf("the problem does not say where and what to do:\n%v", err)
			}
		})
	}
	// An address declared twice is still found when the other machine's network is of the wrong shape.
	static := `{"mode":"static","address":"192.0.2.10/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}`
	if _, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + machine("a", static) + `,` + machine("b", static) + `]}`)); err == nil {
		t.Error("the same address twice")
	}
}

// One unit declared twice, or k3s declared and its service declared not running, contradict
// themselves (B14).
func TestContradictingUnitsAreRefused(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	k3s := `{"kind":"NodeK3s","name":"web/k3s","spec":{"node":"web","role":"server","sha256":"` + sha + `","version":"v1.37.2+k3s1"}}`
	svc := func(service string, enabled, running bool) string {
		return `{"kind":"NodeService","name":"web/` + service + `","spec":{"enabled":` + strconv.FormatBool(enabled) + `,"node":"web","running":` + strconv.FormatBool(running) + `,"service":"` + service + `"}}`
	}
	doc := func(rs ...string) string {
		return `{"schema":"mamela-graph/v2","resources":[` + machine("web", dhcp) + `,` + strings.Join(rs, ",") + `]}`
	}
	for name, in := range map[string]string{
		"the unit with and without .service":      doc(svc("nginx", true, true), svc("nginx.service", true, false)),
		"enabled in one, not in the other":        doc(svc("nginx.service", true, true), svc("nginx", false, true)),
		"k3s declared, its service not running":   doc(k3s, svc("k3s", true, false)),
		"k3s declared, its service.service stops": doc(k3s, svc("k3s.service", true, false)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(in))
			assertCode(t, err, CodeConflict)
			if !strings.Contains(err.Error(), "web/") || !strings.Contains(err.Error(), "hint:") {
				t.Errorf("the problem does not say which and what to do:\n%v", err)
			}
		})
	}
	for name, in := range map[string]string{
		"the same unit twice, the same way":           doc(svc("nginx", true, true), svc("nginx.service", true, true)),
		"two different units":                         doc(svc("nginx", true, true), svc("redis", true, false)),
		"k3s declared and its service runs":           doc(k3s, svc("k3s", true, true)),
		"k3s's service not running, k3s not declared": doc(svc("k3s", false, false)),
	} {
		if _, err := Decode([]byte(in)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A graph of the most resources a document may have is decoded in well under a second: the check
// for a resource declared twice used to look at every resource for each one (32 seconds for
// 10,000).
func TestDecodeManyResources(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"schema":"mamela-graph/v2","resources":[` + machine("web", dhcp))
	const n = MaxResources - 1 // one more is left for a resource declared twice below
	for i := 1; i < n; i++ {
		b.WriteString(`,{"kind":"KVItem","name":"item-` + itoa(i) + `","provider":"kv-example","spec":{"value":"v1"}}`)
	}
	b.WriteString(`]}`)
	start := time.Now()
	g, err := Decode([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Resources) != n {
		t.Fatalf("%d resources", len(g.Resources))
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("decoding %d resources took %s", n, d)
	}
	// The same item declared again the same way is still one resource, and differently still refused.
	again := strings.Replace(b.String(), `]}`, `,{"kind":"KVItem","name":"item-1","provider":"kv-example","spec":{"value":"v1"}}]}`, 1)
	if g, err := Decode([]byte(again)); err != nil || len(g.Resources) != n {
		t.Errorf("declared twice the same way: %v", err)
	}
	other := strings.Replace(b.String(), `]}`, `,{"kind":"KVItem","name":"item-1","provider":"kv-example","spec":{"value":"v2"}}]}`, 1)
	_, err = Decode([]byte(other))
	assertCode(t, err, CodeDuplicateResource)
}

// A cycle is reported with the way round (B14, low).
func TestCycleSaysTheWayRound(t *testing.T) {
	r := func(name, after string) string {
		return `{"kind":"Thing","name":"` + name + `","provider":"test","spec":{},"after":[` + after + `]}`
	}
	_, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + r("a", `"Thing b"`) + `,` + r("b", `"Thing c"`) + `,` + r("c", `"Thing a"`) + `,` + r("d", `"Thing d"`) + `,` + r("e", `"Thing a"`) + `]}`))
	assertCode(t, err, CodeCycle)
	for _, want := range []string{
		"Thing a needs itself through after: Thing a -> Thing b -> Thing c -> Thing a",
		"Thing b needs itself through after: Thing b -> Thing c -> Thing a -> Thing b",
		"Thing c needs itself through after: Thing c -> Thing a -> Thing b -> Thing c",
		"Thing d needs itself through after: Thing d -> Thing d",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "Thing e needs itself") {
		t.Errorf("a resource that only waits for a cycle is not in it:\n%v", err)
	}
}

// Optional members are left out, not written as null (the schema refuses null everywhere), and a
// k3s server's empty server is not a way to say it has none (B14, low).
func TestOptionalMembersAreNotNull(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	k := func(role, server string) string {
		return `{"schema":"mamela-graph/v2","resources":[` + machine("s", dhcp) + `,` +
			`{"kind":"NodeK3s","name":"s/k3s","spec":{"node":"s","role":"` + role + `",` + server + `"sha256":"` + sha + `","version":"v1.37.2+k3s1"}}]}`
	}
	for name, c := range map[string]struct {
		in   string
		code Code
	}{
		"a server with a null server":   {k("server", `"server":null,`), CodeWrongType},
		"a server with an empty server": {k("server", `"server":"",`), CodeInvalidValue},
		"an agent with a null server":   {k("agent", `"server":null,`), CodeWrongType},
		"a null mamelet version":        {`{"schema":"mamela-graph/v2","resources":[` + machine("s", dhcp) + `,{"kind":"NodeMamelet","name":"s/mamelet","spec":{"node":"s","sha256":"` + sha + `","version":null}}]}`, CodeWrongType},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(c.in))
			assertCode(t, err, c.code)
		})
	}
	if _, err := Decode([]byte(k("server", ``))); err != nil {
		t.Fatalf("a server without a server: %v", err)
	}
	// An empty label is how the Mamela Pome says a mamelet has none.
	if _, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + machine("s", dhcp) + `,{"kind":"NodeMamelet","name":"s/mamelet","spec":{"node":"s","sha256":"` + sha + `","version":""}}]}`)); err != nil {
		t.Fatalf("a mamelet without a label: %v", err)
	}
}

// The advice fits the problem: a null, a depth or an unknown member is not about integers, and the
// removed Mamela.k3s_server is not what to declare a server with (B14, low).
func TestHintsFitTheProblem(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 33) + `1` + strings.Repeat(`}`, 33)
	for name, in := range map[string]string{
		"a null in a spec":      item(`{"n":null}`),
		"a spec too deep":       item(deep),
		"a spec of a list":      item(`[1]`),
		"a package of a number": `{"schema":"mamela-graph/v1","resources":[{"kind":"NodePackage","name":"a/nginx","spec":{"node":"a","package":1,"state":"Present"}}]}`,
	} {
		_, err := Decode([]byte(in))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), "numbers must be integers") {
			t.Errorf("%s: the hint is about integers:\n%v", name, err)
		}
		if !strings.Contains(err.Error(), "hint:") {
			t.Errorf("%s: no hint:\n%v", name, err)
		}
	}
	if _, err := Decode([]byte(item(`{"n":1.5}`))); err == nil || !strings.Contains(err.Error(), "whole numbers") {
		t.Errorf("a float: %v", err)
	}
	sha := strings.Repeat("ab", 32)
	_, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + machine("a", dhcp) + `,{"kind":"NodeK3s","name":"a/k3s","spec":{"node":"a","role":"agent","server":"x","sha256":"` + sha + `","version":"v1.37.2+k3s1"}}]}`))
	if err == nil || strings.Contains(err.Error(), "k3s_server") || !strings.Contains(err.Error(), "K3s.server") {
		t.Errorf("an agent of no server: %v", err)
	}
}

// A valid declaration that is probably not what was meant gets a warning, not a refusal (B14,
// B15 of the 0.3.2 audit): an agent newer than its server, and a k3s that nothing starts.
func TestWarnings(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	k := func(node, role, server, version string) string {
		srv := ""
		if server != "" {
			srv = `"server":"` + server + `",`
		}
		return `{"kind":"NodeK3s","name":"` + node + `/k3s","spec":{"node":"` + node + `","role":"` + role + `",` + srv + `"sha256":"` + sha + `","version":"` + version + `"}}`
	}
	svc := func(node string) string {
		return `{"kind":"NodeService","name":"` + node + `/k3s","spec":{"enabled":true,"node":"` + node + `","running":true,"service":"k3s"}}`
	}
	cluster := func(rs ...string) *Graph {
		g, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + machine("cp", dhcp) + `,` + machine("w1", dhcp) + `,` + strings.Join(rs, ",") + `]}`))
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	codes := func(g *Graph) []string {
		var out []string
		for _, w := range g.Warnings() {
			out = append(out, string(w.Code)+" "+w.Resource)
		}
		return out
	}
	same := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("warnings %q, want %q", got, want)
		}
	}
	// A cluster that is as it should be: nothing to say.
	same(codes(cluster(k("cp", "server", "", "v1.37.2+k3s1"), k("w1", "agent", "cp", "v1.37.2+k3s1"), svc("cp"), svc("w1"))))
	// An agent older than its server is how an upgrade goes (the server first).
	same(codes(cluster(k("cp", "server", "", "v1.37.2+k3s1"), k("w1", "agent", "cp", "v1.36.9+k3s1"), svc("cp"), svc("w1"))))
	// An agent newer than its server: by minor, by patch, by the k3s revision.
	same(codes(cluster(k("cp", "server", "", "v1.28.0+k3s1"), k("w1", "agent", "cp", "v1.99.0+k3s1"), svc("cp"), svc("w1"))), "MAM-GRAPH-0201 NodeK3s w1/k3s")
	same(codes(cluster(k("cp", "server", "", "v1.37.2+k3s1"), k("w1", "agent", "cp", "v1.37.3+k3s1"), svc("cp"), svc("w1"))), "MAM-GRAPH-0201 NodeK3s w1/k3s")
	same(codes(cluster(k("cp", "server", "", "v1.37.2+k3s1"), k("w1", "agent", "cp", "v1.37.2+k3s2"), svc("cp"), svc("w1"))), "MAM-GRAPH-0201 NodeK3s w1/k3s")
	// k3s installed, but no service that starts it (the binary is placed and the plan says in sync).
	same(codes(cluster(k("cp", "server", "", "v1.37.2+k3s1"), k("w1", "agent", "cp", "v1.37.2+k3s1"), svc("cp"))), "MAM-GRAPH-0202 NodeK3s w1/k3s")
	same(codes(cluster(k("cp", "server", "", "v1.37.2+k3s1"))), "MAM-GRAPH-0202 NodeK3s cp/k3s")
	// k3s.service is the same unit as k3s.
	svc2 := strings.NewReplacer(`"service":"k3s"`, `"service":"k3s.service"`, `"name":"cp/k3s"`, `"name":"cp/k3s.service"`).Replace(svc("cp"))
	same(codes(cluster(k("cp", "server", "", "v1.37.2+k3s1"), svc2)))
	w := cluster(k("cp", "server", "", "v1.37.2+k3s1")).Warnings()[0]
	if !strings.Contains(w.Message, "cp") || !strings.Contains(w.Hint, "K3s") && !strings.Contains(w.Hint, "service") {
		t.Errorf("the warning does not say what and what to do: %+v", w)
	}
	// Warnings are in the order of the resources, and the same each time.
	g := cluster(k("cp", "server", "", "v1.28.0+k3s1"), k("w1", "agent", "cp", "v1.99.0+k3s1"))
	same(codes(g), "MAM-GRAPH-0202 NodeK3s cp/k3s", "MAM-GRAPH-0201 NodeK3s w1/k3s", "MAM-GRAPH-0202 NodeK3s w1/k3s")
}

func TestParseRelease(t *testing.T) {
	for in, want := range map[string]Release{
		"v1.37.2+k3s1": {Major: 1, Minor: 37, Patch: 2, K3s: 1, HasK3s: true},
		"v1.37.2":      {Major: 1, Minor: 37, Patch: 2},
	} {
		if got, ok := ParseRelease(in); !ok || got != want || got.String() != in {
			t.Errorf("ParseRelease(%q) = %+v %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "latest", "1.37.2", "v1.37", "v1.37.2+k3s", "v1.37.2+rke2r1", "v1.37.2-rc1", " v1.37.2"} {
		if _, ok := ParseRelease(in); ok {
			t.Errorf("ParseRelease(%q) accepted", in)
		}
	}
	rel := func(s string) Release { r, _ := ParseRelease(s); return r }
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v1.37.2+k3s1", "v1.37.2+k3s1", 0}, {"v1.37.2+k3s1", "v1.37.2", 0}, {"v1.37.2", "v1.37.2+k3s5", 0},
		{"v1.37.3", "v1.37.2+k3s9", 1}, {"v1.36.9+k3s1", "v1.37.0", -1}, {"v1.37.2+k3s2", "v1.37.2+k3s1", 1},
		{"v1.99.0", "v1.28.0", 1}, {"v1.9.0", "v1.10.0", -1},
	} {
		if got := rel(c.a).Compare(rel(c.b)); got != c.want {
			t.Errorf("%s compared with %s = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
