package linux

// Regression tests of the review of the linux extension's node side (2026-11-18): another user's
// home, what mamelet opens while a user races it, the directories it makes, the space it needs, what
// an interrupted attempt leaves, the size of its report, and what it does again after it stopped.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// notRoot skips a test that tells users apart by the owner of the test's files: run as root, they
// would all be root's.
func notRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("needs to run as a user other than root (the test's own user stands for another user of the node)")
	}
}

// addBob puts bob on the node with the test's own ids, and his home /home/bob with his key.
func addBob(t *testing.T, n *mnode) {
	t.Helper()
	appendTo(t, n, "etc/passwd", fmt.Sprintf("bob:x:%d:%d::/home/bob:/bin/bash\n", os.Getuid(), os.Getgid()))
	appendTo(t, n, "etc/group", fmt.Sprintf("bob:x:%d:\n", os.Getgid()))
	write(t, filepath.Join(n.root, "home/bob/.ssh/authorized_keys"), keyBob+"\n")
}

// H1: a user declared with another user's home is not made (useradd would take the home as it is,
// and mamelet would then have chowned bob's ~/.ssh and replaced his keys); the report says whose the
// home is, for the plan to block.
func TestMameletNeverTakesAnotherUsersHome(t *testing.T) {
	notRoot(t)
	n, f := newUserNode(t)
	addBob(t, n)
	keys := KeysContent([]string{keyAlice})
	entry := User{Name: "app", Shells: []string{DefaultShell}, UIDs: []int{5000}, Homes: []string{"/home/bob"}, Keys: true}
	n.offer(Part{Users: []User{entry}}, nil)
	st := userStateOf(t, observedOf(t, n.round()), "app")
	if h := st.newHome("/home/bob"); h == nil || !h.Exists || h.What != "a directory" || h.Owner != "bob" || h.Path != "/home/bob" {
		t.Fatalf("the home is reported as %+v", st.NewHomes)
	}
	u := entry
	u.Change = &UserChange{Create: &UserCreate{UID: 5000, Shell: DefaultShell, Home: "/home/bob", CreateHome: true},
		Keys: &KeysWrite{Content: keys, SHA256: ContentSHA256(keys)}, Before: UserBefore{Keys: &Before{}}, Grant: noderev.Once{Operation: opA, Attempt: 1, Action: ActionCreateUser}}
	n.offer(Part{Users: []User{u}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "/home/bob of user app exists already") {
		t.Errorf("result %+v", r)
	}
	if len(f.commands) != 0 {
		t.Errorf("ran %q", f.commands)
	}
	if got := n.read("/home/bob/.ssh/authorized_keys"); got != keyBob+"\n" {
		t.Errorf("bob's keys are now %q", got)
	}
}

// H1: the home of a system user with keys (named by the Control Plane: /home/<name>, never
// useradd's default) is there already (bob's): mamelet does not run useradd, which would take it as it
// is. A grant that makes a home without naming it is refused whole.
func TestMameletRefusesAHomeUseraddWouldTakeAsItIs(t *testing.T) {
	notRoot(t)
	n, f := newUserNode(t)
	addBob(t, n)
	write(t, filepath.Join(n.root, "home/svc/.ssh/authorized_keys"), keyBob+"\n")
	keys := KeysContent([]string{keyAlice})
	u := User{Name: "svc", Shells: []string{DefaultSystemShell}, UIDs: []int{990}, Homes: []string{"/home/svc"}, Keys: true, Change: &UserChange{
		Create: &UserCreate{System: true, UID: 990, Shell: DefaultSystemShell, Home: "/home/svc", CreateHome: true},
		Keys:   &KeysWrite{Content: keys, SHA256: ContentSHA256(keys)}, Before: UserBefore{Keys: &Before{}}, Grant: noderev.Once{Operation: opA, Attempt: 1, Action: ActionCreateUser}}}
	n.offer(Part{Users: []User{u}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "the home /home/svc of user svc exists already") {
		t.Errorf("result %+v", r)
	}
	if len(f.commands) != 0 {
		t.Errorf("ran %q", f.commands)
	}
	if got := n.read("/home/svc/.ssh/authorized_keys"); got != keyBob+"\n" {
		t.Errorf("the keys in the home are now %q", got)
	}
	u.Change.Create.Home = ""
	if err := checkPart(encode(Part{Users: []User{u}})); err == nil || !strings.Contains(err.Error(), "makes a home without naming it") {
		t.Errorf("a home made without a name: %v", err)
	}
}

// rootIsTheTestsUser makes root of the node the test's own user (whose the test's directories are).
func rootIsTheTestsUser(t *testing.T, n *mnode) {
	t.Helper()
	write(t, filepath.Join(n.root, "etc/passwd"), fmt.Sprintf("root:x:%d:%d:root:/root:/bin/bash\n", os.Getuid(), os.Getgid()))
	write(t, filepath.Join(n.root, "etc/group"), fmt.Sprintf("root:x:%d:\ndocker:x:998:\nwww-data:x:33:\n", os.Getgid()))
}

// The home would be made below a directory a user other than root can change (everyone may write
// /home here; or a link leads to one): that user could put a directory or a link of their own at the
// home between mamelet's look and useradd's making it. mamelet reports it, and does not run useradd.
func TestMameletDoesNotMakeAHomeBelowADirectoryOthersCanChange(t *testing.T) {
	for _, c := range []struct {
		name    string
		setup   func(t *testing.T, root string)
		exposed string
	}{
		{"everyone may write /home", func(t *testing.T, root string) {
			os.MkdirAll(filepath.Join(root, "home"), 0o755)
			os.Chmod(filepath.Join(root, "home"), 0o777)
		}, "/home"},
		{"/home is a link to a directory everyone may write", func(t *testing.T, root string) {
			os.MkdirAll(filepath.Join(root, "var/home"), 0o755)
			os.Chmod(filepath.Join(root, "var/home"), 0o777)
			os.Symlink("var/home", filepath.Join(root, "home"))
		}, "/var/home"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, f := newUserNode(t)
			rootIsTheTestsUser(t, n)
			c.setup(t, n.root)
			entry := createApp(opA, 1)
			change := entry.Change
			entry.Change = nil
			n.offer(Part{Users: []User{entry}}, nil)
			h := userStateOf(t, observedOf(t, n.round()), "app").newHome("/home/app")
			if h == nil || h.Exists || len(h.Exposed) != 1 || h.Exposed[0].Dir != c.exposed || h.Exposed[0].Mode != "0777" {
				t.Fatalf("the home is reported as %+v", h)
			}
			entry.Change = change
			n.offer(Part{Users: []User{entry}}, nil)
			if r := result(t, n.round(), opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "is below "+c.exposed+", which everyone may write") {
				t.Errorf("result %+v", r)
			}
			if len(f.commands) != 0 {
				t.Errorf("ran %q", f.commands)
			}
		})
	}
	// Only root can change /home: nothing to report.
	n, _ := newUserNode(t)
	rootIsTheTestsUser(t, n)
	os.MkdirAll(filepath.Join(n.root, "home"), 0o755)
	n.offer(Part{Users: []User{{Name: "app", Shells: []string{DefaultShell}, Homes: []string{"/home/app"}}}}, nil)
	if h := userStateOf(t, observedOf(t, n.round()), "app").newHome("/home/app"); h == nil || len(h.Exposed) != 0 || h.Error != "" {
		t.Errorf("the home is reported as %+v", h)
	}
}

// H1: a user whose home (in /etc/passwd) is another user's: its keys are never written there, and the
// report says so.
func TestMameletDoesNotWriteKeysIntoAnotherUsersHome(t *testing.T) {
	notRoot(t)
	n, f := newUserNode(t)
	addBob(t, n)
	appendTo(t, n, "etc/passwd", "app:x:5000:5000::/home/bob:/bin/bash\n")
	appendTo(t, n, "etc/group", "app:x:5000:\n")
	keys := KeysContent([]string{keyAlice})
	u := User{Name: "app", Shells: []string{DefaultShell}, Keys: true, Change: &UserChange{Keys: &KeysWrite{Content: keys, SHA256: ContentSHA256(keys)},
		Before: UserBefore{Exists: true, Shell: DefaultShell, Keys: &Before{Exists: true, SHA256: ContentSHA256(keyBob + "\n")}},
		Grant:  noderev.Once{Operation: opA, Attempt: 1, Action: ActionChangeUser}}}
	before, _ := os.Stat(filepath.Join(n.root, "home/bob/.ssh"))
	n.offer(Part{Users: []User{u}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "the home /home/bob is bob's") {
		t.Errorf("result %+v", r)
	}
	if got := n.read("/home/bob/.ssh/authorized_keys"); got != keyBob+"\n" || len(f.commands) != 0 {
		t.Errorf("bob's keys are now %q; ran %q", got, f.commands)
	}
	if after, _ := os.Stat(filepath.Join(n.root, "home/bob/.ssh")); after.Mode() != before.Mode() {
		t.Errorf("bob's ~/.ssh was changed: %v -> %v", before.Mode(), after.Mode())
	}
	if ks := userStateOf(t, observedOf(t, rep), "app").Keys; ks == nil || !strings.Contains(ks.NotOwned, "/home/bob is bob's") {
		t.Errorf("observed %+v", ks)
	}
}

// H2: what is at a path is swapped for a link between mamelet's look and its opening: the link is
// not followed (the secret it leads to is never read or hashed).
func TestInspectDoesNotFollowALinkPutMeanwhile(t *testing.T) {
	n := newNode(t)
	write(t, filepath.Join(n.root, "etc/secret"), "secret\n")
	keys := filepath.Join(n.root, "home/app/.ssh/authorized_keys")
	write(t, keys, keyAlice+"\n")
	r, err := os.OpenRoot(n.root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a := readAccounts(r)
	testHookOpenLeaf = func(p string) {
		if p == "/home/app/.ssh/authorized_keys" {
			os.Remove(keys)
			os.Symlink("../../../etc/secret", keys)
		}
	}
	t.Cleanup(func() { testHookOpenLeaf = nil })
	st := inspect(r, "/home/app/.ssh/authorized_keys", a)
	if st.SHA256 == ContentSHA256("secret\n") || !strings.Contains(st.Unsafe, "symbolic link") {
		t.Errorf("observed %+v", st)
	}
	if _, err := readKeys(r, "/home/app"); err == nil {
		t.Error("read through the link")
	}
}

// H2: a directory above the path is swapped for a link (to a directory of the user's own) between
// the look and the opening: what was opened is not the directory looked at, and mamelet stops.
func TestOpenRegularRefusesADirectorySwappedMeanwhile(t *testing.T) {
	n := newNode(t)
	write(t, filepath.Join(n.root, "home/app/.ssh/authorized_keys"), keyAlice+"\n")
	write(t, filepath.Join(n.root, "home/app/decoy/authorized_keys"), keyBob+"\n")
	r, _ := os.OpenRoot(n.root)
	defer r.Close()
	testHookOpenSubdir = func(p string) {
		if p == "/home/app/.ssh" {
			os.Rename(filepath.Join(n.root, "home/app/.ssh"), filepath.Join(n.root, "home/app/old"))
			os.Symlink("decoy", filepath.Join(n.root, "home/app/.ssh"))
		}
	}
	t.Cleanup(func() { testHookOpenSubdir = nil })
	if b, err := readRegular(r, "/home/app/.ssh/authorized_keys", MaxContent); err == nil || !strings.Contains(err.Error(), "changed while mamelet opened it") {
		t.Errorf("read %q, %v", b, err)
	}
}

// L1: a directory mamelet makes is given its owner and mode through what it opened, never through
// its name; one that appears meanwhile (made by someone else, or a link) is refused, not taken.
func TestMakeDirRefusesWhatAppeared(t *testing.T) {
	dir := t.TempDir()
	r, _ := os.OpenRoot(dir)
	defer r.Close()
	top, err := treeDir(r)
	if err != nil {
		t.Fatal(err)
	}
	defer top.Close()
	os.Mkdir(filepath.Join(dir, "there"), 0o777)
	os.Symlink("/etc", filepath.Join(dir, "link"))
	for _, name := range []string{"there", "link"} {
		if err := makeDir(top, name, "/"+name, os.Getuid(), os.Getgid()); err == nil || !strings.Contains(err.Error(), "appeared while mamelet made it") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "there")); fi.Mode().Perm() != 0o777&^umask() {
		t.Errorf("the directory that was there became %v", fi.Mode())
	}
	if err := makeDir(top, "new", "/new", os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, "new")); !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Errorf("made %v", fi.Mode())
	}
}

