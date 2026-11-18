package ext_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/ieyoukan/mamela/internal/ext/k3s"   // as cmd/mamela has them
	_ "github.com/ieyoukan/mamela/internal/ext/linux" // as cmd/mamela has them
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/provider/spec"
)

var update = flag.Bool("update", false, "rewrite the pinned digest")

// The node provider's manifest of the programs Mamela ships (the core's kinds, k3s's and linux's) is
// an input of every plan's ProvidersDigest: a change of it makes every plan made before it, and every
// approval of one, apply to nothing (an apply needs a plan made for the providers it runs with). The
// golden plans of each package fix it without the others (internal/plan without linux, linux with a
// fixed digest), so this pins the digest as it is shipped, and a change that moves it is seen in review.
// Rewrite it only for a change that means to (a kind, an operation, a capability, a condition):
//
//	go test ./internal/ext -run TestTheShippedNodeProviderDigest -update
func TestTheShippedNodeProviderDigest(t *testing.T) {
	got := spec.Providers{node.Manifest()}.Digest() + "\n"
	path := filepath.Join("testdata", "node-providers-digest.txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (write it with -update)", err)
	}
	if got != string(want) {
		t.Errorf("the node provider's manifest of the shipped programs changed: digest %s, pinned %s"+
			"every plan made before it, and every approval of one, no longer applies; if that is meant, rewrite %s with -update",
			strings.TrimSpace(got), want, path)
	}
}
