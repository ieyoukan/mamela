package linux

import (
	"fmt"
	"regexp"
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
	return a
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
