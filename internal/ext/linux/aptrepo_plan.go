package linux

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
)

// The plan notes of NodeAptRepo (doc.go reserves the ranges).
const (
	codeAptRepoAwait       = "MAM-PLAN-0150" // waits for mamelet to report the source
	codeAptRepoPackageGate = "MAM-PLAN-0151" // a package's installation waits for a source of its node
	codeAptRepoUnread      = "MAM-PLAN-0152" // the files are in place, apt has not read the source
	codeAptRepoConflict    = "MAM-PLAN-0153" // another source file of the node names the same URI and suite
	codeAptRepoUnsafe      = "MAM-PLAN-0214" // mamelet does not write there (a link, a directory)
	codeAptRepoUnobserved  = "MAM-PLAN-0215" // mamelet cannot observe it
	codeAptRepoKept        = "MAM-PLAN-0312" // not declared any more: kept on the node
)

var aptRepoPlanCodes = []string{codeAptRepoAwait, codeAptRepoPackageGate, codeAptRepoUnread, codeAptRepoConflict, codeAptRepoUnsafe, codeAptRepoUnobserved, codeAptRepoKept}

// aptUpdate is what applying a source always does last (and the diff that says so, the same in
// every plan of the source: running apply again after apt-get update failed retries it).
const aptUpdate = "apt reads the package lists of every source again (apt-get update)"

// aptRepoPlanner plans NodeAptRepo.
var aptRepoPlanner = plan.Planner{
	Plan: func(k *plan.Kit, r graph.Resource) plan.Change { return aptRepoChange(k, r) },
	Reported: func(k *plan.Kit, r graph.Resource) bool {
		a := AptRepoOf(r)
		o, ok := observed(k, a.Node)
		return ok && o.aptRepo(a.Name) != nil
	},
}

func (o Observed) aptRepo(name string) *AptRepoState {
	for i := range o.AptRepos {
		if o.AptRepos[i].Name == name {
			return &o.AptRepos[i]
		}
	}
	return nil
}

// rootFile: a regular file with that sha256, 0644, root's (the source's file and its key).
func rootFile(f FileState, sum string) bool {
	return f.Exists && !f.Large && f.SHA256 == sum && f.Mode == "0644" && f.Owner == "root" && f.Group == "root"
}

