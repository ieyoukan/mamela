package evaluate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/sumfile"
)

const (
	appURL = "https://example.org/releases/app-1.0.linux-amd64.tar.gz"
	keyURL = "https://deb.example.org/key.asc"
	appSum = "1111111111111111111111111111111111111111111111111111111111111111"
	keySum = "2222222222222222222222222222222222222222222222222222222222222222"
)

// declaration writes a Pome that declares an artifact and an APT source on a machine: with their
// sha256 written (inline), or left to mamela.sum (Mamela.artifact_pinned, Mamela.apt_repo_pinned).
func declaration(t *testing.T, inline bool) string {
	t.Helper()
	dir := t.TempDir()
	mamelaPome, err := filepath.Abs(filepath.Join("..", "..", "pome", "mamela"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "Pome.toml"), []byte(`[pome]
name = "example.invalid/pinned"
version = "0.1.0"
emela = "0.27.0"

[dependencies]
"github.com/ieyoukan/mamela/pome/mamela" = "v0.4.2"

[replace]
"github.com/ieyoukan/mamela/pome/mamela" = { path = "`+mamelaPome+`" }
`), 0o644)
	// The Mamela Pome's dependencies, as every test Pome that uses it locks them.
	lock, err := os.ReadFile(filepath.Join("testdata", "invalid-graph", "Pome.lock"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "Pome.lock"), lock, 0o644)
	artifact := `Mamela.artifact_pinned("/usr/local/bin/app", "` + appURL + `")`
	repo := `Mamela.apt_repo_pinned(web, "vendor", "https://deb.example.org/repo", "stable", ["main"], "` + keyURL + `")`
	if inline {
		artifact = `Mamela.artifact_of("/usr/local/bin/app", "` + appURL + `", "` + appSum + `")`
		repo = `Mamela.apt_repo(web, "vendor", "https://deb.example.org/repo", "stable", ["main"], "` + keyURL + `", "` + keySum + `")`
	}
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "main.emel"), []byte(`import Std.Console.{SystemConsole}
import Mamela
import Mamela.{Node}

fn main() {
  with SystemConsole {
    web = Node(name: "web")
    Mamela.empty()
      |> Mamela.provided("test", "TestVM", "web", [])
      |> Mamela.artifact_with(web, Mamela.Artifact(extract: "app-1.0/app", ..`+artifact+`))
      |> `+repo+`
      |> Mamela.print
  }
}
`), 0o644)
	return dir
}

func writeSums(t *testing.T, dir string, entries ...sumfile.Entry) {
	t.Helper()
	data, err := sumfile.Format(entries)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, sumfile.Name), data, 0o644)
}

// A declaration that leaves its sha256 to mamela.sum evaluates to the graph, and the digest, of the
// one that writes them; without them in mamela.sum it does not evaluate, and says what to run.
func TestEvaluateTakesSha256FromMamelaSum(t *testing.T) {
	t.Parallel()
	requirePomeCache(t)
	e := testEvaluator(t)
	ctx := context.Background()
	inline, err := e.Evaluate(ctx, declaration(t, true))
	if err != nil {
		t.Fatal(err)
	}

	pinned := declaration(t, false)
	// The Pins mamela pin needs, read without mamela.sum.
	pins, err := e.Pins(ctx, pinned)
	if err != nil {
		t.Fatal(err)
	}
	wantPins := []graph.Pin{ // in the order the declaration writes them
		{URL: appURL, Resource: "NodeArtifact web/artifact/usr/local/bin/app", Field: "spec.sha256"},
		{URL: keyURL, Resource: "NodeAptRepo web/apt-repo/vendor", Field: "spec.keySha256", Key: true},
	}
	if !slices.Equal(pins, wantPins) {
		t.Errorf("Pins = %+v, want %+v", pins, wantPins)
	}

	// No mamela.sum: both are named, with what to run.
	_, err = e.Evaluate(ctx, pinned)
	var ee *Error
	if !errors.As(err, &ee) || ee.Code != CodeSumMissing || ee.Stage != StageSums {
		t.Fatalf("err = %v, want %s", err, CodeSumMissing)
	}
	for _, w := range []string{appURL, keyURL, "NodeArtifact web/artifact/usr/local/bin/app spec.sha256"} {
		if !strings.Contains(ee.Message, w) {
			t.Errorf("the message lacks %q: %s", w, ee.Message)
		}
	}
	if !strings.Contains(ee.Hint, "mamela pin "+pinned) {
		t.Errorf("hint: %s", ee.Hint)
	}

	// One of the two: the other is named.
	writeSums(t, pinned, sumfile.Entry{URL: appURL, SHA256: appSum})
	if _, err = e.Evaluate(ctx, pinned); !errors.As(err, &ee) || ee.Code != CodeSumMissing || strings.Contains(ee.Message, appURL) || !strings.Contains(ee.Message, keyURL) {
		t.Errorf("err = %v", err)
	}

	// Both, and one more line the declaration does not use: the same graph as the inline one.
	writeSums(t, pinned, sumfile.Entry{URL: appURL, SHA256: appSum}, sumfile.Entry{URL: keyURL, SHA256: keySum},
		sumfile.Entry{URL: "https://example.org/unused", SHA256: appSum})
	res, err := e.Evaluate(ctx, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != inline.Digest || string(res.Graph.Canonical()) != string(inline.Graph.Canonical()) {
		t.Errorf("from mamela.sum:\n%s\ninline:\n%s", res.Graph.Canonical(), inline.Graph.Canonical())
	}
	// Evaluating does not write the file.
	if data, _ := os.ReadFile(filepath.Join(pinned, sumfile.Name)); !strings.Contains(string(data), "unused") {
		t.Error("evaluating changed mamela.sum")
	}

	// A declaration that writes its sha256 does not read mamela.sum's (another one here).
	inlineDir := declaration(t, true)
	writeSums(t, inlineDir, sumfile.Entry{URL: appURL, SHA256: keySum})
	if res, err := e.Evaluate(ctx, inlineDir); err != nil || res.Digest != inline.Digest {
		t.Errorf("an inline sha256 was replaced: %v", err)
	}
	if pins, err := e.Pins(ctx, inlineDir); err != nil || len(pins) != 0 {
		t.Errorf("Pins of an inline declaration = %+v, %v", pins, err)
	}
}

// A mamela.sum that is not the tool's stops the evaluation before anything is built, whatever the
// declaration leaves to it.
func TestEvaluateRefusesAMalformedMamelaSum(t *testing.T) {
	t.Parallel()
	requirePomeCache(t)
	e := testEvaluator(t)
	line := func(u, h string) string { return u + " sha256:" + h + "\n" }
	for name, body := range map[string]string{
		"duplicate":    line(appURL, appSum) + line(appURL, appSum),
		"out of order": line(appURL, appSum) + line(keyURL, keySum),
		"comment":      "# my notes\n" + line(appURL, appSum),
		"bad sum":      appURL + " sha256:abc\n",
	} {
		dir := declaration(t, true) // even one that leaves nothing to it
		os.WriteFile(filepath.Join(dir, sumfile.Name), []byte(body), 0o644)
		_, err := e.Evaluate(context.Background(), dir)
		var ee *Error
		if !errors.As(err, &ee) || ee.Code != CodeSumMalformed || !strings.Contains(ee.Message, "line") || !strings.Contains(ee.Hint, "mamela pin") {
			t.Errorf("%s: err = %v, want %s", name, err, CodeSumMalformed)
		}
	}
}
