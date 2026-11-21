package main

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/examples/kv/kvapi"
	"github.com/ieyoukan/mamela/internal/policy"
	sitedef "github.com/ieyoukan/mamela/internal/site"
)

// A plan is applied only under the policy it was made under (B3). Approving a deletion, then
// changing the policy to retain what a declaration used to delete (or to protect a node), must not
// let the old plan through on its stored policy: apply names the policy in force (--policy, as plan
// does) and refuses a plan made under another, with a code that says to plan again.
func TestApplyRefusesAPlanMadeUnderAnotherPolicy(t *testing.T) {
	l := newKVLab(t)
	infra := l.declaration("github.com/example/infra", "web", "db")
	l.applyAll(infra)
	infra = l.declaration("github.com/example/infra", "web")
	digest, out := l.plan(infra)
	if !strings.Contains(out, "- KVItem db") || digest == "" {
		t.Fatalf("setup: the plan does not delete db:\n%s", out)
	}
	if code, out := l.approve(digest); code != 0 {
		t.Fatalf("approve: %s", out)
	}

	// The operator now retains what a declaration no longer has.
	retain := policy.Default
	retain.Removal = "retain"
	data, err := json.Marshal(retain)
	if err != nil {
		t.Fatal(err)
	}
	policyFile := filepath.Join(l.dir, "retain.json")
	os.WriteFile(policyFile, data, 0o600)

	code, out := l.apply(digest, "--policy", policyFile)
	if code == 0 || !strings.Contains(out, "MAM-REC-0010") || !strings.Contains(out, "make the plan again") {
		t.Fatalf("apply under another policy: %d\n%s", code, out)
	}
	if _, ok := l.kv.Items()["db"]; !ok {
		t.Fatal("db was deleted under a policy that retains it")
	}

	// The API server applies under its own policy file in the same way.
	b := testBackend(t, l.db, l.providers)
	b.policy = policyFile
	a, err := b.Apply(context.Background(), digest)
	if err != nil || a.OK || !strings.Contains(a.Text, "MAM-REC-0010") {
		t.Fatalf("the API's apply under another policy: %+v %v", a, err)
	}
	if _, ok := l.kv.Items()["db"]; !ok {
		t.Fatal("the API deleted db under a policy that retains it")
	}

	// Under that policy a new plan has no deletion in it.
	if _, out := l.plan(infra, "--policy", policyFile); strings.Contains(out, "- KVItem db") {
		t.Fatalf("the retaining policy still plans a deletion:\n%s", out)
	}

	// Without the flag, the policy in force is the default: the plan made under it is applied.
	if code, out := l.apply(digest); code != 0 || !strings.Contains(out, "delete-item") {
		t.Fatalf("apply under the policy it was made under: %d\n%s", code, out)
	}
	if _, ok := l.kv.Items()["db"]; ok {
		t.Fatal("db was not deleted")
	}
}

// The policy file is named the same way for plan and apply: a plan made with --policy is applied with it.
func TestApplyNeedsTheSamePolicyFileAsPlan(t *testing.T) {
	l := newKVLab(t)
	strict := policy.Default
	strict.MaxObservationAgeSeconds = 300
	data, _ := json.Marshal(strict)
	policyFile := filepath.Join(l.dir, "strict.json")
	os.WriteFile(policyFile, data, 0o600)

	d := l.declaration("github.com/example/infra", "web")
	digest, out := l.plan(d, "--policy", policyFile)
	if digest == "" {
		t.Fatalf("no plan:\n%s", out)
	}
	l.approve(digest)
	if code, out := l.apply(digest); code == 0 || !strings.Contains(out, "MAM-REC-0010") || !strings.Contains(out, "--policy") {
		t.Fatalf("applied without the policy file the plan was made with: %d\n%s", code, out)
	}
	if code, out := l.apply(digest, "--policy", policyFile); code != 0 {
		t.Fatalf("apply with the policy file: %d\n%s", code, out)
	}
	if _, ok := l.kv.Items()["web"]; !ok {
		t.Fatal("web was not created")
	}
}

