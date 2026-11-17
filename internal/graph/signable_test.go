package graph

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// A node's revisions only ever grow, so a declaration the graph accepts but the Control Plane
// cannot sign leaves the node without a revision, and fixing the declaration does not bring it
// back (A11 of the 0.3.2 audit). Whatever the graph accepts of a node resource must be signable:
// the revision made from it passes noderev's Check, which Sign runs.

// quote is s as a JSON string.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// signable decodes a declaration of one node resource of kind with the field value v (the name of
// a package, a service, a manifest, or a k3s release), and, when the graph accepts it, checks that
// the revision of the node, built as the Control Plane builds it, is valid. It returns whether the
// graph accepted the declaration.
func signable(t testing.TB, kind, v string) (accepted bool) {
	t.Helper()
	if !utf8.ValidString(v) {
		return false // not JSON text: the decoder refuses it before the graph sees it
	}
	sha := strings.Repeat("ab", 32)
	web := machine("web", dhcp)
	server := `{"kind":"NodeK3s","name":"web/k3s","spec":{"node":"web","role":"server","sha256":"` + sha + `","version":"v1.37.2+k3s1"}}`
	var res string
	switch kind {
	case "package":
		res = web + `,{"kind":"NodePackage","name":` + quote("web/"+v) + `,"spec":{"node":"web","package":` + quote(v) + `,"state":"Present"}}`
	case "service":
		res = web + `,{"kind":"NodeService","name":` + quote("web/"+v) + `,"spec":{"enabled":true,"node":"web","running":true,"service":` + quote(v) + `}}`
	case "manifest":
		res = web + `,` + server + `,{"kind":"NodeK3sManifest","name":` + quote("web/manifest/"+v) + `,"spec":{"node":"web","name":` + quote(v) + `,"content":"kind: Namespace\n"}}`
	case "content":
		res = web + `,` + server + `,{"kind":"NodeK3sManifest","name":"web/manifest/m","spec":{"node":"web","name":"m","content":` + quote(v) + `}}`
	case "version":
		res = web + `,{"kind":"NodeK3s","name":"web/k3s","spec":{"node":"web","role":"server","sha256":"` + sha + `","version":` + quote(v) + `}}`
	case "sha256":
		res = web + `,{"kind":"NodeK3s","name":"web/k3s","spec":{"node":"web","role":"server","sha256":` + quote(v) + `,"version":"v1.37.2+k3s1"}}`
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	g, err := Decode([]byte(`{"schema":"mamela-graph/v2","resources":[` + res + `]}`))
	if err != nil {
		return false
	}
	rev := noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 100, Serial: 1,
		IssuedAt: time.Unix(1, 0), ExpiresAt: time.Unix(100, 0)}
	for _, r := range g.Sorted() {
		switch {
		case r.NodePackage != nil:
			rev.Packages = append(rev.Packages, noderev.Package{Name: r.NodePackage.Package})
		case r.NodeService != nil:
			rev.Services = append(rev.Services, noderev.Service{Name: r.NodeService.Service})
		case r.Kind == "NodeK3sManifest":
			m := specOf(r)
			rev.Manifests = append(rev.Manifests, noderev.Manifest{Name: m["name"], Content: m["content"]})
		case r.Kind == "NodeK3s":
			k := specOf(r)
			rev.K3s = &noderev.K3s{Version: k["version"], SHA256: k["sha256"], Role: k["role"], Token: strings.Repeat("cd", 32)}
		}
	}
	if err := rev.Check(); err != nil {
		t.Errorf("the graph accepted %s %q, but the revision made from it cannot be signed: %v", kind, v, err)
	}
	return true
}

func TestWhatTheGraphAcceptsCanBeSigned(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	cases := map[string][]string{
		"package": {"nginx", "libstdc++6", "a", "ab", long(127), long(128), long(129), long(150), "nginx;rm", "Nginx", "-nginx", "ng inx", "../x", "a+", "a.b-c+d"},
		"service": {"nginx", "k3s", "a:b", "getty@tty1", "systemd-resolved", "nginx.service", long(127), long(128), long(129), long(200), long(255), long(256),
			"a b", "../x", "-x", "a/b", "A_b.c@d-e"},
		"manifest": {"argocd", "a", long(63), long(64), "ArgoCD", "../x", "a_b", "-a"},
		"content":  {"kind: Namespace\n", "a\tb\n", "a\x00b", "a\rb", "a\x1bb", "‮", "\xff", strings.Repeat("a", noderev.MaxManifest), strings.Repeat("a", noderev.MaxManifest+1)},
		"version":  {"v1.37.2+k3s1", "v1.37.2", "v1.9.9+k3s99", "v1.100.2+k3s1", "v1.37.2+k3s100", "v2.0.0+k3s1", "--help"},
		"sha256":   {strings.Repeat("ab", 32), strings.Repeat("AB", 32), strings.Repeat("a", 63), strings.Repeat("a", 65)},
	}
	for kind, values := range cases {
		for _, v := range values {
			signable(t, kind, v)
		}
	}
	// The cases above are known to straddle the limits: the graph must not accept all of them, or
	// the test would prove nothing.
	if signable(t, "service", "a:b") || signable(t, "package", long(150)) {
		t.Error("a service with ':' or a package name of 150 characters is accepted")
	}
	if !signable(t, "service", "getty@tty1") || !signable(t, "package", "libstdc++6") {
		t.Error("an ordinary service or package is refused")
	}
}

// The same, by search: every name of up to three characters over the characters that matter, and
// names at the limits. Fuzzing (go test -fuzz) goes on from the seeds.
func FuzzWhatTheGraphAcceptsCanBeSigned(f *testing.F) {
	for _, s := range []string{"nginx", "a:b", "getty@tty1", "a.service", "x-y_z", strings.Repeat("a", 128), strings.Repeat("a", 129), "A", "0", "a+b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		for _, kind := range []string{"package", "service", "manifest", "version"} {
			signable(t, kind, v)
		}
	})
}

func TestEveryShortNameThatTheGraphAcceptsCanBeSigned(t *testing.T) {
	const alphabet = "aZ09+.-_:@/ "
	var gen func(prefix string, n int)
	gen = func(prefix string, n int) {
		if prefix != "" {
			for _, kind := range []string{"package", "service", "manifest"} {
				signable(t, kind, prefix)
			}
		}
		if n == 0 {
			return
		}
		for _, c := range alphabet {
			gen(prefix+string(c), n-1)
		}
	}
	gen("", 3)
}
