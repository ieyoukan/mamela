package linux

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
)

// The plan notes of NodeArtifact (doc.go reserves the ranges).
const (
	codeArtifactAwait         = "MAM-PLAN-0155" // waits for mamelet to report the artifact
	codeArtifactNoAccount     = "MAM-PLAN-0156" // the owner or the group does not exist on the node
	codeArtifactStartGate     = "MAM-PLAN-0157" // a service's start waits for an artifact it is restarted by
	codeArtifactAccountComing = "MAM-PLAN-0158" // the owner or the group is declared on the node, not made yet
	codeArtifactReplaces      = "MAM-PLAN-0159" // the file at the path is not one Mamela installed: it is replaced
	codeArtifactUnsafe        = "MAM-PLAN-0216" // mamelet does not install there (a link, a directory)
	codeArtifactUnobserved    = "MAM-PLAN-0217" // mamelet cannot observe it
	codeArtifactKept          = "MAM-PLAN-0313" // not declared any more: kept on the node
)

// artifactPlanCodes are NodeArtifact's codes of plan notes (planCodes).
var artifactPlanCodes = []string{codeArtifactAwait, codeArtifactNoAccount, codeArtifactStartGate, codeArtifactAccountComing,
	codeArtifactReplaces, codeArtifactUnsafe, codeArtifactUnobserved, codeArtifactKept}

// artifactPlanner plans NodeArtifact.
var artifactPlanner = plan.Planner{
	Plan: func(k *plan.Kit, r graph.Resource) plan.Change { return artifactChange(k, r) },
	Reported: func(k *plan.Kit, r graph.Resource) bool {
		a := ArtifactOf(r)
		o, ok := observed(k, a.Node)
		return ok && o.artifact(a.Path) != nil
	},
}

// shownExtract is how the plan shows the member an artifact takes out of its download.
func shownExtract(e string) string {
	if e == "" {
		return "(none: the download itself)"
	}
	return e
}

