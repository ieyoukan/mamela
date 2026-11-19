//go:build linux || darwin

package linux

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"time"

	"golang.org/x/sys/unix"
)

// What mamelet opens of the node's files, on the systems it runs on: the system calls themselves
// (golang.org/x/sys/unix), because os.Root follows a symbolic link at the end of a name (within the
// root) whatever flags it is given, and opens a directory with a plain open of its name: a link that
// a user puts where a file was could then lead mamelet, root, to open a terminal (and take it as its
// controlling terminal) or a device whose opening alone does something (/dev/watchdog), and a pipe
// put where a directory was would hold mamelet in open(2) for ever (no SIGTERM ends it).

// dirFD is a directory mamelet opened by itself, one component at a time from the root (openDir,
// walkDirs): its descriptor, and its path on the node (for messages). Everything done in it is done
// relative to the descriptor (the *at system calls), never through its path.
type dirFD struct {
	fd   int
	path string
}

// treeDir is the root of the tree r (the node's "/").
func treeDir(r *os.Root) (*dirFD, error) {
	f, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var fd int
	err = ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(int(f.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: "/", Err: err}
	}
	return &dirFD{fd: fd, path: "/"}, nil
}

func (d *dirFD) Close() error { return unix.Close(d.fd) }

func (d *dirFD) child(name string) string { return path.Join(d.path, name) }

// subdir opens the directory name of d: it must be a directory, not a link, when it is looked at
// (fstatat, not following a link); it is then opened as a directory only (O_DIRECTORY), without
// following a link (O_NOFOLLOW), waiting for nothing (O_NONBLOCK) and taking no terminal (O_NOCTTY):
// a pipe, a device or a link a user put in its place meanwhile is refused (ENOTDIR, ELOOP) before
// it is opened, never waited on; and what was opened must be the directory looked at (its device
// and inode). A component that does not exist is an error that is fs.ErrNotExist; one that is a
// link or not a directory, an *unsafeError.
func (d *dirFD) subdir(name string) (*dirFD, error) {
	p := d.child(name)
	var before unix.Stat_t
	if err := ignoringEINTR(func() error { return unix.Fstatat(d.fd, name, &before, unix.AT_SYMLINK_NOFOLLOW) }); err != nil {
		return nil, &fs.PathError{Op: "lstat", Path: p, Err: err}
	}
	switch before.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
	case unix.S_IFLNK:
		return nil, &unsafeError{msg: p + " is a symbolic link"}
	default:
		return nil, &unsafeError{msg: p + " is not a directory"}
	}
	if testHookOpenSubdir != nil {
		testHookOpenSubdir(p)
	}
	var fd int
	err := ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		return err
	})
	switch {
	case errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP):
		return nil, &unsafeError{msg: p + " changed while mamelet opened it (a link, a pipe or a device put in its place?)"}
	case err != nil:
		return nil, &fs.PathError{Op: "open", Path: p, Err: err}
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		unix.Close(fd)
		return nil, &fs.PathError{Op: "fstat", Path: p, Err: err}
	}
	if after.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(after.Dev) != uint64(before.Dev) || after.Ino != before.Ino {
		unix.Close(fd)
		return nil, &unsafeError{msg: p + " changed while mamelet opened it (a link put in its place?)"}
	}
	return &dirFD{fd: fd, path: p}, nil
}

// lstat is what is at name in d (a link is not followed).
func (d *dirFD) lstat(name string) (fs.FileInfo, error) {
	var st unix.Stat_t
	if err := ignoringEINTR(func() error { return unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) }); err != nil {
		return nil, &fs.PathError{Op: "lstat", Path: d.child(name), Err: err}
	}
	return &statInfo{name: name, st: st}, nil
}

// stat is d itself.
func (d *dirFD) stat() (fs.FileInfo, error) {
	var st unix.Stat_t
	if err := ignoringEINTR(func() error { return unix.Fstat(d.fd, &st) }); err != nil {
		return nil, &fs.PathError{Op: "fstat", Path: d.path, Err: err}
	}
	return &statInfo{name: path.Base(d.path), st: st}, nil
}

