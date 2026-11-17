package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/api"
	"github.com/ieyoukan/mamela/internal/gitops"
	"github.com/ieyoukan/mamela/internal/gitops/fakegithub"
)

// lockedBuffer is a log that the daemon writes while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// nothingBackend is a Control Plane that is asked for nothing in these tests.
type nothingBackend struct{}

func (nothingBackend) Plan(context.Context, string) (api.Planned, error)            { return api.Planned{}, nil }
func (nothingBackend) Revision(context.Context, string) (string, error)             { return "", nil }
func (nothingBackend) Approve(context.Context, string, string, time.Duration) error { return nil }
func (nothingBackend) Approved(context.Context, string) (bool, error)               { return false, nil }
func (nothingBackend) Apply(context.Context, string) (api.Applied, error)           { return api.Applied{}, nil }

// site is a mamelacd configuration with a fake GitHub and a Control Plane's API on a socket.
type site struct {
	gh     *fakegithub.GitHub
	config string
	token  string
	db     string
}

func newSite(t *testing.T) *site {
	t.Helper()
	dir := t.TempDir()
	gh := fakegithub.New("me/infra", "tok1")
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	sockDir, err := os.MkdirTemp("", "mcd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "api.sock")
	l, err := api.Listen(socket, "")
	if err != nil {
		t.Fatal(err)
	}
	cp := &http.Server{Handler: (&api.Server{Backend: nothingBackend{}, WorkDir: filepath.Join(dir, "trees"), Mamela: "test"}).Handler()}
	go cp.Serve(l)
	t.Cleanup(func() { cp.Close() })

	s := &site{gh: gh, token: filepath.Join(dir, "github-token"), db: filepath.Join(dir, "mamelacd.db"), config: filepath.Join(dir, "mamelacd.toml")}
	os.WriteFile(s.token, []byte("tok1\n"), 0o600)
	os.WriteFile(s.config, []byte(`repo = "me/infra"
branch = "main"
dirs = ["infra"]
api_url = "`+srv.URL+`"
token_file = "`+s.token+`"
authors = ["alice"]
approvers = ["alice"]
interval = "10s"
mamela_socket = "`+socket+`"
state_db = "`+s.db+`"
`), 0o600)
	// A head that no pull request of an author made: it is looked at and left alone.
	tree := t.TempDir()
	os.WriteFile(filepath.Join(tree, "README"), []byte("x"), 0o644)
	sha, err := gh.AddTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	gh.SetBranch("main", sha)
	return s
}

// created makes the database before the daemon runs: a test reads it while the daemon writes it, and
// two openers of a database that does not exist yet are not what is tested here.
func (s *site) created(t *testing.T) *site {
	t.Helper()
	st, err := gitops.OpenStore(context.Background(), s.db, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	return s
}

func TestUsageAndVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "mamelacd status") {
		t.Fatalf("no arguments: %d %q", code, errb.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"version"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "mamelacd ") {
		t.Fatalf("version: %d %q", code, out.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"nonsense"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), `unknown command "nonsense"`) {
		t.Fatalf("unknown: %d %q", code, errb.String())
	}
	for _, args := range [][]string{{"status"}, {"repin", "--config", "x"}, {"once", "--config", "x", "extra"}} {
		errb.Reset()
		if code := run(context.Background(), args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "Usage:") {
			t.Errorf("%v: %d %q", args, code, errb.String())
		}
	}
	errb.Reset()
	if code := run(context.Background(), []string{"status", "--config", filepath.Join(t.TempDir(), "none.toml")}, &out, &errb); code != 1 || errb.Len() == 0 {
		t.Fatalf("no configuration: %d %q", code, errb.String())
	}
}

// `mamelacd status` shows the heartbeat the daemon leaves, and exits 1 when it looks stalled.
func TestStatusCommand(t *testing.T) {
	s := newSite(t)
	var out, errb bytes.Buffer
	// Before any poll: no database.
	if code := run(context.Background(), []string{"status", "--config", s.config}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no state database") {
		t.Fatalf("before the first run: %d %q", code, errb.String())
	}
	// One poll, as the daemon does it.
	s.created(t)
	ctx, stop := context.WithCancel(context.Background())
	logs := &lockedBuffer{}
	done := make(chan int)
	go func() { done <- run(ctx, []string{"run", "--config", s.config}, &out, logs) }()
	waitFor(t, "the first poll", func() bool { return heartbeat(t, s).Polls >= 1 && heartbeat(t, s).PollEndMs > 0 })
	stop()
	if code := <-done; code != 0 {
		t.Fatalf("run: %d\n%s", code, logs.String())
	}
	out.Reset()
	errb.Reset()
	if code := run(context.Background(), []string{"status", "--config", s.config}, &out, &errb); code != 0 {
		t.Fatalf("status of a mamelacd that just polled: %d\n%s\n%s", code, out.String(), errb.String())
	}
	for _, want := range []string{"me/infra (main)", "last finished", "heads:", "infra", "@alice id "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(logs.String(), "following me/infra (main)") || !strings.Contains(logs.String(), "mamelacd status --config") {
		t.Errorf("the start line does not say where to look: %s", logs.String())
	}
	// A heartbeat that is old: stalled.
	st, err := gitops.OpenStore(context.Background(), s.db, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	hb, _, _ := st.LastHeartbeat(context.Background())
	old := time.Now().Add(-30 * time.Minute).UnixMilli()
	hb.PollStartMs, hb.PollEndMs, hb.NextMs = old, old, old+10_000
	st.SetHeartbeat(context.Background(), hb)
	st.Close()
	out.Reset()
	if code := run(context.Background(), []string{"status", "--config", s.config}, &out, &errb); code != 1 || !strings.Contains(out.String(), "PROBLEM: no poll finished for") || !strings.Contains(out.String(), "MAM-GIT-0501") {
		t.Fatalf("a stalled mamelacd: %d\n%s", code, out.String())
	}
	// repin: nothing is pinned for bob; alice is.
	errb.Reset()
	if code := run(context.Background(), []string{"repin", "--config", s.config, "--login", "bob"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no account is pinned for @bob") {
		t.Fatalf("repin bob: %d %q", code, errb.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"repin", "--config", s.config, "--login", "alice"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "forgot the account pinned for @alice") {
		t.Fatalf("repin alice: %d %q", code, out.String())
	}
}

// A SIGHUP makes `mamelacd run` read the token file again, and poll with the new token at once; a
// token GitHub refuses is said in a line that says what to do.
func TestRunReloadsTokenOnSIGHUP(t *testing.T) {
	s := newSite(t).created(t)
	ctx, stop := context.WithCancel(context.Background())
	logs := &lockedBuffer{}
	var out bytes.Buffer
	done := make(chan int)
	go func() { done <- run(ctx, []string{"run", "--config", s.config}, &out, logs) }()
	waitFor(t, "the first poll ("+logs.String()+")", func() bool { return heartbeat(t, s).PollEndMs > 0 })
	polls := heartbeat(t, s).Polls

	// GitHub revokes the token; the next poll is refused; the file holds the same (old) token. (A
	// SIGHUP makes the poll come now instead of at the interval.)
	s.gh.SetToken("tok2")
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the refusal", func() bool { return heartbeat(t, s).Result == "error" })
	if !strings.Contains(logs.String(), "MAM-GIT-0001") || !strings.Contains(logs.String(), "SIGHUP") {
		t.Fatalf("the refusal does not say what to do:\n%s", logs.String())
	}
	// The operator writes the new token and sends SIGHUP: no restart, no waiting for the interval.
	os.WriteFile(s.token, []byte("tok2\n"), 0o600)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a poll with the new token", func() bool { hb := heartbeat(t, s); return hb.Result == "ok" && hb.Polls > polls })
	if !strings.Contains(logs.String(), "SIGHUP: the token file was read again") && !strings.Contains(logs.String(), "holds another one") {
		t.Fatalf("the reload is not in the log:\n%s", logs.String())
	}
	stop()
	if code := <-done; code != 0 {
		t.Fatalf("run: %d\n%s", code, logs.String())
	}
}

func heartbeat(t *testing.T, s *site) gitops.Heartbeat {
	t.Helper()
	st, err := gitops.OpenStore(context.Background(), s.db, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hb, _, err := st.LastHeartbeat(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return hb
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