// artifactChange plans an artifact from mamelet's report of it. It is in sync when the file at the
// path is still the one mamelet installed from a download of the declared sha256 (and member), with
// the mode, the owner and the group declared. Otherwise it is installed, with an approval: the plan
// shows the URL, the sha256 and the member, what was there before (nothing, another download, or a
// file Mamela did not install), what else changes, the directories it creates and the services it
// restarts.
func artifactChange(k *plan.Kit, r graph.Resource) plan.Change {
	want := ArtifactOf(r)
	c := plan.NewChange(r)
	if ok, note := k.NodeReady(want.Node); !ok {
		c.Status, c.Note = plan.StatusWaiting, note
		return c
	}
	if note := k.ExtNotTaken(want.Node, Name, Version, Release); note != nil {
		c.Status, c.Note = plan.StatusBlocked, note
		return c
	}
	e, reported := k.Ext(want.Node, Name)
	o, readable := observed(k, want.Node)
	var got *ArtifactState
	if readable {
		got = o.artifact(want.Path)
	}
	switch {
	case reported && len(e.Payload) > 0 && !readable:
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: codeArtifactUnobserved,
			Message: fmt.Sprintf("the report of mamelet on %s about its artifacts cannot be read", want.Node),
			Hint:    "the node's mamelet and this Control Plane do not agree on the report's form: update mamelet (docs/mamelet-update.md)"}
		return c
	case got == nil:
		c.Status, c.Note = k.AwaitAgent(want.Node, &plan.Note{Code: codeArtifactAwait,
			Message: fmt.Sprintf("waits for the agent on %s to report %s", want.Node, want.Path)})
		return c
	case k.Stale(e.ObservedAt):
		c.Status, c.Note = plan.StatusBlocked, k.StaleNote(e.ObservedAt)
		return c
	case got.Error != "":
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: codeArtifactUnobserved,
			Message: fmt.Sprintf("the agent on %s cannot observe %s: %s", want.Node, want.Path, got.Error),
			Hint:    "fix the cause on the node, then plan again"}
		return c
	case got.Unsafe != "":
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: codeArtifactUnsafe,
			Message: fmt.Sprintf("mamelet does not install %s on %s: %s", want.Path, want.Node, got.Unsafe),
			Hint:    "Mamela never follows a symbolic link: declare the real path, or replace what is there by hand"}
		return c
	}
	from := got.From
	installed := got.Exists && !got.Large && from != nil && from.SHA256 == want.SHA256 && from.Extract == want.Extract
	same := installed && got.Mode == want.Mode && got.Owner == want.Owner && got.Group == want.Group
	var left *Pending
	if installed {
		left = pendingFor(got.Pending, got.SHA256, want.Path, want.Restart)
	}
	if same && left == nil {
		return k.WithOps(c)
	}
	if note := artifactAccountNote(k, o, want); note != nil {
		c.Status, c.Note = note.status, note.note
		return c
	}
	diff := func(field, from, to string) plan.Diff { return plan.Diff{Field: field, From: from, To: to} }
	if same {
		c.Diffs = []plan.Diff{diff("artifact", "", want.Path)}
		return pendingChange(k, c, want.Node, want.Path, *left, want.Restart, OpInstallArtifact)
	}
	// What is installed: always shown, so that the plan says what the approval lets mamelet fetch;
	// what was there before is the "from" of each.
	var oldURL, oldSHA, oldExtract string
	switch {
	case installed:
		// The same download: only the mode or the owner changes (mamelet does not download it again).
	case from != nil:
		oldURL, oldSHA, oldExtract = from.URL, from.SHA256, shownExtract(from.Extract)
	case got.Large:
		oldSHA = fmt.Sprintf("(a file Mamela did not install, larger than %d MiB)", MaxArtifact>>20)
	case got.Exists:
		oldSHA = fmt.Sprintf("(a file Mamela did not install, or that changed since: its sha256 is %s)", got.SHA256)
	}
	if oldURL == want.URL {
		oldURL = ""
	}
	if oldSHA == want.SHA256 {
		oldSHA = ""
	}
	if oldExtract == shownExtract(want.Extract) {
		oldExtract = ""
	}
	c.Diffs = []plan.Diff{diff("artifact", "", want.Path), diff("url", oldURL, want.URL), diff("sha256", oldSHA, want.SHA256)}
	if want.Extract != "" || oldExtract != "" {
		c.Diffs = append(c.Diffs, diff("extract", oldExtract, shownExtract(want.Extract)))
	}
	for _, d := range []struct{ field, from, to string }{{"mode", got.Mode, want.Mode}, {"owner", got.Owner, want.Owner}, {"group", got.Group, want.Group}} {
		if d.from != d.to {
			c.Diffs = append(c.Diffs, diff(d.field, d.from, d.to))
		}
	}
	for _, dir := range got.Missing {
		c.Diffs = append(c.Diffs, diff("create directory", "", dir+" (0755 root:root)"))
	}
	if IsUnitFile(want.Path) {
		c.Diffs = append(c.Diffs, diff("daemon-reload", "", "systemd reads its units again (the file is one of them)"))
	}
	if len(want.Restart) > 0 {
		c.Diffs = append(c.Diffs, diff("restart", "", strings.Join(want.Restart, ", ")+" (each only if it runs: systemctl try-restart)"))
	}
	if w := exposedWarning(want.Node, want.Path, want.Owner, want.Group, got.Exposed); w != nil {
		c.Warnings = append(c.Warnings, *w)
	}
	if got.Exists && from == nil {
		c.Note = &plan.Note{Code: codeArtifactReplaces,
			Message: fmt.Sprintf("%s on %s is not a file Mamela installed (it was put there otherwise, or changed since): installing the artifact replaces it", want.Path, want.Node),
			Hint:    "check on the node what is there before approving: a package or a person may have put it there"}
	}
	return k.WithOps(c, policy.Subject{Op: OpInstallArtifact})
}

