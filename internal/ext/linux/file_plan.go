package linux

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
)

// The plan notes of NodeFile (doc.go reserves the ranges).
const (
	codeFileAwait         = "MAM-PLAN-0140" // waits for mamelet to report the file
	codeFileNoAccount     = "MAM-PLAN-0141" // the owner or the group does not exist on the node
	codeFileStartGate     = "MAM-PLAN-0142" // a service's start waits for a file it is restarted by
	codeFileAccountComing = "MAM-PLAN-0143" // the owner or the group is declared on the node, not made yet
	codeFileUnsafe        = "MAM-PLAN-0210" // mamelet does not write there (a link, a directory)
	codeFileUnobserved    = "MAM-PLAN-0211" // mamelet cannot observe it
	codeFileKept          = "MAM-PLAN-0310" // not declared any more: kept on the node
)

// accountMakers say which resource of a declaration makes a user (group false) or a group on a node:
// a file owned by one waits for it instead of being blocked. NodeUser adds its own (user_plan.go).
var accountMakers []func(g []graph.Resource, node, name string, group bool) (id string, ok bool)

// filePlanner plans NodeFile.
var filePlanner = plan.Planner{
	Plan: func(k *plan.Kit, r graph.Resource) plan.Change { return fileChange(k, r) },
	Reported: func(k *plan.Kit, r graph.Resource) bool {
		f := FileOf(r)
		o, ok := observed(k, f.Node)
		return ok && o.file(f.Path) != nil
	},
}

// observed is the linux observation of node, as its mamelet reported it (ok false: none, or one that
// cannot be read).
func observed(k *plan.Kit, node string) (Observed, bool) {
	e, ok := k.Ext(node, Name)
	if !ok || len(e.Payload) == 0 {
		return Observed{}, false
	}
	var o Observed
	if decodeStrict(e.Payload, &o) != nil {
		return Observed{}, false
	}
	return o, true
}

func (o Observed) file(path string) *FileState {
	for i := range o.Files {
		if o.Files[i].Path == path {
			return &o.Files[i]
		}
	}
	return nil
}

// fileChange plans a file from mamelet's report of it. It is in sync when the file is a regular file
// with the content's sha256, the mode, the owner and the group declared. Otherwise it is written, with
// an approval: the plan shows the whole new content (of the old one, only its sha256: the node never
// sends a file's content), what else changes, the directories it creates, and the services it
// restarts.
func fileChange(k *plan.Kit, r graph.Resource) plan.Change {
	want := FileOf(r)
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
	var got *FileState
	if readable {
		got = o.file(want.Path)
	}
	switch {
	case reported && len(e.Payload) > 0 && !readable:
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: codeFileUnobserved,
			Message: fmt.Sprintf("the report of mamelet on %s about its files cannot be read", want.Node),
			Hint:    "the node's mamelet and this Control Plane do not agree on the report's form: update mamelet (docs/mamelet-update.md)"}
		return c
	case got == nil:
		c.Status, c.Note = k.AwaitAgent(want.Node, &plan.Note{Code: codeFileAwait,
			Message: fmt.Sprintf("waits for the agent on %s to report %s", want.Node, want.Path)})
		return c
	case k.Stale(e.ObservedAt):
		c.Status, c.Note = plan.StatusBlocked, k.StaleNote(e.ObservedAt)
		return c
	case got.Error != "":
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: codeFileUnobserved,
			Message: fmt.Sprintf("the agent on %s cannot observe %s: %s", want.Node, want.Path, got.Error),
			Hint:    "fix the cause on the node, then plan again"}
		return c
	case got.Unsafe != "":
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: codeFileUnsafe,
			Message: fmt.Sprintf("mamelet does not write %s on %s: %s", want.Path, want.Node, got.Unsafe),
			Hint:    "Mamela never follows a symbolic link: declare the real path, or replace what is there by hand"}
		return c
	}
	sum := ContentSHA256(want.Content)
	if got.Exists && !got.Large && got.SHA256 == sum && got.Mode == want.Mode && got.Owner == want.Owner && got.Group == want.Group {
		return k.WithOps(c)
	}
	if note := accountNote(k, o, want); note != nil {
		c.Status, c.Note = note.status, note.note
		return c
	}
	diff := func(field, from, to string) plan.Diff { return plan.Diff{Field: field, From: from, To: to} }
	c.Diffs = []plan.Diff{diff("file", "", want.Path)}
	old := got.SHA256
	if got.Large {
		old = fmt.Sprintf("(larger than %d bytes)", MaxContent)
	}
	if !got.Exists || got.Large || got.SHA256 != sum {
		c.Diffs = append(c.Diffs, diff("sha256", old, sum), diff("content", "", want.Content))
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
	return k.WithOps(c, policy.Subject{Op: OpWriteFile})
}