func (d *dirFD) mkdir(name string, perm fs.FileMode) error {
	if err := ignoringEINTR(func() error { return unix.Mkdirat(d.fd, name, uint32(perm.Perm())) }); err != nil {
		return &fs.PathError{Op: "mkdir", Path: d.child(name), Err: err}
	}
	return nil
}

// remove removes the file name of d (not a directory; a link itself, never what it leads to).
func (d *dirFD) remove(name string) error {
	if err := ignoringEINTR(func() error { return unix.Unlinkat(d.fd, name, 0) }); err != nil {
		return &fs.PathError{Op: "remove", Path: d.child(name), Err: err}
	}
	return nil
}

// rename renames oldname of d to newname of d, replacing what is there (a link too: it is not
// followed).
func (d *dirFD) rename(oldname, newname string) error {
	if err := ignoringEINTR(func() error { return unix.Renameat(d.fd, oldname, d.fd, newname) }); err != nil {
		return &os.LinkError{Op: "rename", Old: d.child(oldname), New: d.child(newname), Err: err}
	}
	return nil
}

// create makes the new file name in d for writing (O_EXCL: never something that is there, a link
// included).
func (d *dirFD) create(name string, perm fs.FileMode) (*os.File, error) {
	var fd int
	err := ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(d.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NOCTTY|unix.O_CLOEXEC, uint32(perm.Perm()))
		return err
	})
	if err != nil {
		return nil, &fs.PathError{Op: "create", Path: d.child(name), Err: err}
	}
	return os.NewFile(uintptr(fd), d.child(name)), nil
}

func (d *dirFD) chown(uid, gid int) error {
	uid, gid = testIDs.swap(uid, gid)
	if err := ignoringEINTR(func() error { return unix.Fchown(d.fd, uid, gid) }); err != nil {
		return &fs.PathError{Op: "chown", Path: d.path, Err: err}
	}
	return nil
}

func (d *dirFD) chmod(mode fs.FileMode) error {
	if err := ignoringEINTR(func() error { return unix.Fchmod(d.fd, uint32(mode.Perm())) }); err != nil {
		return &fs.PathError{Op: "chmod", Path: d.path, Err: err}
	}
	return nil
}

func (d *dirFD) sync() error { return ignoringEINTR(func() error { return unix.Fsync(d.fd) }) }

// free is how many bytes of d's file system mamelet may still write (what is available to users:
// the blocks the file system keeps for root stay free); -1 when it cannot tell.
func (d *dirFD) free() int64 {
	var st unix.Statfs_t
	if err := ignoringEINTR(func() error { return unix.Fstatfs(d.fd, &st) }); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// madeDir opens the directory name of d that mamelet just made (O_DIRECTORY, O_NOFOLLOW): what
// mamelet changes of it (its owner, its mode) is changed through it, never through the name.
func (d *dirFD) madeDir(name string) (*dirFD, error) {
	var fd int
	err := ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, &unsafeError{msg: d.child(name) + " is a symbolic link or not a directory"}
		}
		return nil, &fs.PathError{Op: "open", Path: d.child(name), Err: err}
	}
	return &dirFD{fd: fd, path: d.child(name)}, nil
}

