package evaluate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

// The output of a Pome as the Mamela Pome writes it: a machine, and a file and a manifest on it,
// whose specs are given.
func pomeOutput(fileSpec, manifestSpec string) []byte {
	rs := []string{
		`{"kind":"TestVM","name":"web","provider":"test","spec":{}}`,
		`{"kind":"NodeK3s","name":"web/k3s","spec":{"node":"web","role":"server","sha256":"` + strings.Repeat("ab", 32) + `","version":"v1.33.5+k3s1"}}`,
		`{"kind":"NodeService","name":"web/k3s","spec":{"enabled":true,"node":"web","running":true,"service":"k3s"}}`,
	}
	if fileSpec != "" {
		rs = append(rs, `{"kind":"NodeFile","name":"web/file/etc/app/app.conf","spec":`+fileSpec+`}`)
	}
	if manifestSpec != "" {
		rs = append(rs, `{"kind":"NodeK3sManifest","name":"web/manifest/app","spec":`+manifestSpec+`}`)
	}
	return []byte(`{"resources":[` + strings.Join(rs, ",") + `],"schema":"mamela-graph/v2"}` + "\n")
}

// pomeDir is a Pome's directory with the given files (path relative to it -> content).
func pomeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["Pome.toml"] = "[pome]\nname = \"example.invalid/files\"\nversion = \"0.1.0\"\n"
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// decodeOnly is an evaluator that only decodes (the toolchain is not needed).
func decodeOnly(maxTexts int) *Evaluator {
	return &Evaluator{Limits: Limits{MaxFileTexts: maxTexts}}
}

const unit = "[Unit]\nDescription=app\n\n[Service]\nExecStart=/usr/local/bin/app\n"

