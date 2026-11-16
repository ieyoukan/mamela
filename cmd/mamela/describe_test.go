package main

import (
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/provider/spec/spectest"
)

// `mamela eval` says what each resource of a declaration is. Every kind that lives on a node has a
// line (audit B17: a manifest and a mamelet had a blank), and it names the node.
func TestDescribeSaysSomethingOfEveryNodeKind(t *testing.T) {
	for _, r := range spectest.NodeResources(t) {
		got := describe(r)
		if got == "" {
			t.Errorf("describe(%s) is empty", r.Kind)
		} else if !strings.Contains(got, spectest.Node) {
			t.Errorf("describe(%s) = %q does not name the node %q", r.Kind, got, spectest.Node)
		}
	}
}
