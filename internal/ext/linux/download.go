package linux

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
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
// nil: Go's default transport, with a minute to wait for the response's header, and the node's
// certificates as they are now: systemRoots), DownloadTimeout when base has none, and redirects
// followed only to https (ten at most). base itself is not changed.
func HTTPSClient(base *http.Client) *http.Client {
	c := &http.Client{}
	if base != nil {
		*c = *base
	} else {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = time.Minute
		t.TLSClientConfig = &tls.Config{RootCAs: systemRoots(), MinVersion: tls.VersionTLS12}
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

// The node's certificates, where Go looks for them on Linux (crypto/x509): the first bundle of
// caBundles that is there (SSL_CERT_FILE instead, when it is set), or else the certificates of the
// files of caDirs (SSL_CERT_DIR, when it is set).
var (
	caBundles = []string{
		"/etc/ssl/certs/ca-certificates.crt",                // Debian, Ubuntu (ca-certificates)
		"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora, RHEL 6
		"/etc/ssl/ca-bundle.pem",                            // openSUSE
		"/etc/pki/tls/cacert.pem",                           // OpenELEC
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS, RHEL 7
		"/etc/ssl/cert.pem",                                 // Alpine
	}
	caDirs = []string{"/etc/ssl/certs", "/etc/pki/tls/certs"}
)

// Bounds of what systemRoots reads: a bundle, each file of a directory, and how many of them.
const (
	maxCABundle   = 16 << 20
	maxCAFile     = 1 << 20
	maxCADirFiles = 4096
)

// systemRoots are the node's certificates, read now: Go reads them once per process (crypto/x509's
// SystemCertPool), so a node whose download failed for want of them (ca-certificates not installed
// yet) would keep failing after they are installed, until mamelet restarts. A bundle is read, each
// time a client is made (a download, a source's key), of at most maxCABundle bytes; only when none
// is there are the directories read (Go reads them too; on Debian and Ubuntu the bundle has them).
// When none of them gives a certificate, Go's own: SystemCertPool, or nil (Go's default).
func systemRoots() *x509.CertPool {
	pool := x509.NewCertPool()
	files := caBundles
	if f := os.Getenv("SSL_CERT_FILE"); f != "" {
		files = []string{f}
	}
	found := false
	for _, f := range files {
		if b, err := readBounded(f, maxCABundle); err == nil {
			found = pool.AppendCertsFromPEM(b)
			break
		}
	}
	if !found {
		dirs := caDirs
		if d := os.Getenv("SSL_CERT_DIR"); d != "" {
			dirs = filepath.SplitList(d)
		}
		n := 0
		for _, d := range dirs {
			entries, err := os.ReadDir(d)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if n++; n > maxCADirFiles {
					break
				}
				if b, err := readBounded(filepath.Join(d, e.Name()), maxCAFile); err == nil && pool.AppendCertsFromPEM(b) {
					found = true
				}
			}
		}
	}
	if found {
		return pool
	}
	if sys, err := x509.SystemCertPool(); err == nil {
		return sys
	}
	return nil
}

// readBounded reads the regular file p (a link is followed: the certificates' files are links), of
// at most max bytes.
func readBounded(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return nil, err
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file", p)
	case fi.Size() > max:
		return nil, fmt.Errorf("%s is larger than %d bytes", p, max)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err == nil && int64(len(b)) > max {
		err = fmt.Errorf("%s is larger than %d bytes", p, max)
	}
	return b, err
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
	// Room for it (as much as the server says it is, or nothing when it does not say), and the margin;
	// and, as it streams, the margin is watched (a server that does not say how large the download is,
	// or another program filling the file system meanwhile). Checking for the largest download there
	// may be (max) instead would refuse a small one on a small disk.
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
	w := &freeWatch{w: io.MultiWriter(f, h), free: func() int64 { return downloadFree(f) }, where: "mamelet's state directory"}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
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

// freeCheckEvery is how many bytes a download writes between two looks at the free space.
const freeCheckEvery = 4 << 20

// downloadFree is how many bytes the file system of a download in progress has free (freeBytes;
// tests replace it).
var downloadFree = freeBytes

// freeWatch writes to w, and stops once what free says the file system has left is below
// DiskMargin: a download whose size was not known (or a file system that fills meanwhile) does not
// fill it, which would stop the system's own services.
type freeWatch struct {
	w     io.Writer
	free  func() int64
	where string
	since int64
}

func (f *freeWatch) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if f.since += int64(n); err == nil && f.since >= freeCheckEvery {
		f.since = 0
		if free := f.free(); free >= 0 && free < DiskMargin {
			return n, fmt.Errorf("the file system of %s has %d MiB free, less than the %d MiB mamelet keeps free: the download was stopped and removed; free some space on the node, then apply again",
				f.where, free>>20, DiskMargin>>20)
		}
	}
	return n, err
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
