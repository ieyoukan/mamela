package linux

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// releases is a release server over https: each path serves its bytes (or what its handler does),
// and every request is counted.
type releases struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	funcs map[string]http.HandlerFunc
	hits  map[string]int
}

func newReleases(t *testing.T) *releases {
	t.Helper()
	r := &releases{files: map[string][]byte{}, funcs: map[string]http.HandlerFunc{}, hits: map[string]int{}}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		r.hits[q.URL.Path]++
		f := r.funcs[q.URL.Path]
		body, ok := r.files[q.URL.Path]
		r.mu.Unlock()
		switch {
		case f != nil:
			f(w, q)
		case ok:
			w.Write(body)
		default:
			http.NotFound(w, q)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *releases) url(p string) string { return r.srv.URL + p }

func (r *releases) put(p string, body []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[p] = body
	return sha(body)
}

func (r *releases) handle(p string, f http.HandlerFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.funcs[p] = f
}

func (r *releases) count(p string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[p]
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type entry struct {
	name     string
	typeflag byte
	body     string
	link     string
}

// tarGz is a gzip-compressed tar of the entries.
func tarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0o755, Linkname: e.link, ModTime: time.Unix(0, 0)}
		if e.typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// artifactNode is a mamelet whose downloads go to rel (and trust its certificate).
func artifactNode(t *testing.T, rel *releases) *mnode {
	t.Helper()
	n := newNode(t)
	n.a.System.HTTP = rel.srv.Client()
	return n
}

// install is an approved installation of url at path.
func install(path, url, sum, extract, op string, attempt int) Artifact {
	return Artifact{Path: path, Owner: "root", Group: "root", Install: &Install{URL: url, SHA256: sum, Extract: extract, Mode: "0755",
		Owner: "root", Group: "root", Grant: noderev.Once{Operation: op, Attempt: attempt, Action: ActionInstallArtifact}}}
}

// stateFiles are the files in mamelet's state directory on the node (its record, and any temporary
// file a download left).
func (n *mnode) stateFiles() []string {
	n.t.Helper()
	entries, _ := os.ReadDir(filepath.Join(n.root, "var/lib/mamelet"))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (n *mnode) record() artifactRecord {
	n.t.Helper()
	rec, err := loadRecord(filepath.Join(n.root, "var/lib/mamelet"))
	if err != nil {
		n.t.Fatal(err)
	}
	return rec
}

func (n *mnode) exists(path string) bool {
	_, err := os.Lstat(filepath.Join(n.root, path))
	return err == nil
}

func artifactOf(t *testing.T, rep noderev.Report, path string) ArtifactState {
	t.Helper()
	a := observedOf(t, rep).artifact(path)
	if a == nil {
		t.Fatalf("%s is not reported: %s", path, rep.Ext[Name])
	}
	return *a
}

// A release's binary, downloaded as it is: installed with its mode, recorded, restarted, once.
func TestMameletInstallsAnArtifact(t *testing.T) {
	rel := newReleases(t)
	body := []byte("\x7fELF a tool")
	sum := rel.put("/tool", body)
	n := artifactNode(t, rel)
	a := install("/usr/local/bin/tool", rel.url("/tool"), sum, "", opA, 1)
	a.Install.Restart = []string{"tool"}
	n.offer(Part{Artifacts: []Artifact{a}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "installed /usr/local/bin/tool from "+rel.url("/tool")) || !strings.Contains(r.Detail, "made /usr, /usr/local, /usr/local/bin") {
		t.Fatalf("result %+v", r)
	}
	if got := n.read("/usr/local/bin/tool"); got != string(body) {
		t.Errorf("content %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(n.root, "usr/local/bin/tool")); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", fi.Mode())
	}
	if !slices.Equal(n.sys.commands, []string{"try-restart -- tool"}) {
		t.Errorf("ran %q", n.sys.commands)
	}
	if want := (installed{Source: Source{URL: rel.url("/tool"), SHA256: sum}, Installed: sum}); n.record().Artifacts["/usr/local/bin/tool"] != want {
		t.Errorf("record %+v", n.record())
	}
	if files := n.stateFiles(); !slices.Equal(files, []string{recordName}) {
		t.Errorf("the state directory has %q", files)
	}
	if fi, _ := os.Stat(filepath.Join(n.root, "var/lib/mamelet", recordName)); fi.Mode().Perm() != 0o600 {
		t.Errorf("the record's mode %v", fi.Mode())
	}
	st := artifactOf(t, rep, "/usr/local/bin/tool")
	if !st.Exists || st.SHA256 != sum || st.Mode != "0755" || st.Owner != "root" || st.From == nil || st.From.SHA256 != sum || st.From.URL != rel.url("/tool") {
		t.Errorf("observed %+v %+v", st, st.From)
	}
	// The same grant does not install again.
	rep = n.round()
	if rel.count("/tool") != 1 || len(n.sys.commands) != 0 {
		t.Errorf("installed twice: %d downloads, ran %q", rel.count("/tool"), n.sys.commands)
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone {
		t.Errorf("the result is not reported again: %+v", r)
	}
}

// One member of a .tar.gz: the download's sha256 is checked, the member is installed, and the record
// says both sha256s, so that the plan knows the file is the declared download's.
func TestMameletInstallsAMemberOfAnArchive(t *testing.T) {
	rel := newReleases(t)
	archive := tarGz(t,
		entry{name: "node_exporter-1.8.2.linux-amd64/", typeflag: tar.TypeDir},
		entry{name: "node_exporter-1.8.2.linux-amd64/LICENSE", typeflag: tar.TypeReg, body: "Apache\n"},
		entry{name: "node_exporter-1.8.2.linux-amd64/node_exporter", typeflag: tar.TypeReg, body: "\x7fELF exporter"},
		entry{name: "node_exporter-1.8.2.linux-amd64/NOTICE", typeflag: tar.TypeReg, body: "notice\n"})
	sum := rel.put("/ne.tar.gz", archive)
	n := artifactNode(t, rel)
	n.offer(Part{Artifacts: []Artifact{install("/usr/local/bin/node_exporter", rel.url("/ne.tar.gz"), sum, exporterMember, opA, 1)}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "member "+exporterMember) {
		t.Fatalf("result %+v", r)
	}
	if got := n.read("/usr/local/bin/node_exporter"); got != "\x7fELF exporter" {
		t.Errorf("content %q", got)
	}
	member := sha([]byte("\x7fELF exporter"))
	if want := (installed{Source: Source{URL: rel.url("/ne.tar.gz"), SHA256: sum, Extract: exporterMember}, Installed: member}); n.record().Artifacts["/usr/local/bin/node_exporter"] != want {
		t.Errorf("record %+v", n.record())
	}
	st := artifactOf(t, rep, "/usr/local/bin/node_exporter")
	if st.SHA256 != member || st.From == nil || st.From.SHA256 != sum || st.From.Extract != exporterMember {
		t.Errorf("observed %+v %+v", st, st.From)
	}
	if files := n.stateFiles(); !slices.Equal(files, []string{recordName}) {
		t.Errorf("the state directory has %q", files)
	}
	// Members named as tar writes them ("./a") are the same members.
	dot := tarGz(t, entry{name: "./bin/tool", typeflag: tar.TypeReg, body: "dot"})
	n.offer(Part{Artifacts: []Artifact{install("/usr/local/bin/tool", rel.url("/dot.tar.gz"), rel.put("/dot.tar.gz", dot), "bin/tool", opB, 1)}}, nil)
	if r := result(t, n.round(), opB); r.Outcome != noderev.OutcomeDone || n.read("/usr/local/bin/tool") != "dot" {
		t.Errorf("result %+v", r)
	}
}

// What is refused leaves nothing installed and nothing in mamelet's state directory: a download of
// another sha256, a redirect to http, one too large, an archive whose member is missing, twice
// there, or not a regular file, and a download that is not an archive.
func TestMameletInstallsNothingItWasNotApproved(t *testing.T) {
	rel := newReleases(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("tool")) }))
	t.Cleanup(plain.Close)
	rel.handle("/to-http", func(w http.ResponseWriter, q *http.Request) {
		http.Redirect(w, q, plain.URL+"/tool", http.StatusFound)
	})
	rel.handle("/huge", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(MaxArtifact+1))
		w.WriteHeader(http.StatusOK)
	})
	toolSum := rel.put("/tool", []byte("tool"))
	archives := 0
	archive := func(entries ...entry) (string, string) {
		archives++
		p := fmt.Sprintf("/a%d.tar.gz", archives)
		return rel.url(p), rel.put(p, tarGz(t, entries...))
	}
	missingURL, missingSum := archive(entry{name: "other", typeflag: tar.TypeReg, body: "x"})
	twiceURL, twiceSum := archive(entry{name: "bin/tool", typeflag: tar.TypeReg, body: "a"}, entry{name: "./bin/tool", typeflag: tar.TypeReg, body: "b"})
	linkURL, linkSum := archive(entry{name: "bin/tool", typeflag: tar.TypeSymlink, link: "/etc/shadow"})
	hardURL, hardSum := archive(entry{name: "etc/shadow", typeflag: tar.TypeReg, body: "x"}, entry{name: "bin/tool", typeflag: tar.TypeLink, link: "etc/shadow"})
	dirURL, dirSum := archive(entry{name: "bin/tool/", typeflag: tar.TypeDir})
	for _, c := range []struct {
		name         string
		url, sum     string
		extract      string
		outcome, say string
	}{
		{"another sha256", rel.url("/tool"), sumA, "", noderev.OutcomeRefused, "not the approved " + sumA},
		{"redirect to http", rel.url("/to-http"), toolSum, "", noderev.OutcomeRefused, "which is not https"},
		{"too large", rel.url("/huge"), toolSum, "", noderev.OutcomeRefused, "at most 512 MiB"},
		{"not found", rel.url("/nothing"), toolSum, "", noderev.OutcomeFailed, "404"},
		{"member missing", missingURL, missingSum, "bin/tool", noderev.OutcomeRefused, "is not in the archive"},
		{"member twice", twiceURL, twiceSum, "bin/tool", noderev.OutcomeRefused, "more than once"},
		{"member is a symbolic link", linkURL, linkSum, "bin/tool", noderev.OutcomeRefused, "a symbolic link"},
		{"member is a hard link", hardURL, hardSum, "bin/tool", noderev.OutcomeRefused, "a hard link"},
		{"member is a directory", dirURL, dirSum, "bin/tool", noderev.OutcomeRefused, "a directory"},
		{"not an archive", rel.url("/tool"), toolSum, "bin/tool", noderev.OutcomeRefused, "not a gzip-compressed tar"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := artifactNode(t, rel)
			a := install("/usr/local/bin/tool", c.url, c.sum, c.extract, opA, 1)
			a.Install.Restart = []string{"tool"}
			n.offer(Part{Artifacts: []Artifact{a}}, nil)
			rep := n.round()
			if r := result(t, rep, opA); r.Outcome != c.outcome || !strings.Contains(r.Detail, c.say) {
				t.Errorf("result %+v", r)
			}
			if n.exists("/usr/local/bin/tool") || n.exists("/etc/shadow") {
				t.Error("something was installed")
			}
			if files := n.stateFiles(); len(files) != 0 {
				t.Errorf("the state directory has %q", files)
			}
			if len(n.sys.commands) != 0 {
				t.Errorf("ran %q", n.sys.commands)
			}
			if st := artifactOf(t, rep, "/usr/local/bin/tool"); st.Exists || st.From != nil {
				t.Errorf("observed %+v", st)
			}
		})
	}
}

