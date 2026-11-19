package linux

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// fakeAccounts is useradd and usermod on a node whose file system is a directory of the test: they
// change its /etc/passwd and /etc/group as the real ones would, and record what mamelet ran. A user
// it makes gets the test's own uid and gid (unless the arguments name a uid), so that mamelet can
// give it files without being root.
type fakeAccounts struct {
	t        *testing.T
	root     string
	commands []string
	fail     string // a program (base name) that exits 1
}

func (f *fakeAccounts) Run(_ context.Context, program string, args []string, _ []string) ([]byte, []byte, int, error) {
	if !slices.Contains(mamelet.Programs(), program) {
		return nil, nil, -1, fmt.Errorf("mamelet does not run %s", program)
	}
	if program != progUseradd && program != progUsermod {
		return nil, nil, -1, fmt.Errorf("mamelet ran %s for a user", program)
	}
	f.commands = append(f.commands, filepath.Base(program)+" "+strings.Join(args, " "))
	if filepath.Base(program) == f.fail {
		return nil, []byte(filepath.Base(program) + ": cannot lock /etc/passwd; try again later."), 1, nil
	}
	if len(args) < 2 || args[len(args)-2] != "--" {
		return nil, []byte("usage"), 2, nil
	}
	name := args[len(args)-1]
	passwd, group := f.read("etc/passwd"), f.read("etc/group")
	flag := func(n string) (string, bool) {
		i := slices.Index(args, n)
		if i < 0 {
			return "", false
		}
		if i+1 < len(args) {
			return args[i+1], true
		}
		return "", true
	}
	switch program {
	case progUseradd:
		uid := os.Getuid()
		if v, ok := flag("--uid"); ok {
			uid, _ = strconv.Atoi(v)
		}
		shell, _ := flag("--shell")
		home, ok := flag("--home-dir")
		if !ok {
			home = "/home/" + name
		}
		passwd += fmt.Sprintf("%s:x:%d:%d::%s:%s\n", name, uid, os.Getgid(), home, shell)
		group += fmt.Sprintf("%s:x:%d:\n", name, os.Getgid())
		if _, ok := flag("--create-home"); ok {
			if err := os.MkdirAll(filepath.Join(f.root, home), 0o755); err != nil {
				return nil, nil, -1, err
			}
		}
	case progUsermod:
		if v, ok := flag("--groups"); ok {
			add := strings.Split(v, ",")
			var lines []string
			for _, l := range strings.Split(strings.TrimSuffix(group, "\n"), "\n") {
				fs := strings.Split(l, ":")
				if slices.Contains(add, fs[0]) {
					if fs[3] != "" {
						fs[3] += ","
					}
					fs[3] += name
				}
				lines = append(lines, strings.Join(fs, ":"))
			}
			group = strings.Join(lines, "\n") + "\n"
		}
		if v, ok := flag("--shell"); ok {
			var lines []string
			for _, l := range strings.Split(strings.TrimSuffix(passwd, "\n"), "\n") {
				fs := strings.Split(l, ":")
				if fs[0] == name {
					fs[6] = v
				}
				lines = append(lines, strings.Join(fs, ":"))
			}
			passwd = strings.Join(lines, "\n") + "\n"
		}
	}
	write(f.t, filepath.Join(f.root, "etc/passwd"), passwd)
	write(f.t, filepath.Join(f.root, "etc/group"), group)
	return nil, nil, 0, nil
}

