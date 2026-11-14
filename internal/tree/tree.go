// Package tree writes a commit's tree, as GitHub serves it (a gzipped tarball with one top
// directory), to a directory: only regular files and directories, under it. A path that leaves
// it, a link or any other entry refuses the whole tree. Both mamelacd (which fetches trees) and
// the Control Plane's API (which plans them) rely on it; neither runs anything from a tree.
package tree

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Limits on a tree.
const (
	MaxTarball = 64 << 20  // compressed
	maxTree    = 256 << 20 // uncompressed
	maxFiles   = 20000
)

// Extract writes a GitHub tarball (one top directory) into dir, which must not exist.
func Extract(r io.Reader, dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s exists", dir)
	}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	tmp := dir + ".partial"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			os.RemoveAll(tmp) // a refused or broken tree is not kept
		}
	}()
	var total int64
	files := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue // GitHub's comment with the commit
		}
		// "<owner>-<repo>-<sha>/path": the top directory is dropped. No segment may climb.
		if slices.Contains(strings.Split(h.Name, "/"), "..") || path.IsAbs(h.Name) {
			return fmt.Errorf("tarball entry %q leaves the tree", h.Name)
		}
		name := path.Clean(h.Name)
		_, rest, ok := strings.Cut(name, "/")
		if !ok || rest == "" {
			continue
		}
		if path.IsAbs(rest) || rest == ".." || strings.HasPrefix(rest, "../") || strings.Contains(rest, "\\") {
			return fmt.Errorf("tarball entry %q leaves the tree", h.Name)
		}
		target := filepath.Join(tmp, filepath.FromSlash(rest))
		if files++; files > maxFiles {
			return fmt.Errorf("the tree has over %d files", maxFiles)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += h.Size; total > maxTree {
				return fmt.Errorf("the tree is over %d bytes", maxTree)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("tarball entry %q is a link or a special file: the tree is refused", h.Name)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	done = true
	return nil
}
