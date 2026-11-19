package linux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// The programs mamelet runs for NodeUser, with arguments it builds from checked values only, the
// user's name after "--".
const (
	progUseradd = "/usr/sbin/useradd"
	progUsermod = "/usr/sbin/usermod"
)

// observeUsers observes each user of the part: its line of /etc/passwd and its groups, what it
// needs (the shell, the groups, the uid and the group it would be made with), and its authorized_keys
// when the part asks for it.
func observeUsers(_ context.Context, s *mamelet.System, p Part, o *Observed) {
	if len(p.Users) == 0 {
		return
	}
	r, err := openTree(s)
	if err != nil {
		for _, u := range p.Users {
			o.UserStates = append(o.UserStates, UserState{Name: u.Name, Error: err.Error()})
		}
		return
	}
	defer r.Close()
	a := readAccounts(r)
	if o.Users == nil {
		o.Users = map[string]bool{}
	}
	if o.Groups == nil {
		o.Groups = map[string]bool{}
	}
	for _, u := range p.Users {
		st := observeUser(r, a, u)
		o.UserStates = append(o.UserStates, st)
		if st.Error != "" {
			continue
		}
		_, o.Users[u.Name] = a.users[u.Name]
		for _, g := range append([]string{u.Name}, u.Groups...) {
			if ValidAccount(g) {
				_, o.Groups[g] = a.groups[g]
			}
		}
	}
}

func observeUser(r *os.Root, a accounts, u User) UserState {
	st := UserState{Name: u.Name}
	if why := checkUserName(u.Name); why != "" {
		st.Error = "mamelet refuses the user: " + why
		return st
	}
	for _, sh := range u.Shells {
		if pathShape(sh) == "" {
			if st.Shells == nil {
				st.Shells = map[string]bool{}
			}
			st.Shells[sh] = executable(r, sh)
		}
	}
	e, ok := a.entries[u.Name]
	if !ok {
		for _, id := range u.UIDs {
			if id > 0 {
				if st.UIDs == nil {
					st.UIDs = map[string]string{}
				}
				st.UIDs[strconv.Itoa(id)] = a.userOf[id]
			}
		}
		for _, h := range u.Homes {
			st.NewHomes = append(st.NewHomes, *observeHome(r, a, h))
		}
		return st
	}
	st.Exists, st.UID, st.GID, st.Home, st.Shell = true, e.uid, e.gid, e.home, e.shell
	st.Group = a.groupName(e.gid)
	st.Groups = a.memberOf(u.Name)
	if u.Keys && e.uid != 0 {
		st.Keys = observeKeys(r, a, u.Name, e)
	}
	return st
}

// observeHome observes what is at the home a user would be made with: useradd --create-home takes a
// directory that is there as it is (it only warns, and does not make it the user's), so one that is
// there already, another user's above all, is not made the new user's home (the plan blocks).
func observeHome(r *os.Root, a accounts, home string) *HomeState {
	hs := &HomeState{Path: home}
	if e := CheckHome(home); e != nil {
		hs.Error = "mamelet refuses the home: " + e.Message
		return hs
	}
	fi, err := r.Lstat(rel(home))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return hs
	case err != nil:
		hs.Error = err.Error()
		return hs
	}
	hs.Exists, hs.Mode = true, modeString(fi.Mode())
	switch m := fi.Mode(); {
	case m&fs.ModeSymlink != 0:
		hs.What = "a symbolic link"
	case m.IsDir():
		hs.What = "a directory"
	case m.IsRegular():
		hs.What = "a file"
	default:
		hs.What = "a device, a socket or a pipe"
	}
	if uid, gid, ok := ownerOf(fi); ok {
		hs.Owner, hs.Group = a.userName(uid), a.groupName(gid)
	}
	return hs
}

