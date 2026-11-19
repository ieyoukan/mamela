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

// The plan notes of NodeUser (codes.go reserves the ranges).
const (
	codeUserAwait      = "MAM-PLAN-0145" // waits for mamelet to report the user (or what it needs)
	codeUserNever      = "MAM-PLAN-0146" // the user is not as declared in what Mamela never changes, or cannot be made
	codeUserMissing    = "MAM-PLAN-0147" // a group or the shell the user needs is not on the node
	codeUserAsIs       = "MAM-PLAN-0148" // warning: the user has what the declaration does not say, kept
	codeUserLockout    = "MAM-PLAN-0149" // warning: SSH keys are taken out of authorized_keys
	codeUserUnsafe     = "MAM-PLAN-0212" // mamelet does not write the user's SSH keys there (a link, no home)
	codeUserUnobserved = "MAM-PLAN-0213" // mamelet cannot observe the user, or its report cannot be read
	codeUserKept       = "MAM-PLAN-0311" // not declared any more: kept on the node
)

var userPlanCodes = []string{codeUserAwait, codeUserNever, codeUserMissing, codeUserAsIs, codeUserLockout, codeUserUnsafe, codeUserUnobserved, codeUserKept}

// The fields of a user's diffs. The grant reads the approved change from them (user_node.go), so
// the plan and the grant name them alike.
const (
	diffUser      = "user"
	diffSystem    = "system"
	diffUID       = "uid"
	diffShell     = "shell"
	diffHome      = "home"
	diffGroup     = "group"
	diffGroups    = "groups"
	diffKeysSHA   = "authorized_keys sha256"
	diffKeysFile  = "authorized_keys"
	diffSSHDir    = ".ssh"
	diffAddKey    = "add key"
	diffRemoveKey = "remove key"
	diffOther     = "remove lines"
)

// noGroups is how the plan writes a user that is in no supplementary group.
const noGroups = "(none)"

// noFile is how the plan writes an authorized_keys that does not exist.
const noFile = "(no file)"

// The mode of authorized_keys and of ~/.ssh, as Mamela writes them.
const (
	keysMode = "0600"
	sshMode  = "0700"
)

// userPlanner plans NodeUser.
var userPlanner = plan.Planner{
	Plan: func(k *plan.Kit, r graph.Resource) plan.Change { return userChange(k, r) },
	Reported: func(k *plan.Kit, r graph.Resource) bool {
		u := UserOf(r)
		o, ok := observed(k, u.Node)
		return ok && o.user(u.Name) != nil
	},
}

// A file owned by a user, or a group, that a NodeUser of the declaration makes waits for it.
func init() {
	accountMakers = append(accountMakers, func(rs []graph.Resource, node, name string, _ bool) (string, bool) {
		// A user's own group (useradd --user-group) has its name.
		for _, r := range rs {
			if u := UserOf(r); u != nil && u.Node == node && u.Name == name {
				return r.ID(), true
			}
		}
		return "", false
	})
}