// A redirect to another https URL is followed.
func TestMameletFollowsARedirectToHTTPS(t *testing.T) {
	rel := newReleases(t)
	sum := rel.put("/tool", []byte("tool"))
	rel.handle("/latest", func(w http.ResponseWriter, q *http.Request) { http.Redirect(w, q, "/tool", http.StatusFound) })
	n := artifactNode(t, rel)
	n.offer(Part{Artifacts: []Artifact{install("/usr/local/bin/tool", rel.url("/latest"), sum, "", opA, 1)}}, nil)
	if r := result(t, n.round(), opA); r.Outcome != noderev.OutcomeDone || n.read("/usr/local/bin/tool") != "tool" {
		t.Errorf("result %+v", r)
	}
}

// A download that breaks off fails the attempt and leaves nothing; the next attempt starts over. One
// an earlier mamelet left behind (it was killed in the middle) is removed before the next starts.
func TestMameletStartsAnInterruptedDownloadOver(t *testing.T) {
	rel := newReleases(t)
	body := bytes.Repeat([]byte("x"), 64<<10)
	sum := sha(body)
	var broken atomic.Bool
	broken.Store(true)
	rel.handle("/tool", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if broken.Load() {
			w.Write(body[:1000])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler) // the connection breaks off
		}
		w.Write(body)
	})
	n := artifactNode(t, rel)
	n.offer(Part{Artifacts: []Artifact{install("/usr/local/bin/tool", rel.url("/tool"), sum, "", opA, 1)}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "download") {
		t.Fatalf("result %+v", r)
	}
	if n.exists("/usr/local/bin/tool") || len(n.stateFiles()) != 0 {
		t.Fatalf("left %q", n.stateFiles())
	}
	// A temporary file of a mamelet that was killed while it downloaded, and the next attempt.
	write(t, filepath.Join(n.root, "var/lib/mamelet", downloadName("/usr/local/bin/tool", ".download")), "half a downl")
	broken.Store(false)
	n.offer(Part{Artifacts: []Artifact{install("/usr/local/bin/tool", rel.url("/tool"), sum, "", opA, 2)}}, nil)
	rep = n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || n.read("/usr/local/bin/tool") != string(body) {
		t.Fatalf("result %+v", r)
	}
	if files := n.stateFiles(); !slices.Equal(files, []string{recordName}) {
		t.Errorf("the state directory has %q", files)
	}
}

