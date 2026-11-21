package linux

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
)

// mapFiles are the files of a Pome, for graph.DecodeWith (the evaluator reads them from the Pome's
// directory: internal/evaluate).
type mapFiles map[string]string

func (m mapFiles) Text(path string, max int) (string, error) {
	text, ok := m[path]
	if !ok {
		return "", &graph.FileError{Code: "MAM-EVAL-0501", Message: path + " is not in the Pome", Hint: "commit it"}
	}
	return text, nil
}

func decodeWithFiles(files graph.Files, resources ...string) (*graph.Graph, error) {
	return graph.DecodeWith([]byte(`{"schema":"mamela-graph/v2","resources":[`+strings.Join(append([]string{vm}, resources...), ",")+`]}`), files)
}

// A file whose content is read from a file of the Pome plans as the same file written in: the same
// graph, the same plan and digest (what is approved), and the plan says where the content came from
// without that being approved.
func TestAFileFromThePomePlansAsTheFileWrittenIn(t *testing.T) {
	inline, err := decodeGraph(fileRes("web/file/etc/app/app.conf", `{"node":"web","path":"/etc/app/app.conf","content":"listen 8080\nworkers 4\n","restart":["app"]}`))
	if err != nil {
		t.Fatal(err)
	}
	from, err := decodeWithFiles(mapFiles{"files/app.conf": appConf},
		fileRes("web/file/etc/app/app.conf", `{"node":"web","path":"/etc/app/app.conf","contentFrom":"files/app.conf","restart":["app"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if from.Digest() != inline.Digest() {
		t.Fatalf("another graph:\n%s\n%s", from.Canonical(), inline.Canonical())
	}
	root := accountsOf("root")
	w := world{mamelet: Release, accepts: Version, observed: &Observed{Files: []FileState{present("/etc/app/app.conf", oldConf)}, Users: root, Groups: root}}
	p, q := plan.Build(from, w.snapshot(from), policy.Default), plan.Build(inline, w.snapshot(inline), policy.Default)
	if p.Digest != q.Digest || !bytes.Equal(p.Canonical(), q.Canonical()) {
		t.Fatalf("another plan: %s %s", p.Digest, q.Digest)
	}
	if c := findChange(t, p, "NodeFile web/file/etc/app/app.conf"); c.Status != plan.StatusChange || c.Sources["content"] != "files/app.conf" {
		t.Fatalf("%+v", c)
	}
	var shown, inlineShown bytes.Buffer
	plan.Render(&shown, p)
	plan.Render(&inlineShown, q)
	if !strings.Contains(shown.String(), "      content (from files/app.conf):\n      + listen 8080\n      + workers 4\n") {
		t.Errorf("the plan does not say where the content came from:\n%s", shown.String())
	}
	if strings.Replace(shown.String(), " (from files/app.conf)", "", 1) != inlineShown.String() {
		t.Errorf("the plans differ in more than the source:\n%s\n%s", shown.String(), inlineShown.String())
	}
	// Recomputed from what is stored (the canonical graph), it is the same plan, without the source.
	stored, err := graph.Decode(from.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	if r := plan.Build(stored, w.snapshot(stored), policy.Default); r.Digest != p.Digest {
		t.Errorf("replayed: %s, planned: %s", r.Digest, p.Digest)
	}
}

// A problem of a text read from a file says which file it is in.
func TestAProblemOfAFileFromThePomeNamesTheFile(t *testing.T) {
	_, err := decodeWithFiles(mapFiles{"files/bidi.conf": "a\u202eb\n"},
		fileRes("web/file/etc/app/app.conf", `{"node":"web","path":"/etc/app/app.conf","contentFrom":"files/bidi.conf"}`))
	if err == nil || !strings.Contains(err.Error(), "spec.content (from files/bidi.conf)") || !strings.Contains(err.Error(), string(graph.CodeUnsafeText)) {
		t.Fatalf("got %v", err)
	}
	_, err = decodeWithFiles(mapFiles{}, fileRes("web/file/etc/app/app.conf", `{"node":"web","path":"/etc/app/app.conf","contentFrom":"files/none.conf"}`))
	if err == nil || !strings.Contains(err.Error(), "MAM-EVAL-0501: files/none.conf is not in the Pome") || !strings.Contains(err.Error(), "spec.contentFrom") {
		t.Fatalf("got %v", err)
	}
}

// Sum: these Pomes have no mamela.sum.
func (m mapFiles) Sum(string) (string, bool) { return "", false }