func (f *fakeAccounts) read(p string) string {
	b, err := os.ReadFile(filepath.Join(f.root, p))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// newUserNode is a mamelet on a node with root (uid 0), the groups docker and www-data, and the
// shells /bin/bash and /usr/sbin/nologin; useradd and usermod are fakeAccounts.
func newUserNode(t *testing.T) (*mnode, *fakeAccounts) {
	n := newNode(t)
	write(t, filepath.Join(n.root, "etc/passwd"), "root:x:0:0:root:/root:/bin/bash\n")
	write(t, filepath.Join(n.root, "etc/group"), "root:x:0:\ndocker:x:998:\nwww-data:x:33:\n")
	// The users fakeAccounts makes have the test's own uid, which is not a system user's here.
	write(t, filepath.Join(n.root, "etc/login.defs"), fmt.Sprintf("# login.defs\nUID_MIN\t\t%d\n", os.Getuid()))
	for _, sh := range []string{"bin/bash", "usr/sbin/nologin"} {
		write(t, filepath.Join(n.root, sh), "#!\n")
		if err := os.Chmod(filepath.Join(n.root, sh), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeAccounts{t: t, root: n.root}
	n.a.System.Runner = f
	return n, f
}

// addUser puts app on the node as useradd would have made it (the test's own ids), with its home.
func addUser(t *testing.T, n *mnode, line string) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(n.root, "etc/passwd"))
	write(t, filepath.Join(n.root, "etc/passwd"), string(b)+fmt.Sprintf(line, os.Getuid(), os.Getgid())+"\n")
	g, _ := os.ReadFile(filepath.Join(n.root, "etc/group"))
	write(t, filepath.Join(n.root, "etc/group"), string(g)+fmt.Sprintf("app:x:%d:\n", os.Getgid()))
	if err := os.MkdirAll(filepath.Join(n.root, "home/app"), 0o755); err != nil {
		t.Fatal(err)
	}
}

const appLine = "app:x:%d:%d::/home/app:/bin/bash"

// appendTo adds a line to a file of the node.
func appendTo(t *testing.T, n *mnode, p, line string) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(n.root, p))
	write(t, filepath.Join(n.root, p), string(b)+line)
}

// createApp is the approved creation of app with group docker and alice's key.
func createApp(op string, attempt int) User {
	content := KeysContent([]string{keyAlice})
	return User{Name: "app", Shells: []string{DefaultShell}, Groups: []string{"docker"}, Homes: []string{"/home/app"}, Keys: true, Change: &UserChange{
		Create: &UserCreate{Shell: DefaultShell, Home: "/home/app", CreateHome: true},
		Groups: []string{"docker"},
		Keys:   &KeysWrite{Content: content, SHA256: ContentSHA256(content)},
		Before: UserBefore{Keys: &Before{}},
		Grant:  noderev.Once{Operation: op, Attempt: attempt, Action: ActionCreateUser}}}
}

func userStateOf(t *testing.T, o Observed, name string) UserState {
	t.Helper()
	u := o.user(name)
	if u == nil {
		t.Fatalf("no user %s in %+v", name, o.UserStates)
	}
	return *u
}

// An approved creation: useradd with every flag spelled out and the name after "--", the groups,
// then ~/.ssh and authorized_keys as sshd wants them; once.
func TestMameletCreatesAUserOnce(t *testing.T) {
	n, f := newUserNode(t)
	n.offer(Part{Users: []User{createApp(opA, 1)}}, nil)
	rep := n.round()
	want := []string{
		"useradd --create-home --shell /bin/bash --home-dir /home/app --user-group -- app",
		"usermod --append --groups docker -- app",
	}
	if !slices.Equal(f.commands, want) {
		t.Fatalf("ran %q\nwant %q", f.commands, want)
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "made user app") || !strings.Contains(r.Detail, "wrote /home/app/.ssh/authorized_keys") {
		t.Errorf("result %+v", r)
	}
	if got := n.read("/home/app/.ssh/authorized_keys"); got != keyAlice+"\n" {
		t.Errorf("authorized_keys %q", got)
	}
	for p, mode := range map[string]os.FileMode{"home/app/.ssh": 0o700, "home/app/.ssh/authorized_keys": 0o600} {
		if fi, err := os.Lstat(filepath.Join(n.root, p)); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", p, fi.Mode(), err)
		}
	}
	// What mamelet reported is what the node has now (it observes after acting).
	u := userStateOf(t, observedOf(t, rep), "app")
	if !u.Exists || u.Home != "/home/app" || u.Shell != DefaultShell || !slices.Equal(u.Groups, []string{"docker"}) || u.Keys == nil ||
		len(u.Keys.Found) != 1 || u.Keys.Found[0].Fingerprint != fpAlice || u.Keys.File.Mode != "0600" || !strings.HasPrefix(u.Keys.Dir, "0700 app:") {
		t.Errorf("observed %+v %+v", u, u.Keys)
	}
	// The same grant does nothing again, even after the user changed by hand.
	write(t, filepath.Join(n.root, "home/app/.ssh/authorized_keys"), "changed by hand\n")
	f.commands = nil
	rep = n.round()
	if len(f.commands) != 0 || n.read("/home/app/.ssh/authorized_keys") != "changed by hand\n" {
		t.Errorf("the grant ran twice: %q", f.commands)
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone {
		t.Errorf("the result is not reported again: %+v", r)
	}
}