// The record is mamelet's state on the node: a mamelet that starts again (a new process, the same
// node) still knows the file it installed, and one changed by hand since is not it any more.
func TestMameletsRecordSurvivesARestart(t *testing.T) {
	rel := newReleases(t)
	archive := tarGz(t, entry{name: "x/tool", typeflag: tar.TypeReg, body: "v1"})
	sum := rel.put("/tool.tar.gz", archive)
	n := artifactNode(t, rel)
	n.offer(Part{Artifacts: []Artifact{install("/opt/tool/tool", rel.url("/tool.tar.gz"), sum, "x/tool", opA, 1)}}, nil)
	if r := result(t, n.round(), opA); r.Outcome != noderev.OutcomeDone {
		t.Fatalf("result %+v", r)
	}
	// Another process: its own Agent and System, the node's files and mamelet's state file as they are.
	again := *n
	again.a = &mamelet.Agent{Config: n.a.Config, CP: n.cp, System: &mamelet.System{Runner: n.sys, Root: n.root},
		State: n.a.State, Now: n.a.Now, Version: "test"}
	n.offer(Part{Artifacts: []Artifact{{Path: "/opt/tool/tool", Owner: "root", Group: "root"}}}, nil)
	st := artifactOf(t, again.round(), "/opt/tool/tool")
	if st.From == nil || st.From.SHA256 != sum || st.From.Extract != "x/tool" || st.SHA256 != sha([]byte("v1")) {
		t.Errorf("after a restart: %+v %+v", st, st.From)
	}
	write(t, filepath.Join(n.root, "opt/tool/tool"), "patched by hand")
	if st := artifactOf(t, again.round(), "/opt/tool/tool"); st.From != nil || st.SHA256 != sha([]byte("patched by hand")) {
		t.Errorf("changed by hand: %+v", st)
	}
	// A record that cannot be read: nothing is taken as installed.
	write(t, filepath.Join(n.root, "var/lib/mamelet", recordName), "{")
	write(t, filepath.Join(n.root, "opt/tool/tool"), "v1")
	if st := artifactOf(t, again.round(), "/opt/tool/tool"); st.From != nil {
		t.Errorf("from a broken record: %+v", st)
	}
}

