//go:build unix

package evaluate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
)

// A named pipe is refused before it is opened: evaluating does not wait on it.
func TestANamedPipeIsNotWaitedOn(t *testing.T) {
	dir := pomeDir(t, map[string]string{})
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := decodeOnly(0).decode(pomeOutput(`{"contentFrom":"pipe","node":"web","path":"/etc/app/app.conf"}`, ""), dir)
		done <- err
	}()
	select {
	case err := <-done:
		var ge *graph.Error
		if !errors.As(err, &ge) || !ge.Has(graph.Code(CodeFileNotRegular)) || !strings.Contains(err.Error(), "pipe is a named pipe") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("evaluating waits on a named pipe")
	}
	// Opened directly, it would block; the open itself does not wait either (O_NONBLOCK).
	f, err := newPomeFiles(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	opened := make(chan error, 1)
	go func() {
		file, err := f.root.OpenFile("pipe", os.O_RDONLY|openFlags, 0)
		if err == nil {
			file.Close()
		}
		opened <- err
	}()
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("opening a named pipe waits")
	}
}