// A system user: --system, its uid, nologin, no home (no keys).
func TestMameletCreatesASystemUser(t *testing.T) {
	n, f := newUserNode(t)
	u := User{Name: "svc", Shells: []string{DefaultSystemShell}, UIDs: []int{990}, Change: &UserChange{
		Create: &UserCreate{System: true, UID: 990, Shell: DefaultSystemShell},
		Grant:  noderev.Once{Operation: opA, Attempt: 1, Action: ActionCreateUser}}}
	n.offer(Part{Users: []User{u}}, nil)
	rep := n.round()
	if want := []string{"useradd --system --uid 990 --shell /usr/sbin/nologin --user-group -- svc"}; !slices.Equal(f.commands, want) {
		t.Errorf("ran %q", f.commands)
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone {
		t.Errorf("result %+v", r)
	}
	if _, err := os.Stat(filepath.Join(n.root, "home/svc")); err == nil {
		t.Error("a home was made")
	}
}

// Interrupted after useradd (mamelet was killed before it recorded the outcome): the same attempt
// runs again, finds the user as approved, and does the rest without making it again.
func TestMameletFinishesAnInterruptedCreate(t *testing.T) {
	n, f := newUserNode(t)
	addUser(t, n, appLine)
	n.offer(Part{Users: []User{createApp(opA, 1)}}, nil)
	rep := n.round()
	if want := []string{"usermod --append --groups docker -- app"}; !slices.Equal(f.commands, want) {
		t.Errorf("ran %q", f.commands)
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "existed already as approved") {
		t.Errorf("result %+v", r)
	}
	if n.read("/home/app/.ssh/authorized_keys") != keyAlice+"\n" {
		t.Error("the keys were not written")
	}
}

// A step that fails leaves what was done, says so, and is not tried again by the same attempt; the
// next attempt finds the user made and does the rest.
func TestMameletRetriesAFailedCreateWithANewAttempt(t *testing.T) {
	n, f := newUserNode(t)
	f.fail = "usermod"
	n.offer(Part{Users: []User{createApp(opA, 1)}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "made user app") || !strings.Contains(r.Detail, "cannot lock") {
		t.Fatalf("result %+v", r)
	}
	f.fail, f.commands = "", nil
	n.round()
	if len(f.commands) != 0 {
		t.Errorf("attempt 1 ran again: %q", f.commands)
	}
	n.offer(Part{Users: []User{createApp(opA, 2)}}, nil)
	rep = n.round()
	if want := []string{"usermod --append --groups docker -- app"}; !slices.Equal(f.commands, want) {
		t.Errorf("ran %q", f.commands)
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || r.Attempt != 2 {
		t.Errorf("result %+v", r)
	}
}

// A change of a user that exists: the groups it is not in, the shell, and authorized_keys replaced
// (the key not declared goes).
func TestMameletChangesAUser(t *testing.T) {
	n, f := newUserNode(t)
	addUser(t, n, appLine)
	write(t, filepath.Join(n.root, "home/app/.ssh/authorized_keys"), keyOldCI+"\n")
	write(t, filepath.Join(n.root, "bin/zsh"), "#!\n")
	os.Chmod(filepath.Join(n.root, "bin/zsh"), 0o755)
	content := KeysContent([]string{keyAlice, keyBob})
	u := User{Name: "app", Shells: []string{"/bin/zsh"}, Groups: []string{"docker", "www-data"}, Keys: true, Change: &UserChange{
		Groups: []string{"docker", "www-data"}, Shell: "/bin/zsh", Keys: &KeysWrite{Content: content, SHA256: ContentSHA256(content)},
		Before: UserBefore{Exists: true, Shell: "/bin/bash", Keys: &Before{Exists: true, SHA256: ContentSHA256(keyOldCI + "\n")}},
		Grant:  noderev.Once{Operation: opA, Attempt: 1, Action: ActionChangeUser}}}
	// Before: what the plan reads.
	n.offer(Part{Users: []User{{Name: u.Name, Shells: u.Shells, Groups: u.Groups, Keys: true}}}, nil)
	before := userStateOf(t, observedOf(t, n.round()), "app")
	if before.Keys == nil || len(before.Keys.Found) != 1 || before.Keys.Found[0].Fingerprint != fpOldCI || !before.Shells["/bin/zsh"] {
		t.Fatalf("before %+v", before)
	}
	n.offer(Part{Users: []User{u}}, nil)
	rep := n.round()
	want := []string{"usermod --append --groups docker,www-data -- app", "usermod --shell /bin/zsh -- app"}
	if !slices.Equal(f.commands, want) {
		t.Errorf("ran %q\nwant %q", f.commands, want)
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone {
		t.Errorf("result %+v", r)
	}
	if got := n.read("/home/app/.ssh/authorized_keys"); got != content {
		t.Errorf("authorized_keys %q", got)
	}
	after := userStateOf(t, observedOf(t, rep), "app")
	if after.Shell != "/bin/zsh" || !slices.Equal(after.Groups, []string{"docker", "www-data"}) || len(after.Keys.Found) != 2 {
		t.Errorf("after %+v", after)
	}
}

// mamelet never writes SSH keys through a link the user put in its home: not ~/.ssh, not the home.
func TestMameletDoesNotWriteKeysThroughALink(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(n *mnode)
	}{
		{".ssh is a link to /etc", func(n *mnode) {
			if err := os.Symlink("../../etc", filepath.Join(n.root, "home/app/.ssh")); err != nil {
				t.Fatal(err)
			}
		}},
		{"authorized_keys is a link", func(n *mnode) {
			os.MkdirAll(filepath.Join(n.root, "home/app/.ssh"), 0o700)
			if err := os.Symlink("../../../etc/passwd", filepath.Join(n.root, "home/app/.ssh/authorized_keys")); err != nil {
				t.Fatal(err)
			}
		}},
		{"the home is a link", func(n *mnode) {
			os.Remove(filepath.Join(n.root, "home/app"))
			if err := os.Symlink("../etc", filepath.Join(n.root, "home/app")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, f := newUserNode(t)
			addUser(t, n, appLine)
			c.setup(n)
			passwd := n.read("/etc/passwd")
			content := KeysContent([]string{keyAlice})
			u := User{Name: "app", Shells: []string{DefaultShell}, Keys: true, Change: &UserChange{Keys: &KeysWrite{Content: content, SHA256: ContentSHA256(content)},
				Before: UserBefore{Exists: true, Keys: &Before{}}, Grant: noderev.Once{Operation: opA, Attempt: 1, Action: ActionChangeUser}}}
			n.offer(Part{Users: []User{u}}, nil)
			rep := n.round()
			if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "symbolic link") {
				t.Errorf("result %+v", r)
			}
			if st := userStateOf(t, observedOf(t, rep), "app"); st.Keys == nil || st.Keys.File.Unsafe == "" {
				t.Errorf("observed %+v", st.Keys)
			}
			if _, err := os.Lstat(filepath.Join(n.root, "etc/authorized_keys")); err == nil {
				t.Error("written into /etc")
			}
			if n.read("/etc/passwd") != passwd || len(f.commands) != 0 {
				t.Errorf("/etc/passwd changed, or ran %q", f.commands)
			}
		})
	}
}

// What mamelet refuses whatever the revision says: root (by name or uid), a change that is not the
// part's, a creation of a user that exists otherwise, a home in the system's directories, a group or
// shell the node does not have.
func TestMameletRefusesWhatIsNotTheUsers(t *testing.T) {
	keys := KeysContent([]string{keyAlice})
	once := func(action string) noderev.Once { return noderev.Once{Operation: opA, Attempt: 1, Action: action} }
	for _, c := range []struct {
		name   string
		setup  func(n *mnode)
		user   User
		refuse bool // the part is refused as a whole (ExtRefused) rather than the change
		says   string
	}{
		{"root by name", nil, User{Name: "root", Shells: []string{DefaultShell}, Groups: []string{"docker"}, Change: &UserChange{Groups: []string{"docker"}, Before: UserBefore{Exists: true}, Grant: once(ActionChangeUser)}}, true, "never manages root"},
		{"a group that is no name", nil, User{Name: "app", Shells: []string{DefaultShell}, Change: &UserChange{Groups: []string{"Sudo"}, Before: UserBefore{Exists: true}, Grant: once(ActionChangeUser)}}, true, "each a group's name"},
		{"keys without what the plan saw", nil, User{Name: "app", Shells: []string{DefaultShell}, Change: &UserChange{Keys: &KeysWrite{Content: keys, SHA256: ContentSHA256(keys)}, Before: UserBefore{Exists: true}, Grant: once(ActionChangeUser)}}, true, "without what the plan saw"},
		{"a change of a user the plan did not see", nil, User{Name: "app", Shells: []string{DefaultShell}, Change: &UserChange{Groups: []string{"docker"}, Grant: once(ActionChangeUser)}}, true, "did not see"},
		{"keys not as approved", nil, User{Name: "app", Shells: []string{DefaultShell}, Keys: true, Change: &UserChange{Keys: &KeysWrite{Content: keys, SHA256: ContentSHA256("other")}, Before: UserBefore{Exists: true, Keys: &Before{}}, Grant: once(ActionChangeUser)}}, true, "not the one approved"},
		{"keys with options", nil, func() User {
			k := KeysContent([]string{"restrict " + keyAlice})
			return User{Name: "app", Shells: []string{DefaultShell}, Keys: true, Change: &UserChange{Keys: &KeysWrite{Content: k, SHA256: ContentSHA256(k)}, Before: UserBefore{Exists: true, Keys: &Before{}}, Grant: once(ActionChangeUser)}}
		}(), true, "options"},
		{"a create with the wrong action", nil, User{Name: "app", Shells: []string{DefaultShell}, Change: &UserChange{Create: &UserCreate{Shell: DefaultShell}, Grant: once(ActionChangeUser)}}, true, "not valid"},
		{"a create with a home in /etc", nil, User{Name: "app", Shells: []string{DefaultShell}, Change: &UserChange{Create: &UserCreate{Shell: DefaultShell, Home: "/etc/app", CreateHome: true}, Grant: once(ActionCreateUser)}}, true, "the system's"},
		{"a create with a shell that is no path", nil, User{Name: "app", Shells: []string{DefaultShell}, Change: &UserChange{Create: &UserCreate{Shell: "sh"}, Grant: once(ActionCreateUser)}}, true, `shell "sh"`},
		{"a create of a user the plan saw", nil, User{Name: "app", Shells: []string{DefaultShell}, Change: &UserChange{Create: &UserCreate{Shell: DefaultShell}, Before: UserBefore{Exists: true}, Grant: once(ActionCreateUser)}}, true, "saw already"},
		{"uid 0", func(n *mnode) { appendTo(t, n, "etc/passwd", "app:x:0:0::/home/app:/bin/bash\n") },
			User{Name: "app", Shells: []string{DefaultShell}, Groups: []string{"docker"}, Change: &UserChange{Groups: []string{"docker"}, Before: UserBefore{Exists: true}, Grant: once(ActionChangeUser)}}, false, "uid 0"},
		{"exists otherwise", func(n *mnode) { addUser(t, n, "app:x:%d:%d::/home/app:/bin/sh") },
			User{Name: "app", Shells: []string{DefaultShell}, Change: &UserChange{Create: &UserCreate{Shell: DefaultShell, Home: "/home/app", CreateHome: true}, Grant: once(ActionCreateUser)}}, false, "its shell is /bin/sh"},
		{"does not exist", nil, User{Name: "app", Shells: []string{DefaultShell}, Groups: []string{"docker"}, Change: &UserChange{Groups: []string{"docker"}, Before: UserBefore{Exists: true}, Grant: once(ActionChangeUser)}}, false, "does not exist"},
		{"no such group", nil, User{Name: "app", Shells: []string{DefaultShell}, Groups: []string{"wheel"}, Change: &UserChange{Create: &UserCreate{Shell: DefaultShell}, Groups: []string{"wheel"}, Grant: once(ActionCreateUser)}}, false, "group wheel does not exist"},
		{"no such shell", nil, User{Name: "app", Shells: []string{"/usr/bin/zsh"}, Change: &UserChange{Create: &UserCreate{Shell: "/usr/bin/zsh"}, Grant: once(ActionCreateUser)}}, false, "not an executable file"},
		{"its group exists", func(n *mnode) { appendTo(t, n, "etc/group", "app:x:1500:\n") },
			User{Name: "app", Shells: []string{DefaultShell}, Change: &UserChange{Create: &UserCreate{Shell: DefaultShell}, Grant: once(ActionCreateUser)}}, false, "a group called app exists"},
		{"its uid is taken", func(n *mnode) { appendTo(t, n, "etc/passwd", "daemon:x:1:1::/usr/sbin:/usr/sbin/nologin\n") },
			User{Name: "app", Shells: []string{DefaultShell}, UIDs: []int{1}, Change: &UserChange{Create: &UserCreate{UID: 1, Shell: DefaultShell}, Grant: once(ActionCreateUser)}}, false, "uid 1 is daemon's"},
		{"a home in /etc", func(n *mnode) { addUser(t, n, "app:x:%d:%d::/etc/app:/bin/bash") },
			User{Name: "app", Shells: []string{DefaultShell}, Keys: true, Change: &UserChange{Keys: &KeysWrite{Content: keys, SHA256: ContentSHA256(keys)}, Before: UserBefore{Exists: true, Keys: &Before{}}, Grant: once(ActionChangeUser)}}, false, "the system's"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, f := newUserNode(t)
			if c.setup != nil {
				c.setup(n)
			}
			passwd, group := n.read("/etc/passwd"), n.read("/etc/group")
			n.offer(Part{Users: []User{c.user}}, nil)
			rep := n.round()
			if c.refuse {
				if !strings.Contains(rep.ExtRefused[Name], c.says) {
					t.Errorf("refused %q, want it to say %q", rep.ExtRefused[Name], c.says)
				}
			} else if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, c.says) {
				t.Errorf("result %+v, want it to say %q", r, c.says)
			}
			if len(f.commands) != 0 || n.read("/etc/passwd") != passwd || n.read("/etc/group") != group {
				t.Errorf("ran %q", f.commands)
			}
		})
	}
}

// The programs mamelet runs for users are fixed and registered: ExecRunner runs nothing else.
func TestUserProgramsAreRegistered(t *testing.T) {
	for _, p := range []string{progUseradd, progUsermod, progSystemctl} {
		if !slices.Contains(mamelet.Programs(), p) {
			t.Errorf("%s is not one of mamelet's programs", p)
		}
	}
	if args := useraddArgs("app", UserCreate{System: true, CreateHome: true, UID: 990, Shell: "/usr/sbin/nologin", Home: "/srv/app"}); strings.Join(args, " ") !=
		"--system --create-home --uid 990 --shell /usr/sbin/nologin --home-dir /srv/app --user-group -- app" {
		t.Errorf("%q", args)
	}
}

// A part's users as mamelet checks them before it uses the part.
func TestCheckPartUsers(t *testing.T) {
	ok := createApp(opA, 1)
	if err := checkPart(encode(Part{Users: []User{ok}})); err != nil {
		t.Fatal(err)
	}
	if err := checkPart(encode(Part{Users: []User{ok, ok}})); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a user twice: %v", err)
	}
	for name, f := range map[string]func(u *User){
		"name":       func(u *User) { u.Name = "-o" },
		"shell":      func(u *User) { u.Shells = []string{"bash"} },
		"group":      func(u *User) { u.Groups = []string{"Docker"} },
		"uid":        func(u *User) { u.UIDs = []int{-1} },
		"uids":       func(u *User) { u.UIDs = []int{1500, 1000} },
		"create uid": func(u *User) { u.Change.Create.UID = -1 },
		"op":         func(u *User) { u.Change.Grant.Operation = "op-1" },
		"attempt":    func(u *User) { u.Change.Grant.Attempt = 0 },
		"empty": func(u *User) {
			u.Change = &UserChange{Grant: noderev.Once{Operation: opA, Attempt: 1, Action: ActionChangeUser}}
		},
		"shell set": func(u *User) { u.Change.Shell = "/bin/sh" },
		"keys twice": func(u *User) {
			k := KeysContent([]string{keyAlice, keyAlice})
			u.Change.Keys = &KeysWrite{Content: k, SHA256: ContentSHA256(k)}
		},
		"keys empty":  func(u *User) { u.Change.Keys = &KeysWrite{Content: "", SHA256: ContentSHA256("")} },
		"keys no eol": func(u *User) { u.Change.Keys = &KeysWrite{Content: keyAlice, SHA256: ContentSHA256(keyAlice)} },
		// A creation's keys are written only against what the plan saw: none.
		"keys of a create without what the plan saw": func(u *User) { u.Change.Before.Keys = nil },
		"keys of a create the plan saw":              func(u *User) { u.Change.Before.Keys = &Before{Exists: true, SHA256: ContentSHA256("x")} },
	} {
		u := createApp(opA, 1)
		f(&u)
		if err := checkPart(encode(Part{Users: []User{u}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := checkPart(jsontext.Value(`{"users":[{"name":"app","shell":"/bin/bash","sudo":true}]}`)); err == nil {
		t.Error("an unknown member")
	}
}

// A user's change is a one-time grant like a file's write: its deadline stays with the Control Plane,
// and a change whose grant has ended is not handed out (nor its keys).
func TestUserGrantsAsMameletGetsThem(t *testing.T) {
	live, ended := createApp(opA, 1), createApp(opB, 1)
	live.Change.Grant.Expires = mt0.Add(time.Hour)
	ended.Name = "bob"
	ended.Change.Grant.Expires = mt0.Add(-time.Minute)
	r := noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: 1, IssuedAt: mt0, ExpiresAt: mt0.Add(time.Hour)}
	setPart(&r, Part{Users: []User{live, ended}})
	if g := r.Grants(); len(g) != 2 || g[0].Action != ActionCreateUser || g[0].Expires.IsZero() {
		t.Fatalf("grants %+v", g)
	}
	m := r.ForMamelet(mt0)
	p := partOf(&m)
	if len(p.Users) != 2 || p.Users[0].Name != "app" || p.Users[0].Change == nil || !p.Users[0].Change.Grant.Expires.IsZero() || p.Users[1].Change != nil {
		t.Fatalf("handed out %s", m.Ext[Name])
	}
	if strings.Count(string(m.Ext[Name]), "alice@laptop") != 1 {
		t.Errorf("the keys of an ended grant are handed out: %s", m.Ext[Name])
	}
	// A protected node gets no change.
	protectUsers(&p)
	if p.Users[0].Change != nil {
		t.Error("a change on a protected node")
	}
}

// A creation finds a user of its name that is not exactly what it makes (made by someone since the
// plan, not by an attempt of the creation): it changes nothing, and says what differs.
func TestMameletDoesNotTakeOverAUserMadeSinceThePlan(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, n *mnode)
		says  string
	}{
		{"with keys of its own", func(t *testing.T, n *mnode) {
			addUser(t, n, appLine)
			write(t, filepath.Join(n.root, "home/app/.ssh/authorized_keys"), keyBob+"\n")
		}, "it has an authorized_keys (sha256 " + ContentSHA256(keyBob + "\n")[:12] + ") this change did not write"},
		{"in another group", func(t *testing.T, n *mnode) {
			addUser(t, n, appLine)
			appendTo(t, n, "etc/group", "sudo:x:27:app\n")
		}, "it is in sudo, which this change does not add"},
		{"with another primary group", func(t *testing.T, n *mnode) {
			appendTo(t, n, "etc/passwd", fmt.Sprintf("app:x:%d:33::/home/app:/bin/bash\n", os.Getuid()))
			os.MkdirAll(filepath.Join(n.root, "home/app"), 0o755)
		}, "its primary group is www-data"},
		{"as a system user", func(t *testing.T, n *mnode) {
			addUser(t, n, appLine)
			write(t, filepath.Join(n.root, "etc/login.defs"), fmt.Sprintf("UID_MIN %d\n", os.Getuid()+1))
		}, "it is a system user"},
		{"without its home", func(t *testing.T, n *mnode) {
			addUser(t, n, appLine)
			os.Remove(filepath.Join(n.root, "home/app"))
		}, "its home /home/app cannot be looked at"},
		{"with a home that is not its own", func(t *testing.T, n *mnode) {
			notRoot(t)
			appendTo(t, n, "etc/passwd", "app:x:5000:5000::/home/app:/bin/bash\n")
			appendTo(t, n, "etc/group", "app:x:5000:\n")
			os.MkdirAll(filepath.Join(n.root, "home/app"), 0o755)
		}, "its home /home/app is"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, f := newUserNode(t)
			c.setup(t, n)
			passwd, group := n.read("/etc/passwd"), n.read("/etc/group")
			n.offer(Part{Users: []User{createApp(opA, 1)}}, nil)
			rep := n.round()
			if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "user app changed since the plan") || !strings.Contains(r.Detail, c.says) {
				t.Errorf("result %+v, want it to say %q", r, c.says)
			}
			if len(f.commands) != 0 || n.read("/etc/passwd") != passwd || n.read("/etc/group") != group {
				t.Errorf("ran %q", f.commands)
			}
			if b, err := os.ReadFile(filepath.Join(n.root, "home/app/.ssh/authorized_keys")); err == nil && string(b) == keyAlice+"\n" {
				t.Error("the keys were written")
			}
		})
	}
}

