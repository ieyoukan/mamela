package linux

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
)

// The plan notes of NodeFile (codes.go reserves the ranges).
const (
	codeFileAwait         = "MAM-PLAN-0140" // waits for mamelet to report the file
	codeFileNoAccount     = "MAM-PLAN-0141" // the owner or the group does not exist on the node
	codeFileStartGate     = "MAM-PLAN-0142" // a service's start waits for a file it is restarted by
	codeFileAccountComing = "MAM-PLAN-0143" // the owner or the group is declared on the node, not made yet
	codeFileExposed       = "MAM-PLAN-0144" // warning: a user other than root can change a directory above the file (NodeArtifact too)
	codeFileUnsafe        = "MAM-PLAN-0210" // mamelet does not write there (a link, a directory)
	codeFileUnobserved    = "MAM-PLAN-0211" // mamelet cannot observe it
	codeFileKept          = "MAM-PLAN-0310" // not declared any more: kept on the node
	codePending           = "MAM-PLAN-0218" // written, but its daemon-reload or a restart did not finish (NodeArtifact too)
)

// filePlanCodes are NodeFile's codes of plan notes, with the two it shares with NodeArtifact (0144, 0218).
var filePlanCodes = []string{codeFileAwait, codeFileNoAccount, codeFileStartGate, codeFileAccountComing, codeFileExposed, codeFileUnsafe, codeFileUnobserved, codeFileKept, codePending}

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
	noRoom := roomNote(k, r)
	switch {
	case reported && len(e.Payload) > 0 && !readable:
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: codeFileUnobserved,
			Message: fmt.Sprintf("the report of mamelet on %s about its files cannot be read", want.Node),
			Hint:    "the node's mamelet and this Control Plane do not agree on the report's form: update mamelet (docs/mamelet-update.md)"}
		return c
	case noRoom != nil:
		c.Status, c.Note = plan.StatusWaiting, noRoom
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
	same := got.Exists && !got.Large && got.SHA256 == sum && got.Mode == want.Mode && got.Owner == want.Owner && got.Group == want.Group
	left := pendingFor(got.Pending, sum, want.Path, want.Restart)
	if same && left == nil {
		return k.WithOps(c)
	}
	if note := accountNote(k, o, want); note != nil {
		c.Status, c.Note = note.status, note.note
		return c
	}
	diff := func(field, from, to string) plan.Diff { return plan.Diff{Field: field, From: from, To: to} }
	c.Diffs = []plan.Diff{diff("file", "", want.Path)}
	if same {
		return pendingChange(k, c, want.Node, want.Path, *left, want.Restart, OpWriteFile)
	}
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
	if w := exposedWarning(want.Node, want.Path, want.Owner, want.Group, got.Exposed); w != nil {
		c.Warnings = append(c.Warnings, *w)
	}
	return k.WithOps(c, policy.Subject{Op: OpWriteFile})
}

// pendingFor is what of what mamelet reports pending after the last write of path (left) the
// declaration still asks for: the file is still the declared one (sum), systemd reads it again if it
// is a unit, and the services it restarts that are still declared (nil: nothing).
func pendingFor(left *Pending, sum, path string, restart []string) *Pending {
	if left == nil || left.SHA256 != sum {
		return nil
	}
	out := Pending{SHA256: sum, Reload: left.Reload && IsUnitFile(path)}
	for _, svc := range left.Restart {
		unit := strings.TrimSuffix(svc, ".service")
		if slices.ContainsFunc(restart, func(s string) bool { return strings.TrimSuffix(s, ".service") == unit }) {
			out.Restart = append(out.Restart, svc)
		}
	}
	if !out.Reload && len(out.Restart) == 0 {
		return nil
	}
	return &out
}

// pendingChange is the change of a file (or an artifact) that is as declared, but after whose last
// write what follows did not finish (left): approving it lets mamelet do it, the file being as
// approved already (its grant writes nothing then). change has its first diff (the path).
func pendingChange(k *plan.Kit, c plan.Change, node, path string, left Pending, restart []string, op policy.Op) plan.Change {
	diff := func(field, from, to string) plan.Diff { return plan.Diff{Field: field, From: from, To: to} }
	c.Diffs = append(c.Diffs, diff("pending", "", "the file is as declared; after it was written, "+pendingText(left)+" did not finish"))
	if IsUnitFile(path) {
		c.Diffs = append(c.Diffs, diff("daemon-reload", "", "systemd reads its units again (the file is one of them)"))
	}
	if len(restart) > 0 {
		c.Diffs = append(c.Diffs, diff("restart", "", strings.Join(restart, ", ")+" (each only if it runs: systemctl try-restart)"))
	}
	c = k.WithOps(c, policy.Subject{Op: op})
	c.Note = &plan.Note{Code: codePending,
		Message: fmt.Sprintf("%s on %s was written as declared, but %s did not finish afterwards (it failed, or mamelet stopped meanwhile): what runs may not use it yet", path, node, pendingText(left)),
		Hint:    "approve this plan to have mamelet do it now (the file is not written again); the result of the last attempt says why it did not finish"}
	return c
}

