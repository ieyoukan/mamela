package linux

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
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
