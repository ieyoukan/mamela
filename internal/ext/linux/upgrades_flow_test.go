package linux

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/provider/node"
	"github.com/ieyoukan/mamela/internal/reconcile"
)

const upgradesID = "NodeAptUpgrades web/apt-upgrades"

// debianPolicy allows Debian's archives (the point releases and the security archive).
var debianPolicy = func() policy.Policy {
	p := policy.Default
	p.AllowedAptOrigins = []string{"Debian-Security:", "Debian:"}
	return p
}()

// upgradeFlow is a flow whose node is a Debian 13 machine with pending security updates, and whose
// declaration applies them.
func upgradeFlow(t *testing.T, resources ...string) (*flow, *upgradeApt) {
	f := newFlow(t, append([]string{securityUpgrades}, resources...)...)
	old := aptNow
	aptNow = func() time.Time { return f.now }
	t.Cleanup(func() { aptNow = old })
	apt := newUpgradeApt(t, f.node, aptNow)
	return f, apt
}

// planWith is flow.plan under pol.
func (f *flow) planWith(pol policy.Policy) *plan.Plan {
	f.t.Helper()
	if err := node.Publish(f.ctx, f.st, f.g, pol, "", map[string]int64{"web": 7001}); err != nil {
		f.t.Fatal(err)
	}
	snap, err := f.rec.Provider.Observe(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	p := plan.Build(f.g, snap, pol)
	if err := f.st.SavePlan(f.ctx, f.g, snap, pol, p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// reported: the node's mamelet said it takes linux, got its part, and reported the upgrades.
func (f *flow) reported(pol policy.Policy) {
	f.t.Helper()
	f.planWith(pol)
	f.node.round()
	f.planWith(pol)
	f.node.round()
}

// The way of an upgrade: the plan shows every package and version and asks for an approval; the
// apply grants exactly that change set, mamelet upgrades, and the next plan is in sync.
func TestAptUpgradesFromDeclarationToNode(t *testing.T) {
	f, apt := upgradeFlow(t)
	f.planWith(debianPolicy)
	f.node.round() // it says it takes linux 1
	p := f.planWith(debianPolicy)
	if c := change(t, p, upgradesID); c.Status != plan.StatusWaiting || c.Note.Code != codeUpgradesAwait {
		t.Fatalf("before it reported: %+v", c)
	}
	f.node.round()
	p = f.planWith(debianPolicy)
	c := change(t, p, upgradesID)
	if c.Status != plan.StatusChange || c.Verdict != policy.ApprovalRequired || c.Operations[0].Op != OpUpgradePackages ||
		c.Operations[0].Decision.Code != "MAM-POL-0227" {
		t.Fatalf("the plan: %+v", c)
	}
	var lines []string
	for _, d := range c.Diffs {
		lines = append(lines, d.Field+": "+d.From+" | "+d.To)
	}
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "pending upgrades: 2 from Debian-Security: | 0") ||
		!strings.Contains(got, "apt:  | upgrade bind9-libs 1:9.20.11-4 -> 1:9.20.15-1~deb13u1 (Debian-Security:13/stable-security)") ||
		strings.Contains(got, "base-files") {
		t.Errorf("the diffs:\n%s", got)
	}
	if c.Note == nil || c.Note.Code != codeUpgradesLists || !strings.Contains(c.Note.Message, "1h00m old") {
		t.Errorf("the lists' age: %+v", c.Note)
	}
	rep := f.apply(p)
	if len(rep.Steps) != 1 || rep.Steps[0].Outcome != reconcile.OutcomeDone || rep.Stopped != nil {
		t.Fatalf("apply %+v %+v", rep, rep.Stopped)
	}
	if apt.installed["bind9-libs"] != "1:9.20.15-1~deb13u1" || apt.installed["bind9-host"] != "1:9.20.15-1~deb13u1" || apt.installed["base-files"] != "13.8+deb13u6" {
		t.Fatalf("installed %v", apt.installed)
	}
	if got := apt.ran(); len(got) != 1 || !strings.HasSuffix(got[0], "install --only-upgrade bind9-host=1:9.20.15-1~deb13u1 bind9-libs=1:9.20.15-1~deb13u1") {
		t.Errorf("ran %v", got)
	}
	if c := change(t, f.planWith(debianPolicy), upgradesID); c.Status != plan.StatusInSync || c.Note == nil || c.Note.Code != codeUpgradesLists {
		t.Errorf("after the upgrade: %+v", c)
	}
	// The grant has its deadline in the store, never in what mamelet gets.
	for _, g := range f.revision().Grants() {
		if g.Action == ActionUpgradePackages && g.Expires.IsZero() {
			t.Error("a grant without a deadline")
		}
	}
	// Taken out of the declaration: mamelet stops working the upgrades out; what was upgraded stays.
	f.declare()
	f.apply(f.planWith(debianPolicy))
	if p := partOf(f.revision()); p.AptUpgrades != nil {
		t.Errorf("still worked out: %+v", p.AptUpgrades)
	}
}

// Between the plan and the apply, what upgrading needs changed (a new dependency): mamelet's fresh
// simulation is not the approved change set, so it upgrades nothing, the apply stops saying so, and
// the next plan shows the new set for a new approval.
func TestAnUpgradeWhoseChangeSetChangedIsRefused(t *testing.T) {
	f, apt := upgradeFlow(t)
	f.reported(debianPolicy)
	p := f.planWith(debianPolicy)
	if c := change(t, p, upgradesID); c.Status != plan.StatusChange {
		t.Fatalf("the plan: %+v", c)
	}
	apt.needs["bind9-libs"] = []string{"Inst libuv1t64 (1.50.0-2 Debian:13.6/stable [amd64])"}
	rep := f.apply(p)
	if rep.Stopped == nil || !strings.Contains(rep.Stopped.Message, "refused") || !strings.Contains(rep.Stopped.Message, "not the approved") {
		t.Fatalf("apply %+v", rep.Stopped)
	}
	if got := apt.ran(); len(got) != 0 || apt.installed["bind9-libs"] != "1:9.20.11-4" {
		t.Fatalf("ran %v, installed %v", got, apt.installed)
	}
	f.node.round()
	c := change(t, f.planWith(debianPolicy), upgradesID)
	if c.Status != plan.StatusChange || !slices.ContainsFunc(c.Diffs, func(d plan.Diff) bool { return strings.Contains(d.To, "install libuv1t64 1.50.0-2") }) ||
		c.Note == nil || !strings.Contains(c.Note.Message, "the last approved upgrade did not succeed: refused") {
		t.Fatalf("the next plan: %+v %+v", c, c.Note)
	}
}

// An upgrade cut short (dpkg failed after the first package): the apply stops with apt's words, what
// was upgraded stays, and the next plan shows only what is left; approved, it finishes.
func TestAPartialUpgradeIsPlannedAgain(t *testing.T) {
	f, apt := upgradeFlow(t)
	f.reported(debianPolicy)
	apt.installFail = "E: Sub-process /usr/bin/dpkg returned an error code (1)\n"
	rep := f.apply(f.planWith(debianPolicy))
	if rep.Stopped == nil || !strings.Contains(rep.Stopped.Message, "dpkg returned an error code") {
		t.Fatalf("apply %+v", rep.Stopped)
	}
	apt.installFail = ""
	f.node.round()
	p := f.planWith(debianPolicy)
	c := change(t, p, upgradesID)
	if c.Status != plan.StatusChange || len(c.Diffs) != 3 || !strings.Contains(c.Diffs[2].To, "upgrade bind9-libs") {
		t.Fatalf("what is left: %+v", c.Diffs)
	}
	if rep := f.apply(p); rep.Stopped != nil || len(rep.Steps) != 1 || rep.Steps[0].Outcome != reconcile.OutcomeDone {
		t.Fatalf("apply again %+v", rep.Stopped)
	}
	if apt.installed["bind9-libs"] != "1:9.20.15-1~deb13u1" {
		t.Errorf("installed %v", apt.installed)
	}
}

// An observe-only node: the plan shows the pending upgrades, held (MAM-POL-0312); the node's revision
// lets mamelet neither upgrade nor read the lists again, and an apply runs nothing.
func TestAnObserveOnlyNodesUpgradesAreHeld(t *testing.T) {
	f, apt := upgradeFlow(t)
	pol := debianPolicy
	pol.ObserveOnly = []string{"web"}
	f.reported(pol)
	p := f.planWith(pol)
	c := change(t, p, upgradesID)
	if c.Status != plan.StatusChange || c.Verdict != policy.Denied || !c.Held() || c.Operations[0].Decision.Code != "MAM-POL-0312" {
		t.Fatalf("the plan: %+v", c)
	}
	if u := partOf(f.revision()).AptUpgrades; u == nil || u.Refresh || u.Upgrade != nil {
		t.Fatalf("the part: %+v", u)
	}
	if _, err := f.st.Approve(f.ctx, p.Digest, "test", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rec.Apply(f.ctx, p.Digest); err != nil {
		t.Fatal(err)
	}
	if got := apt.ran(); len(got) != 0 || apt.installed["bind9-libs"] != "1:9.20.11-4" {
		t.Errorf("ran %v", got)
	}
	// Its lists grow old: mamelet does not read them again, and the plan says so.
	f.now = f.now.Add(13 * time.Hour)
	f.node.round()
	c = change(t, f.planWith(pol), upgradesID)
	if c.Status != plan.StatusBlocked || c.Note.Code != codeUpgradesStale || !strings.Contains(c.Note.Message, "MAM-POL-0312") || len(apt.ran()) != 0 {
		t.Errorf("old lists: %+v %+v", c, c.Note)
	}
}

// Lists that grow older than the bound between two reports: the plan waits for mamelet, which reads
// them again itself at its next round.
func TestOldListsAreReadAgainBeforeThePlan(t *testing.T) {
	f, apt := upgradeFlow(t)
	apt.setLists(f.now.Add(-11*time.Hour - 58*time.Minute))
	f.reported(debianPolicy)
	f.now = f.now.Add(5 * time.Minute) // the report is 5 minutes old
	p := f.planWith(debianPolicy)
	c := change(t, p, upgradesID)
	if c.Status != plan.StatusWaiting || c.Note.Code != codeUpgradesRefreshing || !strings.Contains(c.Note.Message, "12h03m old") {
		t.Fatalf("old lists: %+v %+v", c, c.Note)
	}
	f.node.round()
	if got := apt.ran(); !slices.Equal(got, []string{"apt-get -o DPkg::Lock::Timeout=300 update"}) {
		t.Fatalf("ran %v", got)
	}
	if c := change(t, f.planWith(debianPolicy), upgradesID); c.Status != plan.StatusChange || !strings.Contains(c.Note.Message, "less than a minute old") {
		t.Errorf("fresh lists: %+v %+v", c, c.Note)
	}
}