// openLeaf opens the regular file name in d for reading (p is its path, for messages): only if it
// is one when it is looked at (fstatat, not following a link), then without following a link
// (O_NOFOLLOW), taking no terminal (O_NOCTTY) and waiting for no writer (O_NONBLOCK), and only if
// what was opened is still a regular file and the one looked at (fstat of the descriptor). Its
// FileInfo and stamp are the descriptor's.
func (d *dirFD) openLeaf(name, p string) (*os.File, fs.FileInfo, fileStamp, error) {
	var before unix.Stat_t
	if err := ignoringEINTR(func() error { return unix.Fstatat(d.fd, name, &before, unix.AT_SYMLINK_NOFOLLOW) }); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil, fileStamp{}, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
		return nil, nil, fileStamp{}, &fs.PathError{Op: "stat", Path: p, Err: err}
	}
	if why := notRegular(uint32(before.Mode)); why != "" {
		return nil, nil, fileStamp{}, &unsafeError{msg: p + why, leaf: true}
	}
	if testHookOpenLeaf != nil {
		testHookOpenLeaf(p)
	}
	fd, err := openLeafAt(d.fd, name)
	switch {
	case errors.Is(err, unix.ELOOP):
		return nil, nil, fileStamp{}, &unsafeError{msg: p + " is a symbolic link", leaf: true}
	case errors.Is(err, unix.ENOENT):
		return nil, nil, fileStamp{}, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	case err != nil:
		return nil, nil, fileStamp{}, &fs.PathError{Op: "open", Path: p, Err: err}
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		unix.Close(fd)
		return nil, nil, fileStamp{}, &fs.PathError{Op: "fstat", Path: p, Err: err}
	}
	if why := notRegular(uint32(after.Mode)); why != "" || uint64(after.Dev) != uint64(before.Dev) || after.Ino != before.Ino {
		unix.Close(fd)
		return nil, nil, fileStamp{}, &unsafeError{msg: p + " changed while mamelet opened it (it is not the file it looked at)", leaf: true}
	}
	f := os.NewFile(uintptr(fd), p)
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fileStamp{}, err
	}
	return f, fi, fileStamp{Dev: uint64(after.Dev), Ino: after.Ino, Size: after.Size, Mtime: after.Mtim.Nano(), Ctime: after.Ctim.Nano(), OK: true}, nil
}

// openLeafAt opens name in the directory dfd for reading as openLeaf does: no link followed, no
// terminal taken as the controlling one, no writer of a pipe waited for.
func openLeafAt(dfd int, name string) (fd int, err error) {
	err = ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(dfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		return err
	})
	return fd, err
}

// notRegular says what a file of mode is when it is not a regular file ("" when it is one).
func notRegular(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return ""
	case unix.S_IFLNK:
		return " is a symbolic link"
	case unix.S_IFDIR:
		return " is a directory"
	}
	return " is not a regular file (a device, a socket or a pipe)"
}

// statInfo is what fstatat or fstat found, as an fs.FileInfo (ownerOf reads its owner).
type statInfo struct {
	name string
	st   unix.Stat_t
}

func (s *statInfo) Name() string       { return s.name }
func (s *statInfo) Size() int64        { return s.st.Size }
func (s *statInfo) ModTime() time.Time { return time.Unix(s.st.Mtim.Unix()) }
func (s *statInfo) IsDir() bool        { return s.Mode().IsDir() }
func (s *statInfo) Sys() any           { return &s.st }
func (s *statInfo) owner() (uid, gid int) {
	return int(s.st.Uid), int(s.st.Gid)
}

func (s *statInfo) Mode() fs.FileMode {
	m := fs.FileMode(s.st.Mode & 0o777)
	switch uint32(s.st.Mode) & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= fs.ModeDir
	case unix.S_IFLNK:
		m |= fs.ModeSymlink
	case unix.S_IFIFO:
		m |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		m |= fs.ModeSocket
	case unix.S_IFCHR:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		m |= fs.ModeDevice
	}
	if s.st.Mode&unix.S_ISUID != 0 {
		m |= fs.ModeSetuid
	}
	if s.st.Mode&unix.S_ISGID != 0 {
		m |= fs.ModeSetgid
	}
	if s.st.Mode&unix.S_ISVTX != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// freeBytesAt is how many bytes of the file system of the directory dir (a path of the host)
// mamelet may still write; -1 when it cannot tell.
func freeBytesAt(dir string) int64 {
	var st unix.Statfs_t
	if err := ignoringEINTR(func() error { return unix.Statfs(dir, &st) }); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// freeBytes is freeBytesAt of the file system of the open file f.
func freeBytes(f *os.File) int64 {
	var st unix.Statfs_t
	if err := ignoringEINTR(func() error { return unix.Fstatfs(int(f.Fd()), &st) }); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func ignoringEINTR(f func() error) error {
	for {
		if err := f(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