// userChange plans a user from mamelet's report of it. A user that does not exist is made, with an
// approval (linux.create-user); one that exists is changed, with an approval (linux.change-user), when
// it is in fewer of the declared groups, has another shell than the declared one, or its
// authorized_keys is not exactly the declared keys. Mamela never changes its uid or its home, nor
// takes it out of a group: the plan blocks the first and says the second.
func userChange(k *plan.Kit, r graph.Resource) plan.Change {
	want := UserOf(r)
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
	var got *UserState
	if readable {
		got = o.user(want.Name)
	}
	noRoom := roomNote(k, r)
	switch {
	case reported && len(e.Payload) > 0 && !readable:
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: codeUserUnobserved,
			Message: fmt.Sprintf("the report of mamelet on %s about its users cannot be read", want.Node),
			Hint:    "the node's mamelet and this Control Plane do not agree on the report's form: update mamelet (docs/mamelet-update.md)"}
		return c
	case noRoom != nil:
		c.Status, c.Note = plan.StatusWaiting, noRoom
		return c
	case got == nil:
		c.Status, c.Note = k.AwaitAgent(want.Node, &plan.Note{Code: codeUserAwait,
			Message: fmt.Sprintf("waits for the agent on %s to report user %s", want.Node, want.Name)})
		return c
	case k.Stale(e.ObservedAt):
		c.Status, c.Note = plan.StatusBlocked, k.StaleNote(e.ObservedAt)
		return c
	case got.Error != "":
		c.Status, c.Note = plan.StatusBlocked, &plan.Note{Code: codeUserUnobserved,
			Message: fmt.Sprintf("the agent on %s cannot observe user %s: %s", want.Node, want.Name, got.Error),
			Hint:    "fix the cause on the node, then plan again"}
		return c
	}
	var s statusNote
	if got.Exists {
		s = userUpdate(k, &c, want, got, o)
	} else {
		s = userCreate(k, &c, want, got, o)
	}
	if s.note != nil {
		c.Status, c.Note, c.Diffs, c.Warnings = s.status, s.note, []plan.Diff{}, nil
		return c
	}
	if len(c.Diffs) == 0 {
		return k.WithOps(c)
	}
	op := OpChangeUser
	if !got.Exists {
		op = OpCreateUser
	}
	return k.WithOps(c, policy.Subject{Op: op})
}

// awaitReport is the note of a user whose report does not say yet what the plan needs (the
// declaration changed since mamelet last looked: it says once it has the new revision).
func awaitReport(want *UserSpec, what string) statusNote {
	return statusNote{plan.StatusWaiting, &plan.Note{Code: codeUserAwait,
		Message: fmt.Sprintf("waits for the agent on %s to report %s (for user %s)", want.Node, what, want.Name)}}
}

// userCreate plans the making of a user: what useradd makes, the groups it is added to, and its
// authorized_keys. It needs the shell and the groups on the node, and the uid and the user's own
// group free.
func userCreate(k *plan.Kit, c *plan.Change, want *UserSpec, got *UserState, o Observed) statusNote {
	if want.UID != 0 {
		who, checked := got.UIDs[strconv.Itoa(want.UID)]
		switch {
		case !checked:
			return awaitReport(want, fmt.Sprintf("which user has uid %d", want.UID))
		case who != "":
			return statusNote{plan.StatusBlocked, &plan.Note{Code: codeUserNever,
				Message: fmt.Sprintf("user %s cannot be made on %s with uid %d: user %s has it", want.Name, want.Node, want.UID, who),
				Hint:    "declare another uid, or leave uid out (useradd chooses one)"}}
		}
	}
	switch taken, checked := o.Groups[want.Name]; {
	case !checked:
		return awaitReport(want, "whether group "+want.Name+" exists")
	case taken:
		return statusNote{plan.StatusBlocked, &plan.Note{Code: codeUserNever,
			Message: fmt.Sprintf("user %s cannot be made on %s: a group called %s exists already, and useradd makes a group of the user's name", want.Name, want.Node, want.Name),
			Hint:    fmt.Sprintf("remove the group by hand if nothing uses it, or make the user by hand in it (useradd -g %s %s); Mamela then manages the user", want.Name, want.Name)}}
	}
	shell := want.CreateShell()
	if n := needs(k, want, got, o, shell); n != nil {
		return *n
	}
	home, made := want.CreateHome()
	if made && home != "" {
		switch hs := got.newHome(home); {
		case hs == nil:
			return awaitReport(want, "whether its home "+home+" is there already")
		case hs.Error != "":
			return statusNote{plan.StatusBlocked, &plan.Note{Code: codeUserUnobserved,
				Message: fmt.Sprintf("the agent on %s cannot look at the home %s of user %s: %s", want.Node, home, want.Name, hs.Error),
				Hint:    "fix the cause on the node, then plan again"}}
		case hs.Exists:
			return statusNote{plan.StatusBlocked, &plan.Note{Code: codeUserNever,
				Message: fmt.Sprintf("user %s cannot be made on %s with home %s: it %s, and useradd would make it the home of %s as it is (it only warns), without making it %s's: %s would share another's files, and Mamela would write its SSH keys into another's ~/.ssh",
					want.Name, want.Node, home, homeThere(hs), want.Name, want.Name, want.Name),
				Hint: fmt.Sprintf("declare another home for %s, or remove or move %s by hand if nothing needs it; Mamela never takes over a directory that is there", want.Name, home)}}
		}
	}
	c.Diffs = append(c.Diffs, plan.Diff{Field: diffUser, To: want.Name})
	if want.System {
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffSystem, To: "true (useradd --system)"})
	}
	if want.UID != 0 {
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffUID, To: strconv.Itoa(want.UID)})
	}
	c.Diffs = append(c.Diffs, plan.Diff{Field: diffShell, To: shell})
	switch {
	case home != "" && made:
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffHome, To: home + " (made)"})
	case home != "":
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffHome, To: home + " (not made: a system user)"})
	case made:
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffHome, To: "useradd's default for a system user (made, for its SSH keys)"})
	default:
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffHome, To: "useradd's default for a system user (not made)"})
	}
	c.Diffs = append(c.Diffs, plan.Diff{Field: diffGroup, To: want.Name + " (the user's own, made)"})
	if len(want.Groups) > 0 {
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffGroups, To: strings.Join(want.Groups, ", ")})
	}
	if want.AuthorizedKeys != nil {
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffKeysSHA, To: ContentSHA256(want.KeysContent())})
		for _, l := range want.AuthorizedKeys {
			key, _ := ParseAuthorizedKey(l) // the graph checked every line
			c.Diffs = append(c.Diffs, plan.Diff{Field: diffAddKey, To: key.String()})
		}
	}
	return statusNote{}
}

