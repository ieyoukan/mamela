package linux

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
)

// The test's keys: not real keys, only their first bytes are those of an OpenPGP public key.
const (
	testKeyArmored = "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQINBFit2ioBEADhWpZ8/wvZ6hUTiXOwQHXMAlaFHcPH9hAtr4F1y2+OYdbtMuth\n=a9Bc\n-----END PGP PUBLIC KEY BLOCK-----\n"
	testKeyBinary  = "\x99\x02\x0d\x04\x58\xad\xda\x2a\x01\x10\x00\xe1\x5a\x96\x7c"
	testHTML       = "<!DOCTYPE html>\n<html><body><h1>Moved</h1></body></html>\n"
)

// keyServer serves the test's keys over https and counts what it is asked.
type keyServer struct {
	*httptest.Server
	mu    sync.Mutex
	asked map[string]int
}

func newKeyServer(t *testing.T) *keyServer {
	k := &keyServer{asked: map[string]int{}}
	k.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		k.asked[r.URL.Path]++
		k.mu.Unlock()
		switch r.URL.Path {
		case "/armored":
			fmt.Fprint(w, testKeyArmored)
		case "/binary":
			fmt.Fprint(w, testKeyBinary)
		case "/html":
			fmt.Fprint(w, testHTML)
		case "/large":
			w.Write([]byte(strings.Repeat("\x99", MaxAptKey+1)))
		case "/to-http":
			http.Redirect(w, r, "http://example.invalid/key", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(k.Close)
	return k
}

func (k *keyServer) count(p string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.asked[p]
}

// fakeApt is apt-get on the node: an update that succeeds writes the release files of every
// mamela-*.sources file into the lists directory, as apt does; one that fails says why.
type fakeApt struct {
	root       string
	commands   []string
	updateFail string
	// locked: how many updates find apt's lists locked by another apt (and fail at once, as apt does).
	locked int
	// simulate answers apt-get -s install <package> once apt has read a source of these URIs.
	simulate map[string]string
	// unreachable are the URIs an update cannot fetch: as apt does, it says so (Err:, W: Failed to
	// fetch), writes no release file of them, and exits 0.
	unreachable map[string]bool
	// warn is what an update says besides on its errors (and still exits 0).
	warn string
}

func (f *fakeApt) Run(_ context.Context, program string, args []string, env []string) ([]byte, []byte, int, error) {
	switch program {
	case progSystemctl:
		if args[0] == "show" {
			return []byte("LoadState=not-found\nUnitFileState=\nActiveState=inactive\n"), nil, 0, nil
		}
		f.commands = append(f.commands, "systemctl "+strings.Join(args, " "))
		return nil, nil, 0, nil
	case "/usr/bin/dpkg-query":
		return nil, []byte("dpkg-query: no packages found matching " + args[len(args)-1]), 1, nil
	case progAptGet:
	default:
		return nil, nil, -1, fmt.Errorf("mamelet ran %s", program)
	}
	if !slices.Contains(env, "DEBIAN_FRONTEND=noninteractive") {
		return nil, nil, -1, fmt.Errorf("apt-get without DEBIAN_FRONTEND=noninteractive: %v", env)
	}
	if slices.Contains(args, "-s") {
		pkg := args[len(args)-1]
		for uri, line := range f.simulate {
			_, err := os.Stat(filepath.Join(f.root, aptListsDir, aptListsName(uri, "noble", "InRelease")))
			if err == nil && strings.HasPrefix(line, "Inst "+pkg+" ") {
				return []byte(line + "\n"), nil, 0, nil
			}
		}
		return nil, []byte("E: Unable to locate package " + pkg), 100, nil
	}
	f.commands = append(f.commands, "apt-get "+strings.Join(args, " "))
	if f.locked > 0 {
		f.locked--
		return nil, []byte("E: Could not get lock /var/lib/apt/lists/lock. It is held by process 4242 (apt-get)\nE: Unable to lock directory /var/lib/apt/lists/\n"), 100, nil
	}
	if f.updateFail != "" {
		return []byte("Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease\n"), []byte(f.updateFail), 100, nil
	}
	files, _ := filepath.Glob(filepath.Join(f.root, "etc/apt/sources.list.d/mamela-*.sources"))
	out, errOut := "", f.warn
	for _, p := range files {
		b, _ := os.ReadFile(p)
		for i, e := range deb822Entries(p, string(b)) {
			if f.unreachable[e.uri] {
				host := strings.Split(strings.TrimPrefix(strings.TrimPrefix(e.uri, "https://"), "http://"), "/")[0]
				out += fmt.Sprintf("Ign:%d %s %s InRelease\nErr:%d %s %s InRelease\n  Temporary failure resolving '%s'\n", i+1, e.uri, e.suite, i+1, e.uri, e.suite, host)
				errOut += fmt.Sprintf("W: Failed to fetch %s/dists/%s/InRelease  Temporary failure resolving '%s'\n", e.uri, e.suite, host)
				continue
			}
			l := filepath.Join(f.root, aptListsDir, aptListsName(e.uri, e.suite, "InRelease"))
			os.MkdirAll(filepath.Dir(l), 0o755)
			os.WriteFile(l, []byte("signed"), 0o644)
		}
	}
	if len(f.unreachable) > 0 {
		errOut += "W: Some index files failed to download. They have been ignored, or old ones used instead.\n"
	}
	return []byte(out + "Reading package lists...\n"), []byte(errOut), 0, nil
}

// aptNode is a mamelet whose node has apt (fakeApt) and reaches the key server.
func aptNode(t *testing.T) (*mnode, *fakeApt, *keyServer) {
	n := newNode(t)
	apt := &fakeApt{root: n.root}
	n.a.System.Runner = apt
	k := newKeyServer(t)
	n.a.System.HTTP = k.Client()
	return n, apt, k
}

// addGrant is an approved addition of the Docker-like source "docker", its key at keyPath of the key
// server, with the sha256 of key.
func addGrant(k *keyServer, keyPath, key, op string, attempt int) AptRepo {
	s := AptRepoSpec{Name: "docker", URIs: []string{"https://download.docker.com/linux/ubuntu"}, Suites: []string{"noble"},
		Components: []string{"stable"}, KeyURL: k.URL + keyPath, KeySHA256: ContentSHA256(key)}
	armored, _ := aptSourcesSHA256(s)
	return AptRepo{Name: "docker", Where: []AptWhere{{URIs: s.URIs, Suites: s.Suites}}, Add: &AptRepoAdd{URIs: s.URIs, Suites: s.Suites, Components: s.Components,
		KeyURL: s.KeyURL, KeySHA256: s.KeySHA256, SourcesSHA256: armored, Grant: noderev.Once{Operation: op, Attempt: attempt, Action: ActionAddAptRepo}}}
}

func aptState(t *testing.T, rep noderev.Report) AptRepoState {
	t.Helper()
	o := observedOf(t, rep)
	if len(o.AptRepos) != 1 {
		t.Fatalf("observed %+v", o.AptRepos)
	}
	return o.AptRepos[0]
}

// An approved source with an ASCII-armored key: the key in .asc, the source's file naming it, both
// 0644 and root's, then apt-get update; once.
func TestMameletAddsAnAptRepo(t *testing.T) {
	n, apt, k := aptNode(t)
	g := addGrant(k, "/armored", testKeyArmored, opA, 1)
	n.offer(Part{AptRepos: []AptRepo{g}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "ran apt-get update") {
		t.Fatalf("result %+v", r)
	}
	if got := n.read("/etc/apt/keyrings/mamela-docker.asc"); got != testKeyArmored {
		t.Errorf("key %q", got)
	}
	want := AptSources(g.Add.spec("docker"), "/etc/apt/keyrings/mamela-docker.asc")
	if got := n.read("/etc/apt/sources.list.d/mamela-docker.sources"); got != want || !strings.Contains(got, "Signed-By: /etc/apt/keyrings/mamela-docker.asc\n") {
		t.Errorf("sources %q", got)
	}
	for _, p := range []string{"etc/apt/keyrings/mamela-docker.asc", "etc/apt/sources.list.d/mamela-docker.sources"} {
		if fi, _ := os.Stat(filepath.Join(n.root, p)); fi.Mode().Perm() != 0o644 {
			t.Errorf("%s: %v", p, fi.Mode())
		}
	}
	if fi, _ := os.Stat(filepath.Join(n.root, "etc/apt/keyrings")); fi.Mode().Perm() != 0o755 {
		t.Errorf("keyrings: %v", fi.Mode())
	}
	if want := []string{"apt-get -o DPkg::Lock::Timeout=300 update"}; !slices.Equal(apt.commands, want) {
		t.Errorf("ran %q", apt.commands)
	}
	s := aptState(t, rep)
	if !rootFile(s.Sources, ContentSHA256(want)) || !rootFile(s.KeyArmored, armoredSHA) || s.KeyBinary.Exists || len(s.Where[0].Unread) != 0 || len(s.Where[0].Conflicts) != 0 {
		t.Errorf("observed %+v", s)
	}
	// Once: the next round downloads and runs nothing.
	apt.commands = nil
	rep = n.round()
	if len(apt.commands) != 0 || k.count("/armored") != 1 {
		t.Errorf("ran %q, asked %d times", apt.commands, k.count("/armored"))
	}
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone {
		t.Errorf("the result is not reported again: %+v", r)
	}
}

// A binary key goes to .gpg (and Signed-By names it); the key of the other form, an older one, is
// removed.
func TestMameletAddsAnAptRepoWithABinaryKey(t *testing.T) {
	n, _, k := aptNode(t)
	write(t, filepath.Join(n.root, "etc/apt/keyrings/mamela-docker.asc"), "an older key\n")
	g := addGrant(k, "/binary", testKeyBinary, opA, 1)
	g.Add.Before.KeyArmored = Before{Exists: true, SHA256: ContentSHA256("an older key\n")} // what the plan saw
	n.offer(Part{AptRepos: []AptRepo{g}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "removed /etc/apt/keyrings/mamela-docker.asc") {
		t.Fatalf("result %+v", r)
	}
	if n.read("/etc/apt/keyrings/mamela-docker.gpg") != testKeyBinary || n.exists("etc/apt/keyrings/mamela-docker.asc") {
		t.Error("the keys")
	}
	if got := n.read("/etc/apt/sources.list.d/mamela-docker.sources"); !strings.Contains(got, "Signed-By: /etc/apt/keyrings/mamela-docker.gpg\n") {
		t.Errorf("sources %q", got)
	}
	if s := aptState(t, rep); !rootFile(s.KeyBinary, binarySHA) || s.KeyArmored.Exists {
		t.Errorf("observed %+v", s)
	}
}

// What is not the approved key is refused before anything is written: another sha256, an HTML page
// (even one whose sha256 was approved), a download larger than a key, a redirect to http.
func TestMameletRefusesAKeyThatIsNotTheApprovedOne(t *testing.T) {
	for _, c := range []struct {
		name, path, sha, outcome, says string
	}{
		{"another sha256", "/armored", ContentSHA256("another key"), noderev.OutcomeRefused, "not the approved"},
		{"html", "/html", ContentSHA256(testHTML), noderev.OutcomeRefused, "is not an OpenPGP public key"},
		{"large", "/large", ContentSHA256(strings.Repeat("\x99", MaxAptKey+1)), noderev.OutcomeRefused, "larger than"},
		{"redirect to http", "/to-http", armoredSHA, noderev.OutcomeRefused, "not https"},
		{"not found", "/missing", armoredSHA, noderev.OutcomeFailed, "404"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, apt, k := aptNode(t)
			g := addGrant(k, c.path, testKeyArmored, opA, 1)
			g.Add.KeySHA256 = c.sha
			g.Add.SourcesSHA256, _ = aptSourcesSHA256(g.Add.spec("docker"))
			n.offer(Part{AptRepos: []AptRepo{g}}, nil)
			rep := n.round()
			if r := result(t, rep, opA); r.Outcome != c.outcome || !strings.Contains(r.Detail, c.says) || !strings.Contains(r.Detail, "nothing was written") {
				t.Errorf("result %+v", r)
			}
			for _, p := range []string{"etc/apt/keyrings", "etc/apt/sources.list.d/mamela-docker.sources"} {
				if n.exists(p) {
					t.Errorf("%s was written", p)
				}
			}
			if len(apt.commands) != 0 {
				t.Errorf("ran %q", apt.commands)
			}
		})
	}
}

// A failed apt-get update: the outcome says apt's words, the files stay, the report says apt has not
// read the source; a next attempt runs the update again with the key on the node (no download).
func TestMameletRetriesAFailedAptUpdate(t *testing.T) {
	n, apt, k := aptNode(t)
	apt.updateFail = "E: The repository 'https://download.docker.com/linux/ubuntu noble Release' does not have a Release file.\n" + strings.Repeat("x", 2000)
	g := addGrant(k, "/armored", testKeyArmored, opA, 1)
	n.offer(Part{AptRepos: []AptRepo{g}}, nil)
	rep := n.round()
	r := result(t, rep, opA)
	if r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "does not have a Release file") || !strings.Contains(r.Detail, "the files stay") || len(r.Detail) > 1500 {
		t.Fatalf("result %+v", r)
	}
	s := aptState(t, rep)
	if !rootFile(s.KeyArmored, armoredSHA) || !s.Sources.Exists || !slices.Equal(s.Where[0].Unread, []string{"https://download.docker.com/linux/ubuntu noble"}) {
		t.Errorf("observed %+v", s)
	}
	// The same attempt is not made again.
	apt.commands, apt.updateFail = nil, ""
	n.round()
	if len(apt.commands) != 0 {
		t.Errorf("ran %q", apt.commands)
	}
	g.Add.Grant.Attempt = 2
	n.offer(Part{AptRepos: []AptRepo{g}}, nil)
	rep = n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || !strings.Contains(r.Detail, "already as approved") {
		t.Errorf("attempt 2: %+v", r)
	}
	if k.count("/armored") != 1 || len(apt.commands) != 1 {
		t.Errorf("asked %d times, ran %q", k.count("/armored"), apt.commands)
	}
	if s := aptState(t, rep); len(s.Where[0].Unread) != 0 {
		t.Errorf("observed %+v", s)
	}
}

// apt-get update exits 0 when it could not fetch a source (it only warns, as apt does on Debian 12
// and 13 without network): the attempt is not Done. mamelet looks at what apt read of this source
// and said of it: the attempt fails with apt's words about it, the report says so while the grant is
// in the revision, and the plan of the source says it (MAM-PLAN-0152).
func TestMameletSeesAnAptUpdateThatFailedForTheSource(t *testing.T) {
	n, apt, k := aptNode(t)
	apt.unreachable = map[string]bool{"https://download.docker.com/linux/ubuntu": true}
	g := addGrant(k, "/armored", testKeyArmored, opA, 1)
	n.offer(Part{AptRepos: []AptRepo{g}}, nil)
	rep := n.round()
	r := result(t, rep, opA)
	if r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "did not read https://download.docker.com/linux/ubuntu noble") ||
		!strings.Contains(r.Detail, "Temporary failure resolving 'download.docker.com'") || !strings.Contains(r.Detail, "the files stay") {
		t.Fatalf("result %+v", r)
	}
	s := aptState(t, rep)
	if !strings.Contains(s.Failed, "Temporary failure resolving") || !s.Sources.Exists {
		t.Errorf("observed %+v", s)
	}
	// The plan says why, and the hint what to fix.
	g2, err := decodeGraph(aptRes("web/apt-repo/docker", strings.Replace(dockerSpec(""), "https://download.docker.com/linux/ubuntu/gpg", k.URL+"/armored", 1)))
	if err != nil {
		t.Fatal(err)
	}
	o := observedOf(t, rep)
	p := plan.Build(g2, world{mamelet: Release, accepts: Version, observed: &o}.snapshot(g2), policy.Default)
	if c := findChange(t, p, "NodeAptRepo web/apt-repo/docker"); c.Note == nil || c.Note.Code != codeAptRepoUnread ||
		!strings.Contains(c.Note.Message, "the last attempt failed") || !strings.Contains(c.Note.Message, "Temporary failure resolving") || !strings.Contains(c.Note.Hint, "fix what the last attempt says") {
		t.Errorf("plan %+v %+v", c, c.Note)
	}
	// Reached once more (a new attempt): done, and the report says nothing of a failure any more.
	apt.unreachable = nil
	g.Add.Grant.Attempt = 2
	n.offer(Part{AptRepos: []AptRepo{g}}, nil)
	rep = n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone {
		t.Fatalf("attempt 2: %+v", r)
	}
	if s := aptState(t, rep); s.Failed != "" || len(s.Where[0].Unread) != 0 {
		t.Errorf("observed %+v", s)
	}
}

// What apt says decides only for this source: a failure it names (a key that does not sign it, read
// before and kept as it was) fails the attempt; a failure of another source, a warning that is no
// failure (a weak signature's algorithm) and a notice do not.
func TestMameletReadsWhatAptSaidOfTheSource(t *testing.T) {
	for _, c := range []struct {
		name, warn string
		read       bool // apt read a release file of it before
		fails      string
	}{
		{"a key that does not sign it", "W: GPG error: https://download.docker.com/linux/ubuntu noble InRelease: The following signatures couldn't be verified because the public key is not available: NO_PUBKEY 7EA0A9C3F273FCD8\n" +
			"E: The repository 'https://download.docker.com/linux/ubuntu noble InRelease' is not signed.\n", true, "could not read this source again"},
		{"another source", "W: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/noble/InRelease  Temporary failure resolving 'archive.ubuntu.com'\n" +
			"W: Some index files failed to download. They have been ignored, or old ones used instead.\n", false, ""},
		{"a source whose URI starts like it", "W: Failed to fetch https://download.docker.com/linux/ubuntu-old/dists/noble/InRelease  404  Not Found\n", false, ""},
		{"a weak signature", "W: https://download.docker.com/linux/ubuntu/dists/noble/InRelease: Signature by key 9DC858229FC7DD38854AE2D88D81803C0EBFCD88 uses weak algorithm (rsa1024)\n" +
			"N: Skipping acquire of configured file 'stable/binary-i386/Packages' as repository 'https://download.docker.com/linux/ubuntu noble InRelease' doesn't support architecture 'i386'\n", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, apt, k := aptNode(t)
			apt.warn = c.warn
			if c.read {
				write(t, filepath.Join(n.root, aptListsDir, aptListsName("https://download.docker.com/linux/ubuntu", "noble", "InRelease")), "signed")
			}
			n.offer(Part{AptRepos: []AptRepo{addGrant(k, "/armored", testKeyArmored, opA, 1)}}, nil)
			r := result(t, n.round(), opA)
			switch {
			case c.fails == "" && r.Outcome != noderev.OutcomeDone:
				t.Errorf("failed: %+v", r)
			case c.fails != "" && (r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, c.fails) || !strings.Contains(r.Detail, "NO_PUBKEY")):
				t.Errorf("result %+v, want it to say %q", r, c.fails)
			}
		})
	}
	// What apt said is cut between two characters, and made valid UTF-8 (the report is JSON).
	if got := bounded("ab\xffcdé", 7); got != "ab\uFFFDcd..." {
		t.Errorf("bounded: %q", got)
	}
	if got := bounded(strings.Repeat("é", 10), 5); got != "éé..." {
		t.Errorf("bounded: %q", got)
	}
	for line, want := range map[string]bool{
		"Err:1 https://example.com/apt stable InRelease":         true,
		"W: Failed to fetch https://example.com/apt/dists/x":     true,
		"E: The repository 'https://example.com/apt stable' x":   true,
		"https://example.com/apt":                                true,
		"W: Failed to fetch https://example.com/apt2/dists/x":    false,
		"W: Failed to fetch https://example.com/apt.old/dists/x": false,
		"Err:1 https://example.com/ap stable InRelease":          false,
	} {
		if got := mentionsURI(line, "https://example.com/apt/"); got != want {
			t.Errorf("%q names the URI: %v", line, got)
		}
	}
}

