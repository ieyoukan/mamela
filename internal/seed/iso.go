// Package seed builds the cloud-init NoCloud seed for a new VM: an ISO 9660 image labelled CIDATA
// that holds meta-data, user-data, network-config and the mamelet binary. Proxmox can attach an
// uploaded ISO as a CD-ROM; its API cannot upload cloud-init snippets.
package seed

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const sector = 2048

// File is one file in the root directory of the image.
type File struct {
	Name string // lowercase letters, digits, '-' and '_' (Linux shows ISO 9660 names in lowercase)
	Data []byte
}

var fileName = regexp.MustCompile(`^[a-z0-9_-]{1,30}$`)

// ISO returns an ISO 9660 image with the files in its root directory. The image is deterministic:
// the same files give the same bytes (all dates are fixed).
//
// Names are written as "NAME.;1" in uppercase; Linux mounts ISO 9660 with map=normal by default,
// which shows them in lowercase without the version, as cloud-init expects ("user-data").
func ISO(label string, files []File) ([]byte, error) {
	if !regexp.MustCompile(`^[A-Z0-9_]{1,32}$`).MatchString(label) {
		return nil, fmt.Errorf("invalid volume label %q", label)
	}
	files = slices.Clone(files)
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(isoName(a.Name), isoName(b.Name)) })
	for i, f := range files {
		if !fileName.MatchString(f.Name) {
			return nil, fmt.Errorf("invalid file name %q", f.Name)
		}
		if i > 0 && files[i-1].Name == f.Name {
			return nil, fmt.Errorf("file %q appears twice", f.Name)
		}
	}

	// Layout: 0-15 system area, 16 primary volume descriptor, 17 terminator, 18 and 19 the path
	// tables (little and big endian), 20 the root directory, then the files.
	const pvdAt, termAt, lPathAt, mPathAt, rootAt = 16, 17, 18, 19, 20
	next := uint32(rootAt + 1)
	extents := make([]uint32, len(files))
	for i, f := range files {
		extents[i] = next
		next += uint32((len(f.Data) + sector - 1) / sector)
	}
	total := next

	var root bytes.Buffer
	root.Write(dirRecord([]byte{0}, rootAt, sector, true))
	root.Write(dirRecord([]byte{1}, rootAt, sector, true))
	for i, f := range files {
		root.Write(dirRecord([]byte(isoName(f.Name)), extents[i], uint32(len(f.Data)), false))
	}
	if root.Len() > sector {
		return nil, fmt.Errorf("too many files for one directory sector")
	}

	img := make([]byte, int(total)*sector)
	pvd := img[pvdAt*sector : (pvdAt+1)*sector]
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	copy(pvd[8:40], pad("", 32))
	copy(pvd[40:72], pad(label, 32))
	both32(pvd[80:88], total)
	both16(pvd[120:124], 1)
	both16(pvd[124:128], 1)
	both16(pvd[128:132], sector)
	pathTable := []byte{1, 0, 0, 0, 0, 0, 1, 0, 0, 0} // root: id length 1, extent, parent 1, id 0, pad
	both32(pvd[132:140], uint32(len(pathTable)))
	binary.LittleEndian.PutUint32(pvd[140:144], lPathAt)
	binary.BigEndian.PutUint32(pvd[148:152], mPathAt)
	copy(pvd[156:190], dirRecord([]byte{0}, rootAt, sector, true))
	copy(pvd[190:318], pad("", 128))
	copy(pvd[318:446], pad("", 128))
	copy(pvd[446:574], pad("", 128))
	copy(pvd[574:702], pad("MAMELA", 128))
	copy(pvd[702:813], pad("", 111))
	for _, off := range []int{813, 830, 847, 864} {
		copy(pvd[off:off+16], "0000000000000000") // dates not specified
	}
	pvd[881] = 1

	term := img[termAt*sector : (termAt+1)*sector]
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1

	l := img[lPathAt*sector:]
	copy(l, pathTable)
	binary.LittleEndian.PutUint32(l[2:6], rootAt)
	binary.LittleEndian.PutUint16(l[6:8], 1)
	m := img[mPathAt*sector:]
	copy(m, pathTable)
	binary.BigEndian.PutUint32(m[2:6], rootAt)
	binary.BigEndian.PutUint16(m[6:8], 1)

	copy(img[rootAt*sector:], root.Bytes())
	for i, f := range files {
		copy(img[int(extents[i])*sector:], f.Data)
	}
	return img, nil
}

func isoName(name string) string { return strings.ToUpper(name) + ".;1" }

func pad(s string, n int) []byte {
	return []byte(s + strings.Repeat(" ", n-len(s)))
}

func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b[0:2], v)
	binary.BigEndian.PutUint16(b[2:4], v)
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b[0:4], v)
	binary.BigEndian.PutUint32(b[4:8], v)
}

// recorded is a fixed recording date (2026-01-01 00:00:00 UTC), so images are reproducible.
var recorded = []byte{126, 1, 1, 0, 0, 0, 0}

func dirRecord(id []byte, extent, size uint32, dir bool) []byte {
	n := 33 + len(id)
	if n%2 == 1 {
		n++
	}
	r := make([]byte, n)
	r[0] = byte(n)
	both32(r[2:10], extent)
	both32(r[10:18], size)
	copy(r[18:25], recorded)
	if dir {
		r[25] = 2
	}
	both16(r[28:32], 1)
	r[32] = byte(len(id))
	copy(r[33:], id)
	return r
}