// executable is whether p is an executable regular file. The links in it are followed (/bin is one
// on a merged /usr, and a shell of the alternatives is an absolute link such as /usr/bin/ksh ->
// /etc/alternatives/ksh -> /bin/ksh93) by looking only: nothing is opened.
func executable(r *os.Root, p string) bool {
	target, err := resolve(r, p)
	if err != nil {
		return false
	}
	fi, err := r.Lstat(rel(target))
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// observeKeys observes a user's ~/.ssh/authorized_keys: the file, ~/.ssh, the home's and ~/.ssh's
// owners, and the keys in it. The home comes from /etc/passwd, and is checked again (CheckHome); the
// file is read through the directories one at a time, never through a link.
func observeKeys(r *os.Root, a accounts, name string, e passwdEntry) *KeysState {
	p := e.home + "/.ssh/authorized_keys"
	ks := &KeysState{File: FileState{Path: p}}
	if err := CheckHome(e.home); err != nil {
		ks.File.Error = "mamelet does not write SSH keys into the home of " + name + ": " + err.Message
		return ks
	}
	removeStaleTemp(r, p)
	ks.File = inspect(r, p, a)
	// The home is the user's: that the user can change what is in it is the point, not a warning.
	ks.File.Exposed = nil
	if fi, err := r.Lstat(rel(e.home + "/.ssh")); err == nil && fi.IsDir() && fi.Mode()&fs.ModeSymlink == 0 {
		if uid, gid, ok := ownerOf(fi); ok {
			ks.Dir = modeString(fi.Mode()) + " " + a.userName(uid) + ":" + a.groupName(gid)
		}
	}
	ks.NotOwned = notOwned(r, a, name, e)
	if !ks.File.Exists || ks.File.Unsafe != "" || ks.File.Error != "" || ks.File.Large {
		return ks
	}
	content, err := readKeys(r, e.home)
	if err != nil {
		ks.File.Error = err.Error()
		return ks
	}
	// What was read through the directories is what the plan judges.
	ks.File.SHA256 = ContentSHA256(content)
	ks.Found, ks.More, ks.Other = scanAuthorizedKeys(content)
	return ks
}

// notOwned says why the home of user name (e) or its ~/.ssh is not the user's to have SSH keys
// written into ("" when they are): the home must be the user's, and ~/.ssh the user's or root's (one
// mamelet made and had not given yet). A home that is another user's (useradd takes a directory
// that is there as it is) is never taken over.
func notOwned(r *os.Root, a accounts, name string, e passwdEntry) string {
	rootUID, _ := a.rootIDs()
	for _, d := range []struct {
		path   string
		orRoot bool
	}{{e.home, false}, {e.home + "/.ssh", true}} {
		fi, err := r.Lstat(rel(d.path))
		if err != nil || !fi.IsDir() {
			continue // missing, or a link: the file's own state says so
		}
		if uid, _, ok := ownerOf(fi); ok && uid != e.uid && !(d.orRoot && uid == rootUID) {
			return fmt.Sprintf("%s is %s's (uid %d), not %s's", d.path, a.userName(uid), uid, name)
		}
	}
	return ""
}

// readKeys reads the authorized_keys of home, opening each directory down to ~/.ssh by itself and
// the file without following a link (openRegular), so that nothing the user put in its home leads
// the read elsewhere.
func readKeys(r *os.Root, home string) (string, error) {
	b, err := readRegular(r, home+"/.ssh/authorized_keys", MaxContent)
	return string(b), err
}

// actUsers carries out the approved changes of users, each attempt once, from a revision fetched
// this round.
func actUsers(ctx context.Context, x *mamelet.Act, p Part) bool {
	if !x.Online {
		return false
	}
	changed := false
	for _, u := range p.Users {
		c := u.Change
		if ctx.Err() != nil {
			break // mamelet is stopping: what is left goes on in a later round
		}
		if c == nil || x.Done(c.Grant.Operation, c.Grant.Attempt) {
			continue
		}
		outcome, detail, ran := changeUser(ctx, x, u, *c)
		x.Record(c.Grant.Operation, c.Grant.Attempt, outcome, detail)
		changed = changed || ran
	}
	return changed
}

// changeUser carries out one approved change of a user. It checks again, whatever the revision
// says, that the user is not root (by name or uid), that the change is the user's, and that what it
// needs is on the node (the shell, the groups, a free uid and group for a new user), before it
// changes anything. Then it makes the user (unless it exists as approved already: an attempt that
// was interrupted), adds the groups it is not in, sets the shell, and writes authorized_keys; and it
// looks again. ran says whether it changed anything.
func changeUser(ctx context.Context, x *mamelet.Act, u User, c UserChange) (outcome, detail string, ran bool) {
	var did []string
	refused := func(why string) (string, string, bool) {
		return noderev.OutcomeRefused, strings.Join(append(did, why), "; "), ran
	}
	failed := func(why string) (string, string, bool) {
		return noderev.OutcomeFailed, strings.Join(append(did, why), "; "), ran
	}
	if why := checkUserName(u.Name); why != "" {
		return refused("mamelet does not change user " + u.Name + ": " + why)
	}
	if err := checkUserChange(c); err != nil {
		return refused(err.Error())
	}
	r, err := openTree(x.System)
	if err != nil {
		return failed(err.Error())
	}
	defer r.Close()
	a := readAccounts(r)
	e, exists := a.entries[u.Name]
	shell := c.Shell
	if k := c.Create; k != nil {
		shell = k.Shell
	}
	switch {
	case exists && e.uid == 0:
		return refused(fmt.Sprintf("user %s has uid 0, root's: mamelet never changes root", u.Name))
	case !exists && c.Create == nil:
		return refused(fmt.Sprintf("user %s does not exist on this node (plan again)", u.Name))
	case exists && c.Create != nil:
		// Made already (by an attempt that was interrupted before it said so, or by hand): the rest
		// goes on only if it is as approved.
		if why := unlikeCreate(e, *c.Create); why != "" {
			return refused(fmt.Sprintf("user %s exists already, but %s, unlike the approved one (plan again)", u.Name, why))
		}
		did = append(did, "user "+u.Name+" existed already as approved")
	case shell != "" && !executable(r, shell):
		return refused(fmt.Sprintf("shell %s is not an executable file on this node", shell))
	case exists && c.Create == nil && c.Shell != "" && e.shell != c.Shell && e.shell != c.Before.Shell:
		// Compare and swap: the shell is as the approved plan saw it, or already the approved one.
		return refused(fmt.Sprintf("the shell of %s changed since the plan (it was %s, it is %s); nothing was changed (plan again)", u.Name, c.Before.Shell, e.shell))
	}
	for _, g := range c.Groups {
		if _, ok := a.groups[g]; !ok {
			return refused(fmt.Sprintf("group %s does not exist on this node", g))
		}
	}
	if k := c.Create; k != nil && !exists {
		if _, taken := a.groups[u.Name]; taken {
			return refused(fmt.Sprintf("a group called %s exists already, and useradd makes one of the user's name", u.Name))
		}
		if who, taken := a.userOf[k.UID]; k.UID != 0 && taken {
			return refused(fmt.Sprintf("uid %d is %s's", k.UID, who))
		}
		if k.CreateHome && k.Home != "" {
			// useradd --create-home takes a directory that is there as it is (it only warns): the new
			// user would get another's home, and its SSH keys would go into another's ~/.ssh.
			if hs := observeHome(r, a, k.Home); hs.Exists || hs.Error != "" {
				return refused(fmt.Sprintf("the home %s of user %s %s: mamelet makes a user only with a home of its own (plan again)", k.Home, u.Name, homeThere(hs)))
			}
		}
		ran = true
		if _, err := x.System.Run(ctx, progUseradd, useraddArgs(u.Name, *k)); err != nil {
			return failed(err.Error())
		}
		did = append(did, "made user "+u.Name)
		a = readAccounts(r)
		if e, exists = a.entries[u.Name]; !exists || e.uid == 0 {
			return failed("after useradd, user " + u.Name + " is not in /etc/passwd as made")
		}
		if k.CreateHome {
			// Made meanwhile by someone else, or useradd's default home was there: the home is not the
			// user's, and nothing more is done in it.
			if why := notOwned(r, a, u.Name, e); why != "" {
				return refused(fmt.Sprintf("after useradd, %s (useradd takes a home that is there as it is): mamelet does not write into it; give the user a home of its own by hand (usermod --home)", why))
			}
		}
	}
	var add []string
	for _, g := range c.Groups {
		if !a.inGroup(u.Name, e.gid, g) {
			add = append(add, g)
		}
	}
	if len(add) > 0 {
		ran = true
		if _, err := x.System.Run(ctx, progUsermod, []string{"--append", "--groups", strings.Join(add, ","), "--", u.Name}); err != nil {
			return failed(err.Error())
		}
		did = append(did, "added "+u.Name+" to "+strings.Join(add, ", "))
	}
	if c.Shell != "" && e.shell != c.Shell {
		ran = true
		if _, err := x.System.Run(ctx, progUsermod, []string{"--shell", c.Shell, "--", u.Name}); err != nil {
			return failed(err.Error())
		}
		did = append(did, "set the shell of "+u.Name+" to "+c.Shell)
	}
	a = readAccounts(r)
	e = a.entries[u.Name]
	if k := c.Keys; k != nil {
		if err := CheckHome(e.home); err != nil {
			return refused("mamelet does not write SSH keys into the home of " + u.Name + ": " + err.Message)
		}
		switch st := inspect(r, e.home+"/.ssh/authorized_keys", a); {
		case st.Unsafe != "":
			return refused("mamelet does not write the SSH keys of " + u.Name + ": " + st.Unsafe + " (mamelet never follows a symbolic link)")
		case len(st.Missing) > 1 || len(st.Missing) == 1 && st.Missing[0] != e.home+"/.ssh":
			return refused("the home " + e.home + " of " + u.Name + " does not exist")
		case c.Before.Keys != nil && !(st.Exists && !st.Large && st.SHA256 == k.SHA256) && !c.Before.Keys.Is(st):
			// Compare and swap: authorized_keys is as the approved plan saw it, or already the approved
			// one (a user that was made by this change has none the plan saw).
			return refused(fmt.Sprintf("%s/.ssh/authorized_keys changed since the plan (it was %s, it is %s); it was not written (plan again)",
				e.home, c.Before.Keys, beforeOf(st)))
		}
		wrote, err := writeKeys(r, a, u.Name, e.home, k.Content, e.uid, e.gid)
		ran = ran || wrote
		var no *Refusal
		switch {
		case errors.As(err, &no):
			return refused("mamelet does not write the SSH keys of " + u.Name + ": " + err.Error())
		case err != nil:
			return failed("write " + e.home + "/.ssh/authorized_keys: " + err.Error())
		}
		if wrote {
			did = append(did, fmt.Sprintf("wrote %s/.ssh/authorized_keys (sha256 %s, %s)", e.home, k.SHA256[:12], keysMode))
		} else {
			did = append(did, e.home+"/.ssh/authorized_keys was already as approved")
		}
	}
	// Look again: the user is as approved.
	switch {
	case slices.ContainsFunc(c.Groups, func(g string) bool { return !a.inGroup(u.Name, e.gid, g) }):
		return failed("after usermod, " + u.Name + " is not in every approved group")
	case c.Shell != "" && e.shell != c.Shell:
		return failed("after usermod, the shell of " + u.Name + " is " + e.shell)
	}
	if k := c.Keys; k != nil {
		if got, err := readKeys(r, e.home); err != nil || ContentSHA256(got) != k.SHA256 {
			return failed("after writing, authorized_keys is not as approved")
		}
	}
	if len(did) == 0 {
		did = append(did, "user "+u.Name+" was already as approved")
	}
	return noderev.OutcomeDone, strings.Join(did, "; "), ran
}

// userResults are the recorded outcomes of the part's changes of users.
func userResults(p Part, known func(op string, attempt int) (noderev.Result, bool)) []noderev.Result {
	var out []noderev.Result
	for _, u := range p.Users {
		if c := u.Change; c != nil {
			if r, ok := known(c.Grant.Operation, c.Grant.Attempt); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

// useraddArgs are the arguments of useradd that make the user as approved: each flag spelled out,
// the name after "--".
func useraddArgs(name string, k UserCreate) []string {
	var args []string
	if k.System {
		args = append(args, "--system")
	}
	if k.CreateHome {
		args = append(args, "--create-home")
	}
	if k.UID != 0 {
		args = append(args, "--uid", strconv.Itoa(k.UID))
	}
	args = append(args, "--shell", k.Shell)
	if k.Home != "" {
		args = append(args, "--home-dir", k.Home)
	}
	return append(args, "--user-group", "--", name)
}

// unlikeCreate says how a user that exists is not as the approved creation makes it ("" when it is).
func unlikeCreate(e passwdEntry, k UserCreate) string {
	switch {
	case k.UID != 0 && e.uid != k.UID:
		return fmt.Sprintf("its uid is %d", e.uid)
	case e.shell != k.Shell:
		return fmt.Sprintf("its shell is %s", e.shell)
	case k.Home != "" && e.home != k.Home:
		return fmt.Sprintf("its home is %s", e.home)
	}
	return ""
}

// homeThere says what is at a home that is there already (HomeState), for a message.
func homeThere(hs *HomeState) string {
	switch {
	case hs.Error != "":
		return "cannot be looked at (" + hs.Error + ")"
	case hs.What == "a directory":
		return fmt.Sprintf("exists already (a directory, %s %s:%s)", hs.Mode, hs.Owner, hs.Group)
	}
	return fmt.Sprintf("exists already (%s, %s:%s)", hs.What, hs.Owner, hs.Group)
}

// writeKeys writes the ~/.ssh/authorized_keys of user name with content, unless it is so already:
// the home must be the user's and ~/.ssh the user's or root's (never another user's: a Refusal);
// ~/.ssh is made the user's, 0700 (made if missing), and the file is the user's, 0600, written to a
// temporary file in ~/.ssh and renamed over it. The home is the user's, so the user may have put a
// link anywhere in it: the home and ~/.ssh are opened one component at a time, each checked not to
// be a link and to be the directory looked at, their owners are read from what was opened, and what
// is changed of ~/.ssh is changed through it. wrote says whether it changed anything.
func writeKeys(r *os.Root, a accounts, name, home, content string, uid, gid int) (wrote bool, err error) {
	rootUID, _ := a.rootIDs()
	h, err := openDir(r, home)
	if err != nil {
		return false, err
	}
	defer h.Close()
	hfi, err := h.stat()
	if err != nil {
		return false, err
	}
	if u, _, ok := ownerOf(hfi); ok && u != uid {
		return false, &Refusal{Reason: fmt.Sprintf("the home %s is %s's (uid %d), not %s's: mamelet never writes into another's home", home, a.userName(u), u, name)}
	}
	if _, err := h.lstat(".ssh"); errors.Is(err, fs.ErrNotExist) {
		if err = h.mkdir(".ssh", 0o700); err != nil {
			return false, err
		}
		wrote = true
	}
	dir, err := h.subdir(".ssh")
	if err != nil {
		var unsafe *unsafeError
		if errors.As(err, &unsafe) {
			err = fmt.Errorf("%w (mamelet never follows a link)", err)
		}
		return wrote, err
	}
	defer dir.Close()
	// ~/.ssh as sshd wants it, changed through the directory opened (not its name, which the user may
	// have replaced meanwhile).
	dfi, err := dir.stat()
	if err != nil {
		return wrote, err
	}
	u, g, ok := ownerOf(dfi)
	if ok && u != uid && u != rootUID {
		return wrote, &Refusal{Reason: fmt.Sprintf("%s/.ssh is %s's (uid %d), not %s's: mamelet never takes another's ~/.ssh", home, a.userName(u), u, name)}
	}
	if !ok || u != uid || g != gid {
		if err := dir.chown(uid, gid); err != nil {
			return wrote, err
		}
		wrote = true
	}
	if modeString(dfi.Mode()) != sshMode {
		if err := dir.chmod(0o700); err != nil {
			return wrote, err
		}
		wrote = true
	}
	if f, fi, _, err := dir.openLeaf("authorized_keys", home+"/.ssh/authorized_keys"); err == nil {
		same := false
		if u, g, ok := ownerOf(fi); ok && u == uid && g == gid && modeString(fi.Mode()) == keysMode && fi.Size() == int64(len(content)) {
			b, err := io.ReadAll(io.LimitReader(f, MaxContent+1))
			same = err == nil && string(b) == content
		}
		f.Close()
		if same {
			return wrote, nil
		}
	}
	if err := replaceIn(dir, home+"/.ssh", "authorized_keys", strings.NewReader(content), int64(len(content)), ContentSHA256(content), 0o600, uid, gid); err != nil {
		return wrote, err
	}
	return true, nil
}
