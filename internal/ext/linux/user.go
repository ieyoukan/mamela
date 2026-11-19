package linux

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/internal/graph"
)

// KindUser is a user on a node: its account, its supplementary groups and its SSH keys.
const KindUser graph.Kind = "NodeUser"

// UserSpec says a node has the user Name. Mamela makes it if it does not exist (useradd), as a
// system user or not, with UID, Shell and Home when they are declared (useradd's choice otherwise),
// and a group of its name; it adds the user to Groups (never removes it from a group); it sets
// Shell when it is declared; and when AuthorizedKeys is declared (nil: Mamela leaves the file
// alone) ~/.ssh/authorized_keys has exactly those lines. It never changes the uid or the home of a
// user that exists, never manages root, and never deletes a user. The resource's name is
// "<node>/user/<name>".
type UserSpec struct {
	Node   string
	Name   string
	System bool
	UID    int    // 0: not declared (useradd chooses). Used only when the user is made
	Shell  string // "": not declared (DefaultShell, or DefaultSystemShell, when the user is made)
	Home   string // "": not declared (HomeBase/<name> when the home is made; useradd's default, not made, for a system user without keys). Used only when the user is made
	Groups []string
	// AuthorizedKeys are the lines of ~/.ssh/authorized_keys (nil: not declared).
	AuthorizedKeys []string
}

// Defaults of a user's optional members, when Mamela makes it.
const (
	DefaultShell       = "/bin/bash"
	DefaultSystemShell = "/usr/sbin/nologin"
	HomeBase           = "/home"
)

// MaxUID bounds a declared uid (the largest a signed 32-bit id holds).
const MaxUID = 1<<31 - 1

// UserOf is the user a resource declares (nil for a resource of another kind).
func UserOf(r graph.Resource) *UserSpec { s, _ := r.Ext.(*UserSpec); return s }

// UserName is the name of the resource of the user name on node.
func UserName(node, name string) string { return node + "/user/" + name }

// CreateShell is the shell the user is made with.
func (s *UserSpec) CreateShell() string {
	switch {
	case s.Shell != "":
		return s.Shell
	case s.System:
		return DefaultSystemShell
	}
	return DefaultShell
}

// CreateHome is the home the user is made with ("": useradd's default, for a system user whose home
// is not made) and whether useradd makes it: a user's always; a system user's only when its SSH keys
// are declared (they are written there). A home that is made is always named (HomeBase/<name> when
// none is declared), never useradd's default, which /etc/default/useradd on the node decides: the
// plan shows it, and mamelet checks what is there before useradd makes it.
func (s *UserSpec) CreateHome() (home string, made bool) {
	made = !s.System || s.AuthorizedKeys != nil
	home = s.Home
	if home == "" && made {
		home = HomeBase + "/" + s.Name
	}
	return home, made
}

// KeysContent is the content of the user's authorized_keys ("" when it is not declared).
func (s *UserSpec) KeysContent() string { return KeysContent(s.AuthorizedKeys) }

type wireUser struct {
	Node           *string        `json:"node"`
	Name           *string        `json:"name"`
	System         jsontext.Value `json:"system"`         // optional
	UID            jsontext.Value `json:"uid"`            // optional
	Shell          jsontext.Value `json:"shell"`          // optional
	Home           jsontext.Value `json:"home"`           // optional
	Groups         jsontext.Value `json:"groups"`         // optional
	AuthorizedKeys jsontext.Value `json:"authorizedKeys"` // optional
}

type canonUser struct {
	AuthorizedKeys []string `json:"authorizedKeys,omitempty"`
	Groups         []string `json:"groups,omitempty"`
	Home           string   `json:"home,omitempty"`
	Name           string   `json:"name"`
	Node           string   `json:"node"`
	Shell          string   `json:"shell,omitempty"`
	System         bool     `json:"system"`
	UID            int      `json:"uid,omitzero"`
}

