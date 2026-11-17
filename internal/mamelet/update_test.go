package mamelet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

const updateOp = "op-0000000000000000000000d1"

// updateEnv is an agent whose "running binary" and installed one are files under Root.
type updateEnv struct {
	*env
	root string
	self string // the running binary's path
}

func newUpdateEnv(t *testing.T, running string) *updateEnv {
	e := newEnv(t)
	root := t.TempDir()
	u := &updateEnv{env: e, root: root, self: filepath.Join(root, "running")}
	if err := os.MkdirAll(filepath.Join(root, "usr/local/sbin"), 0o755); err != nil {
		t.Fatal(err)
	}
	u.setRunning(running)
	e.a.System.Root, e.a.System.SelfPath = root, u.self
	e.os.nextRuns, e.os.nextVersion = true, "mamelet 0.9.1"
	e.cp.binaries = map[string][]byte{}
	return u
}

// setRunning is the binary that runs now: the file at the path, and the one /proc/self/exe is.
func (u *updateEnv) setRunning(content string) {
	u.t.Helper()
	for _, p := range []string{u.self, filepath.Join(u.root, noderev.MameletPath)} {
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			u.t.Fatal(err)
		}
	}
	u.a.System.selfSHA = ""
}

func (u *updateEnv) installed() string {
	b, _ := os.ReadFile(filepath.Join(u.root, noderev.MameletPath))
	return string(b)
}

func (u *updateEnv) exists(p string) bool {
	_, err := os.Stat(filepath.Join(u.root, p))
	return err == nil
}

func (u *updateEnv) grant(serial int64, sha string, attempt int) {
	u.t.Helper()
	s, err := noderev.Sign(u.key, noderev.Revision{Schema: noderev.Schema, Node: "web", VMID: 7001, Serial: serial,
		IssuedAt: u.now, ExpiresAt: u.now.Add(time.Hour), Packages: []noderev.Package{}, Services: []noderev.Service{},
		Mamelet: &noderev.MameletUpdate{SHA256: sha, Once: noderev.Once{Operation: updateOp, Attempt: attempt, Action: noderev.ActionUpdate}}})
	if err != nil {
		u.t.Fatal(err)
	}
	u.cp.signed, u.cp.err = s, nil
}

func (u *updateEnv) state() State {
	st, err := u.a.State.Load()
	if err != nil {
		u.t.Fatal(err)
	}
	return st
}

// An approved update replaces mamelet only with the binary of the approved sha256 that runs: it
// keeps the previous one, asks for a restart after it reported, and the new mamelet confirms the
// update by reporting once.
func TestMameletUpdate(t *testing.T) {
	u := newUpdateEnv(t, "old mamelet")
	newBin := "new mamelet"
	u.cp.binaries[sum(newBin)] = []byte(newBin)
	u.grant(1, sum(newBin), 1)
	rep := u.round()
	if rep.MameletSHA256 != sum("old mamelet") {
		t.Errorf("the report says %s runs", rep.MameletSHA256)
	}
	if len(rep.Results) != 1 || rep.Results[0].Outcome != noderev.OutcomeDone || !strings.Contains(rep.Results[0].Detail, "restarting") {
		t.Fatalf("results: %+v", rep.Results)
	}
	prev, _ := os.ReadFile(filepath.Join(u.root, noderev.MameletPrevPath))
	if u.installed() != newBin || string(prev) != "old mamelet" || u.exists(noderev.MameletNextPath) {
		t.Fatalf("installed %q, previous %q, staged file left: %v", u.installed(), prev, u.exists(noderev.MameletNextPath))
	}
	if !u.a.Restarting() {
		t.Fatal("no restart asked for")
	}
	if st := u.state(); st.Update == nil || st.Update.From != sum("old mamelet") || st.Update.To != sum(newBin) || st.Update.Starts != 0 {
		t.Fatalf("pending update: %+v", st.Update)
	}
	// The old process reported but did not confirm anything.
	if st := u.state(); st.Update == nil {
		t.Fatal("the old mamelet confirmed the update")
	}

	// The restart: the new binary runs (the running file is now the new one). It counts its start,
	// does not go back, and a reported round confirms the update.
	u.setRunning(newBin)
	u.a.restart = false
	if back, err := u.a.Boot(); err != nil || back {
		t.Fatalf("boot: back %v, %v", back, err)
	}
	if st := u.state(); st.Update == nil || st.Update.Starts != 1 {
		t.Fatalf("after the first start: %+v", st.Update)
	}
	u.cp.err = nil
	rep = u.round()
	if rep.MameletSHA256 != sum(newBin) || u.state().Update != nil || u.a.Restarting() {
		t.Fatalf("after the first round: reported %s, update %+v", rep.MameletSHA256, u.state().Update)
	}
	// The same attempt is never made twice.
	u.cp.fetched = nil
	u.round()
	if len(u.cp.fetched) != 0 {
		t.Fatalf("fetched again: %v", u.cp.fetched)
	}
}

