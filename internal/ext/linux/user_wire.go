package linux

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/names"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// The users' members of the linux part of a revision (Part.Users) and of a report
// (Observed.UserStates), NodeUser's.

// User is a user mamelet observes (NodeUser), and changes once if Change grants it. Besides the
// name, it says what mamelet reports on (the observation targets of the declarations: wire.go):
// whether each of Shells is an executable file on the node (the shells the user is declared with, or
// would be made with), which of Groups exist, which user has each of UIDs already (those declared for
// its creation), and whether to read its authorized_keys (Keys: a declaration has them). None of them
// allows anything: Change carries every value it needs.
type User struct {
	Name   string   `json:"name"`
	Shells []string `json:"shells,omitempty"`
	Groups []string `json:"groups,omitempty"`
	UIDs   []int    `json:"uids,omitempty"`
	Keys   bool     `json:"keys,omitzero"`
	// InForce: the declaration in force has the user (File.InForce).
	InForce bool        `json:"inForce,omitzero"`
	Change  *UserChange `json:"change,omitempty"`
}

// UserChange is an approved, one-time change of a user: mamelet makes it (Create), adds it to the
// supplementary Groups, sets its login Shell and writes its authorized_keys (Keys), in that order,
// each only if it is not so already, and nothing else.
type UserChange struct {
	Create *UserCreate `json:"create,omitempty"`
	Groups []string    `json:"groups,omitempty"`
	Shell  string      `json:"shell,omitempty"`
	Keys   *KeysWrite  `json:"keys,omitempty"`
	// Before is the user as the approved plan saw it: mamelet changes the shell and authorized_keys
	// only while they are still so, or once they are already as approved (compare and swap). A new user
	// is made only while there is none of its name (or one exactly as approved, made by an attempt that
	// was interrupted).
	Before UserBefore `json:"before"`
	// Grant is the core's one-time permission (ActionCreateUser with Create, ActionChangeUser without).
	Grant noderev.Once `json:"grant"`
}

// UserBefore is what the observation an approved change of a user was planned against found: whether
// the user existed, its login shell, and its authorized_keys (when the change writes them).
type UserBefore struct {
	Exists bool    `json:"exists,omitzero"`
	Shell  string  `json:"shell,omitempty"`
	Keys   *Before `json:"keys,omitzero"`
}

// UserCreate is how useradd makes the user: a system user or not, with the uid (0: useradd
// chooses), the login shell, the home directory ("": useradd's default, a system user's), whether
// the home is made, and always a group of the user's name as its primary group.
type UserCreate struct {
	System     bool   `json:"system,omitzero"`
	UID        int    `json:"uid,omitzero"`
	Shell      string `json:"shell"`
	Home       string `json:"home,omitempty"`
	CreateHome bool   `json:"createHome,omitzero"`
}

// KeysWrite is an approved write of a user's ~/.ssh/authorized_keys with exactly Content (whose
// sha256 is SHA256).
type KeysWrite struct {
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

// The actions of a user's grant.
const (
	ActionCreateUser = "create-user"
	ActionChangeUser = "change-user"
)

// UserState is what mamelet found of a user of the part.
type UserState struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists,omitzero"`
	// Of a user that exists (/etc/passwd and /etc/group): its ids, the name of its primary group,
	// its home, its login shell and its supplementary groups (sorted).
	UID    int      `json:"uid,omitzero"`
	GID    int      `json:"gid,omitzero"`
	Group  string   `json:"group,omitempty"`
	Home   string   `json:"home,omitempty"`
	Shell  string   `json:"shell,omitempty"`
	Groups []string `json:"groups,omitempty"`
	// Shells says of the part's shell whether it is an executable file on the node; UIDs, of a user
	// that does not exist, which user has the part's uid already ("" none). A key that is missing was
	// not asked for when mamelet looked (the declaration changed since). Whether the part's groups,
	// and a group of the user's name, exist is in Observed.Groups.
	Shells map[string]bool   `json:"shells,omitempty"`
	UIDs   map[string]string `json:"uids,omitempty"`
	// Keys is its authorized_keys, when the part asks for it and the user exists.
	Keys *KeysState `json:"keys,omitempty"`
	// Error says why mamelet does not observe the user (a name it refuses).
	Error string `json:"error,omitempty"`
}

// KeysState is what is at a user's ~/.ssh/authorized_keys.
type KeysState struct {
	// File is the file (its path, sha256, mode, owner, the directories missing above it; Unsafe when
	// it or a directory above it is a link; Error when mamelet refuses the home).
	File FileState `json:"file"`
	// Dir is ~/.ssh when it is a directory: "0700 app:app".
	Dir string `json:"dir,omitempty"`
	// Found are the keys in the file (fingerprints, not comments); Other counts its lines that are
	// neither a key Mamela reads, a comment nor empty.
	Found []FoundKey `json:"found,omitempty"`
	Other int        `json:"other,omitzero"`
}

func (p *Part) user(name string) *User {
	for i := range p.Users {
		if p.Users[i].Name == name {
			return &p.Users[i]
		}
	}
	return nil
}

func (o Observed) user(name string) *UserState {
	for i := range o.UserStates {
		if o.UserStates[i].Name == name {
			return &o.UserStates[i]
		}
	}
	return nil
}

// sortUsers puts the users of a part in the order of their names.
func sortUsers(p *Part) {
	slices.SortFunc(p.Users, func(a, b User) int { return strings.Compare(a.Name, b.Name) })
}

