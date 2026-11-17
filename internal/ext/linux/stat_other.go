//go:build !unix

package linux

import "io/fs"

// mamelet runs on Linux; elsewhere (the Control Plane's tools built for another system) files have no
// owner to read.
const openNonBlock = 0

func ownerOf(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
