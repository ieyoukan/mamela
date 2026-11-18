//go:build !linux && !darwin

package linux

import (
	"errors"
	"io/fs"
	"os"
)

// mamelet runs on Linux (nofollow_unix.go); elsewhere (the Control Plane's tools built for another
// system, and their tests) the same is done with os.Root: what is opened is checked to be the
// regular file looked at, though the opening itself may follow a link within the directory.

func openLeaf(dir *os.Root, name, p string) (*os.File, fs.FileInfo, fileStamp, error) {
	before, err := dir.Lstat(name)
	if err != nil {
		return nil, nil, fileStamp{}, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fileStamp{}, &unsafeError{msg: p + whatIs(before.Mode()), leaf: true}
	}
	if testHookOpenLeaf != nil {
		testHookOpenLeaf(p)
	}
	f, err := dir.OpenFile(name, os.O_RDONLY|openNonBlock, 0)
	if err != nil {
		return nil, nil, fileStamp{}, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(before, fi) {
		f.Close()
		return nil, nil, fileStamp{}, &unsafeError{msg: p + " changed while mamelet opened it (it is not the file it looked at)", leaf: true}
	}
	return f, fi, fileStamp{}, nil
}

func whatIs(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return " is a symbolic link"
	case m.IsDir():
		return " is a directory"
	}
	return " is not a regular file (a device, a socket or a pipe)"
}

func openChildDir(parent *os.Root, name string) (*os.File, error) {
	fi, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return nil, &unsafeError{msg: name + " is a symbolic link or not a directory"}
	}
	f, err := parent.Open(name)
	if err != nil {
		return nil, err
	}
	if got, err := f.Stat(); err != nil || !os.SameFile(fi, got) {
		f.Close()
		return nil, errors.Join(&unsafeError{msg: name + " changed while mamelet opened it"}, err)
	}
	return f, nil
}

func freeBytes(*os.File) int64 { return -1 }
func freeBytesAt(string) int64 { return -1 }
