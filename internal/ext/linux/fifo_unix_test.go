//go:build linux || darwin

package linux

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// What a user who owns a directory on a path mamelet walks (its home, its ~/.ssh) can put in the
// directory's place between mamelet's look at it and its opening (renameat2 with RENAME_EXCHANGE, in
// a loop): a pipe, which a plain open(2) waits on for ever (mamelet stops, SIGTERM does not end it,
// and every grant of the node waits); a socket; a device, whose opening alone may do something.
// mamelet opens a directory as a directory only (O_DIRECTORY|O_NONBLOCK|O_NOFOLLOW|O_NOCTTY): it
// refuses each before opening it.

// putInPlace makes what replaces the directory at full (root: only root can make it).
type putInPlace struct {
	what string
	root bool
	make func(t *testing.T, full string)
}

// skipUnlessCan skips a test of r where the test cannot make it.
func skipUnlessCan(t *testing.T, r putInPlace) {
	if r.root && os.Geteuid() != 0 {
		t.Skip("making " + r.what + " needs root")
	}
}

var notDirectories = []putInPlace{
	{"a pipe", false, func(t *testing.T, full string) {
		if err := unix.Mkfifo(full, 0o600); err != nil {
			t.Error(err)
		}
	}},
	{"a socket", false, func(t *testing.T, full string) {
		// Bound in a short directory (the length of a socket's path is bounded), then moved in place.
		dir, err := os.MkdirTemp("/tmp", "s")
		if err != nil {
			t.Error(err)
			return
		}
		t.Cleanup(func() { os.Remove(dir) })
		sock := filepath.Join(dir, "s")
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Error(err)
			return
		}
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		t.Cleanup(func() { l.Close() })
		if err := os.Rename(sock, full); err != nil {
			t.Error(err)
		}
	}},
	{"a device", true, func(t *testing.T, full string) {
		// The device of /dev/null.
		if err := unix.Mknod(full, unix.S_IFCHR|0o600, int(unix.Mkdev(1, 3))); err != nil {
			t.Error(err)
		}
	}},
}

// swapDir puts what r makes in place of the directory at p of the node (root) when mamelet is about
// to open it, as a user racing mamelet would; the directory itself is moved aside.
func swapDir(t *testing.T, root, p string, r putInPlace) {
	t.Helper()
	testHookOpenSubdir = func(q string) {
		if q != p {
			return
		}
		testHookOpenSubdir = nil
		full := filepath.Join(root, p)
		if err := os.Rename(full, full+".old"); err != nil {
			t.Error(err)
			return
		}
		r.make(t, full)
	}
	t.Cleanup(func() { testHookOpenSubdir = nil })
}

// within runs f, and fails the test when it has not returned within 10 seconds (mamelet held by
// what was put in place of a directory): a pipe there is then opened for writing, which lets the
// opening that waits on it return, so that the test ends.
func within(t *testing.T, full string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		if fd, err := unix.Open(full, unix.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			unix.Close(fd)
		}
		<-done
		t.Fatalf("mamelet waited on what was put in place of %s", full)
	}
}

// Observing (inspect, and readKeys: openDir) is not held, and says the path is not safe.
func TestObservingRefusesWhatIsPutInPlaceOfADirectory(t *testing.T) {
	for _, r := range notDirectories {
		t.Run(r.what, func(t *testing.T) {
			skipUnlessCan(t, r)
			n := newNode(t)
			write(t, filepath.Join(n.root, "home/app/.ssh/authorized_keys"), keyAlice+"\n")
			tree, err := os.OpenRoot(n.root)
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Close()
			a := readAccounts(tree)
			swapDir(t, n.root, "/home/app/.ssh", r)
			var st FileState
			within(t, filepath.Join(n.root, "home/app/.ssh"), func() { st = inspect(tree, "/home/app/.ssh/authorized_keys", a) })
			if !strings.Contains(st.Unsafe, "/home/app/.ssh changed while mamelet opened it") || st.SHA256 != "" {
				t.Errorf("observed %+v", st)
			}
			swapDir(t, n.root, "/home/app/.ssh", r)
			os.Remove(filepath.Join(n.root, "home/app/.ssh"))
			os.Rename(filepath.Join(n.root, "home/app/.ssh.old"), filepath.Join(n.root, "home/app/.ssh"))
			var rerr error
			within(t, filepath.Join(n.root, "home/app/.ssh"), func() { _, rerr = readKeys(tree, "/home/app") })
			if rerr == nil || !strings.Contains(rerr.Error(), "changed while mamelet opened it") {
				t.Errorf("read: %v", rerr)
			}
		})
	}
}