// mamelet was killed after it wrote the key, before it said so: the same attempt takes the key from
// the node (no download), writes the source's file and runs the update.
func TestMameletFinishesAnInterruptedAptRepo(t *testing.T) {
	n, apt, k := aptNode(t)
	write(t, filepath.Join(n.root, "etc/apt/keyrings/mamela-docker.asc"), testKeyArmored)
	tmp := filepath.Join(n.root, "etc/apt/sources.list.d", tempName("mamela-docker.sources"))
	write(t, tmp, "Types: deb\nURIs: htt")
	g := addGrant(k, "/armored", testKeyArmored, opA, 1)
	n.offer(Part{AptRepos: []AptRepo{g}}, nil)
	rep := n.round()
	if r := result(t, rep, opA); r.Outcome != noderev.OutcomeDone || strings.Contains(r.Detail, "downloaded") || !strings.Contains(r.Detail, "wrote /etc/apt/sources.list.d/mamela-docker.sources") {
		t.Fatalf("result %+v", r)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Error("the temporary file is still there")
	}
	if k.count("/armored") != 0 || len(apt.commands) != 1 {
		t.Errorf("asked %d times, ran %q", k.count("/armored"), apt.commands)
	}
	if !strings.Contains(n.read("/etc/apt/sources.list.d/mamela-docker.sources"), "Signed-By: /etc/apt/keyrings/mamela-docker.asc") {
		t.Error("the source")
	}
}