// userUpdate plans the change of a user that exists. What Mamela never changes (the uid, the home)
// blocks; what the user has besides the declaration (groups, being a system user or not) is said
// and kept.
func userUpdate(k *plan.Kit, c *plan.Change, want *UserSpec, got *UserState, o Observed) statusNote {
	never := func(message, hint string) statusNote {
		return statusNote{plan.StatusBlocked, &plan.Note{Code: codeUserNever, Message: message, Hint: hint}}
	}
	switch {
	case got.UID == 0:
		return never(fmt.Sprintf("user %s on %s has uid 0, root's: Mamela never manages root", want.Name, want.Node),
			"take the user out of the declaration; an account with uid 0 besides root is worth a look by hand")
	case want.UID != 0 && got.UID != want.UID:
		return never(fmt.Sprintf("user %s on %s has uid %d, not the declared %d: Mamela never changes a uid (the user's files keep the old one)", want.Name, want.Node, got.UID, want.UID),
			fmt.Sprintf("declare uid %d, or leave uid out (it is used only when Mamela makes the user)", got.UID))
	case want.Home != "" && got.Home != want.Home:
		return never(fmt.Sprintf("user %s on %s has home %s, not the declared %s: Mamela never moves a home", want.Name, want.Node, got.Home, want.Home),
			fmt.Sprintf("declare home %q, or leave home out (it is used only when Mamela makes the user)", got.Home))
	}
	shell := ""
	if want.Shell != "" && got.Shell != want.Shell {
		shell = want.Shell
	}
	if n := needs(k, want, got, o, shell); n != nil {
		return *n
	}
	var add []string
	for _, g := range want.Groups {
		if !slices.Contains(got.Groups, g) && g != got.Group {
			add = append(add, g)
		}
	}
	if len(add) > 0 {
		after := slices.Sorted(slices.Values(append(slices.Clone(got.Groups), add...)))
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffGroups, From: listOrNone(got.Groups), To: strings.Join(after, ", ")})
	}
	if shell != "" {
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffShell, From: got.Shell, To: shell})
	}
	if want.AuthorizedKeys != nil {
		if n := keysDiffs(c, want, got); n != nil {
			return *n
		}
	}
	// What the user has that the declaration does not say: kept, and said.
	var extra []string
	for _, g := range got.Groups {
		if !slices.Contains(want.Groups, g) {
			extra = append(extra, g)
		}
	}
	if len(extra) > 0 {
		what, stays := "group "+extra[0]+", which the declaration does not name", "it stays"
		if len(extra) > 1 {
			what, stays = "groups "+strings.Join(extra, ", ")+", which the declaration does not name", "they stay"
		}
		c.Warnings = append(c.Warnings, plan.Note{Code: codeUserAsIs,
			Message: fmt.Sprintf("user %s on %s is also in %s: Mamela adds memberships and never removes one, so %s", want.Name, want.Node, what, stays),
			Hint:    fmt.Sprintf("declare the group if it is meant, or take the user out of it by hand (gpasswd -d %s %s)", want.Name, extra[0])})
	}
	// Whether the node's user is a system user is told by its uid. A declared uid made it so (the
	// declaration says both), so this is said only of a user the declaration gives no uid: one made
	// otherwise, which stays as it is on every plan until the declaration says what it is.
	if sys := systemUID(got.UID); sys != want.System && want.UID == 0 {
		what := map[bool]string{true: "a system user", false: "a user (not a system user)"}
		c.Warnings = append(c.Warnings, plan.Note{Code: codeUserAsIs,
			Message: fmt.Sprintf("user %s on %s is %s (uid %d), but is declared as %s: Mamela does not change that, which only says how the user is made", want.Name, want.Node, what[sys], got.UID, what[want.System]),
			Hint:    fmt.Sprintf("declare system: %v to match the node", sys)})
	}
	return statusNote{}
}

