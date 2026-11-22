package linux

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
)

// The plan notes of NodeAptUpgrades (codes.go reserves the ranges).
const (
	codeUpgradesAwait      = "MAM-PLAN-0165" // waits for mamelet to report the pending upgrades
	codeUpgradesStale      = "MAM-PLAN-0166" // the package lists are old, and mamelet could not (or does not) read them again
	codeUpgradesRefreshing = "MAM-PLAN-0167" // waits for mamelet to read the package lists again
	codeUpgradesLists      = "MAM-PLAN-0168" // how old the package lists the plan was made from are (information)
	codeUpgradesReboot     = "MAM-PLAN-0169" // warning: the node says it needs a reboot (information)
	codeUpgradesUnobserved = "MAM-PLAN-0225" // mamelet cannot work out the upgrades
	codeUpgradesDpkg       = "MAM-PLAN-0226" // dpkg was interrupted: apt installs nothing until it is finished by hand
)

var aptUpgradesPlanCodes = []string{codeUpgradesAwait, codeUpgradesStale, codeUpgradesRefreshing, codeUpgradesLists, codeUpgradesReboot,
	codeUpgradesUnobserved, codeUpgradesDpkg}

// AptListsMaxAge is how old the package lists may be: mamelet reads them again (apt-get update) when
// they are older, and a plan does not work from older ones (it waits for mamelet, or is blocked when
// mamelet could not read them).
var AptListsMaxAge = 12 * time.Hour

// aptUpgradesPlanner plans NodeAptUpgrades.
var aptUpgradesPlanner = plan.Planner{
	Plan: func(k *plan.Kit, r graph.Resource) plan.Change { return aptUpgradesChange(k, r) },
	Reported: func(k *plan.Kit, r graph.Resource) bool {
		o, ok := observed(k, AptUpgradesOf(r).Node)
		return ok && o.AptUpgrades != nil
	},
}

// listsTime is when the package lists were last read, as far as mamelet knows: the newer of its own
// apt-get update that read every source and apt's newest release file (zero: never).
func (s AptUpgradesState) listsTime() time.Time {
	if s.RefreshedAt.After(s.ListsAt) {
		return s.RefreshedAt
	}
	return s.ListsAt
}

// refreshFailed is what mamelet's last apt-get update said when it failed after the lists were last
// read ("" when it did not).
func (s AptUpgradesState) refreshFailed() string {
	if s.RefreshFailed == "" || !s.RefreshFailedAt.After(s.listsTime()) {
		return ""
	}
	return s.RefreshFailed
}

// ageText is a duration for a person: "40m", "3h05m", "12 days".
func ageText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// listsText says how old the package lists are at asOf, and from what.
func (s AptUpgradesState) listsText(node string, asOf time.Time) string {
	at := s.listsTime()
	if at.IsZero() {
		return fmt.Sprintf("apt on %s has no package lists (nothing in /var/lib/apt/lists)", node)
	}
	from := "apt's newest index (release file)"
	if at.Equal(s.RefreshedAt) {
		from = "mamelet's last apt-get update"
	}
	return fmt.Sprintf("the package lists on %s are %s old (%s, %s)", node, ageText(asOf.Sub(at)), from, at.UTC().Format(time.RFC3339))
}

