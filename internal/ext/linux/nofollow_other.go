//go:build !linux && !darwin

package linux

import (
	"io/fs"
	"os"
	"path"
)

// mamelet runs on Linux (nofollow_unix.go); elsewhere (the Control Plane's tools built for another
// system, and their tests) the same is done with os.Root: what is opened is checked to be what was
// looked at, though the opening itself may follow a link within the directory (or wait on a pipe).

type dirFD struct {
	root *os.Root
	path string
}

func treeDir(r *os.Root) (*dirFD, error) {
	c, err := r.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	return &dirFD{root: c, path: "/"}, nil
}

func (d *dirFD) Close() error { return d.root.Close() }

func (d *dirFD) child(name string) string { return path.Join(d.path, name) }

func (d *dirFD) subdir(name string) (*dirFD, error) {
	p := d.child(name)
	fi, err := d.root.Lstat(name)
	switch {
	case err != nil:
		return nil, err
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, &unsafeError{msg: p + " is a symbolic link"}
	case !fi.IsDir():
		return nil, &unsafeError{msg: p + " is not a directory"}
	}
	if testHookOpenSubdir != nil {
		testHookOpenSubdir(p)
	}
	next, err := d.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if got, err := next.Stat("."); err != nil || !os.SameFile(fi, got) {
		next.Close()
		return nil, &unsafeError{msg: p + " changed while mamelet opened it (a link put in its place?)"}
	}
	return &dirFD{root: next, path: p}, nil
}

func (d *dirFD) lstat(name string) (fs.FileInfo, error) { return d.root.Lstat(name) }
func (d *dirFD) stat() (fs.FileInfo, error)             { return d.root.Stat(".") }
func (d *dirFD) mkdir(name string, perm fs.FileMode) error {
	return d.root.Mkdir(name, perm)
}
func (d *dirFD) remove(name string) error             { return d.root.Remove(name) }
func (d *dirFD) rename(oldname, newname string) error { return d.root.Rename(oldname, newname) }
func (d *dirFD) create(name string, perm fs.FileMode) (*os.File, error) {
	return d.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}
func (d *dirFD) chown(uid, gid int) error {
	uid, gid = testIDs.swap(uid, gid)
	return d.root.Chown(".", uid, gid)
}
func (d *dirFD) chmod(mode fs.FileMode) error        { return d.root.Chmod(".", mode) }
func (d *dirFD) free() int64                         { return -1 }
func (d *dirFD) madeDir(name string) (*dirFD, error) { return d.subdir(name) }

func (d *dirFD) sync() error {
	f, err := d.root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (d *dirFD) openLeaf(name, p string) (*os.File, fs.FileInfo, fileStamp, error) {
	before, err := d.root.Lstat(name)
	if err != nil {
		return nil, nil, fileStamp{}, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fileStamp{}, &unsafeError{msg: p + whatIs(before.Mode()), leaf: true}
	}
	if testHookOpenLeaf != nil {
		testHookOpenLeaf(p)
	}
	f, err := d.root.OpenFile(name, os.O_RDONLY|openNonBlock, 0)
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

func freeBytesAt(string) int64 { return -1 }
func freeBytes(*os.File) int64 { return -1 }
