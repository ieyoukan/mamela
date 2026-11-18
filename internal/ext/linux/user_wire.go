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
// name, it says what mamelet reports on: whether Shell is an executable file on the node (the shell
// the user is declared with, or would be made with), which of Groups exist, which user has UID
// already (one declared for its creation), what is at the home it would be made with (Home), and
// whether to read its authorized_keys (Keys). None of them allows anything.
type User struct {
	Name   string   `json:"name"`
	Shell  string   `json:"shell"`
	Groups []string `json:"groups,omitempty"`
	UID    int      `json:"uid,omitzero"`
	// Home is the home useradd would make for the user, when it does not exist (mamelet reports
	// whether something is there already: UserState.NewHome).
	Home   string      `json:"home,omitempty"`
	Keys   bool        `json:"keys,omitzero"`
	Change *UserChange `json:"change,omitempty"`
}

// UserChange is an approved, one-time change of a user: mamelet makes it (Create), adds it to the
// supplementary Groups, sets its login Shell and writes its authorized_keys (Keys), in that order,
// each only if it is not so already, and nothing else.
type UserChange struct {
	Create *UserCreate `json:"create,omitempty"`
	Groups []string    `json:"groups,omitempty"`
	Shell  string      `json:"shell,omitempty"`
	Keys   *KeysWrite  `json:"keys,omitempty"`
	// Grant is the core's one-time permission (ActionCreateUser with Create, ActionChangeUser without).
	Grant noderev.Once `json:"grant"`
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
	// NewHome is what is at the part's home of a user that does not exist (nil: the part names none).
	NewHome *HomeState `json:"newHome,omitempty"`
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
	// NotOwned says why the home or ~/.ssh is not the user's to have its keys written into (another
	// user's): mamelet never writes there, and the plan blocks (MAM-PLAN-0212).
	NotOwned string `json:"notOwned,omitempty"`
	// Found are the keys in the file (fingerprints, not comments), each once, at most MaxFoundKeys:
	// More counts the other keys there are besides; Other counts its lines that are neither a key
	// Mamela reads, a comment nor empty.
	Found []FoundKey `json:"found,omitempty"`
	More  int        `json:"more,omitzero"`
	Other int        `json:"other,omitzero"`
}

// HomeState is what is at the home a user would be made with (UserState.NewHome).
type HomeState struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists,omitzero"`
	// Of what is there: what it is ("a directory", "a symbolic link", "a file"), its mode and owner.
	What  string `json:"what,omitempty"`
	Mode  string `json:"mode,omitempty"`
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
	// Error says why it was not looked at (a home mamelet refuses: CheckHome).
	Error string `json:"error,omitempty"`
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

// checkUsers is what mamelet checks of the users of a part before it uses it: names, values and
// grants, and that a change is one of what the part says of the user (its groups, its keys). It
// returns the problems.
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
		if why := pathShape(u.Shell); why != "" {
			add("user %q: shell: %s", u.Name, why)
		}
		for _, g := range u.Groups {
			if !ValidAccount(g) {
				add("user %q: %q is not a group's name", u.Name, g)
			}
		}
		if u.UID < 0 || u.UID > MaxUID {
			add("user %q: uid %d is out of range", u.Name, u.UID)
		}
		if u.Home != "" {
			if e := CheckHome(u.Home); e != nil {
				add("user %q: home: %s", u.Name, e.Message)
			}
		}
		if c := u.Change; c != nil {
			if err := checkUserChange(u, *c); err != nil {
				add("user %q: %v", u.Name, err)
			}
		}
	}
	return problems
}

// checkUserChange checks an approved change of user u: its grant, how it makes the user, that the
// groups it adds and the keys it writes are the user's, and the keys' content against its sha256.
func checkUserChange(u User, c UserChange) error {
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
		case k.UID != u.UID:
			return fmt.Errorf("the uid %d it is made with is not the one the part names (%d)", k.UID, u.UID)
		case k.Shell != u.Shell:
			return fmt.Errorf("the shell %q it is made with is not the one the part names", k.Shell)
		case k.CreateHome && k.Home != u.Home:
			return fmt.Errorf("the home %q it is made with is not the one the part names (%q)", k.Home, u.Home)
		}
		if k.Home != "" {
			if e := CheckHome(k.Home); e != nil {
				return fmt.Errorf("home: %s", e.Message)
			}
		}
	}
	for _, g := range c.Groups {
		if !slices.Contains(u.Groups, g) {
			return fmt.Errorf("it adds group %q, which the user is not declared with", g)
		}
	}
	if c.Shell != "" && c.Shell != u.Shell {
		return fmt.Errorf("it sets shell %q, which the user is not declared with", c.Shell)
	}
	if k := c.Keys; k != nil {
		if !u.Keys {
			return fmt.Errorf("it writes authorized_keys, which the user is not declared with")
		}
		if !names.SHA256(k.SHA256) || ContentSHA256(k.Content) != k.SHA256 {
			return fmt.Errorf("the authorized_keys content is not the one approved (sha256 %s)", k.SHA256)
		}
		if _, err := parseKeysContent(k.Content); err != nil {
			return fmt.Errorf("authorized_keys: %v", err)
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