type statusNote struct {
	status plan.Status
	note   *plan.Note
}

// accountNote says what a file whose owner or group is not on the node waits for (nil when both are
// there): the declaration's resource that makes it, or a person (blocked).
func accountNote(k *plan.Kit, o Observed, want *FileSpec) *statusNote {
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
			return &statusNote{plan.StatusWaiting, &plan.Note{Code: codeFileAwait,
				Message: fmt.Sprintf("waits for the agent on %s to report whether %s %s exists", want.Node, a.what, a.name)}}
		case exists:
			continue
		}
		for _, made := range accountMakers {
			if id, ok := made(k.Resources(), want.Node, a.name, a.group); ok {
				return &statusNote{plan.StatusWaiting, &plan.Note{Code: codeFileAccountComing,
					Message: fmt.Sprintf("waits for %s %s to be made on %s (%s) before %s is written", a.what, a.name, want.Node, id, want.Path)}}
			}
		}
		return &statusNote{plan.StatusBlocked, &plan.Note{Code: codeFileNoAccount,
			Message: fmt.Sprintf("%s %s does not exist on %s, so %s cannot be owned by it", a.what, a.name, want.Node, want.Path),
			Hint:    fmt.Sprintf("install the package that makes %s, declare it (NodeUser, when it comes), or own the file by an existing %s such as root", a.name, a.what)}}
	}
	return nil
}

// fileStartGate: the start of a service that a file of its node restarts waits for that file to be
// in sync, so that the service starts with it.
func fileStartGate(k *plan.Kit, node, service string) *plan.Note {
	unit := strings.TrimSuffix(service, ".service")
	for _, r := range k.Resources() {
		f := FileOf(r)
		if f == nil || f.Node != node || !slices.ContainsFunc(f.Restart, func(s string) bool { return strings.TrimSuffix(s, ".service") == unit }) {
			continue
		}
		if c := fileChange(k, r); c.Status != plan.StatusInSync {
			return &plan.Note{Code: codeFileStartGate,
				Message: fmt.Sprintf("waits for %s to be written before %s is started (the file restarts it)", f.Path, service),
				Hint:    "apply the plan that writes the file, then plan again"}
		}
	}
	return nil
}

// undeclaredFiles are the files mamelet still observes on a machine of the declaration that no
// NodeFile of it has any more: they stay on the node as they are (Mamela does not delete files yet),
// and mamelet stops observing them when the plan is applied.
func undeclaredFiles(k *plan.Kit) []plan.Change {
	machines, declared := map[string]bool{}, map[string]bool{}
	for _, r := range k.Resources() {
		if r.Provided != nil {
			machines[r.Name] = true
		}
		if f := FileOf(r); f != nil {
			declared[FileName(f.Node, f.Path)] = true
		}
	}
	var out []plan.Change
	for _, node := range sortedKeys(machines) {
		o, ok := observed(k, node)
		if !ok {
			continue
		}
		for _, f := range o.Files {
			name := FileName(node, f.Path)
			if declared[name] || !f.Exists || f.Error != "" {
				continue
			}
			out = append(out, plan.Change{Kind: KindFile, Name: name, Status: plan.StatusRetained, Diffs: []plan.Diff{}, Operations: []plan.Operation{},
				Note: &plan.Note{Code: codeFileKept,
					Message: "not in this declaration: the file stays on the node as it is (Mamela does not delete files yet), and mamelet stops watching it once this plan is applied",
					Hint:    "remove it by hand on the node if it should go"}})
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
