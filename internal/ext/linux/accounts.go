package linux

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A user's or a group's name, as Debian's useradd takes one by default (and the system's own, such
// as "_apt" and "systemd-network"): no option, no path, no space.
var accountRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidAccount reports whether s can be the name of a user or a group.
func ValidAccount(s string) bool { return accountRe.MatchString(s) }

// accounts are the users and groups of a node, from its /etc/passwd and /etc/group (mamelet reads
// the files: no cgo, no name service).
type accounts struct {
	users, groups map[string]int // by name
	userOf        map[int]string // the first name of each id
	groupOf       map[int]string
	// entries are the users' lines of /etc/passwd (the first of each name), and members the members
	// each group lists (NodeUser's).
	entries map[string]passwdEntry
	members map[string][]string
}

// passwdEntry is a user's line of /etc/passwd.
type passwdEntry struct {
	uid, gid    int
	home, shell string
}

// parseAccounts reads the contents of /etc/passwd and /etc/group. A line it cannot read is left out.
func parseAccounts(passwd, group string) accounts {
	a := accounts{users: map[string]int{}, groups: map[string]int{}, userOf: map[int]string{}, groupOf: map[int]string{}}
	read := func(text string, byName map[string]int, byID map[int]string) {
		for _, line := range strings.Split(text, "\n") {
			f := strings.Split(line, ":")
			if len(f) < 3 || f[0] == "" || strings.HasPrefix(f[0], "#") || strings.HasPrefix(f[0], "+") || strings.HasPrefix(f[0], "-") {
				continue
			}
			id, err := strconv.Atoi(f[2])
			if err != nil || id < 0 {
				continue
			}
			if _, seen := byName[f[0]]; !seen {
				byName[f[0]] = id
			}
			if _, seen := byID[id]; !seen {
				byID[id] = f[0]
			}
		}
	}
	read(passwd, a.users, a.userOf)
	read(group, a.groups, a.groupOf)
	a.entries, a.members = map[string]passwdEntry{}, map[string][]string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(passwd, "\n") {
		f := strings.Split(line, ":")
		if seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		if len(f) != 7 || !ValidAccount(f[0]) {
			continue
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 == nil && err2 == nil && uid >= 0 && gid >= 0 {
			a.entries[f[0]] = passwdEntry{uid: uid, gid: gid, home: f[5], shell: f[6]}
		}
	}
	for _, line := range strings.Split(group, "\n") {
		f := strings.Split(line, ":")
		if _, listed := a.members[f[0]]; listed || len(f) != 4 || !ValidAccount(f[0]) {
			continue
		}
		a.members[f[0]] = []string{}
		for _, m := range strings.Split(f[3], ",") {
			if m != "" {
				a.members[f[0]] = append(a.members[f[0]], m)
			}
		}
	}
	return a
}

// memberOf are the groups that list user as a member (its supplementary groups), sorted.
func (a accounts) memberOf(user string) []string {
	var out []string
	for g, ms := range a.members {
		if slices.Contains(ms, user) {
			out = append(out, g)
		}
	}
	slices.Sort(out)
	return out
}

// inGroup is whether user (whose primary group is gid) is in group g: its primary group, or a
// group that lists it.
func (a accounts) inGroup(user string, gid int, g string) bool {
	if id, ok := a.groups[g]; ok && id == gid {
		return true
	}
	return slices.Contains(a.members[g], user)
}

// userName is the name of a user id, or "#<id>" when the node has none for it.
func (a accounts) userName(id int) string {
	if n, ok := a.userOf[id]; ok {
		return n
	}
	return fmt.Sprintf("#%d", id)
}

// groupName is the name of a group id, or "#<id>" when the node has none for it.
func (a accounts) groupName(id int) string {
	if n, ok := a.groupOf[id]; ok {
		return n
	}
	return fmt.Sprintf("#%d", id)
}

// ParseMode reads a file's mode as a declaration writes it: four octal digits ("0644"). It refuses
// the setuid, setgid and sticky bits (a file that runs as its owner is not a configuration) and a
// file anyone may write (any user of the node could change what root wrote).
func ParseMode(s string) (uint32, error) {
	if len(s) != 4 || strings.Trim(s, "01234567") != "" {
		return 0, fmt.Errorf("mode %q is not four octal digits", s)
	}
	m, _ := strconv.ParseUint(s, 8, 32)
	switch {
	case m&0o7000 != 0:
		return 0, fmt.Errorf("mode %s has the setuid, setgid or sticky bit; Mamela does not write such a file", s)
	case m&0o002 != 0:
		return 0, fmt.Errorf("mode %s lets every user of the node write the file", s)
	}
	return uint32(m), nil
}

// defaultUIDMin is the first uid of a user who is not a system user, when /etc/login.defs does not
// say (Debian's and Ubuntu's).
const defaultUIDMin = 1000

// uidMin is the first uid useradd gives a user who is not a system user on the node (UID_MIN of
// /etc/login.defs, read through the directories one at a time; defaultUIDMin when it does not say):
// a smaller uid is a system user's.
func uidMin(r *os.Root) int {
	b, err := readRegular(r, "/etc/login.defs", MaxContent)
	if err != nil {
		return defaultUIDMin
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "UID_MIN" {
			if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return defaultUIDMin
}
