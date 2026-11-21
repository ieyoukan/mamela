package pin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/sumfile"
)

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fixture is a TLS server of downloads (path -> bytes) and of a GitHub API, and a Pinner that uses it.
type fixture struct {
	srv   *httptest.Server
	files map[string][]byte
	// release is what the GitHub API says of the assets of /o/r/releases/download/v1+x/<asset>
	// (asset -> digest; "" for null).
	release   map[string]string
	apiStatus int
	hits      atomic.Int32
	onGet     func(path string) // called before a download is answered
	pinner    *Pinner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{files: map[string][]byte{}, apiStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/repos/o/r/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("tag") != "v1+x" || f.release == nil {
			http.NotFound(w, r)
			return
		}
		if f.apiStatus != http.StatusOK {
			w.WriteHeader(f.apiStatus)
			return
		}
		var assets []string
		for name, d := range f.release {
			digest := "null"
			if d != "" {
				digest = `"` + d + `"`
			}
			assets = append(assets, fmt.Sprintf(`{"name":%q,"digest":%s,"size":1}`, name, digest))
		}
		fmt.Fprintf(w, `{"id":7,"tag_name":"v1+x","assets":[%s]}`, strings.Join(assets, ","))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.onGet != nil {
			f.onGet(r.URL.Path)
		}
		b, ok := f.files[r.URL.EscapedPath()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	u, _ := url.Parse(f.srv.URL)
	f.pinner = &Pinner{Client: f.srv.Client(), GitHubHost: u.Host, GitHubAPI: f.srv.URL + "/api"}
	return f
}

// file serves body at path and returns its URL.
func (f *fixture) file(path string, body []byte) string {
	f.files[path] = body
	return f.srv.URL + path
}

func artifactPin(u string) graph.Pin {
	return graph.Pin{URL: u, Resource: "NodeArtifact web/artifact/usr/local/bin/" + filepath.Base(u), Field: "spec.sha256"}
}

func keyPin(u string) graph.Pin {
	return graph.Pin{URL: u, Resource: "NodeAptRepo web/apt-repo/vendor", Field: "spec.keySha256", Key: true}
}

func writeSum(t *testing.T, dir string, entries ...sumfile.Entry) []byte {
	t.Helper()
	data, err := sumfile.Format(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sumfile.Name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

func readSum(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, sumfile.Name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// onlySum checks that dir has mamela.sum and nothing else (no temporary file left behind).
func onlySum(t *testing.T, dir string) {
	t.Helper()
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if e.Name() != sumfile.Name {
			t.Errorf("left in the directory: %s", e.Name())
		}
	}
}

func codeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// mamela pin downloads what mamela.sum lacks, keeps what it has and the declaration needs, removes
// what it no longer needs, and writes the lines in the order of their URLs.
func TestPinAddsRemovesAndSorts(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	zeta := f.file("/zeta/app.tar.gz", []byte("zeta"))
	alpha := f.file("/alpha/app", []byte("alpha"))
	key := f.file("/vendor/gpg.key", []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n"))
	kept := sumfile.Entry{URL: f.srv.URL + "/kept", SHA256: strings.Repeat("c", 64)} // not downloaded again
	old := sumfile.Entry{URL: f.srv.URL + "/old", SHA256: strings.Repeat("d", 64)}
	writeSum(t, dir, old, kept)
	var out bytes.Buffer
	pins := []graph.Pin{artifactPin(zeta), keyPin(key), artifactPin(kept.URL), artifactPin(alpha), artifactPin(zeta)}
	if err := f.pinner.Run(context.Background(), dir, pins, Update, &out); err != nil {
		t.Fatal(err)
	}
	want, _ := sumfile.Format([]sumfile.Entry{
		{URL: alpha, SHA256: hexSum([]byte("alpha"))},
		{URL: kept.URL, SHA256: kept.SHA256},
		{URL: key, SHA256: hexSum([]byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n"))},
		{URL: zeta, SHA256: hexSum([]byte("zeta"))},
	})
	if got := readSum(t, dir); !bytes.Equal(got, want) {
		t.Errorf("mamela.sum:\n%s\nwant:\n%s", got, want)
	}
	if n := f.hits.Load(); n != 3 {
		t.Errorf("%d downloads, want 3 (each URL lacking once)", n)
	}
	onlySum(t, dir)
	text := out.String()
	for _, w := range []string{"3 added, 1 removed", "+ " + alpha + " sha256:" + hexSum([]byte("alpha")), "- " + old.URL,
		"no longer used", "a signing key", "fingerprint", "gpg --show-keys", "NodeAptRepo web/apt-repo/vendor spec.keySha256"} {
		if !strings.Contains(text, w) {
			t.Errorf("the output lacks %q:\n%s", w, text)
		}
	}
	if strings.Count(text, "a signing key") != 1 {
		t.Errorf("the key's reminder is for the key only:\n%s", text)
	}

	// Again: nothing to do, nothing downloaded or written.
	before, _ := os.Stat(filepath.Join(dir, sumfile.Name))
	out.Reset()
	if err := f.pinner.Run(context.Background(), dir, pins, Update, &out); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(dir, sumfile.Name))
	if f.hits.Load() != 3 || !strings.Contains(out.String(), "up to date") || !os.SameFile(before, after) {
		t.Errorf("an up-to-date mamela.sum was downloaded for or written: %s", out.String())
	}
}

// A declaration that leaves nothing to mamela.sum, and has none, gets none.
func TestPinWritesNoFileForNothing(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	var out bytes.Buffer
	if err := f.pinner.Run(context.Background(), dir, nil, Update, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, sumfile.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("mamela.sum was written: %v", err)
	}
}

// Whatever fails, mamela.sum stays as it was: a download that fails, is refused, or differs from
// GitHub's digest adds nothing, and the other downloads are not written either.
func TestPinWritesNothingWhenADownloadFails(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 64)
	for name, c := range map[string]struct {
		setup func(f *fixture) string // the URL that fails
		code  string
		why   string
	}{
		"not found": {func(f *fixture) string { return f.srv.URL + "/missing" }, CodeDownloadFailed, "404"},
		"too large (said)": {func(f *fixture) string {
			f.pinner.MaxBytes = 16
			return f.file("/big", big)
		}, CodeDownloadRefused, "64 bytes"},
		"too large (not said)": {func(f *fixture) string {
			f.pinner.MaxBytes = 16
			f.files["/stream"] = nil
			f.onGet = nil
			mux := f.srv.Config.Handler.(*http.ServeMux)
			mux.HandleFunc("GET /chunked", func(w http.ResponseWriter, r *http.Request) {
				for range 8 {
					w.Write(big[:8])
					w.(http.Flusher).Flush()
				}
			})
			return f.srv.URL + "/chunked"
		}, CodeDownloadRefused, "larger than"},
		"redirect to http": {func(f *fixture) string {
			mux := f.srv.Config.Handler.(*http.ServeMux)
			mux.HandleFunc("GET /to-http", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "http://"+r.Host+"/plain", http.StatusFound)
			})
			return f.srv.URL + "/to-http"
		}, CodeDownloadRefused, "not https"},
		"http": {func(f *fixture) string { return "http://example.invalid/x" }, CodeDownloadRefused, "not https"},
		"github digest differs": {func(f *fixture) string {
			f.release = map[string]string{"app": "sha256:" + hexSum([]byte("what was uploaded"))}
			return f.file("/o/r/releases/download/v1%2Bx/app", []byte("what was downloaded"))
		}, CodeGitHubMismatch, "GitHub lists sha256:" + hexSum([]byte("what was uploaded"))},
		"github release lacks the asset": {func(f *fixture) string {
			f.release = map[string]string{"other": ""}
			return f.file("/o/r/releases/download/v1%2Bx/app", []byte("app"))
		}, CodeGitHubMismatch, "lists no asset app"},
		"github api refuses": {func(f *fixture) string {
			f.release = map[string]string{"app": ""}
			f.apiStatus = http.StatusForbidden
			return f.file("/o/r/releases/download/v1%2Bx/app", []byte("app"))
		}, CodeGitHubUnavailable, "403"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			dir := t.TempDir()
			good := f.file("/good", []byte("good"))
			before := writeSum(t, dir, sumfile.Entry{URL: f.srv.URL + "/old", SHA256: strings.Repeat("d", 64)})
			bad := c.setup(f)
			var out bytes.Buffer
			err := f.pinner.Run(context.Background(), dir, []graph.Pin{artifactPin(good), artifactPin(bad)}, Update, &out)
			if codeOf(err) != c.code || !strings.Contains(err.Error(), c.why) || !strings.Contains(err.Error(), "was not written") {
				t.Errorf("err = %v, want %s (%s)", err, c.code, c.why)
			}
			if got := readSum(t, dir); !bytes.Equal(got, before) {
				t.Errorf("mamela.sum changed:\n%s", got)
			}
			onlySum(t, dir)
		})
	}
}

// A redirect to https is followed (a release's download is redirected to its storage).
func TestPinFollowsARedirectToHTTPS(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.file("/storage/app", []byte("app"))
	f.srv.Config.Handler.(*http.ServeMux).HandleFunc("GET /download/app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/storage/app", http.StatusFound)
	})
	u := f.srv.URL + "/download/app"
	if err := f.pinner.Run(context.Background(), dir, []graph.Pin{artifactPin(u)}, Update, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := sumfile.Read(dir); func() string { h, _ := s.Lookup(u); return h }() != hexSum([]byte("app")) {
		t.Errorf("mamela.sum: %s", readSum(t, dir))
	}
}

// A GitHub release's asset is compared with the digest GitHub lists; one without a digest (uploaded
// before GitHub computed them) is pinned, and the output says so.
func TestPinComparesAGitHubAssetWithItsDigest(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	app := f.file("/o/r/releases/download/v1%2Bx/app", []byte("app"))
	old := f.file("/o/r/releases/download/v1%2Bx/old", []byte("old"))
	f.release = map[string]string{"app": "sha256:" + hexSum([]byte("app")), "old": ""}
	var out bytes.Buffer
	if err := f.pinner.Run(context.Background(), dir, []graph.Pin{artifactPin(app), artifactPin(old)}, Update, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "GitHub's release lists the same sha256") || !strings.Contains(out.String(), "GitHub lists no sha256") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestPinSplitsAGitHubReleaseURL(t *testing.T) {
	p := &Pinner{}
	owner, repo, tag, asset, ok := p.githubAsset("https://github.com/k3s-io/k3s/releases/download/v1.37.2%2Bk3s1/k3s")
	if !ok || owner != "k3s-io" || repo != "k3s" || tag != "v1.37.2+k3s1" || asset != "k3s" {
		t.Errorf("= %s %s %s %s %v", owner, repo, tag, asset, ok)
	}
	for _, u := range []string{
		"https://example.com/k3s-io/k3s/releases/download/v1/k3s",
		"https://github.com/k3s-io/k3s/archive/v1.tar.gz",
		"https://github.com/k3s-io/k3s/releases/download/v1/k3s?x=1",
		"https://github.com/k3s-io/k3s/releases/download/v1/dir/k3s",
		"http://github.com/k3s-io/k3s/releases/download/v1/k3s",
	} {
		if _, _, _, _, ok := p.githubAsset(u); ok {
			t.Errorf("%s was taken for a release's asset", u)
		}
	}
}

// mamela.sum changed by someone else while mamela pin downloaded is not overwritten.
func TestPinRefusesAFileThatChangedMeanwhile(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	writeSum(t, dir)
	theirs, _ := sumfile.Format([]sumfile.Entry{{URL: "https://example.org/theirs", SHA256: strings.Repeat("e", 64)}})
	f.onGet = func(string) { os.WriteFile(filepath.Join(dir, sumfile.Name), theirs, 0o644) }
	u := f.file("/app", []byte("app"))
	err := f.pinner.Run(context.Background(), dir, []graph.Pin{artifactPin(u)}, Update, &bytes.Buffer{})
	if codeOf(err) != CodeSumChanged {
		t.Fatalf("err = %v, want %s", err, CodeSumChanged)
	}
	if got := readSum(t, dir); !bytes.Equal(got, theirs) {
		t.Errorf("the other change was overwritten:\n%s", got)
	}
	onlySum(t, dir)

	// A mamela.sum that appears meanwhile, where there was none, is not overwritten either.
	dir2 := t.TempDir()
	f.onGet = func(string) { os.WriteFile(filepath.Join(dir2, sumfile.Name), theirs, 0o644) }
	if err := f.pinner.Run(context.Background(), dir2, []graph.Pin{artifactPin(u)}, Update, &bytes.Buffer{}); codeOf(err) != CodeSumChanged {
		t.Errorf("err = %v", err)
	}
}

// failingTransport fails the test when anything is sent: --check uses no network.
type failingTransport struct{ t *testing.T }

func (f failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("--check sent a request: %s", r.URL)
	return nil, errors.New("no network")
}

func TestCheckIsOfflineAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	have := sumfile.Entry{URL: "https://example.org/have", SHA256: strings.Repeat("a", 64)}
	unused := sumfile.Entry{URL: "https://example.org/unused", SHA256: strings.Repeat("b", 64)}
	before := writeSum(t, dir, have, unused)
	p := &Pinner{Client: &http.Client{Transport: failingTransport{t}}}
	var out bytes.Buffer
	err := p.Run(context.Background(), dir, []graph.Pin{artifactPin(have.URL), artifactPin("https://example.org/lacking")}, Check, &out)
	if codeOf(err) != CodeMissing || !strings.Contains(err.Error(), "MAM-EVAL-0601") {
		t.Errorf("err = %v, want %s", err, CodeMissing)
	}
	if !strings.Contains(out.String(), "missing  https://example.org/lacking") || !strings.Contains(out.String(), "unused   "+unused.URL) {
		t.Errorf("output:\n%s", out.String())
	}
	// Only unused lines: reported, and not a failure.
	out.Reset()
	if err := p.Run(context.Background(), dir, []graph.Pin{artifactPin(have.URL)}, Check, &out); err != nil {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(out.String(), "no longer needs") {
		t.Errorf("output:\n%s", out.String())
	}
	if got := readSum(t, dir); !bytes.Equal(got, before) {
		t.Errorf("--check wrote mamela.sum:\n%s", got)
	}
}

func TestVerifyDownloadsEveryLineAgain(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	same := f.file("/same", []byte("same"))
	changed := f.file("/changed", []byte("published again"))
	before := writeSum(t, dir, sumfile.Entry{URL: same, SHA256: hexSum([]byte("same"))}, sumfile.Entry{URL: changed, SHA256: hexSum([]byte("first"))})
	var out bytes.Buffer
	err := f.pinner.Run(context.Background(), dir, []graph.Pin{artifactPin(same), artifactPin(changed)}, Verify, &out)
	if codeOf(err) != CodeVerifyMismatch {
		t.Errorf("err = %v, want %s", err, CodeVerifyMismatch)
	}
	if !strings.Contains(out.String(), "ok       "+same) || !strings.Contains(out.String(), "DIFFERS  "+changed) || f.hits.Load() != 2 {
		t.Errorf("output (%d downloads):\n%s", f.hits.Load(), out.String())
	}
	if got := readSum(t, dir); !bytes.Equal(got, before) {
		t.Errorf("--verify wrote mamela.sum:\n%s", got)
	}
	f.files["/changed"] = []byte("first")
	if err := f.pinner.Run(context.Background(), dir, []graph.Pin{artifactPin(same), artifactPin(changed)}, Verify, &out); err != nil {
		t.Errorf("err = %v", err)
	}
}

// A mamela.sum that is not the tool's is not read, and nothing is downloaded for it.
func TestPinRefusesAMalformedFile(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, sumfile.Name), []byte("https://b.example/x sha256:"+strings.Repeat("a", 64)+"\nhttps://a.example/x sha256:"+strings.Repeat("a", 64)+"\n"), 0o644)
	u := f.file("/app", []byte("app"))
	for _, mode := range []Mode{Update, Check, Verify} {
		err := f.pinner.Run(context.Background(), dir, []graph.Pin{artifactPin(u)}, mode, &bytes.Buffer{})
		if codeOf(err) != CodeSumMalformed || !strings.Contains(err.Error(), "order") {
			t.Errorf("mode %d: err = %v", mode, err)
		}
	}
	if f.hits.Load() != 0 {
		t.Error("downloaded for a malformed mamela.sum")
	}
}