// mamelet never writes through a link, and never adds a source that another file of the node names
// already (apt would refuse both, and every other source with them).
func TestMameletRefusesUnsafeOrConflictingAptRepos(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(n *mnode)
		says  string
	}{
		{"the source's file is a link", func(n *mnode) {
			write(t, filepath.Join(n.root, "etc/target"), "untouched\n")
			os.MkdirAll(filepath.Join(n.root, "etc/apt/sources.list.d"), 0o755)
			if err := os.Symlink("../../target", filepath.Join(n.root, "etc/apt/sources.list.d/mamela-docker.sources")); err != nil {
				t.Fatal(err)
			}
		}, "symbolic link"},
		{"the keyrings are a link", func(n *mnode) {
			write(t, filepath.Join(n.root, "etc/real/x"), "")
			os.MkdirAll(filepath.Join(n.root, "etc/apt"), 0o755)
			if err := os.Symlink("../real", filepath.Join(n.root, "etc/apt/keyrings")); err != nil {
				t.Fatal(err)
			}
		}, "symbolic link"},
		{"docker.list names it", func(n *mnode) {
			write(t, filepath.Join(n.root, "etc/apt/sources.list.d/docker.list"),
				"deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable\n")
		}, "docker.list names a URI and suite"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, apt, k := aptNode(t)
			c.setup(n)
			n.offer(Part{AptRepos: []AptRepo{addGrant(k, "/armored", testKeyArmored, opA, 1)}}, nil)
			rep := n.round()
			if r := result(t, rep, opA); r.Outcome != noderev.OutcomeRefused || !strings.Contains(r.Detail, c.says) {
				t.Errorf("result %+v", r)
			}
			if b, err := os.ReadFile(filepath.Join(n.root, "etc/target")); err == nil && string(b) != "untouched\n" {
				t.Errorf("written through the link: %q", b)
			}
			if n.exists("etc/real/mamela-docker.asc") || len(apt.commands) != 0 || k.count("/armored") != 0 {
				t.Errorf("acted: %q", apt.commands)
			}
			s := aptState(t, rep)
			if c.name == "docker.list names it" && !slices.Equal(s.Where[0].Conflicts, []string{"/etc/apt/sources.list.d/docker.list"}) {
				t.Errorf("observed %+v", s)
			}
		})
	}
}

