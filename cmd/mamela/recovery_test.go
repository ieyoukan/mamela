package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/cp"
	sitedef "github.com/ieyoukan/mamela/internal/site"
	"github.com/ieyoukan/mamela/internal/store"
)

// cpFixture is a Control Plane's files in a temporary directory: a site, its key directory and a
// database path.
type cpFixture struct {
	site string // the site file
	dir  string // controlplane.state_dir
	db   string
}

// newCPFixture writes a site whose Control Plane keeps its keys in a new directory (listening on a
// port the system chooses).
func newCPFixture(t *testing.T) cpFixture {
	t.Helper()
	root := t.TempDir()
	example, err := os.ReadFile(filepath.Join("..", "..", "mamela.site.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	f := cpFixture{site: filepath.Join(root, "site.toml"), dir: filepath.Join(root, "cp"), db: filepath.Join(root, "state", "state.db")}
	text := strings.NewReplacer(`"~/.config/mamela/cp"`, `"`+f.dir+`"`, `"0.0.0.0:8443"`, `"0.0.0.0:0"`,
		`ssh_public_key_file = "~/.ssh/id_ed25519.pub"`, ``).Replace(string(example))
	if err := os.WriteFile(f.site, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sitedef.Load(f.site); err != nil {
		t.Fatalf("the fixture's site is not valid: %v", err)
	}
	return f
}

// withKeys makes the Control Plane's keys, as the first mamela serve does.
func (f cpFixture) withKeys(t *testing.T) {
	t.Helper()
	if _, err := cp.LoadPKI(f.dir, []string{"10.0.0.45"}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// withAgent registers a mamelet in the database (enrolled at the time the clock says).
func (f cpFixture) withAgent(t *testing.T, node string, vmid int, at time.Time) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.db), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, f.db, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	token, err := st.IssueEnrollToken(ctx, node, vmid, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serial := "aa" + node
	if err := st.Enroll(ctx, token, node, vmid, serial); err != nil {
		t.Fatal(err)
	}
	return serial
}

func (f cpFixture) args(more ...string) []string {
	return append(more, "--site", f.site, "--db", f.db)
}

func readTar(t *testing.T, path string) map[string][]byte {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	out := map[string][]byte{}
	tr := tar.NewReader(fh)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name], _ = io.ReadAll(tr)
	}
}

func writeTar(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, b := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A backup holds the database and the keys, restores them byte for byte on a host that has none,
// keeps their modes, and writes nothing over what is there (or, asked to, moves it aside).
func TestBackupAndRestore(t *testing.T) {
	src := newCPFixture(t)
	src.withKeys(t)
	serial := src.withAgent(t, "web", 7001, time.Now())
	out := filepath.Join(t.TempDir(), "cp.backup")

	code, stdout, stderr := runCLI(src.args("backup", "--out", out)...)
	if code != 0 {
		t.Fatalf("backup: %d %s %s", code, stdout, stderr)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the backup's mode: %v %v", fi, err)
	}
	if _, err := os.Stat(out + ".partial"); err == nil {
		t.Fatal("a .partial file is left behind")
	}
	if code, _, stderr := runCLI(src.args("backup", "--out", out)...); code == 0 || !strings.Contains(stderr, "MAM-BAK-0001") {
		t.Fatalf("a backup written over another: %d %s", code, stderr)
	}

	dst := newCPFixture(t)
	code, stdout, stderr = runCLI(dst.args("restore", "--in", out)...)
	if code != 0 {
		t.Fatalf("restore: %d %s %s", code, stdout, stderr)
	}
	for _, name := range cp.Files {
		want, err := os.ReadFile(filepath.Join(src.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dst.dir, name))
		fi, _ := os.Stat(filepath.Join(dst.dir, name))
		if err != nil || !bytes.Equal(got, want) || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: restored wrongly (%v, mode %v)", name, err, fi)
		}
	}
	if fi, err := os.Stat(dst.dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the key directory's mode: %v %v", fi, err)
	}
	if fi, err := os.Stat(dst.db); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the database's mode: %v %v", fi, err)
	}
	st, err := store.Open(context.Background(), dst.db, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok, err := st.Agent(context.Background(), "web"); err != nil || !ok || a.CertSerial != serial || a.VMID != 7001 {
		t.Fatalf("the restored database lost the registration: %+v %v %v", a, ok, err)
	}
	st.Close()
	// What was restored starts: the keys that belong to the database are there.
	if _, err := loadPKIFor(t, dst); err != nil {
		t.Fatalf("the restored Control Plane does not load: %v", err)
	}

	// Nothing is written over a Control Plane that is there.
	before, _ := os.ReadFile(filepath.Join(dst.dir, "ca-key.pem"))
	if code, _, stderr := runCLI(dst.args("restore", "--in", out)...); code == 0 || !strings.Contains(stderr, "MAM-BAK-0003") || !strings.Contains(stderr, "--replace") {
		t.Fatalf("a restore over a Control Plane: %d %s", code, stderr)
	}
	// With --replace it is moved aside, not deleted.
	if code, stdout, stderr := runCLI(dst.args("restore", "--in", out, "--replace")...); code != 0 {
		t.Fatalf("restore --replace: %d %s %s", code, stdout, stderr)
	}
	aside, _ := filepath.Glob(filepath.Join(dst.dir, "ca-key.pem.before-restore-*"))
	dbAside, _ := filepath.Glob(dst.db + ".before-restore-*")
	if len(aside) != 1 || len(dbAside) != 1 {
		t.Fatalf("what was there is not moved aside: %v %v", aside, dbAside)
	}
	if kept, _ := os.ReadFile(aside[0]); !bytes.Equal(kept, before) {
		t.Fatal("the file that was moved aside is not the one that was there")
	}
}

// A backup that was changed, cut short or is not one is refused before anything is written.
func TestRestoreRefusesADamagedBackup(t *testing.T) {
	src := newCPFixture(t)
	src.withKeys(t)
	src.withAgent(t, "web", 7001, time.Now())
	good := filepath.Join(t.TempDir(), "good.backup")
	if code, _, stderr := runCLI(src.args("backup", "--out", good)...); code != 0 {
		t.Fatal(stderr)
	}
	files := readTar(t, good)
	mutate := map[string]func(map[string][]byte){
		"a changed database": func(f map[string][]byte) { f["state.db"] = append(f["state.db"], 'x') },
		"a changed key":      func(f map[string][]byte) { f["cp/ca-key.pem"] = []byte("not the key") },
		"a flipped byte": func(f map[string][]byte) {
			b := bytes.Clone(f["state.db"])
			b[len(b)/2] ^= 0xff // the same size: only the sha256 shows it
			f["state.db"] = b
		},
		"no CA":                func(f map[string][]byte) { delete(f, "cp/ca.pem") },
		"no manifest":          func(f map[string][]byte) { delete(f, "manifest.json") },
		"a file it should not": func(f map[string][]byte) { f["../escape"] = []byte("x") },
	}
	for name, change := range mutate {
		bad := map[string][]byte{}
		for k, v := range files {
			bad[k] = v
		}
		change(bad)
		path := filepath.Join(t.TempDir(), "bad.backup")
		writeTar(t, path, bad)
		dst := newCPFixture(t)
		code, _, stderr := runCLI(dst.args("restore", "--in", path)...)
		if code == 0 || !strings.Contains(stderr, "MAM-BAK-0002") {
			t.Errorf("%s: %d %s", name, code, stderr)
		}
		if _, err := os.Stat(dst.db); err == nil {
			t.Errorf("%s: a database was written", name)
		}
		if _, err := os.Stat(dst.dir); err == nil {
			t.Errorf("%s: keys were written", name)
		}
	}
	if code, _, stderr := runCLI(newCPFixture(t).args("restore", "--in", filepath.Join(t.TempDir(), "missing"))...); code == 0 {
		t.Errorf("a missing file: %s", stderr)
	}
}

func loadPKIFor(t *testing.T, f cpFixture) (*cp.PKI, error) {
	t.Helper()
	site, err := sitedef.Load(f.site)
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	return loadPKI(context.Background(), site, st)
}

// The state that stops every node: the database restored, and the keys it belongs to not. The
// Control Plane must not make a new CA and revision key (mamelet is pinned to the old ones); it must
// say what to do.
func TestControlPlaneRefusesADatabaseWithoutItsKeys(t *testing.T) {
	f := newCPFixture(t)
	f.withAgent(t, "web", 7001, time.Now())

	_, err := loadPKIFor(t, f)
	if err == nil || !strings.Contains(err.Error(), "MAM-PKI-0201") || !strings.Contains(err.Error(), "hint:") || !strings.Contains(err.Error(), "ca.pem") {
		t.Fatalf("loadPKI: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.dir, "ca.pem")); statErr == nil {
		t.Fatal("a new CA was made for a database that has mamelets registered")
	}
	code, _, stderr := runCLI(f.args("serve")...)
	if code == 0 || !strings.Contains(stderr, "MAM-PKI-0201") {
		t.Fatalf("serve: %d %s", code, stderr)
	}
	if _, statErr := os.Stat(filepath.Join(f.dir, "revision-key.pem")); statErr == nil {
		t.Fatal("serve made a revision key")
	}
	// A part of them is not enough either: the CA alone would make a new revision key.
	if _, err := cp.LoadPKI(f.dir, []string{"10.0.0.45"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.dir, "revision-key.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPKIFor(t, f); err == nil || !strings.Contains(err.Error(), "revision-key.pem") {
		t.Fatalf("a missing revision key: %v", err)
	}
	// Backing up that state would save something no mamelet trusts.
	if code, _, stderr := runCLI(f.args("backup", "--out", filepath.Join(t.TempDir(), "x"))...); code == 0 || !strings.Contains(stderr, "MAM-PKI-0201") {
		t.Fatalf("backup of a database without its keys: %d %s", code, stderr)
	}

	// With no mamelet registered there is nothing to lose: the keys are made, as on a first start.
	empty := newCPFixture(t)
	if pki, err := loadPKIFor(t, empty); err != nil || pki.CA == nil {
		t.Fatalf("a first start: %v", err)
	}
}

// safeBuf is a writer the server's goroutine and the test may use together.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The opposite loss: keys there, no database. The Control Plane starts (the nodes cannot connect
// whatever it does), and says that a database was lost and how to bring it back.
func TestServeSaysWhenTheDatabaseIsNew(t *testing.T) {
	f := newCPFixture(t)
	f.withKeys(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var out, errOut safeBuf
	if code := run(ctx, f.args("serve"), &out, &errOut); code != 0 {
		t.Fatalf("serve: %d %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "MAM-PKI-0203") || !strings.Contains(errOut.String(), "mamela restore") {
		t.Fatalf("no warning:\n%s", errOut.String())
	}
}

// The listing and the warnings of the certificates: from what the database records, an estimate for
// a node whose certificate end was not recorded.
func TestAgentCertificateListingAndWarnings(t *testing.T) {
	f := newCPFixture(t)
	now := time.Now().UTC()
	f.withAgent(t, "fresh", 7001, now.AddDate(0, 0, -10))
	f.withAgent(t, "soon", 7002, now.AddDate(0, 0, -350)) // no note: counted from the enrollment, 15 days left
	f.withAgent(t, "ended", 7003, now.AddDate(0, 0, -400))
	f.withAgent(t, "renewed", 7004, now.AddDate(0, 0, -400)) // ended by the estimate, but renewed since
	st, err := openStore(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.NoteAgentCert(context.Background(), "renewed", "aarenewed", now.AddDate(0, 6, 0)); err != nil {
		t.Fatal(err)
	}
	if err := st.NoteAgentCert(context.Background(), "renewed", "an-older-registration", now.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	ws, err := agentCertWarnings(context.Background(), st, now)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(ws, "\n")
	if len(ws) != 2 || !strings.Contains(joined, "MAM-PKI-0101: node soon") || !strings.Contains(joined, "MAM-PKI-0102: node ended") ||
		strings.Contains(joined, "node fresh") || strings.Contains(joined, "node renewed") || !strings.Contains(joined, "hint:") {
		t.Fatalf("warnings:\n%s", joined)
	}

	code, stdout, stderr := runCLI("controlplane", "agents", "--db", f.db)
	if code != 0 {
		t.Fatalf("agents: %d %s", code, stderr)
	}
	for _, want := range []string{"NODE", "fresh", "ok", "soon", "ends soon", "ended", "renewed", "(estimated)", "MAM-PKI-0102", "reenroll --node ended"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the listing lacks %q:\n%s", want, stdout)
		}
	}
	if code, _, _ := runCLI("controlplane", "agents", "--db", filepath.Join(t.TempDir(), "none.db")); code == 0 {
		t.Error("a listing of a database that is not there")
	}
}

// Enrolling a node again: a token for a node that is registered, which the node enrolls with
// (replacing the registration); never for one that is not.
func TestReenrollIssuesATokenForARegisteredNode(t *testing.T) {
	f := newCPFixture(t)
	old := f.withAgent(t, "web", 7001, time.Now().AddDate(-2, 0, 0))

	code, stdout, stderr := runCLI("controlplane", "reenroll", "--db", f.db, "--node", "web", "--ttl", "30m")
	if code != 0 {
		t.Fatalf("reenroll: %d %s", code, stderr)
	}
	var token string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && !strings.Contains(strings.TrimSpace(line), " ") {
			token = strings.TrimSpace(line)
		}
	}
	if token == "" || !strings.Contains(stdout, "cert.pem") || !strings.Contains(stdout, "enrollTokenFile") {
		t.Fatalf("no token or no instructions:\n%s", stdout)
	}
	st, err := openStore(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if ok, err := st.EnrollTokenUsable(context.Background(), token, "web", 7001); err != nil || !ok {
		t.Fatalf("the printed token is not usable: %v %v", ok, err)
	}
	if ok, _ := st.EnrollTokenUsable(context.Background(), token, "web", 9999); ok {
		t.Fatal("the token works for another VMID")
	}
	if err := st.Enroll(context.Background(), token, "web", 7001, "new-serial"); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := st.Agent(context.Background(), "web"); a.CertSerial == old || a.CertSerial != "new-serial" {
		t.Fatalf("enrolling again did not replace the registration: %+v", a)
	}

	if code, _, stderr := runCLI("controlplane", "reenroll", "--db", f.db, "--node", "ghost"); code == 0 || !strings.Contains(stderr, "MAM-PKI-0301") {
		t.Fatalf("a node that is not registered: %d %s", code, stderr)
	}
	if code, _, stderr := runCLI("controlplane", "reenroll", "--db", f.db, "--node", "web", "--ttl", "720h"); code == 0 || !strings.Contains(stderr, "24h") {
		t.Fatalf("a token that lasts a month: %d %s", code, stderr)
	}
}

// The CA lasts ten years and cannot be replaced without enrolling every node again, so the last
// year of it is warned about.
func TestCAEndingIsWarnedAbout(t *testing.T) {
	now := time.Now().UTC()
	young, err := cp.LoadPKI(filepath.Join(t.TempDir(), "cp"), []string{"10.0.0.45"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if w := caWarning(young.CA, now); w != "" {
		t.Fatalf("a new CA: %s", w)
	}
	old, err := cp.LoadPKI(filepath.Join(t.TempDir(), "cp"), []string{"10.0.0.45"}, now.AddDate(-10, 0, 100))
	if err != nil {
		t.Fatal(err)
	}
	if w := caWarning(old.CA, now); !strings.Contains(w, "MAM-PKI-0106") || !strings.Contains(w, "hint:") || !strings.Contains(w, "in 99 days") && !strings.Contains(w, "in 100 days") {
		t.Fatalf("a CA with 100 days left: %q", w)
	}
}

// New keys for a database that has mamelets: only when none of the identity files is there, never
// by itself, and with what each node needs (the new CA and revision key) printed.
func TestNewKeysKeepTheDatabaseAndSayWhatNodesNeed(t *testing.T) {
	f := newCPFixture(t)
	f.withAgent(t, "web", 7001, time.Now())

	// Without --confirm nothing is made.
	code, stdout, stderr := runCLI(f.args("controlplane", "new-keys")...)
	if code != 0 || !strings.Contains(stdout, "web (VMID 7001)") || !strings.Contains(stdout, "--confirm") {
		t.Fatalf("dry run: %d %s %s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "ca.pem")); err == nil {
		t.Fatal("the dry run made a CA")
	}
	// Some of the old keys there: refused, nothing changed.
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(f.dir, "ca.pem"), []byte("old"), 0o600)
	if code, _, stderr := runCLI(f.args("controlplane", "new-keys", "--confirm")...); code == 0 || !strings.Contains(stderr, "MAM-PKI-0302") {
		t.Fatalf("over some of the old keys: %d %s", code, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "ca.pem")); string(b) != "old" {
		t.Fatal("an old key file was changed")
	}
	os.Remove(filepath.Join(f.dir, "ca.pem"))

	code, stdout, stderr = runCLI(f.args("controlplane", "new-keys", "--confirm")...)
	if code != 0 || !strings.Contains(stdout, "reenroll --node") || !strings.Contains(stdout, `"revisionKey"`) {
		t.Fatalf("new-keys: %d %s %s", code, stdout, stderr)
	}
	pki, err := loadPKIFor(t, f)
	if err != nil {
		t.Fatalf("the new keys with the old database do not load: %v", err)
	}
	st, _ := openStore(context.Background(), f.db)
	defer st.Close()
	if a, ok, _ := st.Agent(context.Background(), "web"); !ok || a.VMID != 7001 {
		t.Fatalf("the database lost its registration: %+v", a)
	}
	// What a node needs is printed by `trust` as well, and is the CA and key the Control Plane has.
	code, trust, stderr := runCLI(f.args("controlplane", "trust")...)
	if code != 0 {
		t.Fatalf("trust: %d %s", code, stderr)
	}
	var got mameletTrust
	if err := json.Unmarshal([]byte(strings.TrimSpace(trust)), &got); err != nil || got.CA != string(pki.CAPEM()) || !bytes.Equal(got.RevisionKey, pki.RevisionPublicKey()) {
		t.Fatalf("trust printed %q: %v", trust, err)
	}
	if !strings.Contains(stdout, strings.TrimSpace(trust)) {
		t.Fatal("new-keys printed something other than trust does")
	}
	// Without keys there is nothing to print.
	if code, _, stderr := runCLI(newCPFixture(t).args("controlplane", "trust")...); code == 0 || !strings.Contains(stderr, "MAM-PKI-0201") {
		t.Fatalf("trust without keys: %d %s", code, stderr)
	}
}

// After a lost database, a node that is not in it can be given a token if its VMID is told.
func TestReenrollNodeThatTheDatabaseLost(t *testing.T) {
	f := newCPFixture(t)
	f.withAgent(t, "web", 7001, time.Now())
	code, stdout, stderr := runCLI("controlplane", "reenroll", "--db", f.db, "--node", "lost", "--vmid", "7009")
	if code != 0 || !strings.Contains(stdout, "no record of the node") {
		t.Fatalf("%d %s %s", code, stdout, stderr)
	}
	if code, _, stderr := runCLI("controlplane", "reenroll", "--db", f.db, "--node", "web", "--vmid", "9999"); code == 0 || !strings.Contains(stderr, "7001") {
		t.Fatalf("a token for the wrong machine: %d %s", code, stderr)
	}
	if code, _, stderr := runCLI("controlplane", "reenroll", "--db", f.db, "--node", "lost2"); code == 0 || !strings.Contains(stderr, "--vmid") {
		t.Fatalf("an unknown node without a VMID: %d %s", code, stderr)
	}
}
