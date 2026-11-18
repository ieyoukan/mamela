//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// What mamelet opens does not become its controlling terminal (O_NOCTTY): mamelet runs as a service,
// a session leader without one, and a terminal a user led it to would have sent it (and apt and dpkg
// under it) the user's SIGINT and SIGHUP. A child in a session of its own opens a terminal as
// openLeaf opens a file; then it has no controlling terminal. The same child opening the terminal
// without O_NOCTTY gets one: what shows the test can tell.
func TestOpenLeafTakesNoControllingTerminal(t *testing.T) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	defer ptmx.Close()
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("unlockpt: %v", err)
	}
	n, err := unix.IoctlGetUint32(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("ptsname: %v", err)
	}
	pts := "/dev/pts/" + strconv.Itoa(int(n))
	run := func(how string) int {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperOpenTerminal$")
		cmd.Env = append(os.Environ(), "MAMELA_TEST_TTY="+pts, "MAMELA_TEST_TTY_HOW="+how)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("%s: %v %s", how, err, out)
		}
		t.Logf("%s: %s", how, out)
		return cmd.ProcessState.ExitCode()
	}
	if run("plain") != 3 {
		t.Skip("a session leader here does not take a terminal it opens as its own: nothing to tell apart")
	}
	if code := run("leaf"); code != 0 {
		t.Errorf("opening the terminal as openLeaf does made it the controlling terminal (exit %d)", code)
	}
}

// TestHelperOpenTerminal is the child of TestOpenLeafTakesNoControllingTerminal: it opens the
// terminal (as openLeafAt does, or plainly), and exits 3 if it then has a controlling terminal.
func TestHelperOpenTerminal(t *testing.T) {
	pts := os.Getenv("MAMELA_TEST_TTY")
	if pts == "" {
		t.Skip("run by TestOpenLeafTakesNoControllingTerminal")
	}
	var fd int
	var err error
	if os.Getenv("MAMELA_TEST_TTY_HOW") == "leaf" {
		dir, derr := unix.Open(filepath.Dir(pts), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if derr != nil {
			fmt.Println(derr)
			os.Exit(2)
		}
		fd, err = openLeafAt(dir, filepath.Base(pts))
	} else {
		fd, err = unix.Open(pts, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(2)
	}
	defer unix.Close(fd)
	if tty, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NOCTTY, 0); err == nil {
		unix.Close(tty)
		fmt.Println("has a controlling terminal")
		os.Exit(3)
	}
	fmt.Println("no controlling terminal")
	os.Exit(0)
}
