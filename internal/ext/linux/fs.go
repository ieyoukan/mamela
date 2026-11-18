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
	"sync"
	"sync/atomic"

	"github.com/ieyoukan/mamela/internal/mamelet"
)

// mamelet's view of the node's file system for the linux kinds. It works under an os.Root (the
// node's "/", or System.Root in tests), so that nothing it opens can lead out of it, and it never
// follows a symbolic link of its own accord: every directory above a file is opened by itself from
// the one above, checked to be a directory and not a link, and to be the one looked at (a link put
// in its place meanwhile is refused); a file is read only through openLeaf (no link followed, no
// terminal taken, nothing but a regular file); and a file is replaced by a rename in its directory,
// which never follows a link either.

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

// unsafeError says why mamelet does not go through what is at a path: a symbolic link, a directory
// where a file is, a device; leaf: the path itself (not a directory above it). Its message is
// FileState.Unsafe's.
type unsafeError struct {
	msg  string
	leaf bool
}

func (e *unsafeError) Error() string { return e.msg }

// fileStamp is what changes whenever a file's content does: its device and inode, its size, and the
// times of its last change of content and of anything (ctime, which no user can set back). OK false:
// the system does not tell (a file is then hashed every time).
type fileStamp struct {
	Dev, Ino     uint64
	Size         int64
	Mtime, Ctime int64
	OK           bool
}

// openDir opens the directory dir (absolute) one component at a time from the root, each a
// directory and not a link when it is looked at, and the one opened the one looked at (a link a user
// put in its place meanwhile is refused, not followed); it makes nothing. A component that does not
// exist is an error that is fs.ErrNotExist; one that is a link or not a directory, an *unsafeError.
func openDir(r *os.Root, dir string) (*os.Root, error) {
	cur, err := r.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if dir == "/" {
		return cur, nil
	}
	walked := ""
	for _, name := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		walked += "/" + name
		next, err := openSubdir(cur, name, walked)
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// openSubdir opens the directory name of cur (whose path is p): it must be a directory, not a link,
// and what is opened the directory looked at.
func openSubdir(cur *os.Root, name, p string) (*os.Root, error) {
	fi, err := cur.Lstat(name)
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
	next, err := cur.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if got, err := next.Stat("."); err != nil || !os.SameFile(fi, got) {
		next.Close()
		return nil, &unsafeError{msg: p + " changed while mamelet opened it (a link put in its place?)"}
	}
	return next, nil
}

// Tests put something else in place of a file (testHookOpenLeaf) or a directory (testHookOpenSubdir)
// between mamelet's look at it and its opening (p: its path), as a user racing mamelet would.
var testHookOpenLeaf, testHookOpenSubdir func(p string)

// openRegular opens the regular file at p (absolute) for reading, following no link on the way
// (openDir, openLeaf). The FileInfo and the stamp are the opened descriptor's.
func openRegular(r *os.Root, p string) (*os.File, fs.FileInfo, fileStamp, error) {
	dir, err := openDir(r, path.Dir(p))
	if err != nil {
		return nil, nil, fileStamp{}, err
	}
	defer dir.Close()
	return openLeaf(dir, path.Base(p), p)
}

// readRegular reads the regular file at p (openRegular), refusing one larger than max bytes.
func readRegular(r *os.Root, p string, max int64) ([]byte, error) {
	f, fi, _, err := openRegular(r, p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi.Size() > max {
		return nil, fmt.Errorf("%s is %d bytes; mamelet reads at most %d", p, fi.Size(), max)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err == nil && int64(len(b)) > max {
		err = fmt.Errorf("%s is larger than %d bytes", p, max)
	}
	return b, err
}

// inspect observes what is at p (an absolute path CheckPath accepts) and the directories above it.
func inspect(r *os.Root, p string, a accounts) FileState { return inspectUpTo(r, p, a, MaxContent) }

// inspectUpTo is inspect of a file that is hashed only if it has at most max bytes (Large: it has
// more). The directories above it are looked at (which are missing, which is a link, which a user
// other than root can change); the file is read through openRegular, and its mode, owner and size
// are those of the descriptor read.
func inspectUpTo(r *os.Root, p string, a accounts, max int64) FileState {
	st := FileState{Path: p}
	rootUID, rootGID := a.rootIDs()
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
		if uid, gid, ok := ownerOf(fi); ok && (uid != rootUID || (fi.Mode()&0o020 != 0 && gid != rootGID) || fi.Mode()&0o002 != 0) {
			st.Exposed = append(st.Exposed, DirOwner{Dir: d, Mode: modeString(fi.Mode()), Owner: a.userName(uid), Group: a.groupName(gid)})
		}
	}
	f, fi, stamp, err := openRegular(r, p)
	var unsafe *unsafeError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st
	case errors.As(err, &unsafe):
		st.Exists, st.Unsafe = unsafe.leaf, unsafe.msg
		return st
	case err != nil:
		st.Error = err.Error()
		return st
	}
	defer f.Close()
	st.Exists = true
	st.Mode = modeString(fi.Mode())
	if uid, gid, ok := ownerOf(fi); ok {
		st.Owner, st.Group = a.userName(uid), a.groupName(gid)
	}
	if fi.Size() > max {
		st.Large = true
		return st
	}
	key := r.Name() + "\x00" + p
	if sum, ok := hashes.get(key, stamp); ok {
		st.SHA256 = sum
		return st
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, max+1)); err != nil {
		st.Error = err.Error()
		return st
	}
	st.SHA256 = hex.EncodeToString(h.Sum(nil))
	if fi.Size() > MaxContent {
		hashes.put(key, stamp, st.SHA256)
		largeHashed.Add(1)
	}
	return st
}