// userKind is NodeUser in every layer.
var userKind = kind{
	def: graph.KindDef{
		Kind: KindUser, Order: 6,
		New:      func() any { return &UserSpec{} },
		Decode:   decodeUser,
		Validate: validateUser,
		Canonical: func(spec any) any {
			s := spec.(*UserSpec)
			return canonUser{AuthorizedKeys: s.AuthorizedKeys, Groups: s.Groups, Home: s.Home, Name: s.Name, Node: s.Node, Shell: s.Shell, System: s.System, UID: s.UID}
		},
		Node:   func(spec any) string { return spec.(*UserSpec).Node },
		Fields: func(spec any) map[string]string { return map[string]string{"user": spec.(*UserSpec).Name} },
		Describe: func(spec any) string {
			s := spec.(*UserSpec)
			var what []string
			if s.System {
				what = append(what, "system")
			}
			if s.UID != 0 {
				what = append(what, fmt.Sprintf("uid %d", s.UID))
			}
			if len(s.Groups) > 0 {
				what = append(what, "groups "+strings.Join(s.Groups, ","))
			}
			if s.AuthorizedKeys != nil {
				what = append(what, fmt.Sprintf("%d SSH keys", len(s.AuthorizedKeys)))
			}
			d := "user " + s.Name
			if len(what) > 0 {
				d += " (" + strings.Join(what, ", ") + ")"
			}
			return d + " on " + s.Node
		},
	},
	warnings:     userWarnings,
	warningCodes: []graph.Code{CodePrivilegedGroup},
	codes:        userPlanCodes,
	programs:     []string{progUseradd, progUsermod},
	planner:      userPlanner,
	undeclared:   undeclaredUsers,
	ops:          []operation{createUserOp, changeUserOp},
	manifest:     userManifest,
	publish:      publishUsers,
	prune:        pruneUsers,
	protect:      protectUsers,
	observe:      observeUsers,
	act:          actUsers,
	results:      userResults,
}

// uidRe is an integer as a declaration writes a uid: digits only (no sign, fraction or exponent).
var uidRe = regexp.MustCompile(`^[0-9]{1,10}$`)

func decodeUser(d *graph.SpecDecoder) any {
	var s wireUser
	if !d.Strict(&s) {
		return nil
	}
	d.Need(s.Node != nil, "node")
	d.Need(s.Name != nil, "name")
	u := &UserSpec{}
	if s.System != nil {
		if string(s.System) == "null" || json.Unmarshal(s.System, &u.System) != nil {
			d.Invalid(graph.CodeWrongType, "system", "system is not true or false", "leave the member out (a user that is not a system user), or write true or false")
		}
	}
	if s.UID != nil {
		n, err := strconv.ParseInt(string(s.UID), 10, 64)
		switch {
		case !uidRe.MatchString(string(s.UID)) || err != nil:
			d.Invalid(graph.CodeWrongType, "uid", fmt.Sprintf("uid %s is not a positive integer", short(string(s.UID))), "leave the member out (useradd chooses), or write an integer such as 1001")
		case n == 0:
			d.Invalid(graph.CodeInvalidValue, "uid", "uid 0 is root's: Mamela never manages root", "leave the member out (useradd chooses), or write another uid")
		case n > MaxUID:
			d.Invalid(graph.CodeInvalidValue, "uid", fmt.Sprintf("uid %d is larger than %d", n, MaxUID), "write a smaller uid")
		default:
			u.UID = int(n)
		}
	}
	shell := d.OptionalString(s.Shell, "shell")
	home := d.OptionalString(s.Home, "home")
	// An empty string would mean what leaving the member out means, silently: it is refused.
	if shell != nil && *shell == "" {
		d.Invalid(graph.CodeInvalidValue, "shell", "the shell is empty", `leave the member out (useradd's default: /bin/bash, or /usr/sbin/nologin for a system user), or write the absolute path of a shell`)
	}
	if home != nil && *home == "" {
		d.Invalid(graph.CodeInvalidValue, "home", "the home is empty", `leave the member out (/home/<name>, or useradd's default for a system user), or write an absolute path such as "/srv/app"`)
	}
	list := func(v jsontext.Value, field, hint string) []string {
		if v == nil {
			return nil
		}
		var out []string
		if string(v) == "null" || json.Unmarshal(v, &out) != nil {
			d.Invalid(graph.CodeWrongType, field, field+" is not a list of strings", hint)
			return nil
		}
		if out == nil {
			out = []string{}
		}
		return out
	}
	u.Groups = list(s.Groups, "groups", `leave the member out (no supplementary group), or write a list such as ["docker"]`)
	u.AuthorizedKeys = list(s.AuthorizedKeys, "authorizedKeys", `leave the member out (Mamela leaves the file alone), or write a list of public keys such as ["ssh-ed25519 AAAA... alice@laptop"]`)
	if !d.OK() {
		return nil
	}
	u.Node, u.Name = *s.Node, *s.Name
	if shell != nil {
		u.Shell = *shell
	}
	if home != nil {
		u.Home = *home
	}
	if len(u.Groups) == 0 {
		u.Groups = nil
	}
	return u
}