// The site's observe_only is added to the policy's observeOnly, as the Control Plane's host is to its
// protected nodes: the policy in force, which plans are made under and apply checks.
func TestCurrentPolicyHasTheSitesObserveOnly(t *testing.T) {
	file := filepath.Join(t.TempDir(), "policy.json")
	os.WriteFile(file, []byte(`{"version":"x","maxObservationAgeSeconds":600,"allowedAptOrigins":["Ubuntu:"],"observeOnly":["db","web"]}`), 0o600)
	site := &sitedef.Site{ObserveOnly: []string{"web", "legacy"}}
	site.ControlPlane.Host = "cp"
	pol, err := currentPolicy(site, file)
	if err != nil || !slices.Equal(pol.ObserveOnly, []string{"db", "legacy", "web"}) || !slices.Equal(pol.ProtectedNodes, []string{"cp"}) {
		t.Fatalf("%+v %v", pol, err)
	}
	without, _ := currentPolicy(&sitedef.Site{}, file)
	if without.Digest() == pol.Digest() {
		t.Error("the site's observe_only is not part of the policy's digest")
	}
	if d, _ := currentPolicy(nil, ""); d.Digest() != policy.Default.Digest() {
		t.Error("no site, no file: not the built-in policy")
	}
}

// mamela act carries out no explicit operation on an observe-only name but one that adopts it; on the
// Control Plane's host, none.
func TestActOnAHeldName(t *testing.T) {
	pol := policy.Default
	pol.ObserveOnly, pol.ProtectedNodes = []string{"legacy"}, []string{"cp"}
	if err := actHeld(pol, "legacy", "adopt-vm", true); err != nil {
		t.Errorf("adopting an observe-only guest: %v", err)
	}
	if err := actHeld(pol, "legacy", "reboot-vm", false); err == nil || !strings.Contains(err.Error(), "MAM-POL-0312") ||
		!strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(err.Error(), "to reboot-vm it, remove the name") {
		t.Errorf("rebooting an observe-only guest: %v", err)
	}
	for _, adopts := range []bool{false, true} {
		if err := actHeld(pol, "cp", "adopt-vm", adopts); err == nil || !strings.Contains(err.Error(), "MAM-POL-0308") {
			t.Errorf("the Control Plane's host (adopts %v): %v", adopts, err)
		}
	}
	if err := actHeld(pol, "web", "delete-vm", false); err != nil {
		t.Errorf("a name nothing holds: %v", err)
	}
}

// A plan of an observe-only item is held, not blocked: the API counts it apart (mamelacd says so and
// reports no failure). A plan made before the item became observe-only is not applied after (the
// policy changed, MAM-REC-0010): its approval did not know.
func TestObserveOnlyThroughTheAPI(t *testing.T) {
	l := newKVLab(t)
	d := l.declaration("github.com/example/infra", "web")
	l.applyAll(d)
	l.kv.Put(kvapi.Item{Name: "web", Value: "by hand", Owner: "mamela", Version: 50})
	before, _ := l.plan(d)
	if code, out := l.approve(before); code != 0 {
		t.Fatalf("approve: %s", out)
	}
	pol := policy.Default
	pol.ObserveOnly = []string{"web"}
	data, _ := json.Marshal(pol)
	file := filepath.Join(l.dir, "observe-only.json")
	os.WriteFile(file, data, 0o600)
	if code, out := l.apply(before, "--policy", file); code == 0 || !strings.Contains(out, "MAM-REC-0010") || l.kv.Items()["web"].Value != "by hand" {
		t.Fatalf("a plan made before observe-only was applied: %d\n%s", code, out)
	}
	b := testBackend(t, l.db, l.providers)
	b.policy = file
	p, err := b.Plan(context.Background(), d)
	if err != nil || p.Held != 1 || p.Blocked != 0 || p.Changes != 0 || !strings.Contains(p.Text, "[DENIED: observe-only]") {
		t.Fatalf("the API's plan: %+v %v", p, err)
	}
	if err := b.Approve(context.Background(), p.Digest, "github:alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if a, err := b.Apply(context.Background(), p.Digest); err != nil || !a.OK || !strings.Contains(a.Text, "observe-only") || l.kv.Items()["web"].Value != "by hand" {
		t.Fatalf("the API's apply: %+v %v", a, err)
	}
}
