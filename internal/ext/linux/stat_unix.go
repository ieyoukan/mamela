//go:build unix

package linux

import (
	"io/fs"
	"syscall"
)

// openNonBlock: opening a pipe that was put where a file was does not wait for a writer.
const openNonBlock = syscall.O_NONBLOCK

// ownerOf is the user and group ids of a file.
func ownerOf(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