// Writing a file (walkDirs) is not held either, and writes nothing.
func TestWritingRefusesWhatIsPutInPlaceOfADirectory(t *testing.T) {
	for _, r := range notDirectories {
		t.Run(r.what, func(t *testing.T) {
			skipUnlessCan(t, r)
			n := newNode(t)
			if err := os.MkdirAll(filepath.Join(n.root, "srv/app"), 0o755); err != nil {
				t.Fatal(err)
			}
			tree, err := os.OpenRoot(n.root)
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Close()
			swapDir(t, n.root, "/srv/app", r)
			content := "x\n"
			var werr error
			within(t, filepath.Join(n.root, "srv/app"), func() {
				werr = writeTo(tree, "/srv/app/app.conf", Write{Content: content, SHA256: ContentSHA256(content), Mode: "0644"}, os.Getuid(), os.Getgid(), os.Getuid(), os.Getgid())
			})
			if werr == nil || !strings.Contains(werr.Error(), "/srv/app changed while mamelet opened it") {
				t.Errorf("write: %v", werr)
			}
			if _, err := os.Lstat(filepath.Join(n.root, "srv/app.old/app.conf")); err == nil {
				t.Error("the file was written into the directory moved aside")
			}
		})
	}
}

// A user's ~/.ssh swapped while mamelet writes its keys (writeKeys): refused, nothing written.
func TestWritingKeysRefusesWhatIsPutInPlaceOfSSH(t *testing.T) {
	for _, r := range notDirectories {
		t.Run(r.what, func(t *testing.T) {
			skipUnlessCan(t, r)
			n, _ := newUserNode(t)
			addUser(t, n, appLine)
			write(t, filepath.Join(n.root, "home/app/.ssh/authorized_keys"), keyBob+"\n")
			tree, err := os.OpenRoot(n.root)
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Close()
			a := readAccounts(tree)
			swapDir(t, n.root, "/home/app/.ssh", r)
			var werr error
			within(t, filepath.Join(n.root, "home/app/.ssh"), func() {
				_, werr = writeKeys(tree, a, "app", "/home/app", KeysContent([]string{keyAlice}), os.Getuid(), os.Getgid())
			})
			if werr == nil || !strings.Contains(werr.Error(), "/home/app/.ssh changed while mamelet opened it") {
				t.Errorf("write: %v", werr)
			}
			if b, err := os.ReadFile(filepath.Join(n.root, "home/app/.ssh.old/authorized_keys")); err != nil || string(b) != keyBob+"\n" {
				t.Errorf("bob's key: %q %v", b, err)
			}
		})
	}
}

// A whole round, with a pipe put in place of ~/.ssh as mamelet first opens it: the round ends, and
// the report says why the keys were not observed.
func TestARoundIsNotHeldByAPipeInPlaceOfSSH(t *testing.T) {
	n, _ := newUserNode(t)
	addUser(t, n, appLine)
	write(t, filepath.Join(n.root, "home/app/.ssh/authorized_keys"), keyBob+"\n")
	n.offer(Part{Users: []User{{Name: "app", Shells: []string{DefaultShell}, Keys: true}}}, nil)
	swapDir(t, n.root, "/home/app/.ssh", notDirectories[0])
	var rep noderev.Report
	within(t, filepath.Join(n.root, "home/app/.ssh"), func() { rep = n.round() })
	if ks := userStateOf(t, observedOf(t, rep), "app").Keys; ks == nil || ks.File.Unsafe == "" {
		t.Errorf("observed %+v", ks)
	}
}
