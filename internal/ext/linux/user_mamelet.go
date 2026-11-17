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
	if pathShape(u.Shell) == "" {
		st.Shells = map[string]bool{u.Shell: executable(r, u.Shell)}
	}
	e, ok := a.entries[u.Name]
	if !ok {
		if u.UID > 0 {
			who := ""
			if n, taken := a.userOf[u.UID]; taken {
				who = n
			}
			st.UIDs = map[string]string{strconv.Itoa(u.UID): who}
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

// executable is whether p is an executable regular file (a link to one is followed: /bin is one on
// a merged /usr).
func executable(r *os.Root, p string) bool {
	fi, err := r.Stat(rel(p))
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// observeKeys observes a user's ~/.ssh/authorized_keys: the file, ~/.ssh, and the keys in it. The
// home comes from /etc/passwd, and is checked again (CheckHome); the file is read through the
// directories one at a time, never through a link.
func observeKeys(r *os.Root, a accounts, name string, e passwdEntry) *KeysState {
	p := e.home + "/.ssh/authorized_keys"
	ks := &KeysState{File: FileState{Path: p}}
	if err := CheckHome(e.home); err != nil {
		ks.File.Error = "mamelet does not write SSH keys into the home of " + name + ": " + err.Message
		return ks
	}
	ks.File = inspect(r, p, a)
	if fi, err := r.Lstat(rel(e.home + "/.ssh")); err == nil && fi.IsDir() && fi.Mode()&fs.ModeSymlink == 0 {
		if uid, gid, ok := ownerOf(fi); ok {
			ks.Dir = modeString(fi.Mode()) + " " + a.userName(uid) + ":" + a.groupName(gid)
		}
	}
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
	ks.Found, ks.Other = scanAuthorizedKeys(content)
	return ks
}

// readKeys reads the authorized_keys of home, opening each directory down to ~/.ssh by itself (no
// link followed), so that nothing the user put in its home leads the read elsewhere.
func readKeys(r *os.Root, home string) (string, error) {
	dir, err := openDir(r, home+"/.ssh")
	if err != nil {
		return "", err
	}
	defer dir.Close()
	f, err := dir.OpenFile("authorized_keys", os.O_RDONLY|openNonBlock, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s/.ssh/authorized_keys is not a regular file", home)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxContent+1))
	return string(b), err
}

// openDir opens the directory dir (absolute) one component at a time from the root, each a
// directory and not a link when it is looked at; it makes nothing.
func openDir(r *os.Root, dir string) (*os.Root, error) {
	cur, err := r.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		fi, err := cur.Lstat(name)
		if err == nil && (fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir()) {
			err = fmt.Errorf("%s: %s is a symbolic link or not a directory", dir, name)
		}
		var next *os.Root
		if err == nil {
			next, err = cur.OpenRoot(name)
		}
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
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
	if err := checkUserChange(u, c); err != nil {
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
		ran = true
		if _, err := x.System.Run(ctx, progUseradd, useraddArgs(u.Name, *k)); err != nil {
			return failed(err.Error())
		}
		did = append(did, "made user "+u.Name)
		a = readAccounts(r)
		if e, exists = a.entries[u.Name]; !exists || e.uid == 0 {
			return failed("after useradd, user " + u.Name + " is not in /etc/passwd as made")
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
		}
		wrote, err := writeKeys(r, e.home, k.Content, e.uid, e.gid)
		ran = ran || wrote
		if err != nil {
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

// writeKeys writes a user's ~/.ssh/authorized_keys with content, unless it is so already: ~/.ssh is
// a directory of the user's, 0700 (made if missing), and the file the user's, 0600, written to a
// temporary file in ~/.ssh and renamed over it. The home is the user's, so the user may have put a
// link anywhere in it: the home and ~/.ssh are opened one component at a time, each checked not to
// be a link, and nothing opened under one can lead out of it. wrote says whether it changed
// anything.
func writeKeys(r *os.Root, home, content string, uid, gid int) (wrote bool, err error) {
	h, err := openDir(r, home)
	if err != nil {
		return false, err
	}
	defer h.Close()
	fi, err := h.Lstat(".ssh")
	if errors.Is(err, fs.ErrNotExist) {
		if err = h.Mkdir(".ssh", 0o700); err == nil {
			wrote = true
			fi, err = h.Lstat(".ssh")
		}
	}
	switch {
	case err != nil:
		return wrote, err
	case fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir():
		return wrote, fmt.Errorf("%s/.ssh is a symbolic link or not a directory (mamelet never follows a link)", home)
	}
	dir, err := h.OpenRoot(".ssh")
	if err != nil {
		return wrote, err
	}
	defer dir.Close()
	// ~/.ssh as sshd wants it, changed through the directory opened (not its name, which the user may
	// have replaced meanwhile).
	d, err := dir.Open(".")
	if err != nil {
		return wrote, err
	}
	defer d.Close()
	dfi, err := d.Stat()
	if err != nil {
		return wrote, err
	}
	if u, g, ok := ownerOf(dfi); !ok || u != uid || g != gid {
		if err := d.Chown(uid, gid); err != nil {
			return wrote, err
		}
		wrote = true
	}
	if modeString(dfi.Mode()) != sshMode {
		if err := d.Chmod(0o700); err != nil {
			return wrote, err
		}
		wrote = true
	}
	if fi, err := dir.Lstat("authorized_keys"); err == nil && fi.Mode().IsRegular() && modeString(fi.Mode()) == keysMode {
		if u, g, ok := ownerOf(fi); ok && u == uid && g == gid {
			if got, err := readKeys(r, home); err == nil && got == content {
				return wrote, nil
			}
		}
	}
	if err := replaceFile(dir, "authorized_keys", content, 0o600, uid, gid); err != nil {
		return wrote, err
	}
	return true, nil
}