// A text read from a file of the Pome is the text written in: the same canonical form and digest,
// byte for byte (the last newline, a missing last newline, spaces at the end and blank lines are kept,
// nothing is trimmed); only Sources says where it came from.
func TestATextFromAFileIsTheTextWrittenIn(t *testing.T) {
	for name, text := range map[string]string{
		"a unit":                      unit,
		"no newline at the end":       "key = value",
		"blank lines and spaces kept": "\n\n  a = 1  \n\t\n\n",
		"non-ASCII":                   "# 日本語のコメント\nname = \"ç\"\n",
		"empty":                       "",
	} {
		t.Run(name, func(t *testing.T) {
			dir := pomeDir(t, map[string]string{"files/app.conf": text, "manifests/app.yaml": "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: app\n"})
			quoted := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(text,
				`\`, `\\`), `"`, `\"`), "\n", `\n`), "\t", `\t`), "\r", `\r`)
			inline, err := decodeOnly(0).decode(pomeOutput(
				`{"content":"`+quoted+`","node":"web","path":"/etc/app/app.conf","restart":["app"]}`,
				`{"content":"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: app\n","name":"app","node":"web"}`), dir)
			if err != nil {
				t.Fatal(err)
			}
			from, err := decodeOnly(0).decode(pomeOutput(
				`{"contentFrom":"files/app.conf","node":"web","path":"/etc/app/app.conf","restart":["app"]}`,
				`{"contentFrom":"manifests/app.yaml","name":"app","node":"web"}`), dir)
			if err != nil {
				t.Fatal(err)
			}
			if from.Digest() != inline.Digest() || string(from.Canonical()) != string(inline.Canonical()) {
				t.Fatalf("another graph:\n%s\n%s", from.Canonical(), inline.Canonical())
			}
			if strings.Contains(string(from.Canonical()), "contentFrom") {
				t.Errorf("the canonical form names the file: %s", from.Canonical())
			}
			sources := map[string]string{}
			for _, r := range from.Resources {
				for m, p := range r.Sources {
					sources[r.ID()+" "+m] = p
				}
			}
			for _, r := range inline.Resources {
				if r.Sources != nil {
					t.Errorf("%s written in has sources %v", r.ID(), r.Sources)
				}
			}
			if len(sources) != 2 || sources["NodeFile web/file/etc/app/app.conf content"] != "files/app.conf" ||
				sources["NodeK3sManifest web/manifest/app content"] != "manifests/app.yaml" {
				t.Errorf("sources %v", sources)
			}
			// Read back from its canonical form (the database), it is the same graph, with no sources.
			back, err := graph.Decode(from.Canonical())
			if err != nil || back.Digest() != from.Digest() {
				t.Fatalf("read back: %v", err)
			}
		})
	}
}

// What a spec may not name, and what may not be in the file, each with its code, the path named in
// the message, and the member it is about.
func TestFilesThatAreRefused(t *testing.T) {
	dir := pomeDir(t, map[string]string{
		"files/app.conf":   unit,
		"files/latin1.txt": "caf\xe9\n",
		"files/nul.txt":    "a\x00b\n",
		"files/big.conf":   strings.Repeat("a", 256<<10+1),
		"files/crlf.conf":  "a = 1\r\nb = 2\r\n",
		"big.yaml":         "a: " + strings.Repeat("b", 64<<10) + "\n",
		"elsewhere/x.conf": unit,
	})
	outside := filepath.Join(filepath.Dir(dir), "outside.conf")
	if err := os.WriteFile(outside, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	mustLink := func(target, link string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(dir, filepath.FromSlash(link))); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
	}
	mustLink("app.conf", "files/link.conf")               // inside the Pome, still a link
	mustLink(outside, "files/out.conf")                   // out of the Pome
	mustLink("elsewhere", "linked")                       // a directory on the way
	mustLink(filepath.Dir(dir), "files/up")               // a directory out of the Pome
	mustLink("../../outside.conf", "files/relative.conf") // relative, up and out
	fileSpec := func(from string) string {
		return `{"contentFrom":"` + from + `","node":"web","path":"/etc/app/app.conf"}`
	}
	for _, c := range []struct {
		name, file, manifest string
		code                 Code
		graph                graph.Code
		says                 string
	}{
		{name: "missing", file: fileSpec("files/nothing.conf"), code: CodeFileMissing, says: "files/nothing.conf is not in the Pome: there is no such file"},
		{name: "missing directory", file: fileSpec("nowhere/app.conf"), code: CodeFileMissing, says: "there is no directory nowhere"},
		{name: "a file as a directory", file: fileSpec("files/app.conf/x"), code: CodeFileMissing, says: "files/app.conf is not a directory"},
		{name: "up and out", file: fileSpec("../outside.conf"), code: CodeFileOutside, says: "out of the Pome"},
		{name: "up inside", file: fileSpec("files/../files/app.conf"), code: CodeFileOutside, says: "out of the Pome"},
		{name: "absolute", file: fileSpec("/etc/passwd"), code: CodeFileOutside, says: "absolute"},
		{name: "unclean", file: fileSpec("./files/app.conf"), code: CodeFileOutside, says: `write "files/app.conf"`},
		{name: "double slash", file: fileSpec("files//app.conf"), code: CodeFileOutside, says: "not clean"},
		{name: "trailing slash", file: fileSpec("files/"), code: CodeFileOutside, says: "not clean"},
		{name: "empty", file: fileSpec(""), code: CodeFileOutside, says: "empty"},
		{name: "the Pome itself", file: fileSpec("."), code: CodeFileOutside, says: "the Pome's directory itself"},
		{name: "backslash", file: fileSpec(`files\\app.conf`), code: CodeFileOutside, says: "backslash"},
		{name: "control character", file: fileSpec(`files/app\u001b.conf`), code: CodeFileOutside, says: "escape"},
		{name: "a link to a file of the Pome", file: fileSpec("files/link.conf"), code: CodeFileLink, says: "the file files/link.conf is a symbolic link"},
		{name: "a link out of the Pome", file: fileSpec("files/out.conf"), code: CodeFileLink, says: "files/out.conf is a symbolic link"},
		{name: "a relative link up and out", file: fileSpec("files/relative.conf"), code: CodeFileLink, says: "symbolic link"},
		{name: "a linked directory", file: fileSpec("linked/x.conf"), code: CodeFileLink, says: "the directory linked on the way to linked/x.conf"},
		{name: "a linked directory out", file: fileSpec("files/up/outside.conf"), code: CodeFileLink, says: "the directory files/up"},
		{name: "a directory", file: fileSpec("files"), code: CodeFileNotRegular, says: "files is a directory"},
		{name: "not UTF-8", file: fileSpec("files/latin1.txt"), code: CodeFileNotText, says: "byte 4 (line 1) is not UTF-8"},
		{name: "NUL", file: fileSpec("files/nul.txt"), code: CodeFileNotText, says: "byte 2 (line 1) is NUL"},
		{name: "larger than a file's content", file: fileSpec("files/big.conf"), code: CodeFileTooLarge, says: "files/big.conf is 262145 bytes; the text it is taken for is at most 262144"},
		{name: "larger than a manifest", manifest: `{"contentFrom":"big.yaml","name":"app","node":"web"}`, code: CodeFileTooLarge, says: "at most 65536"},
		// The text is as it is: a carriage return is refused as it is in a text written in, at the
		// member, and the problem says which file it is in.
		{name: "carriage returns", file: fileSpec("files/crlf.conf"), graph: graph.CodeUnsafeText, says: "(from files/crlf.conf)"},
		{name: "both", file: `{"content":"x","contentFrom":"files/app.conf","node":"web","path":"/etc/app/app.conf"}`, graph: graph.CodeInvalidValue, says: "both spec.content and spec.contentFrom"},
		{name: "both, manifest", manifest: `{"content":"x","contentFrom":"big.yaml","name":"app","node":"web"}`, graph: graph.CodeInvalidValue, says: "both spec.content and spec.contentFrom"},
		{name: "neither", file: `{"node":"web","path":"/etc/app/app.conf"}`, graph: graph.CodeMissingField, says: "NodeFile has no spec.content"},
		{name: "neither, manifest", manifest: `{"name":"app","node":"web"}`, graph: graph.CodeMissingField, says: "NodeK3sManifest has no spec.content"},
		{name: "null", file: `{"contentFrom":null,"node":"web","path":"/etc/app/app.conf"}`, graph: graph.CodeWrongType, says: "contentFrom is null"},
		{name: "not a string", file: `{"contentFrom":["files/app.conf"],"node":"web","path":"/etc/app/app.conf"}`, graph: graph.CodeWrongType, says: "contentFrom"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, err := decodeOnly(0).decode(pomeOutput(c.file, c.manifest), dir)
			var ge *graph.Error
			if !errors.As(err, &ge) {
				t.Fatalf("accepted (%v): %s", err, g.Canonical())
			}
			want := graph.Code(c.code)
			if c.graph != "" {
				want = c.graph
			}
			if len(ge.Problems) != 1 || ge.Problems[0].Code != want || !strings.Contains(ge.Problems[0].String(), c.says) {
				t.Fatalf("want one %s saying %q, got:\n%v", want, c.says, err)
			}
			p := ge.Problems[0]
			if c.code != "" && (!strings.HasSuffix(p.Path, "spec.contentFrom") || p.Hint == "") {
				t.Errorf("at %q, hint %q", p.Path, p.Hint)
			}
			if strings.Contains(p.String(), dir) {
				t.Errorf("the problem names the Pome's directory on this machine: %s", p)
			}
		})
	}
}