func umask() fs.FileMode {
	d := filepath.Join(os.TempDir(), fmt.Sprintf("mamela-umask-%d", time.Now().UnixNano()))
	os.Mkdir(d, 0o777)
	defer os.Remove(d)
	fi, err := os.Stat(d)
	if err != nil {
		return 0
	}
	return 0o777 &^ fi.Mode().Perm()
}

// L2: a file root writes below a directory another user can change (here, everyone): mamelet says
// which, for the plan to warn.
func TestMameletReportsDirectoriesOthersCanChange(t *testing.T) {
	n := newNode(t)
	if err := os.MkdirAll(filepath.Join(n.root, "srv/shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(n.root, "srv/shared"), 0o757)
	n.offer(Part{Files: []File{{Path: "/srv/shared/app.conf", Owners: []string{"root"}, Groups: []string{"root"}}, {Path: "/etc/app.conf", Owners: []string{"root"}, Groups: []string{"root"}}}}, nil)
	o := observedOf(t, n.round())
	if ex := o.file("/srv/shared/app.conf").Exposed; len(ex) != 1 || ex[0].Dir != "/srv/shared" || ex[0].Mode != "0757" {
		t.Errorf("exposed %+v", ex)
	}
	if ex := o.file("/etc/app.conf").Exposed; len(ex) != 0 {
		t.Errorf("/etc is exposed: %+v", ex)
	}
	if w := exposedWarning("web", "/srv/shared/app.conf", "root", "root", o.file("/srv/shared/app.conf").Exposed); w == nil || w.Code != codeFileExposed || !strings.Contains(w.Message, "every user") {
		t.Errorf("warning %+v", w)
	}
	// The owner of the file may own the directory: nothing to warn of.
	if w := exposedWarning("web", "/home/app/x", "app", "app", []DirOwner{{Dir: "/home/app", Mode: "0750", Owner: "app", Group: "app"}}); w != nil {
		t.Errorf("warning %+v", w)
	}
}

// L3: what an interrupted write and download left (mamelet was killed) is removed at the next round:
// only mamelet's own temporary files.
func TestMameletRemovesStaleTemporaryFiles(t *testing.T) {
	n := newNode(t)
	stale := filepath.Join(n.root, "etc/app", tempName("app.conf"))
	write(t, stale, "half a file")
	other := filepath.Join(n.root, "etc/app", ".mamelet-notmine.tmp")
	write(t, other, "someone's")
	state := filepath.Join(n.root, "var/lib/mamelet")
	for _, f := range []string{downloadName("/usr/local/bin/tool", ".download"), ".linux-apt-key-docker.download", "." + recordName + ".tmp"} {
		write(t, filepath.Join(state, f), "partial")
	}
	write(t, filepath.Join(state, recordName), `{"artifacts":{}}`)
	n.offer(Part{Files: []File{{Path: "/etc/app/app.conf", Owners: []string{"root"}, Groups: []string{"root"}}}}, nil)
	n.round()
	if n.exists("/etc/app/" + tempName("app.conf")) {
		t.Error("the temporary file of the write is still there")
	}
	if !n.exists("/etc/app/.mamelet-notmine.tmp") {
		t.Error("a file that is not the write's was removed")
	}
	if files := n.stateFiles(); !slices.Equal(files, []string{recordName}) {
		t.Errorf("the state directory has %q", files)
	}
}

// L3: no room on the file system for the file and the margin: mamelet writes nothing, and says why.
func TestMameletRefusesAWriteWithoutRoom(t *testing.T) {
	r, _ := os.OpenRoot(t.TempDir())
	defer r.Close()
	if freeBytes(must(r.Open("."))) < 0 {
		t.Skip("this system does not say how much is free")
	}
	old := DiskMargin
	DiskMargin = 1 << 62
	t.Cleanup(func() { DiskMargin = old })
	n := newNode(t)
	n.offer(Part{Files: []File{grant("/etc/app.conf", "x\n", opA, 1)}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "MiB free") {
		t.Errorf("result %+v", r)
	}
	if n.exists("/etc/app.conf") || n.exists("/etc/"+tempName("app.conf")) {
		t.Error("something was written")
	}
	if err := enoughFree(100<<20, "/x", 10<<20); err == nil && DiskMargin > 90<<20 {
		t.Error("enoughFree took 100 MiB free for 10 MiB and the margin")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// L4: a shell that is an absolute link (the alternatives) is followed by looking, and is
// executable; a loop of links is not.
func TestExecutableFollowsAbsoluteLinks(t *testing.T) {
	n, _ := newUserNode(t)
	write(t, filepath.Join(n.root, "bin/ksh93"), "#!\n")
	os.Chmod(filepath.Join(n.root, "bin/ksh93"), 0o755)
	os.MkdirAll(filepath.Join(n.root, "etc/alternatives"), 0o755)
	os.MkdirAll(filepath.Join(n.root, "usr/bin"), 0o755)
	os.Symlink("/bin/ksh93", filepath.Join(n.root, "etc/alternatives/ksh"))
	os.Symlink("/etc/alternatives/ksh", filepath.Join(n.root, "usr/bin/ksh"))
	os.Symlink("loop2", filepath.Join(n.root, "usr/bin/loop1"))
	os.Symlink("loop1", filepath.Join(n.root, "usr/bin/loop2"))
	r, _ := os.OpenRoot(n.root)
	defer r.Close()
	if !executable(r, "/usr/bin/ksh") {
		t.Error("/usr/bin/ksh is not taken as executable")
	}
	if executable(r, "/usr/bin/loop1") || executable(r, "/etc/passwd") {
		t.Error("a loop or a file that does not run is taken as executable")
	}
	n.offer(Part{Users: []User{{Name: "app", Shells: []string{"/usr/bin/ksh"}}}}, nil)
	if st := userStateOf(t, observedOf(t, n.round()), "app"); !st.Shells["/usr/bin/ksh"] {
		t.Errorf("observed %+v", st)
	}
}

// L4: a source file apt reads but mamelet cannot (a link to nothing, a pipe): mamelet does not judge
// without it, and adds nothing. A link to a file is read where it leads (a conflict through it is
// found).
func TestMameletFailsClosedOnUnreadableAptSources(t *testing.T) {
	t.Run("unreadable", func(t *testing.T) {
		n, apt, k := aptNode(t)
		os.MkdirAll(filepath.Join(n.root, "etc/apt/sources.list.d"), 0o755)
		os.Symlink("/usr/share/vendor/gone.list", filepath.Join(n.root, "etc/apt/sources.list.d/vendor.list"))
		n.offer(Part{AptRepos: []AptRepo{addGrant(k, "/armored", testKeyArmored, opA, 1)}}, nil)
		rep := n.round()
		if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "cannot read the APT source files /etc/apt/sources.list.d/vendor.list") {
			t.Errorf("result %+v", r)
		}
		if s := aptState(t, rep); !strings.Contains(s.Error, "vendor.list") {
			t.Errorf("observed %+v", s)
		}
		if len(apt.commands) != 0 || n.exists("/etc/apt/sources.list.d/mamela-docker.sources") || k.count("/armored") != 0 {
			t.Errorf("ran %q", apt.commands)
		}
	})
	t.Run("a link to a file", func(t *testing.T) {
		n, _, k := aptNode(t)
		write(t, filepath.Join(n.root, "usr/share/vendor/docker.list"), "deb https://download.docker.com/linux/ubuntu noble stable\n")
		os.MkdirAll(filepath.Join(n.root, "etc/apt/sources.list.d"), 0o755)
		os.Symlink("/usr/share/vendor/docker.list", filepath.Join(n.root, "etc/apt/sources.list.d/docker.list"))
		n.offer(Part{AptRepos: []AptRepo{addGrant(k, "/armored", testKeyArmored, opA, 1)}}, nil)
		rep := n.round()
		if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, "docker.list names a URI and suite") {
			t.Errorf("result %+v", r)
		}
	})
}

// S1: apt-get update finds apt's lists locked (apt-daily): mamelet waits and tries again, within a
// bound, instead of failing at once.
func TestMameletWaitsForTheListsLock(t *testing.T) {
	oldWait, oldRetry := AptListsLockWait, AptListsLockRetry
	AptListsLockRetry = time.Millisecond
	t.Cleanup(func() { AptListsLockWait, AptListsLockRetry = oldWait, oldRetry })

	n, apt, k := aptNode(t)
	apt.locked = 2
	n.offer(Part{AptRepos: []AptRepo{addGrant(k, "/armored", testKeyArmored, opA, 1)}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || len(apt.commands) != 3 {
		t.Errorf("result %+v; ran %q", r, apt.commands)
	}

	AptListsLockWait = 20 * time.Millisecond
	n, apt, k = aptNode(t)
	apt.locked = 1 << 20
	n.offer(Part{AptRepos: []AptRepo{addGrant(k, "/armored", testKeyArmored, opA, 1)}}, nil)
	rep = n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "another apt held /var/lib/apt/lists/lock") {
		t.Errorf("result %+v", r)
	}
}

// M2: a few bytes after a key type are not a key; real keys of every type are.
func TestKeyBlobsMustBeKeys(t *testing.T) {
	for _, l := range []string{keyAlice, keyBob, keyOldCI} {
		if _, err := ParseAuthorizedKey(l); err != nil {
			t.Errorf("%s: %v", l[:20], err)
		}
	}
	fake := func(parts ...[]byte) string {
		var b []byte
		for _, p := range parts {
			b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
			b = append(b, p...)
		}
		return base64.StdEncoding.EncodeToString(b)
	}
	for name, line := range map[string]string{
		"one byte":       "ssh-ed25519 " + fake([]byte("ssh-ed25519"), []byte{1}),
		"31 bytes":       "ssh-ed25519 " + fake([]byte("ssh-ed25519"), make([]byte, 31)),
		"trailing bytes": "ssh-ed25519 " + fake([]byte("ssh-ed25519"), make([]byte, 32), []byte{0}),
		"small rsa":      "ssh-rsa " + fake([]byte("ssh-rsa"), []byte{1, 0, 1}, append([]byte{0x7f}, make([]byte, 63)...)),
		"wrong curve":    "ecdsa-sha2-nistp256 " + fake([]byte("ecdsa-sha2-nistp256"), []byte("nistp384"), append([]byte{4}, make([]byte, 64)...)),
		"no app":         "sk-ssh-ed25519@openssh.com " + fake([]byte("sk-ssh-ed25519@openssh.com"), make([]byte, 32)),
	} {
		if _, err := ParseAuthorizedKey(line); err == nil {
			t.Errorf("%s: taken", name)
		}
		if keys, _, other := scanAuthorizedKeys(line + "\n"); len(keys) != 0 || other != 1 {
			t.Errorf("%s: found %+v", name, keys)
		}
	}
}

// randomKeys are n lines of distinct Ed25519 public keys (random, well formed).
func randomKeys(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	for range n {
		pub := make([]byte, 32)
		rand.Read(pub)
		blob := binary.BigEndian.AppendUint32(nil, 11)
		blob = append(blob, "ssh-ed25519"...)
		blob = binary.BigEndian.AppendUint32(blob, 32)
		blob = append(blob, pub...)
		b.WriteString("ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + "\n")
	}
	return b.String()
}

// M2: the keys found are listed once each and at most MaxFoundKeys, the rest counted.
func TestScanCapsAndDedupes(t *testing.T) {
	content := keyAlice + "\n" + keyAlice + "\n" + randomKeys(t, 100)
	keys, more, other := scanAuthorizedKeys(content)
	if len(keys) != MaxFoundKeys || more != 101-MaxFoundKeys || other != 0 || keys[0].Fingerprint != fpAlice || keys[1].Fingerprint == fpAlice {
		t.Errorf("%d keys, %d more, %d other", len(keys), more, other)
	}
}

// M2: two users whose authorized_keys are full of keys (256 KiB each): the report stays small (it
// was more than the Control Plane's 1 MiB before).
func TestReportStaysBoundedForAdversarialKeys(t *testing.T) {
	n, _ := newUserNode(t)
	lines := (MaxContent - 100) / 81
	for _, u := range []string{"app", "deploy"} {
		appendTo(t, n, "etc/passwd", fmt.Sprintf("%s:x:%d:%d::/home/%s:/bin/bash\n", u, os.Getuid(), os.Getgid(), u))
		appendTo(t, n, "etc/group", fmt.Sprintf("%s:x:%d:\n", u, os.Getgid()))
		write(t, filepath.Join(n.root, "home", u, ".ssh/authorized_keys"), randomKeys(t, lines))
	}
	n.offer(Part{Users: []User{{Name: "app", Shells: []string{DefaultShell}, Keys: true}, {Name: "deploy", Shells: []string{DefaultShell}, Keys: true}}}, nil)
	rep := n.round()
	if size := len(rep.Ext[Name]); size > 32<<10 {
		t.Errorf("the linux part of the report is %d bytes", size)
	}
	if ks := userStateOf(t, observedOf(t, rep), "app").Keys; ks == nil || len(ks.Found) != MaxFoundKeys || ks.More != lines-MaxFoundKeys {
		t.Errorf("observed %+v", ks)
	}
}

// M2: an observation larger than MaxObserved is sent as what each entry is and why its details were
// left out, never as more.
func TestEncodeObservedDegrades(t *testing.T) {
	var o Observed
	for i := range MaxNodeFiles {
		o.Files = append(o.Files, FileState{Path: fmt.Sprintf("/etc/app/%03d.conf", i), Error: strings.Repeat("x", 4096)})
	}
	o.UserStates = []UserState{{Name: "app", Groups: []string{"docker"}}}
	out := encodeObserved(o)
	var got Observed
	if err := decodeStrict(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(out) > MaxObserved || got.Error == "" || len(got.Files) != MaxNodeFiles || !strings.Contains(got.Files[0].Error, "left the details out") || got.UserStates[0].Groups != nil {
		t.Errorf("%d bytes: %+v", len(out), got.Files[0])
	}
	if small := encodeObserved(Observed{Files: []FileState{{Path: "/etc/a"}}}); string(small) != string(encode(Observed{Files: []FileState{{Path: "/etc/a"}}})) {
		t.Errorf("a small one changed: %s", small)
	}
}

// M3: an artifact is hashed again only when it changed (its inode, size, times), not every round; a
// change that keeps its size and mtime is still seen (ctime).
func TestMameletHashesAnArtifactOnlyWhenItChanged(t *testing.T) {
	rel := newReleases(t)
	body := []byte(strings.Repeat("a", MaxContent+1024))
	sum := rel.put("/big", body)
	n := artifactNode(t, rel)
	n.offer(Part{Artifacts: []Artifact{install("/usr/local/bin/big", rel.url("/big"), sum, "", opA, 1)}}, nil)
	n.round()
	before := largeHashed.Load()
	for range 3 {
		if st := artifactOf(t, n.round(), "/usr/local/bin/big"); st.SHA256 != sum || st.From == nil {
			t.Fatalf("observed %+v", st)
		}
	}
	if got := largeHashed.Load() - before; got != 0 && runtimeStamps() {
		t.Errorf("hashed %d times in 3 rounds", got)
	}
	p := filepath.Join(n.root, "usr/local/bin/big")
	fi, _ := os.Stat(p)
	f, _ := os.OpenFile(p, os.O_WRONLY, 0)
	f.WriteAt([]byte("b"), 0)
	f.Close()
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	if st := artifactOf(t, n.round(), "/usr/local/bin/big"); st.SHA256 == sum || st.From != nil {
		t.Errorf("the change was not seen: %+v", st)
	}
}

// runtimeStamps: this system gives files a stamp (nofollow_unix.go), so the cache is used.
func runtimeStamps() bool {
	f, err := os.CreateTemp("", "stamp")
	if err != nil {
		return false
	}
	defer os.Remove(f.Name())
	f.Close()
	r, _ := os.OpenRoot(filepath.Dir(f.Name()))
	defer r.Close()
	d, err := treeDir(r)
	if err != nil {
		return false
	}
	defer d.Close()
	g, _, s, err := d.openLeaf(filepath.Base(f.Name()), f.Name())
	if err == nil {
		g.Close()
	}
	return s.OK
}

// M3: a part with more of a kind than mamelet observes on one node is refused, and says how many.
func TestCheckPartBoundsTheCounts(t *testing.T) {
	var p Part
	for i := range MaxNodeFiles + 1 {
		p.Files = append(p.Files, File{Path: fmt.Sprintf("/etc/app/%d.conf", i), Owners: []string{"root"}, Groups: []string{"root"}})
	}
	if err := checkPart(encode(p)); err == nil || !strings.Contains(err.Error(), "it names 257 files; mamelet observes at most 256") {
		t.Errorf("%v", err)
	}
	p.Files = p.Files[:MaxNodeFiles]
	if err := checkPart(encode(p)); err != nil {
		t.Error(err)
	}
}

// cancelOn is a runner that cancels the round when mamelet runs verb (as SIGTERM would), and fails
// it as a cancelled command does.
type cancelOn struct {
	inner  mamelet.Runner
	verb   string
	cancel context.CancelFunc
}

func (c *cancelOn) Run(ctx context.Context, program string, args []string, env []string) ([]byte, []byte, int, error) {
	if c.cancel != nil && len(args) > 0 && args[0] == c.verb {
		c.cancel()
		c.cancel = nil
		return nil, nil, -1, context.Canceled
	}
	return c.inner.Run(ctx, program, args, env)
}

// M4: mamelet is stopped while it restarts what a write names: the attempt is not recorded as failed
// (it was cut short), and the next round carries it on with the same grant, without writing again.
func TestMameletResumesAnAttemptCutShort(t *testing.T) {
	n := newNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	n.a.System.Runner = &cancelOn{inner: n.sys, verb: "try-restart", cancel: cancel}
	f := grant("/etc/app.conf", "x\n", opA, 1)
	f.Write.Restart = []string{"app"}
	n.offer(Part{Files: []File{f}}, nil)
	rep, err := n.a.Round(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(rep.Results, func(r noderev.Result) bool { return r.Operation == opA }) {
		t.Fatalf("the attempt cut short was recorded: %+v", rep.Results)
	}
	rep = n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "already as approved") || !strings.Contains(r.Detail, "restarted app") {
		t.Errorf("result %+v", r)
	}
	if !slices.Equal(n.sys.commands, []string{"try-restart -- app"}) {
		t.Errorf("ran %q", n.sys.commands)
	}
	if fs := observedOf(t, rep).file("/etc/app.conf"); fs.Pending != nil {
		t.Errorf("still pending: %+v", fs.Pending)
	}
}

// M4: a write whose restart failed is reported pending (the file is as approved, the service does
// not use it yet) until a new grant restarts it; the file is not written again.
func TestMameletKeepsARestartThatFailedPending(t *testing.T) {
	n := newNode(t)
	n.sys.fail = "try-restart"
	f := grant("/etc/systemd/system/app.service", "[Service]\nExecStart=/bin/true\n", opA, 1)
	f.Write.Restart = []string{"app"}
	n.offer(Part{Files: []File{f}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "is pending") {
		t.Fatalf("result %+v", r)
	}
	want := &Pending{SHA256: f.Write.SHA256, Restart: []string{"app"}}
	got := observedOf(t, rep).file(f.Path).Pending
	if got == nil || got.SHA256 != want.SHA256 || got.Reload || !slices.Equal(got.Restart, want.Restart) {
		t.Fatalf("pending %+v", got)
	}
	// Still pending in a later round (the same attempt is not made again).
	if got := observedOf(t, n.round()).file(f.Path).Pending; got == nil {
		t.Error("forgotten")
	}
	n.sys.fail = ""
	f.Write.Grant.Operation = opB
	n.offer(Part{Files: []File{f}}, nil)
	rep = n.round()
	if r := result(t, rep, opB); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "already as approved") {
		t.Errorf("result %+v", r)
	}
	if !slices.Equal(n.sys.commands, []string{"daemon-reload", "try-restart -- app"}) {
		t.Errorf("ran %q", n.sys.commands)
	}
	if got := observedOf(t, rep).file(f.Path).Pending; got != nil {
		t.Errorf("still pending: %+v", got)
	}
	if n.exists("/var/lib/mamelet/" + pendingName) {
		t.Error("the record of what is pending is left")
	}
}

// M4: a pending restart is forgotten once the file changed (the next write restarts anyway), and is
// not reported for a file the part no longer names.
func TestPendingIsForgottenWhenTheFileChanges(t *testing.T) {
	n := newNode(t)
	n.sys.fail = "try-restart"
	f := grant("/etc/app.conf", "x\n", opA, 1)
	f.Write.Restart = []string{"app"}
	n.offer(Part{Files: []File{f}}, nil)
	n.round()
	write(t, filepath.Join(n.root, "etc/app.conf"), "changed by hand\n")
	if got := observedOf(t, n.round()).file(f.Path).Pending; got != nil {
		t.Errorf("pending %+v", got)
	}
	if n.exists("/var/lib/mamelet/" + pendingName) {
		t.Error("the record is left")
	}
}

// H1 (graph): a file or an artifact in a declared user's home, a home in another's, a home that is a
// file's path: refused on one node (and not across nodes).
func TestGraphRefusesWhatIsInAHome(t *testing.T) {
	ok := `"url":"https://example.com/tool","sha256":"` + sumA + `"`
	for _, c := range []struct {
		name string
		res  []string
		says string
	}{
		{"a file in a home", []string{userRes("web/user/app", `{"node":"web","name":"app"}`), fileRes("web/file/home/app/.bashrc", `{"node":"web","path":"/home/app/.bashrc","content":""}`)},
			"NodeFile web/file/home/app/.bashrc declares /home/app/.bashrc on web, in the home /home/app of user app"},
		{"an artifact in a declared home", []string{userRes("web/user/app", `{"node":"web","name":"app","home":"/srv/app"}`), artifactRes("web/artifact/srv/app/bin/tool", `{"node":"web","path":"/srv/app/bin/tool",`+ok+`}`)},
			"in the home /srv/app of user app"},
		{"a home that is a file", []string{userRes("web/user/app", `{"node":"web","name":"app","home":"/srv/app"}`), fileRes("web/file/srv/app", `{"node":"web","path":"/srv/app","content":""}`)},
			"in the home /srv/app"},
		{"a home under a file", []string{userRes("web/user/app", `{"node":"web","name":"app","home":"/srv/app/home"}`), fileRes("web/file/srv/app", `{"node":"web","path":"/srv/app","content":""}`)},
			"the home /srv/app/home of user app is under /srv/app, which NodeFile web/file/srv/app declares as a file"},
		{"a home in another's", []string{userRes("web/user/app", `{"node":"web","name":"app"}`), userRes("web/user/sub", `{"node":"web","name":"sub","home":"/home/app/sub"}`)},
			"home /home/app/sub is in home /home/app on web"},
	} {
		_, err := decodeGraph(c.res...)
		if err == nil || !strings.Contains(err.Error(), string(graph.CodeConflict)) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// A system user without a home made claims none; homes side by side are fine.
	if _, err := decodeGraph(userRes("web/user/svc", `{"node":"web","name":"svc","system":true}`), fileRes("web/file/home/svc/x", `{"node":"web","path":"/home/svc/x","content":""}`),
		userRes("web/user/a", `{"node":"web","name":"a"}`), userRes("web/user/ab", `{"node":"web","name":"ab"}`)); err != nil {
		t.Errorf("%v", err)
	}
}

// M1 (graph): no file or artifact restarts mamelet.
func TestGraphRefusesRestartingMamelet(t *testing.T) {
	ok := `"url":"https://example.com/tool","sha256":"` + sumA + `"`
	for _, res := range []string{
		fileRes("web/file/etc/app.conf", `{"node":"web","path":"/etc/app.conf","content":"","restart":["mamelet"]}`),
		fileRes("web/file/etc/app.conf", `{"node":"web","path":"/etc/app.conf","content":"","restart":["mamelet.service"]}`),
		artifactRes("web/artifact/opt/tool", `{"node":"web","path":"/opt/tool",`+ok+`,"restart":["mamelet"]}`),
	} {
		if _, err := decodeGraph(res); err == nil || !strings.Contains(err.Error(), "mamelet does not restart itself") {
			t.Errorf("%v", err)
		}
	}
	w := grant("/etc/app.conf", "x\n", opA, 1)
	w.Write.Restart = []string{"mamelet"}
	if err := checkPart(encode(Part{Files: []File{w}})); err == nil || !strings.Contains(err.Error(), "mamelet does not restart itself") {
		t.Errorf("mamelet took %v", err)
	}
}

// M3 (graph): more of a kind on one node than mamelet observes is refused.
func TestGraphBoundsTheCountsPerNode(t *testing.T) {
	var files []string
	for i := range MaxNodeFiles + 1 {
		files = append(files, fileRes(fmt.Sprintf("web/file/etc/app/%d", i), fmt.Sprintf(`{"node":"web","path":"/etc/app/%d","content":""}`, i)))
	}
	if _, err := decodeGraph(files...); err == nil || !strings.Contains(err.Error(), "more than 256 NodeFile are declared on web") {
		t.Errorf("%.300v", err)
	}
	if _, err := decodeGraph(files[:MaxNodeFiles]...); err != nil {
		t.Errorf("%.300v", err)
	}
}

// M1: what changes what mamelet runs, or every service, is not a file to write; and every program
// mamelet runs is refused, with its merged-/usr name.
// What the programs mamelet runs as root (useradd, apt-get, dpkg and the packages' scripts) run or
// trust without the approver seeing it in the file: each refused, saying why and what to do instead.
func TestCheckPathProtectsWhatRootRunsAndTrusts(t *testing.T) {
	for p, why := range map[string]string{
		// A program that runs by name: in the packages' directories, any (it replaces one, or shadows
		// one of a later directory of dpkg's PATH); in /usr/local, those root runs.
		"/usr/sbin/systemctl":                "packages' programs",
		"/usr/sbin/dpkg-deb":                 "packages' programs",
		"/usr/sbin/tar":                      "packages' programs",
		"/sbin/sed":                          "packages' programs",
		"/usr/bin/newtool":                   "packages' programs",
		"/usr/local/sbin/systemctl":          "first in the PATH",
		"/usr/local/bin/dpkg":                "first in the PATH",
		"/usr/local/sbin/start-stop-daemon":  "first in the PATH",
		"/usr/local/bin/sh":                  "first in the PATH",
		"/usr/local/bin/update-alternatives": "first in the PATH",
		"/usr/local/sbin/useradd":            "first in the PATH",
		"/usr/local/bin/sudo":                "first in the PATH",
		// useradd and usermod (and adduser, which the packages' scripts run).
		"/etc/shadow-maint/useradd-post.d/x": "useradd and usermod run",
		"/etc/default/useradd":               "SKEL",
		"/etc/login.defs":                    "uid ranges",
		"/etc/adduser.conf":                  "adduser",
		"/usr/local/sbin/adduser.local":      "adduser",
		// The dynamic linker, the name service, the libraries and modules.
		"/etc/ld.so.cache":                                 "dynamic linker",
		"/etc/nsswitch.conf":                               "modules",
		"/lib64/ld-linux-x86-64.so.2":                      "system's libraries",
		"/usr/lib/x86_64-linux-gnu/libc.so.6":              "system's libraries",
		"/lib/aarch64-linux-gnu/security/pam_unix.so":      "system's libraries",
		"/usr/lib/x86_64-linux-gnu/gconv/gconv-modules":    "system's libraries",
		"/usr/local/lib/x86_64-linux-gnu/perl/5.36.0/x.pm": "system's libraries",
		"/usr/local/lib/libc.so.6":                         "shared object",
		"/usr/lib/libfoo.so":                               "shared object",
		"/usr/lib/python3/dist-packages/apt_pkg.so":        "interpreters apt and dpkg run",
		"/usr/lib/python3/dist-packages/apt/x.py":          "interpreters apt and dpkg run",
		"/usr/local/lib/python3.12/dist-packages/apt.py":   "interpreters apt and dpkg run",
		"/usr/local/share/perl/5.36.0/Debconf/Db.pm":       "interpreters apt and dpkg run",
		"/etc/perl/Debconf/Db.pm":                          "interpreters apt and dpkg run",
		"/usr/share/perl5/Debconf/Db.pm":                   "interpreters apt and dpkg run",
		"/usr/share/debconf/confmodule":                    "debconf",
		"/usr/lib/dpkg/methods/x":                          "dpkg runs",
		// What APT trusts: sources only through NodeAptRepo, keys, pinning.
		"/etc/apt/sources.list":                          "Signed-By",
		"/etc/apt/sources.list.d/docker.list":            "Signed-By",
		"/etc/apt/sources.list.d/docker.sources":         "Signed-By",
		"/etc/apt/trusted.gpg":                           "every source",
		"/etc/apt/trusted.gpg.d/docker.gpg":              "every source",
		"/etc/apt/keyrings/docker.asc":                   "keys APT trusts",
		"/usr/share/keyrings/ubuntu-archive-keyring.gpg": "archive key",
		"/etc/apt/preferences":                           "pinning",
		"/etc/apt/preferences.d/pin":                     "pinning",
		// The slices every service runs in.
		"/etc/systemd/system/system.slice.d/limits.conf": "slice every service runs in",
		"/etc/systemd/system/-.slice.d/x.conf":           "slice every service runs in",
		"/usr/lib/systemd/system/slice.d/x.conf":         "slice every service runs in",
		"/etc/systemd/system/system.slice":               "slice every service runs in",
	} {
		err := CheckPath(p)
		if err == nil || !strings.Contains(err.Message, why) {
			t.Errorf("%s: %v, want it to say %q", p, err, why)
			continue
		}
		if err.Hint == "" && !strings.Contains(p, "shadow-maint") && !strings.Contains(p, "debconf") && !strings.Contains(p, "dpkg/") && !strings.Contains(p, "adduser.local") {
			t.Errorf("%s: no hint", p)
		}
	}
	// Every program mamelet runs, under any directory of a PATH.
	for _, p := range mamelet.Programs() {
		for _, d := range pathDirs {
			if q := d + "/" + filepath.Base(p); CheckPath(q) == nil {
				t.Errorf("%s may be written", q)
			}
		}
	}
}

func TestCheckPathProtectsMamelet(t *testing.T) {
	for p, why := range map[string]string{
		"/etc/systemd/system/mamelet.service.d/override.conf":         "mamelet's own unit",
		"/usr/lib/systemd/system/mamelet.service":                     "mamelet's own unit",
		"/lib/systemd/system/mamelet.service.d/x.conf":                "mamelet's own unit",
		"/etc/systemd/system.control/mamelet.service.d/50-x.conf":     "mamelet's own unit",
		"/etc/systemd/system.attached/mamelet.service":                "mamelet's own unit",
		"/etc/systemd/system/multi-user.target.wants/mamelet.service": "mamelet's own unit",
		"/usr/local/lib/systemd/system/k3s.service.d/x.conf":          "k3s unit",
		"/etc/systemd/system/service.d/env.conf":                      "every service",
		"/usr/lib/systemd/system/service.d/env.conf":                  "every service",
		"/etc/systemd/system.conf":                                    "every service",
		"/etc/systemd/system.conf.d/env.conf":                         "every service",
		"/usr/lib/systemd/system-generators/x":                        "generator",
		"/etc/systemd/system-environment-generators/x":                "environment",
		"/etc/ld.so.preload":                                          "dynamic linker",
		"/etc/ld.so.conf":                                             "libraries",
		"/etc/ld.so.conf.d/x.conf":                                    "libraries",
		"/etc/apt/apt.conf.d/99hook":                                  "Pre-Invoke",
		"/etc/apt/apt.conf":                                           "Pre-Invoke",
		"/etc/dpkg/dpkg.cfg.d/hook":                                   "pre-invoke",
		"/usr/lib/apt/methods/https":                                  "apt-get runs",
		"/tmp/app.conf":                                               "every user",
		"/var/tmp/x":                                                  "every user",
		"/usr/bin/systemctl":                                          "programs mamelet runs",
		"/bin/systemctl":                                              "programs mamelet runs",
		"/usr/bin/dpkg":                                               "programs mamelet runs",
		"/bin/dpkg":                                                   "programs mamelet runs",
		"/usr/sbin/useradd":                                           "programs mamelet runs",
		"/sbin/usermod":                                               "programs mamelet runs",
		"/bin/apt-get":                                                "programs mamelet runs",
		"/usr/bin/dpkg-query":                                         "programs mamelet runs",
		"/etc/sudoers.d/app":                                          "become root",
		"/etc/systemd/system/multi-user.target.wants/k3s.service":     "k3s unit",
	} {
		err := CheckPath(p)
		if err == nil || !strings.Contains(err.Message, why) {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range mamelet.Programs() {
		if CheckPath(p) == nil {
			t.Errorf("%s, a program mamelet runs, may be written", p)
		}
	}
	if e := CheckPath("/etc/sudoers.d/app"); !strings.Contains(e.Hint, "not supported yet") {
		t.Errorf("sudoers hint: %q", e.Hint)
	}
	for _, p := range []string{"/etc/systemd/system/app.service.d/override.conf", "/etc/systemd/system/mameletx.service", "/usr/local/bin/app", "/srv/tmp/x"} {
		if err := CheckPath(p); err != nil {
			t.Errorf("%s refused: %s", p, err.Message)
		}
	}
	if !IsUnitFile("/etc/systemd/system.control/app.service.d/50-x.conf") {
		t.Error("a file of system.control is not taken as a unit")
	}
}

// A record of the artifacts that cannot be read is set aside, not written over with the one artifact
// installed next (which would lose what it said of the others); the report says so while it is there,
// and the plan of an artifact it may have named says why it is taken as not installed by Mamela.
func TestAnUnreadableRecordOfArtifactsIsSetAside(t *testing.T) {
	rel := newReleases(t)
	sum := rel.put("/tool", []byte("tool"))
	n := artifactNode(t, rel)
	state := filepath.Join(n.root, "var/lib/mamelet")
	write(t, filepath.Join(state, recordName), `{"artifacts":{"/usr/local/bin/other":{"url":"https://x`)
	write(t, filepath.Join(n.root, "usr/local/bin/other"), "other")
	n.offer(Part{Artifacts: []Artifact{install("/usr/local/bin/tool", rel.url("/tool"), sum, "", opA, 1), {Path: "/usr/local/bin/other", Owners: []string{"root"}, Groups: []string{"root"}}}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone {
		t.Fatalf("result %+v", r)
	}
	if b, err := os.ReadFile(filepath.Join(state, recordName+corruptSuffix)); err != nil || !strings.Contains(string(b), "/usr/local/bin/other") {
		t.Errorf("set aside: %q %v", b, err)
	}
	if rec := n.record(); len(rec.Artifacts) != 1 || rec.Artifacts["/usr/local/bin/tool"].Installed != sum {
		t.Errorf("record %+v", rec)
	}
	o := observedOf(t, rep)
	if !slices.Equal(o.SetAside, []string{recordName + corruptSuffix}) || o.artifact("/usr/local/bin/other").From != nil {
		t.Errorf("observed %+v %+v", o.SetAside, o.artifact("/usr/local/bin/other"))
	}
	// Still said while it is there; no more once a person removed it.
	if o := observedOf(t, n.round()); len(o.SetAside) != 1 {
		t.Errorf("forgotten: %+v", o.SetAside)
	}
	os.Remove(filepath.Join(state, recordName+corruptSuffix))
	if o := observedOf(t, n.round()); len(o.SetAside) != 0 {
		t.Errorf("still said: %+v", o.SetAside)
	}
}

// A record of what is pending that cannot be read is set aside, and what it may have said is not
// forgotten: each file found is reported as not known whether what follows its write finished, and
// the plan asks to approve that again (a unit's daemon-reload, the declared restarts); approving it
// does it, without writing the file again.
func TestAnUnreadableRecordOfWhatIsPendingIsNotForgotten(t *testing.T) {
	n := newNode(t)
	unit := "[Service]\nExecStart=/bin/true\n"
	write(t, filepath.Join(n.root, "etc/systemd/system/app.service"), unit)
	os.Chmod(filepath.Join(n.root, "etc/systemd/system/app.service"), 0o640)
	state := filepath.Join(n.root, "var/lib/mamelet")
	write(t, filepath.Join(state, pendingName), "{not json")
	entry := File{Path: "/etc/systemd/system/app.service", Owners: []string{"root"}, Groups: []string{"root"}}
	n.offer(Part{Files: []File{entry}}, nil)
	o := observedOf(t, n.round())
	got := o.file(entry.Path).Pending
	if got == nil || !got.Unknown || !got.Reload || got.SHA256 != ContentSHA256(unit) || !slices.Equal(o.SetAside, []string{pendingName + corruptSuffix}) {
		t.Fatalf("pending %+v, set aside %q", got, o.SetAside)
	}
	if b, _ := os.ReadFile(filepath.Join(state, pendingName+corruptSuffix)); string(b) != "{not json" {
		t.Errorf("set aside: %q", b)
	}
	// Reported again in the next round (the record says so now).
	if got := observedOf(t, n.round()).file(entry.Path).Pending; got == nil || !got.Unknown {
		t.Fatalf("forgotten: %+v", got)
	}
	// What the plan asks for: the declared restarts, and the unit's daemon-reload.
	left := pendingFor(got, ContentSHA256(unit), entry.Path, []string{"app"})
	if left == nil || !left.Unknown || !left.Reload || !slices.Equal(left.Restart, []string{"app"}) {
		t.Errorf("asked for %+v", left)
	}
	if pendingFor(&Pending{SHA256: sumA, Unknown: true}, sumA, "/etc/app.conf", nil) != nil {
		t.Error("a file nothing follows is asked for")
	}
	// The approval: the file is as approved, what follows it runs, and nothing is pending any more.
	f := grant(entry.Path, unit, opA, 1)
	f.Write.Restart = []string{"app"}
	f.Write.Before = Before{Exists: true, SHA256: ContentSHA256(unit)}
	n.offer(Part{Files: []File{f}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "already as approved") {
		t.Errorf("result %+v", r)
	}
	if !slices.Equal(n.sys.commands, []string{"daemon-reload", "try-restart -- app"}) {
		t.Errorf("ran %q", n.sys.commands)
	}
	if got := observedOf(t, rep).file(entry.Path).Pending; got != nil {
		t.Errorf("still pending: %+v", got)
	}
}

// What follows a write is recorded before the file is replaced (mamelet killed, or the power lost,
// between the two would forget it otherwise), and put back as it was when the write fails.
func TestWhatFollowsAWriteIsRecordedBeforeIt(t *testing.T) {
	n := newNode(t)
	f := grant("/etc/app.conf", "x\n", opA, 1)
	f.Write.Restart = []string{"app"}
	state := filepath.Join(n.root, "var/lib/mamelet")
	seen := false
	testHookOpenLeaf = func(p string) {
		// The look after the write: the file is replaced, and its restart is recorded already.
		if p != f.Path || !n.exists("/etc/app.conf") || n.read("/etc/app.conf") != "x\n" || seen {
			return
		}
		seen = true
		rec, err := loadPending(state)
		if left, ok := rec.Pending[f.Path]; err != nil || !ok || left.SHA256 != f.Write.SHA256 || !slices.Equal(left.Restart, []string{"app"}) {
			t.Errorf("when the file was replaced, the record was %+v %v", rec, err)
		}
	}
	t.Cleanup(func() { testHookOpenLeaf = nil })
	n.offer(Part{Files: []File{f}}, nil)
	if r := result(t, n.round(), opA); r.Outcome != noderev.OutcomeDone || !seen {
		t.Fatalf("result %+v, seen %v", r, seen)
	}
	// A write that fails (no room) leaves the record as it was.
	r, _ := os.OpenRoot(t.TempDir())
	defer r.Close()
	if freeBytes(must(r.Open("."))) < 0 {
		t.Skip("this system does not say how much is free")
	}
	old := DiskMargin
	DiskMargin = 1 << 62
	t.Cleanup(func() { DiskMargin = old })
	g := grant("/etc/other.conf", "y\n", opB, 1)
	g.Write.Restart = []string{"other"}
	n.offer(Part{Files: []File{g}}, nil)
	if r := result(t, n.round(), opB); r.Outcome != noderev.OutcomeFailed {
		t.Fatalf("result %+v", r)
	}
	if rec, err := loadPending(state); err != nil || len(rec.Pending) != 0 {
		t.Errorf("the record is %+v %v", rec, err)
	}
}
