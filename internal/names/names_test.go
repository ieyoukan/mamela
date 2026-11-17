package names

import (
	"strings"
	"testing"
)

// The limits are the ones a signed revision has always enforced: a declaration is held to them.
func TestShapes(t *testing.T) {
	rep := func(n int) string { return strings.Repeat("a", n) }
	for _, c := range []struct {
		name string
		f    func(string) bool
		ok   []string
		bad  []string
	}{
		{"Node", Node, []string{"a", "web-1", rep(63)}, []string{"", "-a", "a-", "Web", "a_b", rep(64)}},
		{"Package", Package, []string{"nginx", "libstdc++6", "a.b-c", rep(128)}, []string{"", "a", "Nginx", "-a", "a b", "a;b", "../x", rep(129), rep(150)}},
		{"Service", Service, []string{"nginx", "getty@tty1", "a_b.c-d", "nginx.service", rep(128)}, []string{"", "a:b", "-a", "a b", "a/b", rep(129), rep(255)}},
		{"Manifest", Manifest, []string{"argocd", "a-b", rep(63)}, []string{"", "-a", "ArgoCD", "a_b", "a.b", rep(64)}},
		{"K3sVersion", K3sVersion, []string{"v1.37.2+k3s1", "v1.9.9+k3s99"}, []string{"", "v1.37.2", "latest", "v1.37.2+k3s1/../x", "v1.100.2+k3s1"}},
		{"SHA256", SHA256, []string{strings.Repeat("ab", 32)}, []string{"", strings.Repeat("AB", 32), strings.Repeat("a", 63), strings.Repeat("a", 65)}},
	} {
		for _, s := range c.ok {
			if !c.f(s) {
				t.Errorf("%s(%q) = false", c.name, s)
			}
		}
		for _, s := range c.bad {
			if c.f(s) {
				t.Errorf("%s(%q) = true", c.name, s)
			}
		}
	}
}