// Installed already (the attempt was interrupted before it said so): the attempt does not download
// again, and still restarts what it names. Another mode is set without a download either.
func TestMameletDoesNotDownloadWhatIsInPlace(t *testing.T) {
	rel := newReleases(t)
	sum := rel.put("/tool", []byte("tool"))
	n := artifactNode(t, rel)
	n.offer(Part{Artifacts: []Artifact{install("/usr/local/bin/tool", rel.url("/tool"), sum, "", opA, 1)}}, nil)
	n.round()
	// The same installation again, as an attempt whose result was lost.
	a := install("/usr/local/bin/tool", rel.url("/tool"), sum, "", opB, 1)
	a.Install.Restart = []string{"tool"}
	n.offer(Part{Artifacts: []Artifact{a}}, nil)
	rep := n.round()
	if r := result(t, rep, opB); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "already as approved") {
		t.Errorf("result %+v", r)
	}
	if rel.count("/tool") != 1 || !slices.Equal(n.sys.commands, []string{"try-restart -- tool"}) {
		t.Errorf("%d downloads, ran %q", rel.count("/tool"), n.sys.commands)
	}
	// Another mode: the installed file is put in place again with it.
	m := install("/usr/local/bin/tool", rel.url("/tool"), sum, "", "op-cccccccccccccccccccccccc", 1)
	m.Install.Mode = "0750"
	n.offer(Part{Artifacts: []Artifact{m}}, nil)
	rep = n.round()
	if r := result(t, rep, "op-cccccccccccccccccccccccc"); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "set /usr/local/bin/tool to 0750 root:root") {
		t.Errorf("result %+v", r)
	}
	if fi, _ := os.Stat(filepath.Join(n.root, "usr/local/bin/tool")); fi.Mode().Perm() != 0o750 || n.read("/usr/local/bin/tool") != "tool" {
		t.Errorf("mode %v", fi.Mode())
	}
	if rel.count("/tool") != 1 {
		t.Errorf("%d downloads", rel.count("/tool"))
	}
	// Changed by hand: the next installation downloads it again.
	write(t, filepath.Join(n.root, "usr/local/bin/tool"), "patched")
	n.offer(Part{Artifacts: []Artifact{install("/usr/local/bin/tool", rel.url("/tool"), sum, "", "op-dddddddddddddddddddddddd", 1)}}, nil)
	if r := result(t, n.round(), "op-dddddddddddddddddddddddd"); r.Outcome != noderev.OutcomeDone || n.read("/usr/local/bin/tool") != "tool" || rel.count("/tool") != 2 {
		t.Errorf("result %+v, %d downloads", r, rel.count("/tool"))
	}
}