// aptRepoChange plans a source from mamelet's report of it. It is in sync when its key is in one of
// its two files with the declared sha256, the source's file is what AptSources renders for that key,
// both 0644 and root's, the key's other file is not there, and apt has the release file of each URI
// and suite (an apt-get update has read them). Otherwise mamelet adds it, with an approval: the plan
// shows the whole file, the key's URL and sha256, and that apt-get update runs.
func aptRepoChange(k *plan.Kit, r graph.Resource) plan.Change {
	want := AptRepoOf(r)
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
	var got *AptRepoState
	if readable {
		got = o.aptRepo(want.Name)
	}
	blocked := func(code, message, hint string) plan.Change {
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: code, Message: message, Hint: hint}
		return c
	}
	switch {
	case reported && len(e.Payload) > 0 && !readable:
		return blocked(codeAptRepoUnobserved, fmt.Sprintf("the report of mamelet on %s about its APT sources cannot be read", want.Node),
			"the node's mamelet and this Control Plane do not agree on the report's form: update mamelet (docs/mamelet-update.md)")
	case got == nil:
		c.Status, c.Note = k.AwaitAgent(want.Node, &plan.Note{Code: codeAptRepoAwait,
			Message: fmt.Sprintf("waits for the agent on %s to report APT source %s", want.Node, want.Name)})
		return c
	case k.Stale(e.ObservedAt):
		c.Status, c.Note = plan.StatusBlocked, k.StaleNote(e.ObservedAt)
		return c
	case got.Error != "":
		return blocked(codeAptRepoUnobserved, fmt.Sprintf("the agent on %s cannot observe APT source %s: %s", want.Node, want.Name, got.Error),
			"fix the cause on the node, then plan again")
	}
	for _, f := range []FileState{got.Sources, got.KeyArmored, got.KeyBinary} {
		switch {
		case f.Error != "":
			return blocked(codeAptRepoUnobserved, fmt.Sprintf("the agent on %s cannot observe %s: %s", want.Node, f.Path, f.Error),
				"fix the cause on the node, then plan again")
		case f.Unsafe != "":
			return blocked(codeAptRepoUnsafe, fmt.Sprintf("mamelet does not write %s on %s: %s", f.Path, want.Node, f.Unsafe),
				"Mamela never follows a symbolic link: replace what is there by hand (the mamela-* sources and keys are Mamela's)")
		}
	}
	if len(got.Conflicts) > 0 {
		return blocked(codeAptRepoConflict, fmt.Sprintf("%s on %s names a URI and suite of APT source %s too: apt refuses one source with two keys (Signed-By), and would then read no source at all",
			strings.Join(got.Conflicts, ", "), want.Node, want.Name),
			"remove that source from the node by hand (it came from a vendor's instructions or a package), or do not declare this one")
	}

	// The key decides which file it is in (and which Signed-By names): until it is on the node, the
	// plan shows the file for an ASCII-armored key.
	has := func(f FileState) bool { return f.Exists && !f.Large && f.SHA256 == want.KeySHA256 }
	binary := has(got.KeyBinary) && !has(got.KeyArmored)
	known := binary || has(got.KeyArmored)
	key, other := got.KeyArmored, got.KeyBinary
	if binary {
		key, other = got.KeyBinary, got.KeyArmored
	}
	keyPath := AptKeyPath(want.Name, !binary)
	sources := AptSources(*want, keyPath)
	sum := ContentSHA256(sources)
	srcPath := AptSourcesPath(want.Name)
	diff := func(field, from, to string) plan.Diff { return plan.Diff{Field: field, From: from, To: to} }

	if known && rootFile(key, want.KeySHA256) && rootFile(got.Sources, sum) && !other.Exists {
		if len(got.Unread) == 0 {
			return k.WithOps(c)
		}
		// In place, but apt has not read it: the update failed, or did not run. Only the update is
		// left (the same diffs as when the source was added, so that apply can retry it).
		c.Diffs = []plan.Diff{diff("apt source", "", srcPath), diff("apt-get update", "", aptUpdate)}
		c = k.WithOps(c, policy.Subject{Op: OpAddAptRepo})
		c.Note = &plan.Note{Code: codeAptRepoUnread,
			Message: fmt.Sprintf("the files of APT source %s are in place on %s, but apt has not read %s: an apt-get update failed, or has not run since they were written",
				want.Name, want.Node, strings.Join(got.Unread, ", ")),
			Hint: "applying runs apt-get update again (approve this plan, or run apply again on the plan whose update failed); the result of the last attempt says why it failed"}
		return c
	}

	c.Diffs = []plan.Diff{diff("apt source", "", srcPath)}
	old := ""
	if got.Sources.Exists && got.Sources.SHA256 != sum {
		old = got.Sources.SHA256
		if got.Sources.Large {
			old = fmt.Sprintf("(larger than %d bytes)", MaxContent)
		}
	}
	c.Diffs = append(c.Diffs, diff("sha256", old, sum), diff("content", "", sources))
	if f := got.Sources; f.Exists && !f.Large && f.SHA256 == sum && !rootFile(f, sum) {
		c.Diffs = append(c.Diffs, diff("mode and owner", fmt.Sprintf("%s %s:%s", f.Mode, f.Owner, f.Group), "0644 root:root"))
	}
	c.Diffs = append(c.Diffs, diff("key", "", want.KeyURL))
	oldKey := ""
	for _, f := range []FileState{key, other} {
		if f.Exists && f.SHA256 != want.KeySHA256 {
			oldKey = f.SHA256
			break
		}
	}
	c.Diffs = append(c.Diffs, diff("key sha256", oldKey, want.KeySHA256))
	if known {
		form := "ASCII-armored"
		if binary {
			form = "binary"
		}
		c.Diffs = append(c.Diffs, diff("key file", "", fmt.Sprintf("%s (0644 root:root, %s)", keyPath, form)))
		if !rootFile(key, want.KeySHA256) {
			c.Diffs = append(c.Diffs, diff("key mode and owner", fmt.Sprintf("%s %s:%s", key.Mode, key.Owner, key.Group), "0644 root:root"))
		}
		if other.Exists {
			c.Diffs = append(c.Diffs, diff("remove", "", other.Path+" (the key in its other form, which Signed-By does not name)"))
		}
	} else {
		c.Diffs = append(c.Diffs, diff("key file", "", fmt.Sprintf("%s (0644 root:root) if the downloaded key is ASCII-armored, as Signed-By names it above; %s if it is binary, which Signed-By then names instead; the file of the other form is removed",
			AptKeyPath(want.Name, true), AptKeyPath(want.Name, false))))
	}
	var dirs []string
	for _, d := range append(slices.Clone(got.Sources.Missing), key.Missing...) {
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		c.Diffs = append(c.Diffs, diff("create directory", "", d+" (0755 root:root)"))
	}
	c.Diffs = append(c.Diffs, diff("apt-get update", "", aptUpdate))
	return k.WithOps(c, policy.Subject{Op: OpAddAptRepo})
}