// Whatever is wrong with the update, nothing is replaced, no restart is asked for, and the
// operation says why.
func TestMameletUpdateRefusals(t *testing.T) {
	newBin := "new mamelet"
	for name, c := range map[string]struct {
		setup func(u *updateEnv)
		sha   string
		want  string
	}{
		"the Control Plane serves another binary": {func(u *updateEnv) { u.cp.binaries[sum(newBin)] = []byte("something else") }, sum(newBin), "not the approved"},
		"the Control Plane does not have it":      {func(u *updateEnv) {}, sum(newBin), "does not serve"},
		"the new binary does not run":             {func(u *updateEnv) { u.cp.binaries[sum(newBin)] = []byte(newBin); u.os.nextRuns = false }, sum(newBin), "does not run"},
		"it is not a mamelet":                     {func(u *updateEnv) { u.cp.binaries[sum(newBin)] = []byte(newBin); u.os.nextVersion = "k3s v1" }, sum(newBin), "does not run"},
		"an empty binary":                         {func(u *updateEnv) { u.cp.binaries[sum("")] = nil }, sum(""), "0 bytes"},
	} {
		u := newUpdateEnv(t, "old mamelet")
		c.setup(u)
		u.grant(1, c.sha, 1)
		rep := u.round()
		if len(rep.Results) != 1 || rep.Results[0].Outcome != noderev.OutcomeFailed || !strings.Contains(rep.Results[0].Detail, c.want) {
			t.Errorf("%s: %+v", name, rep.Results)
		}
		if u.installed() != "old mamelet" || u.exists(noderev.MameletNextPath) || u.exists(noderev.MameletPrevPath) || u.a.Restarting() || u.state().Update != nil {
			t.Errorf("%s: something changed (installed %q)", name, u.installed())
		}
	}
	// A mamelet that already runs the release has nothing to fetch.
	u := newUpdateEnv(t, "old mamelet")
	u.grant(1, sum("old mamelet"), 1)
	if rep := u.round(); rep.Results[0].Outcome != noderev.OutcomeDone || len(u.cp.fetched) != 0 || u.a.Restarting() {
		t.Errorf("already running: %+v, fetched %v", rep.Results, u.cp.fetched)
	}
	// Offline (a cached revision), a one-time grant is not used.
	u = newUpdateEnv(t, "old mamelet")
	u.cp.binaries[sum(newBin)] = []byte(newBin)
	u.grant(1, sum(newBin), 1)
	u.round() // accepted and applied
	u2 := newUpdateEnv(t, "old mamelet")
	u2.cp.binaries[sum(newBin)] = []byte(newBin)
	u2.grant(1, sum(newBin), 1)
	u2.cp.err = os.ErrDeadlineExceeded
	u2.e().round()
	if u2.installed() != "old mamelet" {
		t.Error("an update without a fresh revision")
	}
}

func (u *updateEnv) e() *env { return u.env }

// A new mamelet that has not proved itself goes back to the previous one when it starts too often,
// or does not report in time; a previous mamelet that runs after an update ends it.
func TestMameletUpdateRollsBack(t *testing.T) {
	setup := func() *updateEnv {
		u := newUpdateEnv(t, "old mamelet")
		u.cp.binaries[sum("new mamelet")] = []byte("new mamelet")
		u.grant(1, sum("new mamelet"), 1)
		u.round()
		u.setRunning("new mamelet") // the restart
		u.a.restart = false
		return u
	}

	// Four starts without a reported round: back.
	u := setup()
	for i := 1; i <= maxUpdateStarts; i++ {
		if back, err := u.a.Boot(); err != nil || back {
			t.Fatalf("start %d: back %v %v", i, back, err)
		}
	}
	back, err := u.a.Boot()
	if err != nil || !back {
		t.Fatalf("start %d: back %v %v", maxUpdateStarts+1, back, err)
	}
	if err := u.a.Rollback("it keeps failing"); err != nil {
		t.Fatal(err)
	}
	if u.installed() != "old mamelet" || !u.a.Restarting() || u.state().Update != nil {
		t.Fatalf("installed %q, restarting %v, update %+v", u.installed(), u.a.Restarting(), u.state().Update)
	}
	if r := u.state().Attempts[attemptKey(updateOp, 1)]; r.Outcome != noderev.OutcomeFailed || !strings.Contains(r.Detail, "went back") {
		t.Fatalf("the operation: %+v", r)
	}
	// The previous mamelet runs again: it reports the failure and it is not asked for the update again.
	u.setRunning("old mamelet")
	u.a.restart = false
	if back, err := u.a.Boot(); err != nil || back {
		t.Fatalf("boot of the previous: %v %v", back, err)
	}
	u.cp.err = nil
	rep := u.round()
	if len(rep.Results) != 1 || rep.Results[0].Outcome != noderev.OutcomeFailed || rep.MameletSHA256 != sum("old mamelet") {
		t.Fatalf("report: %+v", rep)
	}

	// Past the deadline without a reported round: overdue, and back at the next start too.
	u = setup()
	if u.a.UpdateOverdue() {
		t.Fatal("overdue at once")
	}
	u.e().now = u.e().now.Add(updateGrace + time.Minute)
	if !u.a.UpdateOverdue() {
		t.Fatal("not overdue after the grace")
	}
	if back, _ := u.a.Boot(); !back {
		t.Fatal("no rollback at a start after the grace")
	}

	// A restart that could not be made: the previous binary is put back at once.
	u = setup()
	if err := u.a.AbortRestart(os.ErrPermission); err != nil {
		t.Fatal(err)
	}
	if u.installed() != "old mamelet" || u.state().Update != nil {
		t.Fatalf("after an aborted restart: %q, %+v", u.installed(), u.state().Update)
	}
}

var _ = context.Background

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
