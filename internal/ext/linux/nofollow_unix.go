//go:build linux || darwin

package linux

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// What mamelet opens of the node's files, on the systems it runs on: the system calls themselves
// (golang.org/x/sys/unix), because os.Root follows a symbolic link at the end of a name (within the
// root) whatever flags it is given, and a link that a user puts where a file was could then lead
// mamelet, root, to open a terminal (and take it as its controlling terminal) or a device whose
// opening alone does something (/dev/watchdog).

// openLeaf opens the regular file name in dir for reading (p is its path, for messages): only if
// it is one when it is looked at (fstatat, not following a link), then without following a link
// (O_NOFOLLOW), taking no terminal (O_NOCTTY) and waiting for no writer (O_NONBLOCK), and only if
// what was opened is still a regular file and the one looked at (fstat of the descriptor). Its
// FileInfo and stamp are the descriptor's.
func openLeaf(dir *os.Root, name, p string) (*os.File, fs.FileInfo, fileStamp, error) {
	d, err := dir.Open(".")
	if err != nil {
		return nil, nil, fileStamp{}, err
	}
	defer d.Close()
	dfd := int(d.Fd())
	var before unix.Stat_t
	if err := ignoringEINTR(func() error { return unix.Fstatat(dfd, name, &before, unix.AT_SYMLINK_NOFOLLOW) }); err != nil {
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
	fd, err := openLeafAt(dfd, name)
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

// openChildDir opens the directory name in parent without following a link (O_DIRECTORY,
// O_NOFOLLOW): what mamelet changes of a directory it made (its owner, its mode) is changed through
// it, never through the name.
func openChildDir(parent *os.Root, name string) (*os.File, error) {
	d, err := parent.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var fd int
	err = ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(int(d.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, &unsafeError{msg: name + " is a symbolic link or not a directory"}
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

// freeBytes is how many bytes of the file system of f mamelet may still write (what is available
// to users: the blocks the file system keeps for root stay free); -1 when it cannot tell.
func freeBytes(f *os.File) int64 {
	var st unix.Statfs_t
	if err := ignoringEINTR(func() error { return unix.Fstatfs(int(f.Fd()), &st) }); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// freeBytesAt is freeBytes of the file system of the directory dir (a path of the host).
func freeBytesAt(dir string) int64 {
	var st unix.Statfs_t
	if err := ignoringEINTR(func() error { return unix.Statfs(dir, &st) }); err != nil {
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
