package linux

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/ieyoukan/mamela/internal/mamelet"
)

// mamelet's view of the node's file system for the linux kinds. It works under an os.Root (the
// node's "/", or System.Root in tests), so that nothing it opens can lead out of it, and it never
// follows a symbolic link of its own accord: every directory above a file is checked to be a
// directory, not a link, before mamelet writes, and the file is replaced by a rename in that
// directory, which never follows a link either.

// openTree opens the node's file system as mamelet writes into it.
func openTree(s *mamelet.System) (*os.Root, error) {
	root := s.Root
	if root == "" {
		root = "/"
	}
	return os.OpenRoot(root)
}

// rel is an absolute path of the node as a name under the root ("." for "/").
func rel(p string) string {
	if p = strings.TrimPrefix(p, "/"); p == "" {
		return "."
	}
	return p
}

// readAccounts reads the node's users and groups (none when the files cannot be read).
func readAccounts(r *os.Root) accounts {
	passwd, _ := r.ReadFile("etc/passwd")
	group, _ := r.ReadFile("etc/group")
	return parseAccounts(string(passwd), string(group))
}

// inspect observes what is at p (an absolute path CheckPath accepts) and the directories above it.
func inspect(r *os.Root, p string, a accounts) FileState { return inspectUpTo(r, p, a, MaxContent) }

// inspectUpTo is inspect of a file that is hashed only if it has at most max bytes (Large: it has
// more).
func inspectUpTo(r *os.Root, p string, a accounts, max int64) FileState {
	st := FileState{Path: p}
	for i, d := range parents(p) {
		fi, err := r.Lstat(rel(d))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			st.Missing = parents(p)[i:]
			return st
		case err != nil:
			st.Error = err.Error()
			return st
		case fi.Mode()&fs.ModeSymlink != 0:
			st.Unsafe = d + " is a symbolic link"
			return st
		case !fi.IsDir():
			st.Unsafe = d + " is not a directory"
			return st
		}
	}
	fi, err := r.Lstat(rel(p))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st
	case err != nil:
		st.Error = err.Error()
		return st
	}
	st.Exists = true
	switch m := fi.Mode(); {
	case m&fs.ModeSymlink != 0:
		st.Unsafe = p + " is a symbolic link"
		return st
	case m.IsDir():
		st.Unsafe = p + " is a directory"
		return st
	case !m.IsRegular():
		st.Unsafe = p + " is not a regular file (a device, a socket or a pipe)"
		return st
	}
	st.Mode = modeString(fi.Mode())
	if uid, gid, ok := ownerOf(fi); ok {
		st.Owner, st.Group = a.userName(uid), a.groupName(gid)
	}
	if fi.Size() > max {
		st.Large = true
		return st
	}
	sum, err := hashFile(r, p, max)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.SHA256 = sum
	return st
}

// hashFile is the sha256 of the regular file at p, of its first max+1 bytes at most (a pipe put
// there meanwhile does not block it).
func hashFile(r *os.Root, p string, max int64) (string, error) {
	f, err := r.OpenFile(rel(p), os.O_RDONLY|openNonBlock, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, max+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// modeString is a file's mode as four octal digits, with the setuid, setgid and sticky bits.
func modeString(m fs.FileMode) string {
	v := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		v |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		v |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		v |= 0o1000
	}
	return fmt.Sprintf("%04o", v)
}

// tempName is the temporary file a write of base goes through, in the same directory: the same for
// every attempt, so that a write that was interrupted leaves one at most, and the next removes it.
func tempName(base string) string {
	sum := sha256.Sum256([]byte(base))
	return ".mamelet-" + hex.EncodeToString(sum[:8]) + ".tmp"
}

// writeTo writes the file at p as w says: the directories above it that are missing (0755, owned by
// dirUID:dirGID), then the content into a temporary file in its directory (synced, owned, with its
// mode), renamed over p, and the directory synced. The caller checked the path, the directories
// above it and what is at it (inspect) just before.
func writeTo(r *os.Root, p string, w Write, uid, gid, dirUID, dirGID int) error {
	return writeFrom(r, p, strings.NewReader(w.Content), w.SHA256, w.Mode, uid, gid, dirUID, dirGID)
}

// writeFrom is writeTo of the content src reads (to its end), whose sha256 must be sum: what is
// read is hashed as it is written, and a content of another sha256 is never renamed over p.
func writeFrom(r *os.Root, p string, src io.Reader, sum, modeText string, uid, gid, dirUID, dirGID int) error {
	mode, err := ParseMode(modeText)
	if err != nil {
		return err
	}
	dir, err := walkDirs(r, p, dirUID, dirGID)
	if err != nil {
		return err
	}
	defer dir.Close()
	return replaceIn(dir, path.Base(p), src, sum, fs.FileMode(mode), uid, gid)
}

// replaceIn writes the content src reads (whose sha256 must be sum) into a temporary file in dir
// (synced, owned by uid:gid, with mode), renames it over base, and syncs dir.
func replaceIn(dir *os.Root, base string, src io.Reader, sum string, mode fs.FileMode, uid, gid int) error {
	tmp := tempName(base)
	p := base
	if err := dir.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the temporary file an earlier write left: %w", err)
	}
	// O_EXCL: the temporary file is a new file, never something put there (a link included).
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			f.Close()
			dir.Remove(tmp)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), src); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("the content has sha256 %s, not %s; nothing was put at %s", got, sum, p)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Chown(uid, gid); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// A rename replaces what is at base (a link too) and never follows it.
	if err := dir.Rename(tmp, base); err != nil {
		dir.Remove(tmp)
		done = true
		return err
	}
	done = true
	if d, err := dir.Open("."); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// walkDirs opens the directory of p one component at a time from the root, and makes those that are
// missing (0755, owned by dirUID:dirGID). Each must be a directory, not a link, when it is looked at;
// and nothing opened under one can lead out of it (os.Root). So a link put in place of a directory
// between the look and the open (by a user who owns the directory above it) can lead only somewhere
// else in that same directory, never to another part of the system.
func walkDirs(r *os.Root, p string, dirUID, dirGID int) (*os.Root, error) {
	cur, err := r.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, d := range parents(p) {
		name := path.Base(d)
		fi, err := cur.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			if err = cur.Mkdir(name, 0o755); err == nil || errors.Is(err, fs.ErrExist) {
				if err = cur.Lchown(name, dirUID, dirGID); err == nil {
					err = cur.Chmod(name, 0o755)
				}
			}
			if err == nil {
				fi, err = cur.Lstat(name)
			}
		}
		switch {
		case err != nil:
			cur.Close()
			return nil, err
		case fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir():
			cur.Close()
			return nil, fmt.Errorf("%s is a symbolic link or not a directory", d)
		}
		next, err := cur.OpenRoot(name)
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}