func validateUser(v *graph.Validator, r graph.Resource) {
	s := UserOf(r)
	v.NodeRef(r, s.Node)
	if why := checkUserName(s.Name); why != "" {
		v.Problem(r, graph.CodeInvalidValue, "spec.name", fmt.Sprintf("%q: %s", s.Name, why),
			`a user's name: lowercase letters, digits, "_" and "-", starting with a letter or "_", such as "app" or "deploy"`)
	} else if want := UserName(s.Node, s.Name); r.Name != want {
		v.Problem(r, graph.CodeNameMismatch, "name", fmt.Sprintf("the name should be %q (node/user/name)", want), "")
	}
	if s.Shell != "" {
		if why := pathShape(s.Shell); why != "" {
			v.Problem(r, graph.CodeInvalidValue, "spec.shell", "the shell "+why, `write the absolute path of a shell, such as "/bin/bash" or "/usr/sbin/nologin"`)
		}
	}
	if s.Home != "" {
		if e := CheckHome(s.Home); e != nil {
			v.Problem(r, graph.CodeInvalidValue, "spec.home", e.Message, e.Hint)
		}
	}
	seen := map[string]bool{}
	for _, g := range s.Groups {
		switch {
		case !ValidAccount(g):
			v.Problem(r, graph.CodeInvalidValue, "spec.groups", fmt.Sprintf("%q is not a group's name", g), `a group's name, such as "docker" or "www-data"`)
		case seen[g]:
			v.Problem(r, graph.CodeInvalidValue, "spec.groups", fmt.Sprintf("group %s is named twice", g), "name each group once")
		case g == s.Name:
			// useradd makes the group of the user's name its primary group, and refuses to make the user
			// while a group of that name exists: the plan could only wait for it, or block.
			v.Problem(r, graph.CodeInvalidValue, "spec.groups", fmt.Sprintf("group %s is the user's own group: useradd makes it the user's primary group", g),
				"leave it out of groups (the user is in it already)")
		}
		seen[g] = true
	}
	if s.AuthorizedKeys != nil {
		validateKeys(v, r, s.AuthorizedKeys)
	}
	// What the users of one node may not share: a uid, a home; and their keys, which ride in the
	// node's revision with their approved changes, are bounded together (keysBefore: the bytes of the
	// keys of the node's users before each one, in canonical order).
	type taken struct {
		uids, homes map[string][]string
		keysBefore  map[string]int
	}
	byNode := v.Memo("linux.users", func() any {
		m := map[string]taken{}
		total := map[string]int{}
		for _, o := range v.Graph().Sorted() {
			u := UserOf(o)
			if u == nil {
				continue
			}
			t, ok := m[u.Node]
			if !ok {
				t = taken{uids: map[string][]string{}, homes: map[string][]string{}, keysBefore: map[string]int{}}
				m[u.Node] = t
			}
			if u.UID != 0 {
				t.uids[strconv.Itoa(u.UID)] = append(t.uids[strconv.Itoa(u.UID)], o.ID())
			}
			if h, _ := u.CreateHome(); h != "" {
				t.homes[h] = append(t.homes[h], o.ID())
			}
			t.keysBefore[o.ID()] = total[u.Node]
			total[u.Node] += len(u.KeysContent())
		}
		return m
	}).(map[string]taken)
	if before, mine := byNode[s.Node].keysBefore[r.ID()], len(s.KeysContent()); before <= MaxNodeContent && before+mine > MaxNodeContent {
		v.Problem(r, graph.CodeInvalidValue, "spec.authorizedKeys", fmt.Sprintf("the SSH keys of the users declared on %s hold more than %d bytes (%d with these): their approved changes ride in the node's revision",
			s.Node, MaxNodeContent, before+mine), "declare fewer keys on one node")
	}
	t := byNode[s.Node]
	if s.UID != 0 {
		if others := slices.DeleteFunc(slices.Clone(t.uids[strconv.Itoa(s.UID)]), func(id string) bool { return id == r.ID() }); len(others) > 0 {
			v.Problem(r, graph.CodeConflict, "spec.uid", fmt.Sprintf("uid %d is also declared by %s on %s", s.UID, strings.Join(others, ", "), s.Node), "give each user a uid of its own")
		}
	}
	if h, _ := s.CreateHome(); h != "" {
		if others := slices.DeleteFunc(slices.Clone(t.homes[h]), func(id string) bool { return id == r.ID() }); len(others) > 0 {
			v.Problem(r, graph.CodeConflict, "spec.home", fmt.Sprintf("home %s is also the home of %s on %s", h, strings.Join(others, ", "), s.Node), "give each user a home of its own")
		}
		// A home in another's (or around it): the user owning the outer one can replace the inner.
		for _, o := range slices.Sorted(maps.Keys(t.homes)) {
			if o != h && (Under(h, o) || Under(o, h)) {
				inner, outer := h, o
				if Under(o, h) {
					inner, outer = o, h
				}
				v.Problem(r, graph.CodeConflict, "spec.home", fmt.Sprintf("home %s is in home %s on %s (%s): the user of the outer one can replace what is in the inner", inner, outer, s.Node, strings.Join(t.homes[o], ", ")),
					"give each user a home of its own, side by side (such as /home/app and /home/worker)")
			}
		}
	}
}