// A change of a user whose authorized_keys changed since the plan changes nothing at all: the
// compare and swap of the keys comes before the groups and the shell.
func TestMameletComparesTheKeysBeforeItChangesAnything(t *testing.T) {
	n, f := newUserNode(t)
	addUser(t, n, appLine)
	write(t, filepath.Join(n.root, "home/app/.ssh/authorized_keys"), keyBob+"\n")
	keys := KeysContent([]string{keyAlice})
	n.offer(Part{Users: []User{{Name: "app", Shells: []string{DefaultShell}, Groups: []string{"docker"}, Keys: true, Change: &UserChange{
		Groups: []string{"docker"}, Keys: &KeysWrite{Content: keys, SHA256: ContentSHA256(keys)},
		Before: UserBefore{Exists: true, Shell: DefaultShell, Keys: &Before{}},
		Grant:  noderev.Once{Operation: opA, Attempt: 1, Action: ActionChangeUser}}}}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "authorized_keys changed since the plan (it was absent") {
		t.Errorf("result %+v", r)
	}
	if len(f.commands) != 0 || n.read("/home/app/.ssh/authorized_keys") != keyBob+"\n" {
		t.Errorf("ran %q", f.commands)
	}
}

// cancelDuring is a runner that cancels the round (as SIGTERM would) when mamelet runs program, and
// says whether what it ran then was cancelled with it, and bounded.
type cancelDuring struct {
	inner    mamelet.Runner
	program  string
	cancel   context.CancelFunc
	canceled bool
	deadline bool
}

func (c *cancelDuring) Run(ctx context.Context, program string, args []string, env []string) ([]byte, []byte, int, error) {
	if c.cancel != nil && program == c.program {
		c.cancel()
		c.cancel = nil
		c.canceled = ctx.Err() != nil
		_, c.deadline = ctx.Deadline()
	}
	return c.inner.Run(ctx, program, args, env)
}

// mamelet stopping while useradd runs does not kill it (killed, it could leave /etc/passwd and
// /etc/shadow half changed): it runs to its end, within AccountsTimeout. The attempt stops after it
// without being recorded, and goes on in the next round from there.
func TestMameletDoesNotKillUseraddWhenItStops(t *testing.T) {
	n, f := newUserNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	run := &cancelDuring{inner: f, program: progUseradd, cancel: cancel}
	n.a.System.Runner = run
	n.offer(Part{Users: []User{createApp(opA, 1)}}, nil)
	rep, err := n.a.Round(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run.canceled || !run.deadline {
		t.Errorf("useradd ran with a context cancelled with mamelet's (%v), or unbounded (%v)", run.canceled, !run.deadline)
	}
	if want := []string{"useradd --create-home --shell /bin/bash --home-dir /home/app --user-group -- app"}; !slices.Equal(f.commands, want) {
		t.Fatalf("ran %q", f.commands)
	}
	if slices.ContainsFunc(rep.Results, func(r noderev.Result) bool { return r.Operation == opA }) {
		t.Fatalf("the attempt cut short was recorded: %+v", rep.Results)
	}
	f.commands = nil
	if r := result(t, n.round(), opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "existed already as approved") {
		t.Errorf("result %+v", r)
	}
	if want := []string{"usermod --append --groups docker -- app"}; !slices.Equal(f.commands, want) {
		t.Errorf("ran %q", f.commands)
	}
}