// aptRepoPackageGate: the installation of any package on a node waits while an APT source of that
// node is not in place and read by apt: the package may come from it (Mamela cannot tell which
// source a package comes from before apt has read them all), and its change set is worked out only
// then. A package that is installed is not affected.
func aptRepoPackageGate(k *plan.Kit, node, pkg string) *plan.Note {
	for _, r := range k.Resources() {
		a := AptRepoOf(r)
		if a == nil || a.Node != node {
			continue
		}
		if c := aptRepoChange(k, r); c.Status != plan.StatusInSync {
			return &plan.Note{Code: codeAptRepoPackageGate,
				Message: fmt.Sprintf("waits for APT source %s on %s to be in place and read by apt before %s is installed: the package may come from it", a.Name, node, pkg),
				Hint: "apply the plan that adds the source; the next plan installs the package, with what apt would change " +
					"(mamelacd with sync = \"push\" applies it at its next poll; otherwise approve that plan too)"}
		}
	}
	return nil
}

// undeclaredAptRepos are the sources mamelet still observes on a machine of the declaration that no
// NodeAptRepo of it has any more: they stay on the node as they are (Mamela does not remove sources
// yet), and mamelet stops observing them when the plan is applied.
func undeclaredAptRepos(k *plan.Kit) []plan.Change {
	machines, declared := map[string]bool{}, map[string]bool{}
	for _, r := range k.Resources() {
		if r.Provided != nil {
			machines[r.Name] = true
		}
		if a := AptRepoOf(r); a != nil {
			declared[AptRepoName(a.Node, a.Name)] = true
		}
	}
	var out []plan.Change
	for _, node := range sortedKeys(machines) {
		o, ok := observed(k, node)
		if !ok {
			continue
		}
		for _, s := range o.AptRepos {
			name := AptRepoName(node, s.Name)
			if declared[name] || s.Error != "" || !(s.Sources.Exists || s.KeyArmored.Exists || s.KeyBinary.Exists) {
				continue
			}
			out = append(out, plan.Change{Kind: KindAptRepo, Name: name, Status: plan.StatusRetained, Diffs: []plan.Diff{}, Operations: []plan.Operation{},
				Note: &plan.Note{Code: codeAptRepoKept,
					Message: "not in this declaration: the APT source and its key stay on the node as they are (Mamela does not remove sources yet), and apt keeps installing from it; mamelet stops watching them once this plan is applied",
					Hint: fmt.Sprintf("to remove it, delete %s and %s (or .gpg) by hand on the node, then run apt-get update",
						AptSourcesPath(s.Name), AptKeyPath(s.Name, true))}})
		}
	}
	return out
}