// userGrants are the one-time grants of the users of a part.
func userGrants(p Part) []noderev.Grant {
	var out []noderev.Grant
	for _, u := range p.Users {
		if c := u.Change; c != nil {
			out = append(out, noderev.Grant{Operation: c.Grant.Operation, Attempt: c.Grant.Attempt, Action: c.Grant.Action, Expires: c.Grant.Expires})
		}
	}
	return out
}

// handOutUsers takes out of the part the changes whose grant has ended (with their content), and
// clears the deadline of the others.
func handOutUsers(p *Part, now time.Time) {
	for i := range p.Users {
		if c := p.Users[i].Change; c != nil {
			if g := noderev.HandOut(&c.Grant, now); g != nil {
				n := *c
				n.Grant = *g
				p.Users[i].Change = &n
			} else {
				p.Users[i].Change = nil
			}
		}
	}
}

// checkUsers is what mamelet checks of the users of a part before it uses it: names, observation
// targets and changes. It returns the problems.
func checkUsers(users []User) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	seen := map[string]bool{}
	for _, u := range users {
		if seen[u.Name] {
			add("user %q appears twice", u.Name)
		}
		seen[u.Name] = true
		if why := checkUserName(u.Name); why != "" {
			add("user %q: %s", u.Name, why)
			continue
		}
		if why := targetsProblem(u.Shells, func(s string) bool { return pathShape(s) == "" }); why != "" {
			add("user %q: shells: %s", u.Name, why)
		}
		if why := targetsProblem(u.Groups, ValidAccount); why != "" {
			add("user %q: groups: %s", u.Name, why)
		}
		if why := targetsProblem(u.UIDs, func(id int) bool { return id > 0 && id <= MaxUID }); why != "" {
			add("user %q: uids: %s", u.Name, why)
		}
		if c := u.Change; c != nil {
			if err := checkUserChange(*c); err != nil {
				add("user %q: %v", u.Name, err)
			}
		}
	}
	return problems
}

// checkUserChange checks an approved change of a user by itself (it carries every value it needs; what
// the entry asks mamelet to observe is no part of it): its grant, how it makes the user, the groups it
// adds, the shell it sets, the keys' content against its sha256, and what it was planned against.
func checkUserChange(c UserChange) error {
	action := ActionChangeUser
	if c.Create != nil {
		action = ActionCreateUser
	}
	if !noderev.ValidOnce(c.Grant, action) {
		return fmt.Errorf("the grant %+v is not valid", c.Grant)
	}
	if k := c.Create; k != nil {
		switch {
		case k.UID < 0 || k.UID > MaxUID:
			return fmt.Errorf("uid %d is out of range", k.UID)
		case pathShape(k.Shell) != "":
			return fmt.Errorf("shell %q: %s", k.Shell, pathShape(k.Shell))
		case c.Shell != "":
			return fmt.Errorf("a user that is made gets its shell from how it is made")
		case c.Before.Exists:
			return fmt.Errorf("it makes a user the plan saw already")
		}
		if k.Home != "" {
			if e := CheckHome(k.Home); e != nil {
				return fmt.Errorf("home: %s", e.Message)
			}
		}
	} else if !c.Before.Exists {
		return fmt.Errorf("it changes a user the plan did not see")
	}
	for i, g := range c.Groups {
		if !ValidAccount(g) || i > 0 && c.Groups[i-1] >= g {
			return fmt.Errorf("groups %v: each a group's name, sorted, once", c.Groups)
		}
	}
	if c.Shell != "" {
		if why := pathShape(c.Shell); why != "" {
			return fmt.Errorf("shell %q: %s", c.Shell, why)
		}
	}
	if c.Before.Shell != "" {
		if why := pathShape(c.Before.Shell); why != "" {
			return fmt.Errorf("the shell the plan saw, %q: %s", c.Before.Shell, why)
		}
	}
	if k := c.Keys; k != nil {
		if !names.SHA256(k.SHA256) || ContentSHA256(k.Content) != k.SHA256 {
			return fmt.Errorf("the authorized_keys content is not the one approved (sha256 %s)", k.SHA256)
		}
		if _, err := parseKeysContent(k.Content); err != nil {
			return fmt.Errorf("authorized_keys: %v", err)
		}
		if c.Create == nil && c.Before.Keys == nil {
			return fmt.Errorf("it writes the authorized_keys of a user without what the plan saw of them")
		}
	}
	if b := c.Before.Keys; b != nil {
		if why := b.check(); why != "" {
			return fmt.Errorf("the authorized_keys the plan saw: %s", why)
		}
	}
	if c.Create == nil && len(c.Groups) == 0 && c.Shell == "" && c.Keys == nil {
		return fmt.Errorf("the change changes nothing")
	}
	return nil
}

// parseKeysContent reads the content of authorized_keys as Mamela writes it (KeysContent): one key a
// line, each as a declaration writes it, none twice.
func parseKeysContent(content string) ([]AuthorizedKey, error) {
	if content == "" || !strings.HasSuffix(content, "\n") {
		return nil, fmt.Errorf("the content is empty or does not end with a newline")
	}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if len(lines) > MaxKeys {
		return nil, fmt.Errorf("%d keys; at most %d", len(lines), MaxKeys)
	}
	var out []AuthorizedKey
	seen := map[string]bool{}
	for i, l := range lines {
		k, err := ParseAuthorizedKey(l)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", i+1, err)
		}
		if seen[k.Fingerprint] {
			return nil, fmt.Errorf("line %d: key %s is there twice", i+1, k.Fingerprint)
		}
		seen[k.Fingerprint] = true
		out = append(out, k)
	}
	return out, nil
}
