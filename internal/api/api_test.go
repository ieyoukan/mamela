package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeBackend struct {
	planned  []string
	approved map[string]string
}

func (b *fakeBackend) Plan(_ context.Context, dir string) (Planned, error) {
	b.planned = append(b.planned, dir)
	if _, err := os.Stat(filepath.Join(dir, "main.emel")); err != nil {
		return Planned{}, err
	}
	return Planned{Digest: "sha256:" + strings.Repeat("a", 64), Revision: "sha256:" + strings.Repeat("b", 64), Changes: 1}, nil
}
func (b *fakeBackend) Revision(context.Context, string) (string, error) {
	return "sha256:" + strings.Repeat("b", 64), nil
}
func (b *fakeBackend) Approve(_ context.Context, digest, by string, _ time.Duration) error {
	b.approved[digest] = by
	return nil
}
func (b *fakeBackend) Approved(_ context.Context, digest string) (bool, error) {
	_, ok := b.approved[digest]
	return ok, nil
}
func (b *fakeBackend) Apply(context.Context, string) (Applied, error) {
	return Applied{OK: true, Text: "done"}, nil
}

func tarball(t *testing.T, entries map[string]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, typ := range entries {
		h := &tar.Header{Name: "o-r-1/" + name, Typeflag: typ, Mode: 0o644}
		if typ == tar.TypeReg {
			h.Size = 1
		} else {
			h.Linkname = "/etc/passwd"
		}
		tw.WriteHeader(h)
		if typ == tar.TypeReg {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// serve starts a server on a socket (in a short directory: socket paths are limited).
func serve(t *testing.T) (*Client, *fakeBackend, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "mapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	b := &fakeBackend{approved: map[string]string{}}
	s := &Server{Backend: b, WorkDir: filepath.Join(dir, "trees"), Mamela: "test"}
	l, err := Listen(socket, "")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: s.Handler()}
	go hs.Serve(l)
	t.Cleanup(func() { hs.Close() })
	return NewClient(socket), b, socket
}

func TestAPI(t *testing.T) {
	ctx := context.Background()
	c, b, socket := serve(t)
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode: %v %v", info.Mode(), err)
	}
	if i, err := c.Info(ctx); err != nil || i.API != Version {
		t.Fatalf("info: %+v %v", i, err)
	}
	tree, err := c.Tree(ctx, tarball(t, map[string]byte{"infra/main.emel": tar.TypeReg}))
	if err != nil || !strings.HasPrefix(tree, "sha256:") {
		t.Fatalf("tree: %q %v", tree, err)
	}
	again, err := c.Tree(ctx, tarball(t, map[string]byte{"infra/main.emel": tar.TypeReg}))
	if err != nil || again != tree {
		t.Fatalf("the same tree again: %q %v", again, err)
	}
	p, err := c.Plan(ctx, tree, "infra")
	if err != nil || p.Changes != 1 {
		t.Fatalf("plan: %+v %v", p, err)
	}
	if err := c.Approve(ctx, p.Digest, "github:alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.Approved(ctx, p.Digest); !ok || err != nil || b.approved[p.Digest] != "github:alice" {
		t.Fatalf("approved: %v %v %v", ok, err, b.approved)
	}
	if a, err := c.Apply(ctx, p.Digest); err != nil || !a.OK {
		t.Fatalf("apply: %+v %v", a, err)
	}
}

// What the API refuses: trees with links, directories that leave the tree or are not there,
// approvals without a source, too long, or of something that is not a digest.
func TestAPIRefuses(t *testing.T) {
	ctx := context.Background()
	c, b, _ := serve(t)
	if _, err := c.Tree(ctx, tarball(t, map[string]byte{"infra/main.emel": tar.TypeReg, "infra/link": tar.TypeSymlink})); !Refused(err) {
		t.Fatalf("a tree with a link: %v", err)
	}
	if _, err := c.Tree(ctx, []byte("not gzip")); !Refused(err) {
		t.Fatalf("not a tarball: %v", err)
	}
	tree, err := c.Tree(ctx, tarball(t, map[string]byte{"infra/main.emel": tar.TypeReg}))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"../infra", "/etc", "infra/../../x", "nothere", `infra\..`} {
		if _, err := c.Plan(ctx, tree, dir); !Refused(err) {
			t.Errorf("dir %q: %v", dir, err)
		}
	}
	if _, err := c.Plan(ctx, "sha256:"+strings.Repeat("0", 64), "infra"); !Refused(err) {
		t.Errorf("an unknown tree: %v", err)
	}
	if len(b.planned) != 0 {
		t.Fatalf("planned %v", b.planned)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	for name, req := range map[string]ApproveRequest{
		"no source": {Digest: digest, By: "alice", TTL: "1h"},
		"too long":  {Digest: digest, By: "github:alice", TTL: "720h"},
		"negative":  {Digest: digest, By: "github:alice", TTL: "-1h"},
		"digest":    {Digest: "abc", By: "github:alice", TTL: "1h"},
	} {
		if err := c.post(ctx, "/v1/approve", req, nil); !Refused(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(b.approved) != 0 {
		t.Fatalf("approved %v", b.approved)
	}
	if err := c.post(ctx, "/v1/apply", map[string]string{"digest": digest, "force": "yes"}, nil); !Refused(err) {
		t.Errorf("unknown member: %v", err)
	}
}

// Listen replaces a stale socket, but neither another file nor a live server's socket.
func TestListen(t *testing.T) {
	dir, err := os.MkdirTemp("", "mapi")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "file")
	os.WriteFile(file, []byte("x"), 0o600)
	if _, err := Listen(file, ""); err == nil {
		t.Error("listened over a file")
	}
	socket := filepath.Join(dir, "s.sock")
	l, err := Listen(socket, "")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if _, err := Listen(socket, ""); err == nil {
		t.Error("listened over a live server")
	}
	l.Close()
	// A socket left behind (the listener closed without removing it, like a crash).
	if _, err := os.Lstat(socket); err != nil {
		stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		stale.SetUnlinkOnClose(false)
		stale.Close()
	}
	l2, err := Listen(socket, "")
	if err != nil {
		t.Fatalf("a stale socket: %v", err)
	}
	l2.Close()
}