// count is "1 key", "2 keys".
func count(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// systemUID is whether a uid is a system user's, in Debian's ranges (below 1000, and nobody).
func systemUID(uid int) bool { return uid < 1000 || uid == 65534 }

func listOrNone(l []string) string {
	if len(l) == 0 {
		return noGroups
	}
	return strings.Join(l, ", ")
}

// needs says what the user waits for, or why it is blocked, when the shell it is made with or set
// to ("" none) or a declared group is not on the node: a package of the node that is not installed
// yet may make it (docker-ce makes the group docker, zsh the shell /usr/bin/zsh); otherwise a person
// must.
func needs(k *plan.Kit, want *UserSpec, got *UserState, o Observed, shell string) *statusNote {
	var missing []string
	if shell != "" {
		ok, checked := got.Shells[shell]
		switch {
		case !checked:
			n := awaitReport(want, "whether shell "+shell+" exists")
			return &n
		case !ok:
			missing = append(missing, "shell "+shell)
		}
	}
	var groups []string
	for _, g := range want.Groups {
		exists, checked := o.Groups[g]
		switch {
		case !checked:
			n := awaitReport(want, "whether group "+g+" exists")
			return &n
		case !exists:
			groups = append(groups, g)
		}
	}
	if len(groups) > 0 {
		missing = append(missing, "group "+strings.Join(groups, ", "))
	}
	if len(missing) == 0 {
		return nil
	}
	if pkg := pendingPackage(k, want.Node); pkg != "" {
		return &statusNote{plan.StatusWaiting, &plan.Note{Code: codeUserMissing,
			Message: fmt.Sprintf("waits for %s on %s (for user %s): package %s is not installed there yet, and a package may make it", strings.Join(missing, " and "), want.Node, want.Name, pkg)}}
	}
	hint := "the group is made by a package, such as docker-ce for docker: declare it (NodePackage), or make the group by hand"
	if len(groups) == 0 {
		hint = "install the package that has the shell (NodePackage, such as zsh), or declare another shell"
	}
	return &statusNote{plan.StatusBlocked, &plan.Note{Code: codeUserMissing,
		Message: fmt.Sprintf("%s of user %s does not exist on %s", strings.Join(missing, " and "), want.Name, want.Node),
		Hint:    hint}}
}

// pendingPackage is a package declared on node that is not installed yet ("" when there is none).
func pendingPackage(k *plan.Kit, node string) string {
	packages := k.Snapshot().Packages
	for _, r := range k.Resources() {
		if p := r.NodePackage; p != nil && p.Node == node {
			if got, ok := packages[r.Name]; !ok || !got.Installed {
				return p.Package
			}
		}
	}
	return ""
}

// keysDiffs adds what writing the declared authorized_keys changes: the file's sha256, the keys it
// adds and takes out (by fingerprint), its mode and owner, and ~/.ssh. Keys taken out (and lines
// Mamela does not read, and options) get a warning: logins with them stop working.
func keysDiffs(c *plan.Change, want *UserSpec, got *UserState) *statusNote {
	ks := got.Keys
	switch {
	case ks == nil:
		n := awaitReport(want, "the SSH keys")
		return &n
	case ks.File.Error != "":
		return &statusNote{plan.StatusBlocked, &plan.Note{Code: codeUserUnobserved,
			Message: fmt.Sprintf("the agent on %s cannot observe the SSH keys of user %s: %s", want.Node, want.Name, ks.File.Error),
			Hint:    "fix the cause on the node, then plan again"}}
	case ks.File.Unsafe != "":
		return &statusNote{plan.StatusBlocked, &plan.Note{Code: codeUserUnsafe,
			Message: fmt.Sprintf("mamelet does not write the SSH keys of user %s on %s: %s", want.Name, want.Node, ks.File.Unsafe),
			Hint:    "Mamela never follows a symbolic link: replace the link by a directory by hand, or leave authorizedKeys out"}}
	}
	if ks.NotOwned != "" {
		return &statusNote{plan.StatusBlocked, &plan.Note{Code: codeUserUnsafe,
			Message: fmt.Sprintf("mamelet does not write the SSH keys of user %s on %s: %s, and Mamela never writes into another user's home or ~/.ssh", want.Name, want.Node, ks.NotOwned),
			Hint:    fmt.Sprintf("give %s a home of its own by hand (usermod --home), or make the directory %s's (chown) if it is meant to be; or leave authorizedKeys out", want.Name, want.Name)}}
	}
	sshDir := got.Home + "/.ssh"
	if len(ks.File.Missing) > 1 || len(ks.File.Missing) == 1 && ks.File.Missing[0] != sshDir {
		return &statusNote{plan.StatusBlocked, &plan.Note{Code: codeUserUnsafe,
			Message: fmt.Sprintf("the home %s of user %s does not exist on %s, so its SSH keys cannot be written", got.Home, want.Name, want.Node),
			Hint:    fmt.Sprintf("make the home by hand (mkhomedir_helper %s), or leave authorizedKeys out", want.Name)}}
	}
	owner := want.Name + ":" + got.Group
	content := want.KeysContent()
	sum := ContentSHA256(content)
	f := ks.File
	if !f.Exists || f.Large || f.SHA256 != sum {
		from := f.SHA256
		switch {
		case !f.Exists:
			from = noFile
		case f.Large:
			from = fmt.Sprintf("(larger than %d bytes)", MaxContent)
		}
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffKeysSHA, From: from, To: sum})
		declared := map[string]bool{}
		var add []string
		for _, l := range want.AuthorizedKeys {
			key, _ := ParseAuthorizedKey(l)
			declared[key.Fingerprint] = true
			if !slices.ContainsFunc(ks.Found, func(k FoundKey) bool { return k.Fingerprint == key.Fingerprint }) {
				add = append(add, key.String())
			}
		}
		for _, a := range add {
			c.Diffs = append(c.Diffs, plan.Diff{Field: diffAddKey, To: a})
		}
		var lost []string
		for _, k := range ks.Found {
			switch {
			case !declared[k.Fingerprint]:
				c.Diffs = append(c.Diffs, plan.Diff{Field: diffRemoveKey, To: k.Type + " " + k.Fingerprint})
				lost = append(lost, k.Fingerprint)
			case k.Options:
				c.Diffs = append(c.Diffs, plan.Diff{Field: diffRemoveKey, To: "the options of " + k.Type + " " + k.Fingerprint + " (the key stays, without them)"})
			}
		}
		if ks.More > 0 {
			c.Diffs = append(c.Diffs, plan.Diff{Field: diffRemoveKey, To: count(ks.More, "more key") + " (the agent lists at most " + fmt.Sprint(MaxFoundKeys) + ")"})
		}
		if ks.Other > 0 {
			c.Diffs = append(c.Diffs, plan.Diff{Field: diffOther, To: count(ks.Other, "line") + " that Mamela does not read as a key (a certificate, another key type)"})
		}
		if len(lost) > 0 || ks.More > 0 || ks.Other > 0 {
			var what []string
			if len(lost) > 0 {
				what = append(what, count(len(lost), "key")+" ("+strings.Join(lost, ", ")+")")
			}
			if ks.More > 0 {
				what = append(what, count(ks.More, "more key"))
			}
			if ks.Other > 0 {
				what = append(what, count(ks.Other, "other line"))
			}
			c.Warnings = append(c.Warnings, plan.Note{Code: codeUserLockout,
				Message: fmt.Sprintf("taken out of the authorized_keys of user %s on %s: %s; logins with them stop working once this plan is applied", want.Name, want.Node, strings.Join(what, " and ")),
				Hint:    "make sure the keys that stay are the ones you log in with (or that you have another way in) before you approve"})
		}
	}
	if f.Exists {
		if is := f.Mode + " " + f.Owner + ":" + f.Group; is != keysMode+" "+owner {
			c.Diffs = append(c.Diffs, plan.Diff{Field: diffKeysFile, From: is, To: keysMode + " " + owner})
		}
	} else {
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffKeysFile, From: noFile, To: sshDir + "/authorized_keys (" + keysMode + " " + owner + ")"})
	}
	switch want := sshMode + " " + owner; {
	case len(f.Missing) == 1:
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffSSHDir, From: "(none)", To: sshDir + " (" + want + ", made)"})
	case ks.Dir != want:
		c.Diffs = append(c.Diffs, plan.Diff{Field: diffSSHDir, From: ks.Dir, To: want})
	}
	return nil
}

