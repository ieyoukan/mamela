package gitops

import (
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/api"
)

// What a poll does with a declaration at the head, from its last state.
func TestNextStep(t *testing.T) {
	for _, c := range []struct {
		name     string
		last     Head
		known    bool
		sameHead bool
		drift    bool
		rev      string
		want     headStep
	}{
		{"never seen", Head{}, false, false, false, "r1", stepPlanNew},
		{"a new commit with another declaration", Head{Revision: "r0", State: HeadInSync}, true, false, false, "r1", stepPlanNew},
		{"a new commit, a declaration that was pending", Head{Revision: "r1", State: HeadPending}, true, false, false, "r1", stepPlanNew},
		{"a new commit, the same declaration in sync", Head{Revision: "r1", State: HeadInSync}, true, false, false, "r1", stepUnchanged},
		{"same head, waiting", Head{Revision: "r1", State: HeadWaiting}, true, true, false, "r1", stepPlanAgain},
		{"same head, blocked", Head{Revision: "r1", State: HeadBlocked}, true, true, false, "r1", stepPlanAgain},
		{"same head, could not be planned", Head{Revision: "r1", State: HeadUnplanned}, true, true, false, "r1", stepPlanAgain},
		{"same head, pending an approval", Head{Revision: "r1", State: HeadPending}, true, true, false, "r1", stepLeave},
		{"same head, in sync", Head{Revision: "r1", State: HeadInSync}, true, true, false, "r1", stepLeave},
		{"same head, apply failed", Head{Revision: "r1", State: HeadFailed}, true, true, false, "r1", stepLeave},
		{"same head, apply failed, a retry is due", Head{Revision: "r1", State: HeadFailed}, true, true, true, "r1", stepPlanAgain},
		{"same head, in sync, the resync interval has passed", Head{Revision: "r1", State: HeadInSync}, true, true, true, "r1", stepPlanAgain},
	} {
		if got := nextStep(c.last, c.known, c.sameHead, c.drift, c.rev); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestApplyOutcome(t *testing.T) {
	for _, c := range []struct {
		name        string
		a           api.Applied
		err         error
		outcome     string
		textContain string
	}{
		{"applied", api.Applied{OK: true, Text: "done"}, nil, outcomeApplied, "done"},
		{"the world changed", api.Applied{Text: "stopped", Stale: true}, nil, outcomeStale, "stopped"},
		{"not ok", api.Applied{Text: "guard blocked"}, nil, outcomeFailed, "guard blocked"},
		{"refused", api.Applied{}, &api.Error{Status: 422, Message: "no such plan"}, outcomeFailed, "no such plan"},
	} {
		outcome, text := applyOutcome(c.a, c.err)
		if outcome != c.outcome || !strings.Contains(text, c.textContain) {
			t.Errorf("%s: %q %q", c.name, outcome, text)
		}
	}
}

func TestFailureSummary(t *testing.T) {
	text := "== guard (before)\nok    proxmox PVE-0001  the cluster is quorate\n\nfailed upgrade-k3s NodeK3s a/k3s (x)\n\nstopped: MAM-REC-0004: upgrade-k3s on NodeK3s a/k3s failed: the revision names server \"https://192.0.2.1:6443\", the approved plan \"\" (plan again)\n  hint: fix the cause, then run apply again\n\n== guard (after)\nok    proxmox PVE-0400  nothing changed"
	got := failureSummary(text, 90)
	if !strings.HasPrefix(got, "MAM-REC-0004: upgrade-k3s on NodeK3s a/k3s failed: the revision names server") || strings.ContainsAny(got, "\n") || len([]rune(got)) > 90 || !strings.HasSuffix(got, "…") {
		t.Errorf("%q", got)
	}
	if got := failureSummary("guard blocked", 90); got != "guard blocked" {
		t.Errorf("%q", got)
	}
	if got := failureSummary("== guard (before)\nok  PVE-0001 fine\nconnection reset by peer", 90); got != "connection reset by peer" {
		t.Errorf("the last line when none gives a reason: %q", got)
	}
}