// mamelet never installs through a symbolic link, nor where it does not write, nor for an owner the
// node does not have; a unit is read again by systemd before its services restart.
func TestMameletChecksWhereItInstalls(t *testing.T) {
	rel := newReleases(t)
	sum := rel.put("/tool", []byte("tool"))
	n := artifactNode(t, rel)
	write(t, filepath.Join(n.root, "etc/target"), "untouched\n")
	if err := os.Symlink("target", filepath.Join(n.root, "etc/tool")); err != nil {
		t.Fatal(err)
	}
	owned := install("/usr/local/bin/owned", rel.url("/tool"), sum, "", "op-cccccccccccccccccccccccc", 1)
	owned.Install.Owner = "app"
	unit := install("/etc/systemd/system/tool.service", rel.url("/tool"), sum, "", "op-dddddddddddddddddddddddd", 1)
	unit.Install.Mode = "0644"
	unit.Install.Restart = []string{"tool"}
	n.offer(Part{Artifacts: []Artifact{
		install("/etc/tool", rel.url("/tool"), sum, "", opA, 1),
		install("/etc/sudoers.d/tool", rel.url("/tool"), sum, "", opB, 1),
		owned, unit,
	}}, nil)
	rep := n.round()
	for op, say := range map[string]string{opA: "symbolic link", opB: "become root", "op-cccccccccccccccccccccccc": "user app does not exist"} {
		if r := result(t, rep, op); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, say) {
			t.Errorf("%s: %+v", op, r)
		}
	}
	if got := n.read("/etc/target"); got != "untouched\n" {
		t.Errorf("written through the link: %q", got)
	}
	if n.exists("/etc/sudoers.d/tool") || n.exists("/usr/local/bin/owned") {
		t.Error("installed where it was refused")
	}
	if st := artifactOf(t, rep, "/etc/tool"); st.Unsafe == "" {
		t.Errorf("observed %+v", st)
	}
	if st := artifactOf(t, rep, "/etc/sudoers.d/tool"); !strings.Contains(st.Error, "refuses") {
		t.Errorf("observed %+v", st)
	}
	if r := result(t, rep, "op-dddddddddddddddddddddddd"); r.Outcome != noderev.OutcomeDone {
		t.Errorf("the unit: %+v", r)
	}
	if want := []string{"daemon-reload", "try-restart -- tool"}; !slices.Equal(n.sys.commands, want) {
		t.Errorf("ran %q, want %q", n.sys.commands, want)
	}
	if o := observedOf(t, rep); o.Users["app"] || !o.Users["root"] {
		t.Errorf("accounts %v", o.Users)
	}
	if rel.count("/tool") != 1 {
		t.Errorf("%d downloads", rel.count("/tool"))
	}
}

// A failed restart is reported (the artifact stays installed and recorded).
func TestMameletReportsAFailedRestartOfAnArtifact(t *testing.T) {
	rel := newReleases(t)
	sum := rel.put("/tool", []byte("tool"))
	n := artifactNode(t, rel)
	n.sys.fail = "try-restart"
	a := install("/usr/local/bin/tool", rel.url("/tool"), sum, "", opA, 1)
	a.Install.Restart = []string{"tool"}
	n.offer(Part{Artifacts: []Artifact{a}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "restart tool") || !strings.Contains(r.Detail, "installed /usr/local/bin/tool") {
		t.Errorf("result %+v", r)
	}
	if st := artifactOf(t, rep, "/usr/local/bin/tool"); st.From == nil {
		t.Errorf("not recorded: %+v", st)
	}
}