// undeclaredUsers are the users the declaration in force has on a machine of this declaration
// (InForce) that this declaration does not have any more: they stay on the node as they are (Mamela
// never deletes a user), and mamelet stops observing them when the plan is applied.
func undeclaredUsers(k *plan.Kit) []plan.Change {
	machines, declared := map[string]bool{}, map[string]bool{}
	for _, r := range k.Resources() {
		if r.Provided != nil {
			machines[r.Name] = true
		}
		if u := UserOf(r); u != nil {
			declared[UserName(u.Node, u.Name)] = true
		}
	}
	var out []plan.Change
	for _, node := range sortedKeys(machines) {
		o, ok := observed(k, node)
		if !ok {
			continue
		}
		for _, u := range o.UserStates {
			name := UserName(node, u.Name)
			if declared[name] || !u.Exists || u.Error != "" || !o.inForce(func(i *InForce) []string { return i.Users }, u.Name) {
				continue
			}
			out = append(out, plan.Change{Kind: KindUser, Name: name, Status: plan.StatusRetained, Diffs: []plan.Diff{}, Operations: []plan.Operation{},
				Note: &plan.Note{Code: codeUserKept,
					Message: "not in this declaration: the user stays on the node as it is, with its groups, home and SSH keys (Mamela never deletes a user), and mamelet stops watching it once this plan is applied",
					Hint:    "remove it by hand on the node (userdel) if it should go"}})
		}
	}
	return out
}
