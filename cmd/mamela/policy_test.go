package main

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/policy"
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