// exposedWarning is the warning of a file (or an artifact) owned by owner:group that is written
// below a directory someone else than root and its owner can change (exposed: what mamelet reported
// of the directories above it): they can replace it, or put something in its way (nil when no one
// can).
func exposedWarning(node, path, owner, group string, exposed []DirOwner) *plan.Note {
	var who []string
	add := func(w string) {
		if !slices.Contains(who, w) {
			who = append(who, w)
		}
	}
	first := ""
	for _, d := range exposed {
		mode, _ := strconv.ParseUint(d.Mode, 8, 32)
		n := len(who)
		if d.Owner != "root" && d.Owner != owner {
			add("user " + d.Owner)
		}
		if mode&0o020 != 0 && d.Group != "root" && d.Group != group {
			add("group " + d.Group)
		}
		if mode&0o002 != 0 {
			add("every user")
		}
		if len(who) > n && first == "" {
			first = fmt.Sprintf("%s (%s %s:%s)", d.Dir, d.Mode, d.Owner, d.Group)
		}
	}
	if len(who) == 0 {
		return nil
	}
	return &plan.Note{Code: codeFileExposed,
		Message: fmt.Sprintf("%s on %s is below %s, which %s can change: they can replace what Mamela writes there (owned by %s:%s), or put something in its way", path, node, first, strings.Join(who, ", "), owner, group),
		Hint:    "write the file under a directory only root can change (such as /etc/<app> or /opt/<app>), or make the directory root's by hand (chown root:root, chmod go-w)"}
}

type statusNote struct {
	status plan.Status
	note   *plan.Note
}

// accountCodes are the codes of what a kind's resource owned by a user and a group waits for.
type accountCodes struct {
	await  string // mamelet has not said whether the user or group exists
	coming string // something of the declaration makes it: a NodeUser, or a package not installed yet
	none   string // nothing does: blocked
}

// accountNote says what a file whose owner or group is not on the node waits for (nil when both are
// there).
func accountNote(k *plan.Kit, o Observed, want *FileSpec) *statusNote {
	return accountWait(k, o, want.Node, want.Path, want.Owner, want.Group, "written", "the file",
		accountCodes{codeFileAwait, codeFileAccountComing, codeFileNoAccount})
}

// accountWait says what a resource at path on node owned by owner and group waits for while one of
// them is not on the node (nil when both are there): the declaration's resource that makes it
// (accountMakers: a NodeUser), a package declared on the node that is not installed yet (it may make
// it, as the package prometheus makes the user prometheus: the same as NodeUser's needs), or a person
// (blocked). done is what happens to the resource ("written"), what what it is ("the file").
func accountWait(k *plan.Kit, o Observed, node, path, owner, group, done, what string, codes accountCodes) *statusNote {
	for _, a := range []struct {
		name   string
		group  bool
		exists map[string]bool
		what   string
	}{{owner, false, o.Users, "user"}, {group, true, o.Groups, "group"}} {
		exists, observedName := a.exists[a.name]
		switch {
		case !observedName:
			// The revision named another owner when mamelet last looked (the declaration changed): it
			// says once it has the new revision.
			return &statusNote{plan.StatusWaiting, &plan.Note{Code: codes.await,
				Message: fmt.Sprintf("waits for the agent on %s to report whether %s %s exists", node, a.what, a.name)}}
		case exists:
			continue
		}
		for _, made := range accountMakers {
			if id, ok := made(k.Resources(), node, a.name, a.group); ok {
				return &statusNote{plan.StatusWaiting, &plan.Note{Code: codes.coming,
					Message: fmt.Sprintf("waits for %s %s to be made on %s (%s) before %s is %s", a.what, a.name, node, id, path, done)}}
			}
		}
		if pkg := pendingPackage(k, node); pkg != "" {
			return &statusNote{plan.StatusWaiting, &plan.Note{Code: codes.coming,
				Message: fmt.Sprintf("waits for %s %s on %s before %s is %s: package %s is not installed there yet, and a package may make it", a.what, a.name, node, path, done, pkg)}}
		}
		return &statusNote{plan.StatusBlocked, &plan.Note{Code: codes.none,
			Message: fmt.Sprintf("%s %s does not exist on %s, so %s cannot be owned by it", a.what, a.name, node, path),
			Hint:    fmt.Sprintf("declare the package that makes %s (NodePackage) or the %s itself (NodeUser), or own %s by an existing %s such as root", a.name, a.what, what, a.what)}}
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

// undeclaredFiles are the files the declaration in force has on a machine of this declaration (InForce:
// Mamela may have written them) that this declaration does not have any more: they stay on the node as
// they are (Mamela does not delete files yet), and mamelet stops observing them when the plan is
// applied. A file only another declaration planned (a pull request's) is not this plan's business.
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
			if declared[name] || !f.Exists || f.Error != "" || !o.inForce(func(i *InForce) []string { return i.Files }, f.Path) {
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
