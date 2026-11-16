package main

import (
	"strings"
	"testing"
)

// An approval can be ended (B3): what needed it does not run, the records stay, and the plan can be
// approved again. Approving a plan that is approved already changes nothing and says so (B11).
func TestRevokeAndReapprove(t *testing.T) {
	l := newKVLab(t)
	digest, out := l.plan(l.declaration("github.com/example/infra", "web"))
	if digest == "" {
		t.Fatalf("no plan:\n%s", out)
	}

	if code, out := l.mamela("revoke", "--db", l.db, "--by", "carol", digest); code != 1 || !strings.Contains(out, "MAM-APR-0001") {
		t.Fatalf("revoking what is not approved: %d\n%s", code, out)
	}
	if code, out := l.mamela("approve", "--db", l.db, "--by", "alice", digest); code != 0 || !strings.Contains(out, "approved "+digest+" by alice") {
		t.Fatalf("approve: %d\n%s", code, out)
	}
	code, out := l.mamela("approve", "--db", l.db, "--by", "bob", digest)
	if code != 0 || !strings.Contains(out, "MAM-APR-0002") || !strings.Contains(out, "by alice") || strings.Contains(out, "by bob until") {
		t.Fatalf("approving again: %d\n%s", code, out)
	}

	// A prefix of the digest names it, as for approve.
	if code, out := l.mamela("revoke", "--db", l.db, "--by", "carol", "--reason", "wrong plan", strings.TrimPrefix(digest, "sha256:")[:12]); code != 0 || !strings.Contains(out, "revoked the approval of "+digest) {
		t.Fatalf("revoke: %d\n%s", code, out)
	}
	code, out = l.apply(digest)
	if !strings.Contains(out, "needs-approval") || len(l.kv.Items()) != 0 {
		t.Fatalf("a revoked plan ran: %d\n%s", code, out)
	}
	if code, out := l.mamela("revoke", "--db", l.db, "--by", "carol", digest); code != 1 || !strings.Contains(out, "MAM-APR-0001") {
		t.Fatalf("revoking twice: %d\n%s", code, out)
	}

	if code, out := l.mamela("approve", "--db", l.db, "--by", "dave", digest); code != 0 || !strings.Contains(out, "approved "+digest+" by dave") {
		t.Fatalf("approving anew: %d\n%s", code, out)
	}
	if code, out := l.apply(digest); code != 0 {
		t.Fatalf("apply: %d\n%s", code, out)
	}
	if _, ok := l.kv.Items()["web"]; !ok {
		t.Fatal("web was not created")
	}
}