// validateKeys checks the declared lines of authorized_keys: at least one (to leave the file alone,
// the member is left out), each a public key Mamela takes, none twice, and all of them within
// MaxContent.
func validateKeys(v *graph.Validator, r graph.Resource, lines []string) {
	const keyHint = `write each key as "ssh-keygen" writes a public key (the content of id_ed25519.pub): "<type> <base64> <comment>", without options`
	switch {
	case len(lines) == 0:
		v.Problem(r, graph.CodeInvalidValue, "spec.authorizedKeys", "the list of SSH keys is empty: nobody could log in with a key",
			"leave the member out to have Mamela leave the user's authorized_keys alone; Mamela does not empty it")
		return
	case len(lines) > MaxKeys:
		v.Problem(r, graph.CodeInvalidValue, "spec.authorizedKeys", fmt.Sprintf("%d SSH keys; at most %d", len(lines), MaxKeys), "")
		return
	case len(KeysContent(lines)) > MaxContent:
		v.Problem(r, graph.CodeInvalidValue, "spec.authorizedKeys", fmt.Sprintf("the SSH keys hold %d bytes; at most %d", len(KeysContent(lines)), MaxContent), "")
		return
	}
	seen := map[string]int{}
	for i, l := range lines {
		k, err := ParseAuthorizedKey(l)
		if err != nil {
			v.Problem(r, graph.CodeInvalidValue, fmt.Sprintf("spec.authorizedKeys[%d]", i), err.Error(), keyHint)
			continue
		}
		if j, dup := seen[k.Fingerprint]; dup {
			v.Problem(r, graph.CodeInvalidValue, fmt.Sprintf("spec.authorizedKeys[%d]", i), fmt.Sprintf("the key %s is also line %d", k.Fingerprint, j), "write each key once")
		}
		seen[k.Fingerprint] = i
	}
}

