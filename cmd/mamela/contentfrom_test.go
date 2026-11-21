package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/internal/api"
)

// declarationTree is the tarball of declarationRepo: the commit's tree, as GitHub serves it to
// mamelacd (a gzipped tarball with one top directory, every file of the commit in it).
func declarationTree(t *testing.T, main string, files map[string]string) []byte {
	t.Helper()
	return tarballOf(t, declarationRepo(t, main, files))
}

// declarationRepo is a repository whose directory infra is the homelab example with main as its
// src/main.emel, and files (path in the repository -> content) besides.
func declarationRepo(t *testing.T, main string, files map[string]string) string {
	t.Helper()
	repo, _ := filepath.Abs("../..")
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.CopyFS(filepath.Join(dir, "infra"), os.DirFS("../../examples/homelab")); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(dir, "infra", "target"))
	pome := mustRead(t, filepath.Join(dir, "infra/Pome.toml"))
	pome = bytes.ReplaceAll(pome, []byte(`"../../pome/mamela"`), []byte(`"`+filepath.Join(repo, "pome/mamela")+`"`))
	pome = bytes.ReplaceAll(pome, []byte(`"../ec2/pome"`), []byte(`"`+filepath.Join(repo, "examples/ec2/pome")+`"`))
	files["infra/Pome.toml"] = string(pome)
	files["infra/src/main.emel"] = main
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func tarballOf(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: "me-infra-0123456/" + filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// homelabWith is the homelab declaration with more on its machine.
func homelabWith(more string) string {
	return `import Std.Console.{SystemConsole}
import Mamela
import Ec2.{Instance}

fn main() {
  app = Instance(
    name: "app-server",
    image_id: "ami-0123456789abcdef0",
    instance_type: "t3.small",
    subnet_id: "subnet-0123456789abcdef0",
    security_group_id: "sg-0123456789abcdef0",
    root_device: "/dev/sda1",
    disk_gib: 10,
  )
  with SystemConsole {
    Mamela.empty()
      |> Ec2.instance(app)
      ` + more + `
      |> Mamela.print
  }
}
`
}

const (
	appUnit = "[Unit]\nDescription=app\n\n[Service]\nExecStart=/usr/local/bin/app\n"
	motd    = "hello\n"
)

// A declaration that takes texts from files of its Pome (contentFrom) plans through the Control
// Plane's API as mamelacd sends it: the commit's whole tree, extracted there and evaluated from it. Its
// graph is the one with the texts written in; a file that is not in the commit is refused with what to
// do, and the answer does not show where the Control Plane keeps the tree.
func TestTheAPIPlansADeclarationThatTakesItsFiles(t *testing.T) {
	toolchainDir(t)
	b := testBackend(t, filepath.Join(t.TempDir(), "state.db"), fakeEC2(t))
	work := filepath.Join(t.TempDir(), "trees")
	hs := httptest.NewServer((&api.Server{Backend: b, WorkDir: work}).Handler())
	defer hs.Close()
	client := &api.Client{Socket: "test", HTTP: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", hs.Listener.Addr().String())
		}}}}
	ctx := context.Background()

	fromFiles := homelabWith(`|> Mamela.file_from(Ec2.node(app), "/etc/motd", "files/motd")
      |> Mamela.systemd_service_from(Ec2.node(app), "app", "files/app.service")`)
	tree, err := client.Tree(ctx, declarationTree(t, fromFiles, map[string]string{"infra/files/motd": motd, "infra/files/app.service": appUnit}))
	if err != nil {
		t.Fatal(err)
	}
	planned, err := client.Plan(ctx, tree, "infra")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, want := range []string{"NodeFile app-server/file/etc/motd", "NodeFile app-server/file/etc/systemd/system/app.service", "NodeService app-server/app"} {
		if !strings.Contains(planned.Text, want) {
			t.Errorf("the plan lacks %s:\n%s", want, planned.Text)
		}
	}

	// The same texts written in: the same revision.
	inline := homelabWith(`|> Mamela.file(Ec2.node(app), "/etc/motd", "hello\n")
      |> Mamela.systemd_service(Ec2.node(app), "app", "[Unit]\nDescription=app\n\n[Service]\nExecStart=/usr/local/bin/app\n")`)
	other, err := client.Tree(ctx, declarationTree(t, inline, map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := client.Revision(ctx, other, "infra")
	if err != nil {
		t.Fatal(err)
	}
	if digest != planned.Revision {
		t.Errorf("written in: %s; from the files: %s", digest, planned.Revision)
	}

	// A file the commit does not have, and one outside the declaration's Pome (in the repository, not
	// in infra): refused, saying which and what to do.
	for name, c := range map[string]struct {
		files map[string]string
		says  []string
	}{
		"not committed":      {map[string]string{"infra/files/app.service": appUnit}, []string{"MAM-EVAL-0501", "files/motd is not in the Pome", "Commit the file"}},
		"outside the Pome":   {map[string]string{"files/motd": motd, "infra/files/app.service": appUnit}, []string{"MAM-EVAL-0501", "files/motd"}},
		"up out of the Pome": {nil, []string{"MAM-EVAL-0502", "out of the Pome"}},
	} {
		main := fromFiles
		if c.files == nil {
			main = homelabWith(`|> Mamela.file_from(Ec2.node(app), "/etc/motd", "../motd")`)
			c.files = map[string]string{"motd": motd}
		}
		tree, err := client.Tree(ctx, declarationTree(t, main, c.files))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Plan(ctx, tree, "infra")
		if err == nil {
			t.Fatalf("%s: planned", name)
		}
		for _, want := range c.says {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal does not say %q: %v", name, want, err)
			}
		}
		if strings.Contains(err.Error(), work) {
			t.Errorf("%s: the refusal shows where the trees are: %v", name, err)
		}
	}
}

// mamela eval of a declaration that takes texts from its files says which; its canonical graph has
// the texts, and not where they came from.
func TestEvalSaysWhereATextCameFrom(t *testing.T) {
	tools := toolchainDir(t)
	repo := declarationRepo(t, homelabWith(`|> Mamela.systemd_service_from(Ec2.node(app), "app", "files/app.service")`),
		map[string]string{"infra/files/app.service": appUnit})
	code, stdout, stderr := runCLI("eval", "--toolchain", tools, filepath.Join(repo, "infra"))
	if code != 0 || !strings.Contains(stdout, "file /etc/systemd/system/app.service (63 bytes, 0644 root:root) on app-server (content from files/app.service)") {
		t.Fatalf("code=%d\n%s\n%s", code, stdout, stderr)
	}
	code, stdout, stderr = runCLI("eval", "--json", "--toolchain", tools, filepath.Join(repo, "infra"))
	if code != 0 || !strings.Contains(stdout, `"content":"[Unit]\nDescription=app\n\n[Service]\nExecStart=/usr/local/bin/app\n"`) || strings.Contains(stdout, "contentFrom") {
		t.Fatalf("code=%d\n%s\n%s", code, stdout, stderr)
	}
}