// The texts of one declaration are bounded in all, each file counted each time a spec takes it: the
// graph is never larger than the declaration could have written in.
func TestTheTextsOfADeclarationAreBoundedInAll(t *testing.T) {
	dir := pomeDir(t, map[string]string{"files/a.conf": strings.Repeat("a", 600), "files/b.conf": strings.Repeat("b", 500)})
	out := pomeOutput(`{"contentFrom":"files/a.conf","node":"web","path":"/etc/app/app.conf"}`,
		`{"contentFrom":"files/b.conf","name":"app","node":"web"}`)
	if _, err := decodeOnly(1100).decode(out, dir); err != nil {
		t.Fatalf("within the limit: %v", err)
	}
	_, err := decodeOnly(1099).decode(out, dir)
	var ge *graph.Error
	if !errors.As(err, &ge) || len(ge.Problems) != 1 || ge.Problems[0].Code != graph.Code(CodeFileTooLarge) ||
		!strings.Contains(ge.Problems[0].Message, "files/b.conf is one file too many") || !strings.Contains(ge.Problems[0].Message, "1099 bytes in all") {
		t.Fatalf("over the limit: %v", err)
	}
	// The default is as much as one graph document holds.
	if DefaultLimits.MaxFileTexts != graph.MaxDocumentBytes {
		t.Errorf("default %d", DefaultLimits.MaxFileTexts)
	}
}

// The same tree gives the same graph, however often it is read; what the file is when it is read is
// what the graph has (no cache across evaluations).
func TestTheSameTreeGivesTheSameGraph(t *testing.T) {
	dir := pomeDir(t, map[string]string{"files/app.conf": unit})
	out := pomeOutput(`{"contentFrom":"files/app.conf","node":"web","path":"/etc/app/app.conf"}`, "")
	a, err := decodeOnly(0).decode(out, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := decodeOnly(0).decode(out, dir)
	if err != nil || a.Digest() != b.Digest() {
		t.Fatalf("%v: %s %s", err, a.Digest(), b.Digest())
	}
	if err := os.WriteFile(filepath.Join(dir, "files/app.conf"), []byte(unit+"# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := decodeOnly(0).decode(out, dir)
	if err != nil || c.Digest() == a.Digest() {
		t.Fatalf("a changed file gave the same graph: %v", err)
	}
}

// A graph that is not the output of a Pome being evaluated (read back, or written by hand) cannot
// name a file: there is no Pome to read it from.
func TestContentFromNeedsAPome(t *testing.T) {
	_, err := graph.Decode(pomeOutput(`{"contentFrom":"files/app.conf","node":"web","path":"/etc/app/app.conf"}`, ""))
	var ge *graph.Error
	if !errors.As(err, &ge) || !ge.Has(graph.CodeUnknownField) || !strings.Contains(err.Error(), "not the output of a Pome being evaluated") {
		t.Fatalf("got %v", err)
	}
}