// largeHashed counts the large files hashed (tests: the cache spares the hashing).
var largeHashed atomic.Int64

// hashCache keeps the sha256 of the large files mamelet observes (artifacts, up to MaxArtifact) by
// their stamp, so that a file is hashed again only when it changed, not every round. It is mamelet's
// memory only: after a restart, each is hashed once.
type hashCache struct {
	mu sync.Mutex
	m  map[string]cachedSum
}

type cachedSum struct {
	stamp fileStamp
	sum   string
}

// maxCached bounds the files the cache keeps (it starts over beyond).
const maxCached = 4 * MaxNodeArtifacts

var hashes hashCache

func (c *hashCache) get(key string, s fileStamp) (string, bool) {
	if !s.OK {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	return e.sum, ok && e.stamp == s
}

func (c *hashCache) put(key string, s fileStamp, sum string) {
	if !s.OK {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxCached {
		c.m = map[string]cachedSum{}
	}
	c.m[key] = cachedSum{s, sum}
}

// rootIDs are the ids of root and of its group on the node (0 when the files do not name them).
func (a accounts) rootIDs() (uid, gid int) {
	return a.users["root"], a.groups["root"]
}

// resolve is p (absolute) with every symbolic link in it followed, as the kernel follows them (an
// absolute link from the root, at most 40 links), by looking only (Lstat, Readlink): it opens
// nothing. It is for what is read of the system's own directories (a shell that is a link of the
// alternatives, an APT source that is a link), never for a path a user can change.
func resolve(r *os.Root, p string) (string, error) {
	todo := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur, links := "/", 0
	for len(todo) > 0 {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = path.Dir(cur)
			continue
		}
		next := path.Join(cur, c)
		fi, err := r.Lstat(rel(next))
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > 40 {
			return "", fmt.Errorf("%s: too many symbolic links", p)
		}
		t, err := r.Readlink(rel(next))
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(t, "/") {
			cur = "/"
		}
		todo = append(strings.Split(strings.TrimPrefix(t, "/"), "/"), todo...)
	}
	return cur, nil
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

// tempPrefix and tempSuffix make the name of mamelet's temporary files in the directories it writes
// into (tempName).
const (
	tempPrefix = ".mamelet-"
	tempSuffix = ".tmp"
)

// tempName is the temporary file a write of base goes through, in the same directory: the same for
// every attempt, so that a write that was interrupted leaves one at most, and the next removes it
// (or mamelet's next round: removeStaleTemp).
func tempName(base string) string {
	sum := sha256.Sum256([]byte(base))
	return tempPrefix + hex.EncodeToString(sum[:8]) + tempSuffix
}

// removeStaleTemp removes the temporary file a write of p that was interrupted (mamelet was killed)
// left in p's directory: only that name, through the directories opened one at a time (no link
// followed), and only a regular file. Nothing happens when the directory is not there or not safe.
func removeStaleTemp(r *os.Root, p string) {
	dir, err := openDir(r, path.Dir(p))
	if err != nil {
		return
	}
	defer dir.Close()
	tmp := tempName(path.Base(p))
	if fi, err := dir.Lstat(tmp); err == nil && fi.Mode().IsRegular() {
		dir.Remove(tmp)
	}
}

// DiskMargin is the free space mamelet leaves on a file system it writes a file into (or downloads
// into), besides the file itself: a full file system stops the system's own services (journald,
// apt, dpkg).
var DiskMargin int64 = 64 << 20

// checkFree says why a file of size bytes is not written into dir (nil when it may be): its file
// system has less than size and DiskMargin free.
func checkFree(dir *os.Root, where string, size int64) error {
	d, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return enoughFree(freeBytes(d), where, size)
}

// enoughFree is checkFree of a file system with free bytes (-1: unknown, which is not checked).
func enoughFree(free int64, where string, size int64) error {
	if need := size + DiskMargin; free >= 0 && free < need {
		return fmt.Errorf("the file system of %s has %d MiB free; writing %d MiB there would leave less than the %d MiB mamelet keeps free: free some space on the node, then apply again",
			where, free>>20, (size+(1<<20)-1)>>20, DiskMargin>>20)
	}
	return nil
}

// writeTo writes the file at p as w says: the directories above it that are missing (0755, owned by
// dirUID:dirGID), then the content into a temporary file in its directory (synced, owned, with its
// mode), renamed over p, and the directory synced. The caller checked the path, the directories
// above it and what is at it (inspect) just before.
func writeTo(r *os.Root, p string, w Write, uid, gid, dirUID, dirGID int) error {
	return writeFrom(r, p, strings.NewReader(w.Content), int64(len(w.Content)), w.SHA256, w.Mode, uid, gid, dirUID, dirGID)
}

// writeFrom is writeTo of the content src reads (to its end, size bytes), whose sha256 must be sum:
// what is read is hashed as it is written, and a content of another sha256 is never renamed over p.
func writeFrom(r *os.Root, p string, src io.Reader, size int64, sum, modeText string, uid, gid, dirUID, dirGID int) error {
	mode, err := ParseMode(modeText)
	if err != nil {
		return err
	}
	dir, err := walkDirs(r, p, dirUID, dirGID)
	if err != nil {
		return err
	}
	defer dir.Close()
	return replaceIn(dir, path.Dir(p), path.Base(p), src, size, sum, fs.FileMode(mode), uid, gid)
}

// replaceIn writes the content src reads (size bytes, whose sha256 must be sum) into a temporary file
// in dir (whose path is where: synced, owned by uid:gid, with mode), renames it over base, and syncs
// dir. It first checks that the file system has room for it (checkFree).
func replaceIn(dir *os.Root, where, base string, src io.Reader, size int64, sum string, mode fs.FileMode, uid, gid int) error {
	tmp := tempName(base)
	p := base
	if err := dir.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the temporary file an earlier write left: %w", err)
	}
	if err := checkFree(dir, where, size); err != nil {
		return err
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

// walkDirs opens the directory of p one component at a time from the root (openSubdir: each a
// directory, not a link, and the one looked at), and makes those that are missing (0755, owned by
// dirUID:dirGID). A directory it makes is given its owner and mode through a descriptor opened
// without following a link, never through its name; and one that appears between the look and the
// making (made by someone else) is refused, not taken.
func walkDirs(r *os.Root, p string, dirUID, dirGID int) (*os.Root, error) {
	cur, err := r.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, d := range parents(p) {
		name := path.Base(d)
		if _, err := cur.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			if err := makeDir(cur, name, d, dirUID, dirGID); err != nil {
				cur.Close()
				return nil, err
			}
		}
		next, err := openSubdir(cur, name, d)
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// makeDir makes the directory name in parent (whose path is p), 0755 and owned by uid:gid.
func makeDir(parent *os.Root, name, p string, uid, gid int) error {
	if err := parent.Mkdir(name, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s appeared while mamelet made it (made by someone else meanwhile): plan again", p)
		}
		return err
	}
	d, err := openChildDir(parent, name)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Chown(uid, gid); err != nil {
		return err
	}
	return d.Chmod(0o755)
}
