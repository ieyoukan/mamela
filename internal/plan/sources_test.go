package plan

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
)

// A text read from a file of the declaration's Pome is shown with the file it came from, which is
// not part of the plan's digest: the same text is the same plan, written in or read from a file.
func TestTheSourceOfATextIsShownAndNotDigested(t *testing.T) {
	r := graph.Resource{Kind: "NodeFile", Name: "web/file/etc/app.conf", Sources: map[string]string{"content": "files/app\x1b.conf"}}
	c := NewChange(r)
	if c.Sources["content"] != "files/app\x1b.conf" {
		t.Fatalf("NewChange: %+v", c)
	}
	c.Status = StatusChange
	c.Diffs = []Diff{{Field: "file", To: "/etc/app.conf"}, {Field: "content", To: "a\nb\n"}, {Field: "mode", From: "0600", To: "0644"}}
	p := &Plan{Changes: []Change{c}}
	plain := &Plan{Changes: []Change{c}}
	plain.Changes[0].Sources = nil
	if digest(p) != digest(plain) || !bytes.Equal(p.Canonical(), plain.Canonical()) {
		t.Fatal("the source is in the digest")
	}
	var b bytes.Buffer
	Render(&b, p)
	if !strings.Contains(b.String(), "      content (from files/app\\x1b.conf):\n      + a\n      + b\n") || strings.Contains(b.String(), "file (from") ||
		strings.Contains(b.String(), "mode (from") {
		t.Errorf("rendered:\n%s", b.String())
	}
	// A text of one line says so too.
	c.Diffs = []Diff{{Field: "content", To: "one line"}}
	b.Reset()
	Render(&b, &Plan{Changes: []Change{c}})
	if !strings.Contains(b.String(), "      content (from files/app\\x1b.conf): one line\n") {
		t.Errorf("rendered:\n%s", b.String())
	}
}