// checkUserName says why Mamela does not manage the user called name ("" when it may).
func checkUserName(name string) string {
	switch {
	case !ValidAccount(name):
		return "not a user's name"
	case name == "root":
		return "Mamela never manages root"
	}
	return ""
}

// pathShape says why p is not a clean absolute path of letters, digits and . _ + @ = : , - ("" when
// it is one), of at most MaxPath bytes and not "/".
func pathShape(p string) string {
	switch {
	case p == "" || !strings.HasPrefix(p, "/"):
		return fmt.Sprintf("%q is not an absolute path", short(p))
	case len(p) > MaxPath:
		return fmt.Sprintf("is %d bytes; at most %d", len(p), MaxPath)
	case p == "/":
		return `is "/"`
	case path.Clean(p) != p:
		return fmt.Sprintf("%q is not a clean path (it would be %q)", p, path.Clean(p))
	}
	for _, c := range strings.Split(p[1:], "/") {
		if c == "." || c == ".." || !componentRe.MatchString(c) {
			return fmt.Sprintf("%q has a component other than letters, digits and . _ + @ = : , -", p)
		}
	}
	return ""
}

// systemDirs are where no user's home is: the system's own directories, root's home, and the
// directories everyone may write to (a link put there before the home is made would lead elsewhere).
var systemDirs = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32", "/proc", "/root",
	"/run", "/sbin", "/sys", "/tmp", "/usr", "/var/lib/dpkg", "/var/lib/mamelet", "/var/lib/rancher", "/var/run", "/var/tmp"}

// CheckHome says why Mamela does not make, or write SSH keys into, the home directory h of a user
// (nil when it may): h is a clean absolute path of at least two components, and not in the system's
// directories (/etc, /usr, /root, /tmp and the like). The graph checks a declared home with it, and
// mamelet checks the home of the user it finds on the node before it writes there.
func CheckHome(h string) *PathError {
	const hint = `a home of the user's own, such as "/home/app" or "/srv/app"`
	if why := pathShape(h); why != "" {
		return &PathError{Path: h, Message: "the home " + why, Hint: hint}
	}
	if strings.Count(h, "/") < 2 {
		return &PathError{Path: h, Message: fmt.Sprintf("the home %s is a directory right under /", h), Hint: hint}
	}
	for _, d := range systemDirs {
		if Under(h, d) {
			return &PathError{Path: h, Message: fmt.Sprintf("the home %s is in %s, the system's", h, d), Hint: hint}
		}
	}
	return nil
}

// The graph warning of NodeUser.
const (
	// CodePrivilegedGroup: a user is added to a group that lets it become root.
	CodePrivilegedGroup graph.Code = "MAM-GRAPH-0206"
)

// privilegedGroups are the groups whose members can become root (or read what only root reads)
// without a password, on Debian and Ubuntu.
var privilegedGroups = map[string]string{
	"root":        "the files of group root",
	"sudo":        "sudo",
	"admin":       "sudo (older Ubuntu)",
	"wheel":       "sudo or su",
	"docker":      "the Docker daemon, which runs containers as root with the host's files",
	"lxd":         "LXD, which runs containers as root with the host's files",
	"incus-admin": "Incus, which runs containers as root with the host's files",
	"libvirt":     "libvirt, which runs machines with the host's disks",
	"disk":        "the raw disks",
	"shadow":      "the password hashes (/etc/shadow)",
}

// userWarnings: a user added to a group that lets it become root.
func userWarnings(g *graph.Graph) []graph.Warning {
	var out []graph.Warning
	for _, r := range g.Sorted() {
		u := UserOf(r)
		if u == nil {
			continue
		}
		for _, grp := range u.Groups {
			if what, ok := privilegedGroups[grp]; ok {
				out = append(out, graph.Warning{Code: CodePrivilegedGroup, Resource: r.ID(),
					Message: fmt.Sprintf("user %s is added to group %s on %s: its members control %s, so the user can become root there", u.Name, grp, u.Node, what),
					Hint:    "declare the group only if the user is meant to administer the node; Mamela never removes a membership, so taking it out of the declaration later does not take it away"})
			}
		}
	}
	return out
}
