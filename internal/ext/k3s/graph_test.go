package k3s

import (
	"errors"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

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

// A NodeK3s without its sha256 (K3s.server_pinned) takes mamela.sum's for the release's linux/amd64
// binary, and is then the graph with the sha256 written (docs/design/declaration-inputs.md, 2).
func TestAK3sSha256FromTheSum(t *testing.T) {
	const sum = "2222222222222222222222222222222222222222222222222222222222222222"
	doc := func(sha string) []byte {
		member := ""
		if sha != "" {
			member = `"sha256":"` + sha + `",`
		}
		return []byte(`{"schema":"mamela-graph/v2","resources":[{"kind":"TestVM","name":"s","provider":"test","spec":{}},` +
			`{"kind":"NodeK3s","name":"s/k3s","spec":{"node":"s","role":"server",` + member + `"version":"v1.37.2+k3s1"}}]}`)
	}
	const url = "https://github.com/k3s-io/k3s/releases/download/v1.37.2%2Bk3s1/k3s"
	if got := PinnedURL("v1.37.2+k3s1"); got != url {
		t.Fatalf("PinnedURL = %s", got)
	}
	inline, err := graph.Decode(doc(sum))
	if err != nil {
		t.Fatal(err)
	}
	summed, err := graph.DecodeSummed(doc(""), func(u string) (string, bool) { return sum, u == url })
	if err != nil {
		t.Fatal(err)
	}
	if summed.Digest() != inline.Digest() || string(summed.Canonical()) != string(inline.Canonical()) {
		t.Errorf("from mamela.sum:\n%s\nwritten:\n%s", summed.Canonical(), inline.Canonical())
	}
	_, err = graph.DecodeSummed(doc(""), nil)
	var m *graph.MissingSums
	if !errors.As(err, &m) || len(m.Pins) != 1 || m.Pins[0] != (graph.Pin{URL: url, Resource: "NodeK3s s/k3s", Field: "spec.sha256"}) {
		t.Errorf("err = %v", err)
	}
	if _, err := graph.Decode(doc("")); err == nil || !strings.Contains(err.Error(), "MAM-GRAPH-0004") {
		t.Errorf("without mamela.sum: %v", err)
	}
	// An invalid version is the graph's problem, not mamela.sum's.
	_, err = graph.DecodeSummed([]byte(strings.Replace(string(doc("")), "v1.37.2+k3s1", "latest", 1)), nil)
	var ge *graph.Error
	if !errors.As(err, &ge) || !strings.Contains(err.Error(), "not a k3s release") {
		t.Errorf("an invalid version: %v", err)
	}
}