// A part whose source is not the approved one (or not one Mamela writes) is refused before anything
// is looked at.
func TestCheckPartAptRepos(t *testing.T) {
	k := &keyServer{Server: &httptest.Server{URL: "https://keys.example"}}
	ok := addGrant(k, "/armored", testKeyArmored, opA, 1)
	if err := checkPart(encode(Part{AptRepos: []AptRepo{ok}})); err != nil {
		t.Fatal(err)
	}
	binary := addGrant(k, "/binary", testKeyBinary, opA, 1)
	_, binary.Add.SourcesSHA256 = aptSourcesSHA256(binary.Add.spec("docker"))
	if err := checkPart(encode(Part{AptRepos: []AptRepo{binary}})); err != nil {
		t.Errorf("the file for a binary key: %v", err)
	}
	for name, f := range map[string]func(r *AptRepo){
		"sources sha256": func(r *AptRepo) { r.Add.SourcesSHA256 = ContentSHA256("something else") },
		"suite changed":  func(r *AptRepo) { r.Add.Suites = []string{"jammy"} },
		"injected URI":   func(r *AptRepo) { r.Add.URIs = []string{"https://a.example/x\nSigned-By: /evil"} },
		"http key":       func(r *AptRepo) { r.Add.KeyURL = "http://keys.example/armored" },
		"action":         func(r *AptRepo) { r.Add.Grant.Action = ActionWrite },
		"attempt":        func(r *AptRepo) { r.Add.Grant.Attempt = 0 },
		"name":           func(r *AptRepo) { r.Name = "../../etc/x" },
		"observed URI":   func(r *AptRepo) { r.Where[0].URIs = []string{"file:/x"} },
	} {
		c := addGrant(k, "/armored", testKeyArmored, opA, 1)
		f(&c)
		if err := checkPart(encode(Part{AptRepos: []AptRepo{c}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := checkPart(encode(Part{AptRepos: []AptRepo{ok, ok}})); err == nil {
		t.Error("a source twice")
	}
	// The whole part is refused (and reported), nothing is written.
	n, apt, ks := aptNode(t)
	bad := addGrant(ks, "/armored", testKeyArmored, opA, 1)
	bad.Add.SourcesSHA256 = ContentSHA256("x")
	n.offer(Part{AptRepos: []AptRepo{bad}}, nil)
	rep := n.round()
	if !strings.Contains(rep.ExtRefused[Name], "not the one approved") || n.exists("etc/apt/keyrings") || len(apt.commands) != 0 {
		t.Errorf("refused %v, ran %q", rep.ExtRefused, apt.commands)
	}
}
