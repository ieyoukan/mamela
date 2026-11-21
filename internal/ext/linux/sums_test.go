package linux

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

// The sha256 a declaration leaves out (Mamela.artifact_pinned, Mamela.apt_repo_pinned) are
// mamela.sum's (docs/design/declaration-inputs.md, 2).

const keyURL = "https://download.docker.com/linux/ubuntu/gpg"

// sumFiles are a Pome with no file but its mamela.sum (URL -> sha256), for graph.DecodeWith.
type sumFiles map[string]string

func (s sumFiles) Text(path string, _ int) (string, error) {
	return "", &graph.FileError{Code: "MAM-EVAL-0501", Message: path + " is not in the Pome"}
}

func (s sumFiles) Sum(url string) (string, bool) { h, ok := s[url]; return h, ok }

func summedGraph(sums sumFiles, resources ...string) (*graph.Graph, error) {
	return decodeWithFiles(sums, resources...)
}

func pinnedArtifact(sha string) string {
	sum := ""
	if sha != "" {
		sum = `"sha256":"` + sha + `",`
	}
	return artifactRes("web/artifact/usr/local/bin/node_exporter",
		`{"node":"web","url":"`+exporterURL+`",`+sum+`"path":"/usr/local/bin/node_exporter","extract":"`+exporterMember+`","restart":["node_exporter"]}`)
}

func pinnedAptRepo(sha string) string {
	sum := ""
	if sha != "" {
		sum = `"keySha256":"` + sha + `",`
	}
	return `{"kind":"NodeAptRepo","name":"web/apt-repo/docker","spec":{"node":"web","name":"docker","uris":["https://download.docker.com/linux/ubuntu"],` +
		`"suites":["noble"],"components":["stable"],` + sum + `"keyUrl":"` + keyURL + `"}}`
}

// From mamela.sum, the graph is the one with the sha256 written: the same canonical form and digest.
func TestASha256FromTheSumIsTheSameGraphAsOneWritten(t *testing.T) {
	inline, err := decodeGraph(pinnedArtifact(sumA), pinnedAptRepo(sumB))
	if err != nil {
		t.Fatal(err)
	}
	summed, err := summedGraph(sumFiles{exporterURL: sumA, keyURL: sumB}, pinnedArtifact(""), pinnedAptRepo(""))
	if err != nil {
		t.Fatal(err)
	}
	if string(summed.Canonical()) != string(inline.Canonical()) || summed.Digest() != inline.Digest() {
		t.Errorf("from mamela.sum:\n%s\nwritten:\n%s", summed.Canonical(), inline.Canonical())
	}
	// Written in the declaration, the sha256 is the declaration's: mamela.sum is not read.
	written, err := summedGraph(sumFiles{exporterURL: sumB, keyURL: sumA}, pinnedArtifact(sumA), pinnedAptRepo(sumB))
	if err != nil || written.Digest() != inline.Digest() {
		t.Errorf("a sha256 the declaration writes was replaced by mamela.sum's: %v", err)
	}
}

func TestASha256MamelaSumLacksStopsTheEvaluation(t *testing.T) {
	_, err := summedGraph(sumFiles{exporterURL: sumA}, pinnedArtifact(""), pinnedAptRepo(""), pinnedArtifact(""))
	var m *graph.MissingSums
	if !errors.As(err, &m) {
		t.Fatalf("err = %v, want MissingSums", err)
	}
	want := []graph.Pin{{URL: keyURL, Resource: "NodeAptRepo web/apt-repo/docker", Field: "spec.keySha256", Key: true}}
	if !slices.Equal(m.Pins, want) {
		t.Errorf("pins = %+v, want %+v", m.Pins, want)
	}
	// Collecting (mamela pin): nothing is in the sum, so every pin is named, once.
	_, err = summedGraph(nil, pinnedArtifact(""), pinnedAptRepo(""), pinnedArtifact(""))
	if !errors.As(err, &m) || len(m.Pins) != 2 || m.Pins[0].URL != exporterURL || m.Pins[0].Field != "spec.sha256" || m.Pins[0].Key || !m.Pins[1].Key {
		t.Errorf("collected %+v (%v)", m, err)
	}
	if !strings.Contains(err.Error(), exporterURL) || !strings.Contains(err.Error(), keyURL) {
		t.Errorf("the error does not name the URLs: %v", err)
	}
}

// A graph Mamela refuses is reported as such, before what mamela.sum lacks: it is not one to pin.
func TestAnInvalidGraphIsReportedBeforeWhatTheSumLacks(t *testing.T) {
	bad := artifactRes("web/artifact/usr/local/bin/x", `{"node":"web","url":"http://example.com/x","path":"/usr/local/bin/x"}`)
	_, err := summedGraph(nil, bad)
	var ge *graph.Error
	if !errors.As(err, &ge) || !strings.Contains(err.Error(), "not https") {
		t.Errorf("err = %v, want the graph's problem", err)
	}
}

// A graph decoded outside an evaluation (a stored one) has every sha256: one left out is missing,
// as it always was.
func TestWithoutMamelaSumASha256IsNeeded(t *testing.T) {
	for _, r := range []string{pinnedArtifact(""), pinnedAptRepo("")} {
		_, err := decodeGraph(r)
		var ge *graph.Error
		if !errors.As(err, &ge) || ge.Problems[0].Code != graph.CodeMissingField || !strings.Contains(strings.ToLower(ge.Problems[0].Message), "sha256") {
			t.Errorf("err = %v, want MAM-GRAPH-0004", err)
		}
	}
	// Without a URL, there is nothing to take the sha256 for.
	_, err := summedGraph(nil, artifactRes("web/artifact/usr/local/bin/x", `{"node":"web","path":"/usr/local/bin/x"}`))
	var ge *graph.Error
	if !errors.As(err, &ge) || !strings.Contains(err.Error(), "no spec.url") {
		t.Errorf("err = %v", err)
	}
}
