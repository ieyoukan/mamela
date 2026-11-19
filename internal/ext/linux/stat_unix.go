//go:build unix

package linux

import (
	"io/fs"
	"syscall"
)

// openNonBlock: opening a pipe that was put where a file was does not wait for a writer.
const openNonBlock = syscall.O_NONBLOCK

// ownerOf is the user and group ids of a file (of what os or a dirFD found).
func ownerOf(fi fs.FileInfo) (uid, gid int, ok bool) {
	if o, is := fi.(interface{ owner() (int, int) }); is {
		uid, gid = o.owner()
		uid, gid = testIDs.swap(uid, gid)
		return uid, gid, true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	uid, gid = testIDs.swap(int(st.Uid), int(st.Gid))
	return uid, gid, true
}