// aptUpgradesChange plans the upgrades of a node from mamelet's report: in sync when nothing is
// pending from the declared origins; otherwise a change that shows every package with its versions
// and origins, and the change set's digest, which an approval binds mamelet to. It is not planned from
// package lists older than AptListsMaxAge.
func aptUpgradesChange(k *plan.Kit, r graph.Resource) plan.Change {
	want := AptUpgradesOf(r)
	c := plan.NewChange(r)
	if ok, note := k.NodeReady(want.Node); !ok {
		c.Status, c.Note = plan.StatusWaiting, note
		return c
	}
	if status, note := k.ExtNotTaken(want.Node, Name, Version, Release); note != nil {
		c.Status, c.Note = status, note
		return c
	}
	e, reported := k.Ext(want.Node, Name)
	o, readable := observed(k, want.Node)
	var got *AptUpgradesState
	if readable {
		got = o.AptUpgrades
	}
	blocked := func(code, message, hint string) plan.Change {
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: code, Message: message, Hint: hint}
		return c
	}
	switch {
	case reported && len(e.Payload) > 0 && !readable:
		return blocked(codeUpgradesUnobserved, fmt.Sprintf("the report of mamelet on %s about its pending upgrades cannot be read", want.Node),
			"the node's mamelet and this Control Plane do not agree on the report's form: update mamelet (docs/mamelet-update.md)")
	case got == nil:
		c.Status, c.Note = k.AwaitAgent(want.Node, &plan.Note{Code: codeUpgradesAwait,
			Message: fmt.Sprintf("waits for the agent on %s to report its pending upgrades", want.Node)})
		return c
	case k.Stale(e.ObservedAt):
		c.Status, c.Note = plan.StatusBlocked, k.StaleNote(e.ObservedAt)
		return c
	}
	// What the node says of itself is shown whatever the rest is.
	if got.RebootRequired {
		w := plan.Note{Code: codeUpgradesReboot,
			Message: fmt.Sprintf("%s says it needs a reboot (/run/reboot-required)", want.Node),
			Hint:    "what was upgraded (a kernel, libc) takes effect after a reboot, which Mamela never does by itself: reboot the machine when it suits, with its provider's explicit operation (mamela act)"}
		if len(got.RebootPackages) > 0 {
			w.Message += ", for " + strings.Join(got.RebootPackages, ", ")
		}
		c.Warnings = append(c.Warnings, w)
	}
	switch {
	case got.Error != "":
		return blocked(codeUpgradesUnobserved, fmt.Sprintf("the agent on %s cannot work out its pending upgrades: %s", want.Node, got.Error),
			"fix the cause on the node (run apt-get -s dist-upgrade there to see what apt says), then plan again")
	case got.DpkgInterrupted:
		return blocked(codeUpgradesDpkg, fmt.Sprintf("dpkg on %s did not finish what it was doing (an upgrade or an installation was cut short): apt installs nothing until it is finished", want.Node),
			"finish it by hand on the node: dpkg --configure -a (then apt-get -f install if it asks), and plan again; Mamela does not do it, because it runs whatever was left half-done")
	}

	asOf := k.Snapshot().AsOf
	at := got.listsTime()
	lists := got.listsText(want.Node, asOf)
	if at.IsZero() || asOf.Sub(at) > AptListsMaxAge {
		old := lists
		if !at.IsZero() {
			old += ", older than " + ageText(AptListsMaxAge)
		}
		failed := got.refreshFailed()
		switch {
		case failed != "":
			return blocked(codeUpgradesStale, fmt.Sprintf("%s, and mamelet could not read them again: its apt-get update at %s said: %s",
				old, got.RefreshFailedAt.UTC().Format(time.RFC3339), failed),
				fmt.Sprintf("fix what apt-get update says on %s (a network, proxy or DNS it cannot use, a source that is gone or whose key changed: sources.list or sources.list.d), "+
					"or run apt-get update there by hand; mamelet tries again every %s, and the next plan has the pending upgrades", want.Node, ageText(AptRefreshRetry)))
		case k.Policy().Holds(want.Node):
			d, _ := k.Policy().Held(want.Node)
			return blocked(codeUpgradesStale, fmt.Sprintf("%s: mamelet does not read them again on a machine Mamela does not change (%s)", old, d.Code),
				fmt.Sprintf("run apt-get update on %s by hand to see what is pending, then plan again", want.Node))
		}
		c.Status, c.Note = plan.StatusWaiting, &plan.Note{Code: codeUpgradesRefreshing,
			Message: fmt.Sprintf("waits for mamelet to read the package lists again: %s (mamelet runs apt-get update itself then, as apt-daily does)", old),
			Hint:    "the next report has fresh lists; if it does not come, the plan says what apt-get update said"}
		return c
	}

	set := got.set(want.Origins)
	switch {
	case set == nil:
		c.Status, c.Note = plan.StatusWaiting, &plan.Note{Code: codeUpgradesAwait,
			Message: fmt.Sprintf("waits for the agent on %s to work out the pending upgrades from the origins declared here (%s)", want.Node, strings.Join(want.Origins, ", "))}
		return c
	case set.Error != "":
		return blocked(codeUpgradesUnobserved, fmt.Sprintf("the agent on %s cannot work out the upgrades from the origins declared here (%s): %s", want.Node, strings.Join(want.Origins, ", "), set.Error),
			"fix the cause on the node (apt-get -s install --only-upgrade of the packages shows what apt says: a held package, a dependency it cannot install), then plan again")
	}

	info := &plan.Note{Code: codeUpgradesLists, Message: lists}
	if f := got.refreshFailed(); f != "" {
		info.Message += fmt.Sprintf("; mamelet's last apt-get update, at %s, did not read every source: %s", got.RefreshFailedAt.UTC().Format(time.RFC3339), f)
		info.Hint = "fix what it says on the node, or it is used as it is: the plan was made from the lists apt has"
	}
	if got.Failed != "" {
		info.Message += "; the last approved upgrade did not succeed: " + got.Failed
		if info.Hint == "" {
			info.Hint = "what it did stays; this plan shows what is left to upgrade"
		}
	}
	if len(set.Changes) == 0 {
		c = k.WithOps(c)
		c.Note = info
		return c
	}

	// The diffs name every package apt would touch and the change set's digest: approving the plan
	// approves exactly this set, and mamelet refuses to upgrade anything else.
	upgrades := 0
	origins := slices.Clone(want.Origins)
	var lines []plan.Diff
	for _, ch := range set.Changes {
		if ch.Action == noderev.ChangeUpgrade {
			upgrades++
		}
		lines = append(lines, plan.Diff{Field: "apt", To: ch.Line()})
		origins = append(origins, ch.Origins...)
	}
	c.Diffs = append([]plan.Diff{
		{Field: "pending upgrades", From: fmt.Sprintf("%d from %s", upgrades, strings.Join(want.Origins, ", ")), To: "0"},
		{Field: "changeSet", To: set.Digest},
	}, lines...)
	warnings := c.Warnings
	c = k.WithOps(c, policy.Subject{Op: OpUpgradePackages, Removes: set.Removes(), Origins: slices.Compact(slices.Sorted(slices.Values(origins)))})
	c.Note, c.Warnings = info, warnings
	return c
}
