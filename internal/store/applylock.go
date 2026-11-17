package store

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// ErrApplyBusy is returned when another apply holds the database's apply lock.
var ErrApplyBusy = errors.New("another apply is running on this database")

// ApplyLock is the right to apply a plan to the world this database describes. Only one is held at a
// time, across the programs of the Control Plane (the CLI, mamela api): a plan is applied on what
// its apply observes first, and a plan applied while another is half done would observe a world
// that is about to change (two plans that both create the same machine, both having seen it missing).
type ApplyLock struct{ f *os.File }

// LockApply takes the apply lock, or returns ErrApplyBusy with who holds it ("owner plan", as the
// holder wrote it). The lock is a file next to the database that the operating system releases when
// its holder ends, however it ends (a kill -9 included), so a crashed apply never keeps a later one
// out.
func (s *Store) LockApply(owner, planDigest string) (*ApplyLock, string, error) {
	f, err := os.OpenFile(s.path+".apply.lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("store: the apply lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := ""
		if b, rerr := os.ReadFile(f.Name()); rerr == nil {
			holder = strings.TrimSpace(string(b))
		}
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, holder, ErrApplyBusy
		}
		return nil, "", fmt.Errorf("store: the apply lock: %w", err)
	}
	// Who holds it, for whoever finds it busy (the lock does not depend on this being written).
	if f.Truncate(0) == nil {
		fmt.Fprintf(f, "%s %s\n", owner, planDigest)
	}
	return &ApplyLock{f: f}, "", nil
}

// Release gives the lock up.
func (l *ApplyLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	l.f.Truncate(0)
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
}
