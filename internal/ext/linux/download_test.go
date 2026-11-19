package linux

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fetch keeps a download only when it is the approved one, within its bound; whatever it refuses
// leaves no file.
func TestFetch(t *testing.T) {
	rel := newReleases(t)
	body := bytes.Repeat([]byte("0123456789"), 100)
	sum := rel.put("/f", body)
	// Without a Content-Length: the bound is found while it streams.
	rel.handle("/chunked", func(w http.ResponseWriter, _ *http.Request) {
		for range 10 {
			w.Write(body[:100])
			w.(http.Flusher).Flush()
		}
	})
	// Sent gzip-encoded: the bytes are taken as they are sent (a .tar.gz's sha256 is of its bytes).
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(body)
	zw.Close()
	rel.handle("/encoded", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz.Bytes())
	})
	client := HTTPSClient(rel.srv.Client())
	dir := t.TempDir()
	for _, c := range []struct {
		name, url, sum string
		max            int64
		refused        string // "" when it is kept
	}{
		{"kept", rel.url("/f"), sum, 1000, ""},
		{"another sha256", rel.url("/f"), sumA, 1000, "not the approved"},
		{"over the bound", rel.url("/f"), sum, 999, "it is 1000 bytes"},
		{"over the bound, streamed", rel.url("/chunked"), sum, 999, "larger than"},
		{"as sent", rel.url("/encoded"), sha(gz.Bytes()), 1000, ""},
		{"http", "http://example.com/f", sum, 1000, "not https"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dest := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-"))
			err := Fetch(context.Background(), client, c.url, c.sum, dest, c.max)
			var r *Refusal
			switch {
			case c.refused == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.refused != "" && (!errors.As(err, &r) || !strings.Contains(err.Error(), c.refused)):
				t.Fatalf("got %v, want a refusal saying %q", err, c.refused)
			}
			_, statErr := os.Stat(dest)
			if (c.refused == "") != (statErr == nil) {
				t.Errorf("kept %v, refused %q", statErr == nil, c.refused)
			}
			if c.refused == "" {
				if fi, _ := os.Stat(dest); fi.Mode().Perm() != 0o600 {
					t.Errorf("mode %v", fi.Mode())
				}
			}
		})
	}
	// The file is new: Fetch does not write into one that is there.
	if err := Fetch(context.Background(), client, rel.url("/f"), sum, filepath.Join(dir, "kept"), 1000); err == nil {
		t.Error("wrote into a file that was there")
	}
}

// HTTPSClient keeps the transport of the client it is given, and changes nothing of it.
func TestHTTPSClientLeavesItsBaseAlone(t *testing.T) {
	base := &http.Client{}
	c := HTTPSClient(base)
	if base.CheckRedirect != nil || base.Timeout != 0 {
		t.Error("the base client was changed")
	}
	if c.Timeout != DownloadTimeout || c.CheckRedirect == nil {
		t.Errorf("%+v", c)
	}
	if HTTPSClient(nil).Transport == nil {
		t.Error("no transport of its own")
	}
}

// ExtractMember takes one regular file of at most max bytes; names are compared as tar writes them.
func TestExtractMember(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archive, tarGz(t, entry{name: "/abs/x", typeflag: tar.TypeReg, body: "abs"},
		entry{name: "./d/y", typeflag: tar.TypeReg, body: "0123456789"}), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"abs/x": "abs", "d/y": "0123456789"} {
		dest := filepath.Join(dir, strings.ReplaceAll(name, "/", "-"))
		sum, err := ExtractMember(archive, name, dest, 100)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, _ := os.ReadFile(dest)
		if string(got) != want || sum != sha(got) {
			t.Errorf("%s: %q %s", name, got, sum)
		}
	}
	dest := filepath.Join(dir, "small")
	if _, err := ExtractMember(archive, "d/y", dest, 9); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("over the bound: %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a refused member was kept")
	}
	// A tar that is not compressed is not what was declared.
	plain := filepath.Join(dir, "plain.tar")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "x", Typeflag: tar.TypeReg, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	os.WriteFile(plain, buf.Bytes(), 0o600)
	if _, err := ExtractMember(plain, "x", filepath.Join(dir, "p"), 100); err == nil || !strings.Contains(err.Error(), "not a gzip-compressed tar") {
		t.Errorf("a plain tar: %v", err)
	}
	// A truncated archive.
	full := tarGz(t, entry{name: "x", typeflag: tar.TypeReg, body: strings.Repeat("z", 4096)})
	cut := filepath.Join(dir, "cut.tar.gz")
	os.WriteFile(cut, full[:len(full)/2], 0o600)
	if _, err := ExtractMember(cut, "x", filepath.Join(dir, "c"), 1<<20); err == nil {
		t.Error("a truncated archive was read")
	}
}