// artifactAccountNote says what an artifact whose owner or group is not on the node waits for (nil
// when both are there): the declaration's resource that makes it (accountMakers), or a person
// (blocked).
func artifactAccountNote(k *plan.Kit, o Observed, want *ArtifactSpec) *statusNote {
	for _, a := range []struct {
		name   string
		group  bool
		exists map[string]bool
		what   string
	}{{want.Owner, false, o.Users, "user"}, {want.Group, true, o.Groups, "group"}} {
		exists, observedName := a.exists[a.name]
		switch {
		case !observedName:
			// The revision named another owner when mamelet last looked (the declaration changed): it
			// says once it has the new revision.
			return &statusNote{plan.StatusWaiting, &plan.Note{Code: codeArtifactAwait,
				Message: fmt.Sprintf("waits for the agent on %s to report whether %s %s exists", want.Node, a.what, a.name)}}
		case exists:
			continue
		}
		for _, made := range accountMakers {
			if id, ok := made(k.Resources(), want.Node, a.name, a.group); ok {
				return &statusNote{plan.StatusWaiting, &plan.Note{Code: codeArtifactAccountComing,
					Message: fmt.Sprintf("waits for %s %s to be made on %s (%s) before %s is installed", a.what, a.name, want.Node, id, want.Path)}}
			}
		}
		return &statusNote{plan.StatusBlocked, &plan.Note{Code: codeArtifactNoAccount,
			Message: fmt.Sprintf("%s %s does not exist on %s, so %s cannot be owned by it", a.what, a.name, want.Node, want.Path),
			Hint:    fmt.Sprintf("install the package that makes %s, declare it (NodeUser, when it comes), or own the artifact by an existing %s such as root", a.name, a.what)}}
	}
	return nil
}

// artifactStartGate: the start of a service that an artifact of its node restarts waits for that
// artifact to be installed, so that the service starts with it.
func artifactStartGate(k *plan.Kit, node, service string) *plan.Note {
	unit := strings.TrimSuffix(service, ".service")
	for _, r := range k.Resources() {
		a := ArtifactOf(r)
		if a == nil || a.Node != node || !slices.ContainsFunc(a.Restart, func(s string) bool { return strings.TrimSuffix(s, ".service") == unit }) {
			continue
		}
		if c := artifactChange(k, r); c.Status != plan.StatusInSync {
			return &plan.Note{Code: codeArtifactStartGate,
				Message: fmt.Sprintf("waits for %s to be installed before %s is started (the artifact restarts it)", a.Path, service),
				Hint:    "apply the plan that installs the artifact, then plan again"}
		}
	}
	return nil
}

// undeclaredArtifacts are the artifacts mamelet still observes on a machine of the declaration that
// no NodeArtifact of it has any more: they stay on the node as they are (Mamela does not delete
// them, ADR 0016), and mamelet stops observing them when the plan is applied.
func undeclaredArtifacts(k *plan.Kit) []plan.Change {
	machines, declared := map[string]bool{}, map[string]bool{}
	for _, r := range k.Resources() {
		if r.Provided != nil {
			machines[r.Name] = true
		}
		if a := ArtifactOf(r); a != nil {
			declared[ArtifactName(a.Node, a.Path)] = true
		}
	}
	var out []plan.Change
	for _, node := range sortedKeys(machines) {
		o, ok := observed(k, node)
		if !ok {
			continue
		}
		for _, a := range o.Artifacts {
			name := ArtifactName(node, a.Path)
			if declared[name] || !a.Exists || a.Error != "" {
				continue
			}
			out = append(out, plan.Change{Kind: KindArtifact, Name: name, Status: plan.StatusRetained, Diffs: []plan.Diff{}, Operations: []plan.Operation{},
				Note: &plan.Note{Code: codeArtifactKept,
					Message: "not in this declaration: the file stays on the node as it is (Mamela does not delete what it installed yet), and mamelet stops watching it once this plan is applied",
					Hint:    "remove it by hand on the node if it should go"}})
		}
	}
	return out
}
