package linux

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Downloads pinned by sha256, as mamelet makes them: over https only (a redirect too), bounded in
// size and time, streamed into a new file of mamelet's own while they are hashed, and kept only when
// their sha256 is the approved one; and one member taken out of a gzip-compressed tar. They run no
// program and need no tool on the node. NodeArtifact installs from them; they take no type of the
// extension, so that another one (k3s's download of its binary) can move onto them.

// DownloadTimeout bounds one download, from the request to its last byte.
const DownloadTimeout = 10 * time.Minute

// maxUnpacked bounds what is read of an archive once it is decompressed (all its members, while
// looking for one): a small download cannot make mamelet read without end.
const maxUnpacked = 8 * MaxArtifact

// Refusal is an error that says mamelet refuses what it was given (a download that is not the
// approved one, a redirect to http, an archive whose member is a link), not that something failed
// on the way and could work another time.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

func refusal(format string, args ...any) error { return &Refusal{Reason: fmt.Sprintf(format, args...)} }

// HTTPSClient is the client downloads are made with: base's transport and timeout (when base is
// nil: Go's default transport, with a minute to wait for the response's header), DownloadTimeout
// when base has none, and redirects followed only to https (ten at most). base itself is not
// changed.
func HTTPSClient(base *http.Client) *http.Client {
	c := &http.Client{}
	if base != nil {
		*c = *base
	} else {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = time.Minute
		c.Transport = t
	}
	if c.Timeout == 0 {
		c.Timeout = DownloadTimeout
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		switch {
		case req.URL.Scheme != "https":
			return refusal("%s redirects to %s, which is not https: mamelet downloads over https only", via[0].URL.Redacted(), req.URL.Redacted())
		case len(via) >= 10:
			return refusal("%s redirects more than 10 times", via[0].URL.Redacted())
		}
		return nil
	}
	return c
}

// Fetch downloads rawURL (https: CheckURL) with client (HTTPSClient) into a new file at dest
// (created by Fetch, readable by its owner only), hashing it as it streams, and keeps it only when
// it has at most max bytes and its sha256 is want. Otherwise, and whenever it fails, dest is
// removed: nothing of a download that is not the approved one stays. The body is asked for as it is
// (no transparent decompression), so that its sha256 is the one a release's checksum file lists.
func Fetch(ctx context.Context, client *http.Client, rawURL, want, dest string, max int64) (err error) {
	if err := CheckURL(rawURL); err != nil {
		return &Refusal{Reason: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		var r *Refusal
		if errors.As(err, &r) {
			return r
		}
		return fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("download %s: the server answered %s", rawURL, resp.Status)
	case resp.Request != nil && resp.Request.URL.Scheme != "https":
		return refusal("download %s: it came over %s, not https", rawURL, resp.Request.URL.Scheme)
	case resp.ContentLength > max:
		return refusal("download %s: it is %d bytes; mamelet downloads at most %d MiB", rawURL, resp.ContentLength, max>>20)
	}
	// Room for it (as much as the server says it is, or nothing when it does not say), and the margin.
	if err := enoughFree(freeBytesAt(filepath.Dir(dest)), "mamelet's state directory", max64(resp.ContentLength, 0)); err != nil {
		return fmt.Errorf("download %s: %w", rawURL, err)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dest)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, max+1))
	switch {
	case err != nil:
		return fmt.Errorf("download %s: %w (after %d bytes)", rawURL, err, n)
	case n > max:
		return refusal("download %s: it is larger than %d MiB, which mamelet downloads at most", rawURL, max>>20)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return refusal("the download of %s has sha256 %s, not the approved %s; nothing was installed", rawURL, got, want)
	}
	return f.Sync()
}

// ExtractMember copies the member called name out of the gzip-compressed tar at archive into a new
// file at dest (created by it, readable by its owner only), and returns its sha256. The member must
// be in the archive exactly once (its name compared as tar would write it: without a leading "/" or
// "./") and be a regular file of at most max bytes: a link, a directory or a device is refused, and
// so is a name that is missing or twice there. The whole archive is read, so that a second member
// of the name after the first one is found too. Whenever it fails, dest is removed.
func ExtractMember(archive, name, dest string, max int64) (sum string, err error) {
	src, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer src.Close()
	zr, err := gzip.NewReader(src)
	if err != nil {
		return "", refusal("the download is not a gzip-compressed tar (%v), so %s cannot be taken out of it", err, name)
	}
	defer zr.Close()
	tr := tar.NewReader(&boundedReader{r: zr, left: maxUnpacked})
	var f *os.File
	defer func() {
		if f != nil {
			if cerr := f.Close(); err == nil && cerr != nil {
				err = cerr
			}
		}
		if err != nil {
			os.Remove(dest)
		}
	}()
	found := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", unpackError(err)
		}
		if memberName(hdr.Name) != name {
			continue
		}
		found++
		switch {
		case found > 1:
			return "", refusal("%s is in the archive more than once: which one is meant is not clear", name)
		case hdr.Typeflag != tar.TypeReg:
			return "", refusal("%s in the archive is not a regular file (%s): mamelet installs a file, never a link or a device", name, tarType(hdr.Typeflag))
		case hdr.Size > max:
			return "", refusal("%s in the archive is %d bytes; mamelet installs at most %d MiB", name, hdr.Size, max>>20)
		}
		if err := enoughFree(freeBytesAt(filepath.Dir(dest)), "mamelet's state directory", hdr.Size); err != nil {
			return "", err
		}
		if f, err = os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			return "", err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(tr, max+1))
		switch {
		case err != nil:
			return "", unpackError(err)
		case n != hdr.Size:
			return "", fmt.Errorf("%s in the archive has %d bytes, not the %d its header says", name, n, hdr.Size)
		}
		sum = hex.EncodeToString(h.Sum(nil))
	}
	if found == 0 {
		return "", refusal("%s is not in the archive: name the member as `tar -tzf` lists it", name)
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return sum, nil
}

// memberName is the name of a member of a tar as it is compared with the declared one: relative and
// clean ("./a/b", "/a/b" and "a//b/" are all "a/b"). A name that only cleaning makes another's is
// still that one, so that two of them are found twice, not once.
func memberName(n string) string {
	return strings.TrimPrefix(path.Clean("/"+n), "/")
}

func tarType(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "a symbolic link"
	case tar.TypeLink:
		return "a hard link"
	case tar.TypeDir:
		return "a directory"
	case tar.TypeChar, tar.TypeBlock:
		return "a device"
	case tar.TypeFifo:
		return "a pipe"
	}
	return fmt.Sprintf("tar type %q", t)
}

var errUnpackedTooLarge = errors.New("too large")

func unpackError(err error) error {
	if errors.Is(err, errUnpackedTooLarge) {
		return refusal("the archive is larger than %d MiB once decompressed, which mamelet reads at most", maxUnpacked>>20)
	}
	return refusal("the download is not a gzip-compressed tar that can be read: %v", err)
}

// boundedReader reads at most left bytes of r, and then fails (errUnpackedTooLarge), never ends as if
// r had ended.
type boundedReader struct {
	r    io.Reader
	left int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errUnpackedTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
