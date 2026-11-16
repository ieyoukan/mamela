package evaluate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckReplacements(t *testing.T) {
	t.Parallel()
	const head = "[pome]\nname = \"example.invalid/p\"\nversion = \"0.1.0\"\nemela = \"0.27.0\"\n\n"
	tests := []struct {
		name     string
		replace  string
		setup    func(root string) // runs with root = the directory that holds the Pome's directory
		wantInfo []string          // substrings of the Detail; none means the Pome is accepted
	}{
		{name: "no [replace]", replace: ""},
		{name: "a local Pome next to it", replace: "[replace]\n\"example.invalid/dep\" = { path = \"../dep\" }\n",
			setup: func(root string) { writeFile(t, filepath.Join(root, "dep", "Pome.toml"), "[pome]\n") }},
		{name: "an absolute path to a Pome", replace: "[replace]\n\"example.invalid/dep\" = { path = \"@ROOT@/elsewhere\" }\n",
			setup: func(root string) { writeFile(t, filepath.Join(root, "elsewhere", "Pome.toml"), "[pome]\n") }},
		{name: "an entry that is not a local path is left to emela", replace: "[replace]\n\"example.invalid/dep\" = { git = \"https://example.invalid/dep\" }\n"},
		{name: "the directory is not there", replace: "[replace]\n\"example.invalid/dep\" = { path = \"../dep\" }\n",
			wantInfo: []string{"example.invalid/dep", `"../dep"`, "is not a directory"}},
		{name: "the directory is not a Pome", replace: "[replace]\n\"example.invalid/dep\" = { path = \"../dep\" }\n",
			setup:    func(root string) { writeFile(t, filepath.Join(root, "dep", "README"), "x") },
			wantInfo: []string{"example.invalid/dep", "has no Pome.toml"}},
		{name: "the path is a file", replace: "[replace]\n\"example.invalid/dep\" = { path = \"../dep\" }\n",
			setup:    func(root string) { writeFile(t, filepath.Join(root, "dep"), "x") },
			wantInfo: []string{"is not a directory"}},
		{name: "every missing entry is listed, in name order", replace: "[replace]\n\"example.invalid/b\" = { path = \"../b\" }\n\"example.invalid/a\" = { path = \"../a\" }\n",
			wantInfo: []string{"example.invalid/a", "example.invalid/b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dir := filepath.Join(root, "p")
			writeFile(t, filepath.Join(dir, "Pome.toml"), strings.ReplaceAll(head+tt.replace, "@ROOT@", filepath.ToSlash(root)))
			if tt.setup != nil {
				tt.setup(root)
			}
			err := checkReplacements(dir)
			if len(tt.wantInfo) == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var ee *Error
			if !errors.As(err, &ee) || ee.Code != CodeReplaceMissing || ee.Stage != StageBuild {
				t.Fatalf("want %s at the build stage, got: %v", CodeReplaceMissing, err)
			}
			for _, want := range tt.wantInfo {
				if !strings.Contains(ee.Detail, want) {
					t.Errorf("detail does not say %q:\n%s", want, ee.Detail)
				}
			}
			if !strings.Contains(ee.Hint, "copy the whole repository") || !strings.Contains(ee.Hint, "remove the entry") {
				t.Errorf("hint does not say what to do: %s", ee.Hint)
			}
			if i, j := strings.Index(ee.Detail, "example.invalid/a"), strings.Index(ee.Detail, "example.invalid/b"); i >= 0 && j >= 0 && i > j {
				t.Errorf("entries are not in name order:\n%s", ee.Detail)
			}
		})
	}
}

// An unreadable Pome.toml is not this check's to report: emela says it better.
func TestCheckReplacementsLeavesBrokenTomlToEmela(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Pome.toml"), "this is = = not toml")
	if err := checkReplacements(dir); err != nil {
		t.Fatalf("refused a Pome.toml it cannot read: %v", err)
	}
}

// The reported defect: an example copied out of the checkout has [replace] paths that lead nowhere,
// and emela only said "Pome.toml: No such file or directory (os error 2)".
func TestEvaluateExampleCopiedOutOfTheCheckout(t *testing.T) {
	t.Parallel()
	e := testEvaluator(t)
	copied := filepath.Join(t.TempDir(), "kv-demo")
	if err := os.CopyFS(copied, os.DirFS(filepath.Join("..", "..", "examples", "kv-demo"))); err != nil {
		t.Fatal(err)
	}
	_, err := e.Evaluate(context.Background(), copied)
	var ee *Error
	if !errors.As(err, &ee) || ee.Code != CodeReplaceMissing {
		t.Fatalf("want %s, got: %v", CodeReplaceMissing, err)
	}
	if strings.Contains(err.Error(), "os error") || strings.Contains(err.Error(), "No such file") {
		t.Errorf("the report still carries the operating system's words:\n%v", err)
	}
	for _, want := range []string{"github.com/ieyoukan/mamela/pome/mamela", "../../pome/mamela", "github.com/ieyoukan/mamela/examples/kv/pome"} {
		if !strings.Contains(ee.Detail, want) {
			t.Errorf("detail does not name %q:\n%s", want, ee.Detail)
		}
	}
}

// The examples in the checkout have their [replace] paths in place.
func TestExamplesKeepTheirReplacePaths(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"homelab", "kv-demo", "kv/pome", "ec2/pome"} {
		if err := checkReplacements(filepath.Join("..", "..", "examples", filepath.FromSlash(name))); err != nil {
			t.Errorf("examples/%s: %v", name, err)
		}
	}
}