// What is read of an archive once it is decompressed is bounded: past it, reading fails rather than
// ends.
func TestBoundedReader(t *testing.T) {
	b := &boundedReader{r: strings.NewReader("0123456789"), left: 4}
	got, err := io.ReadAll(b)
	if string(got) != "0123" || !errors.Is(err, errUnpackedTooLarge) {
		t.Errorf("%q %v", got, err)
	}
	if !strings.Contains(unpackError(err).Error(), "once decompressed") {
		t.Errorf("%v", unpackError(err))
	}
}

// A download whose size the server does not say is watched as it streams: once the file system has
// less than DiskMargin left, it stops, and nothing of it stays.
func TestFetchStopsWhenTheFileSystemFills(t *testing.T) {
	rel := newReleases(t)
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	whole := bytes.Repeat(chunk, 12)
	rel.handle("/big", func(w http.ResponseWriter, _ *http.Request) {
		for range 12 {
			w.Write(chunk)
			w.(http.Flusher).Flush()
		}
	})
	old := downloadFree
	t.Cleanup(func() { downloadFree = old })
	written := int64(0)
	downloadFree = func(f *os.File) int64 {
		fi, _ := f.Stat()
		written = fi.Size()
		return DiskMargin + 6<<20 - written // the file system fills as the download grows
	}
	dest := filepath.Join(t.TempDir(), "big")
	err := Fetch(context.Background(), HTTPSClient(rel.srv.Client()), rel.url("/big"), sha(whole), dest, MaxArtifact)
	if err == nil || !strings.Contains(err.Error(), "the download was stopped") {
		t.Fatalf("%v", err)
	}
	if written < freeCheckEvery || written >= int64(len(whole)) {
		t.Errorf("stopped after %d bytes", written)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("what was downloaded stays")
	}
	// Room enough: kept.
	downloadFree = func(*os.File) int64 { return DiskMargin + 1<<30 }
	if err := Fetch(context.Background(), HTTPSClient(rel.srv.Client()), rel.url("/big"), sha(whole), dest, MaxArtifact); err != nil {
		t.Fatal(err)
	}
}

// The node's certificates are read for each download, not once per process: a download that failed
// for want of them (ca-certificates not installed yet) works once they are there, without a restart
// of mamelet.
func TestHTTPSClientReadsTheCertificatesForEachDownload(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("x")) }))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	bundle := filepath.Join(dir, "ca-certificates.crt")
	oldB, oldD := caBundles, caDirs
	caBundles, caDirs = []string{bundle}, []string{filepath.Join(dir, "certs")}
	t.Cleanup(func() { caBundles, caDirs = oldB, oldD })
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	get := func() error {
		c := HTTPSClient(nil)
		defer c.CloseIdleConnections()
		resp, err := c.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(); err == nil {
		t.Fatal("a server whose certificate the node does not have was trusted")
	}
	// ca-certificates is installed (here, the bundle has the server's certificate).
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	write(t, bundle, string(cert))
	if err := get(); err != nil {
		t.Fatalf("the certificate installed since is not used: %v", err)
	}
	// Only in a directory of certificates (no bundle): read too.
	os.Remove(bundle)
	write(t, filepath.Join(dir, "certs", "server.pem"), string(cert))
	if err := get(); err != nil {
		t.Fatalf("the directory's certificate is not used: %v", err)
	}
	// A bundle larger than its bound is not read.
	write(t, bundle, string(cert)+strings.Repeat("#", maxCABundle))
	os.Remove(filepath.Join(dir, "certs", "server.pem"))
	if err := get(); err == nil {
		t.Error("a bundle larger than its bound was read")
	}
}
