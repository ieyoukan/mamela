package k3s

import "testing"

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
