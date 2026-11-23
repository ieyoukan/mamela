package linux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
	"github.com/ieyoukan/mamela/internal/safetext"
)

// What mamelet does with the writes of a round that name a check (file_check.go). A check group is
// the writes of the round's part that name one check. mamelet:
//
//  1. checks each write as it checks any (prepareWrite); one it refuses refuses the group: nothing of
//     it is written;
//  2. keeps what each write goes over (its content in the state directory, by sha256, with its mode
//     and owner, or that there was nothing), and records the group in its record of the checks
//     (checksName) before it writes anything: the intent comes first;
//  3. writes every file of the group, then runs the check once;
//  4. if it passes, takes the group out of the record, reloads and restarts what the files name, and
//     confirms that those services are still active (followFiles);
//  5. if it fails (or the check cannot run, or a write failed), records that it failed and what the
//     program said, puts back each file it wrote (the kept content, mode and owner, or no file), puts
//     back what was pending for it, reloads and restarts nothing, and records each file as put back
//     (reported while it stays so: FileState.RolledBack, MAM-PLAN-0230).
//
// mamelet stopped (killed, its node's power lost) between 2 and the end finds the group in its record
// when it starts again (recoverChecks, at the start of every round, whatever the revision): it runs
// the check again, and puts the files back if it fails (or goes on putting them back). A group it
// cannot finish stays in the record, and is reported (FileState.Unchecked: the plan waits, or blocks
// with what stops it), never taken as done.

// checksName is mamelet's record of the checks of its writes, in its state directory; keptDirName the
// directory there of the contents it keeps to put back.
const (
	checksName  = "linux-checks.json"
	keptDirName = "linux-checks.d"
)

// checksRecord is what mamelet has not finished of its checks, and what it put back.
type checksRecord struct {
	// Groups are the groups it wrote (or is writing) and has not finished: their check has not run, or
	// it failed and the files are not all put back.
	Groups []checkGroup `json:"groups,omitempty"`
	// RolledBack are the files it put back, by path, while they are as it put them back.
	RolledBack map[string]rolledBack `json:"rolledBack,omitempty"`
}

// checkGroup is a check group mamelet wrote.
type checkGroup struct {
	Check string `json:"check"`
	// Grants are the grants whose writes the group has (their outcome is the group's).
	Grants []noderev.Once `json:"grants"`
	Files  []checkedFile  `json:"files"`
	// Failed: the check failed (what its program said), or a write did (WriteFailed): the files are
	// being put back.
	Failed      string `json:"failed,omitempty"`
	WriteFailed bool   `json:"writeFailed,omitzero"`
	// Error says why putting them back has not finished (it is tried again every round).
	Error string `json:"error,omitempty"`
}

// checkedFile is a file of a group: what mamelet writes there, and what it writes over.
type checkedFile struct {
	Path   string   `json:"path"`
	SHA256 string   `json:"sha256"`
	Mode   string   `json:"mode"`
	Owner  string   `json:"owner"`
	Group  string   `json:"group"`
	Prev   prevFile `json:"prev"`
	// Pending is what was pending after the write before this one (put back with the file).
	Pending *Pending `json:"pending,omitempty"`
}

// prevFile is what was at a path before mamelet wrote it: nothing, or a regular file whose content
// (SHA256) mamelet kept, with its mode and owner (names as mamelet observes them, "#<id>" for an id the
// node has no name for).
type prevFile struct {
	Exists bool   `json:"exists,omitzero"`
	SHA256 string `json:"sha256,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Owner  string `json:"owner,omitempty"`
	Group  string `json:"group,omitempty"`
}

// rolledBack is a file mamelet put back: what is reported, what the path is once put back (reported
// while it still is), and the outcome of the grants of its group.
type rolledBack struct {
	RolledBack `json:",inline"`
	Restored   Before         `json:"restored"`
	Grants     []noderev.Once `json:"grants,omitempty"`
	Detail     string         `json:"detail,omitempty"`
}

// The bounds of the check: how long its program may run, and how much of what it said is kept.
const (
	checkTimeout   = time.Minute
	maxCheckOutput = 1000
)

// testHookBeforeCheck runs, in tests, once the files of a group are written and before its check (a
// test stops mamelet there).
var testHookBeforeCheck func()

// errStopping: mamelet is stopping (SIGTERM) while the check runs: it is run again later.
var errStopping = errors.New("mamelet is stopping")

// runCheck runs the program of the check, and says whether it passed; out is what it said when it did
// not (escaped and bounded), or why it could not run (a check that cannot run does not pass).
func runCheck(ctx context.Context, s *mamelet.System, check string) (passed bool, out string, err error) {
	v, ok := validatorOf(check)
	if !ok {
		return false, fmt.Sprintf("mamelet has no check %q", check), nil
	}
	if ctx.Err() != nil {
		return false, "", errStopping
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	stdout, stderr, code, err := s.Runner.Run(cctx, v.Program, v.Args, nil)
	switch {
	case ctx.Err() != nil:
		return false, "", errStopping
	case cctx.Err() != nil:
		return false, fmt.Sprintf("%s did not finish within %s", v.Shown, checkTimeout), nil
	case err != nil:
		return false, fmt.Sprintf("%s could not run: %v", v.Program, err), nil
	case code == 0:
		return true, "", nil
	}
	return false, checkOutput(stderr, stdout, code), nil
}

// checkOutput is what a check's program said (its error output, then its output), each line escaped,
// joined with " | ", at most maxCheckOutput bytes.
func checkOutput(stderr, stdout []byte, code int) string {
	var lines []string
	for _, b := range [][]byte{stderr, stdout} {
		for _, l := range safetext.EscapeLines(strings.TrimSpace(string(b))) {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("exit %d, and nothing else", code)
	}
	return bounded(strings.Join(lines, " | "), maxCheckOutput)
}

// loadChecks reads the record (empty when there is none). One whose content cannot be decoded is set
// aside (checksName + corruptSuffix, reported in Observed.SetAside: the plan blocks the files declared
// with a check while it is there, MAM-PLAN-0232) and taken as empty. One that cannot be read is an
// error (this round does not use it). The record returned always has its map.
func loadChecks(dir string) (checksRecord, error) {
	rec := checksRecord{RolledBack: map[string]rolledBack{}}
	if dir == "" {
		return rec, errors.New("mamelet has no state directory")
	}
	data, err := readRecord(dir, checksName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return rec, nil
	case err != nil:
		return rec, fmt.Errorf("mamelet's record of the checks of its writes (%s) cannot be read: %w", checksName, err)
	}
	if err := decodeRecord(data, &rec); err != nil {
		if aerr := setAside(dir, checksName); aerr != nil {
			return rec, fmt.Errorf("mamelet's record of the checks of its writes (%s) cannot be decoded (%v), nor set aside: %w", checksName, err, aerr)
		}
		return checksRecord{RolledBack: map[string]rolledBack{}}, nil
	}
	if rec.RolledBack == nil {
		rec.RolledBack = map[string]rolledBack{}
	}
	return rec, nil
}

// saveChecks replaces the record (and removes it when it has nothing), then removes the contents it
// no longer keeps.
func saveChecks(dir string, rec checksRecord) error {
	var err error
	if len(rec.Groups) == 0 && len(rec.RolledBack) == 0 {
		if err = os.Remove(filepath.Join(dir, checksName)); errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	} else {
		if err = os.MkdirAll(dir, 0o700); err == nil {
			err = saveJSON(dir, checksName, rec)
		}
	}
	if err == nil {
		dropKept(dir, rec)
	}
	return err
}

// keepContent keeps content (whose sha256 is sum) in the directory of kept contents, synced: the same
// content is kept once.
func keepContent(dir, sum string, content []byte) error {
	kept := filepath.Join(dir, keptDirName)
	if got, err := keptContent(dir, sum); err == nil && bytes.Equal(got, content) {
		return nil
	}
	if err := os.MkdirAll(kept, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(kept, "."+sum+".tmp")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(kept, sum))
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(kept); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// keptContent is the content kept under sum, checked against it.
func keptContent(dir, sum string) ([]byte, error) {
	if len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
		return nil, fmt.Errorf("%q is not a sha256", sum)
	}
	data, err := os.ReadFile(filepath.Join(dir, keptDirName, sum))
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != sum {
		return nil, fmt.Errorf("the content mamelet kept as %s is not that any more", sum[:12])
	}
	return data, nil
}

// dropKept removes the kept contents (and temporary files) that no group of rec needs.
func dropKept(dir string, rec checksRecord) {
	kept := filepath.Join(dir, keptDirName)
	entries, err := os.ReadDir(kept)
	if err != nil {
		return
	}
	needed := map[string]bool{}
	for _, g := range rec.Groups {
		for _, f := range g.Files {
			needed[f.Prev.SHA256] = true
		}
	}
	for _, e := range entries {
		if !needed[e.Name()] && e.Type()&fs.ModeType == 0 {
			os.Remove(filepath.Join(kept, e.Name()))
		}
	}
	if len(rec.Groups) == 0 {
		os.Remove(kept) // only when it is empty
	}
}

// unfinished is the group of rec that has path (nil: none).
func (rec checksRecord) unfinished(p string) *checkGroup {
	for i := range rec.Groups {
		if slices.ContainsFunc(rec.Groups[i].Files, func(f checkedFile) bool { return f.Path == p }) {
			return &rec.Groups[i]
		}
	}
	return nil
}

// writeGroup carries out the writes of one check group of the round (files: those of the part whose
// write names check and has not been made), and records the outcome of each of their grants. It says
// whether it changed anything.
func writeGroup(ctx context.Context, x *mamelet.Act, check string, files []File) bool {
	dir := stateDir(x.System)
	v, _ := validatorOf(check)
	var grants []noderev.Once
	var paths []string
	for _, f := range files {
		if g := f.Write.Grant; !slices.ContainsFunc(grants, func(o noderev.Once) bool { return o.Operation == g.Operation && o.Attempt == g.Attempt }) {
			grants = append(grants, g)
		}
		paths = append(paths, f.Path)
	}
	record := func(outcome, detail string) {
		for _, g := range grants {
			x.Record(g.Operation, g.Attempt, outcome, detail)
		}
	}
	nothing := "; nothing of " + strings.Join(paths, ", ") + " (checked together by " + v.Shown + ") was written"

	rec, err := loadChecks(dir)
	if err != nil {
		record(noderev.OutcomeFailed, err.Error()+nothing)
		return false
	}
	// A grant whose writes mamelet put back when it started again (it had stopped before the check
	// ran: recoverChecks) has that outcome; it is not written again.
	for _, g := range grants {
		for _, rb := range rec.RolledBack {
			if slices.ContainsFunc(rb.Grants, func(o noderev.Once) bool { return o.Operation == g.Operation && o.Attempt == g.Attempt }) {
				x.Record(g.Operation, g.Attempt, noderev.OutcomeFailed, rb.Detail)
				break
			}
		}
	}
	files = slices.DeleteFunc(slices.Clone(files), func(f File) bool { return x.Done(f.Write.Grant.Operation, f.Write.Grant.Attempt) })
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if g := rec.unfinished(f.Path); g != nil {
			record(noderev.OutcomeFailed, fmt.Sprintf("mamelet has not finished the check of its last write of %s (%s)%s; it finishes it first (every round)",
				f.Path, orNothing(g.Error), nothing))
			return false
		}
	}
	r, err := openTree(x.System)
	if err != nil {
		record(noderev.OutcomeFailed, err.Error()+nothing)
		return false
	}
	defer r.Close()
	var preps []prepared
	for _, f := range files {
		if outcome, detail := refuseWrite(x, f.Path, *f.Write); outcome != "" {
			record(outcome, detail+nothing)
			return false
		}
		pw, outcome, detail := prepareWrite(r, f.Path, *f.Write)
		if outcome != "" {
			record(outcome, detail+nothing)
			return false
		}
		preps = append(preps, pw)
	}

	// What each write goes over is kept, and the group recorded, before anything is written.
	pend, err := loadPending(dir)
	if err != nil {
		record(noderev.OutcomeFailed, "record what follows the writes: "+err.Error()+nothing)
		return false
	}
	g := checkGroup{Check: check, Grants: grants}
	for _, pw := range preps {
		if !pw.write {
			continue
		}
		cf := checkedFile{Path: pw.path, SHA256: pw.w.SHA256, Mode: pw.w.Mode, Owner: pw.w.Owner, Group: pw.w.Group}
		if left, ok := pend.Pending[pw.path]; ok {
			cf.Pending = &left
		}
		if pw.got.Exists {
			if pw.got.Large {
				record(noderev.OutcomeRefused, fmt.Sprintf("mamelet does not write %s with a check: what is there is larger than %d bytes, and mamelet keeps what it writes over, to put it back if the check fails%s",
					pw.path, MaxContent, nothing))
				return false
			}
			content, err := readRegular(r, pw.path, MaxContent)
			if err != nil {
				record(noderev.OutcomeFailed, "keep what is at "+pw.path+": "+err.Error()+nothing)
				return false
			}
			if ContentSHA256(string(content)) != pw.got.SHA256 {
				record(noderev.OutcomeRefused, pw.path+" changed while mamelet read it"+nothing+" (plan again)")
				return false
			}
			if err := keepContent(dir, pw.got.SHA256, content); err != nil {
				record(noderev.OutcomeFailed, "keep what is at "+pw.path+" in mamelet's state directory: "+err.Error()+nothing)
				return false
			}
			cf.Prev = prevFile{Exists: true, SHA256: pw.got.SHA256, Mode: pw.got.Mode, Owner: pw.got.Owner, Group: pw.got.Group}
		}
		g.Files = append(g.Files, cf)
	}
	var undos []func()
	undo := func() {
		for _, u := range slices.Backward(undos) {
			u()
		}
	}
	for _, pw := range preps {
		if pw.write {
			u, err := pendingBeforeWith(dir, pw.path, pw.w.SHA256, pw.w.Restart, pw.w.Reload)
			if err != nil {
				undo()
				record(noderev.OutcomeFailed, "record what follows the write of "+pw.path+": "+err.Error()+nothing)
				return false
			}
			undos = append(undos, u)
		}
	}
	if len(g.Files) > 0 {
		rec.Groups = append(rec.Groups, g)
		for _, f := range g.Files {
			delete(rec.RolledBack, f.Path) // a new write
		}
		if err := saveChecks(dir, rec); err != nil {
			undo()
			record(noderev.OutcomeFailed, "record the check of the writes in mamelet's state directory: "+err.Error()+nothing)
			return false
		}
	}

	var did []string
	for _, pw := range preps {
		if !pw.write {
			did = append(did, pw.path+" was already as approved")
			continue
		}
		d, err := makeWrite(r, pw)
		if err != nil {
			why := "write " + pw.path + ": " + err.Error()
			rec.Groups[len(rec.Groups)-1].Failed, rec.Groups[len(rec.Groups)-1].WriteFailed = why, true
			saveChecks(dir, rec)
			record(noderev.OutcomeFailed, putBackDetail(r, dir, &rec, len(rec.Groups)-1, did, why, false))
			return true
		}
		did = append(did, d...)
	}
	if testHookBeforeCheck != nil {
		testHookBeforeCheck()
	}

	passed, out, err := runCheck(ctx, x.System, check)
	if err != nil {
		// Stopping: the group stays in the record, and the check runs when mamelet starts again (or
		// in its next round); the attempt is not recorded (Act.Record), and goes on while it lasts.
		record(noderev.OutcomeFailed, "mamelet stopped before "+v.Shown+" ran")
		return true
	}
	if !passed {
		if len(g.Files) == 0 {
			record(noderev.OutcomeFailed, strings.Join(append(did, v.Shown+" failed: "+out,
				"nothing was written, so nothing was put back; nothing was reloaded or restarted: fix the declaration (what the check said), or what it reads on the node"), "; "))
			return true
		}
		rec.Groups[len(rec.Groups)-1].Failed = out
		saveChecks(dir, rec)
		record(noderev.OutcomeFailed, putBackDetail(r, dir, &rec, len(rec.Groups)-1, did, v.Shown+" failed: "+out, true))
		return true
	}
	did = append(did, v.Shown+" passed")
	if len(g.Files) > 0 {
		rec.Groups = rec.Groups[:len(rec.Groups)-1]
		if err := saveChecks(dir, rec); err != nil {
			did = append(did, "(its record could not be cleared: "+err.Error()+")")
		}
	}
	// What follows: the files written now, and those already as approved whose follow-up is pending.
	pend, _ = loadPending(dir)
	var follow []after
	for _, pw := range preps {
		if left, ok := pend.Pending[pw.path]; pw.write || ok && left.SHA256 == pw.w.SHA256 {
			follow = append(follow, after{path: pw.path, sum: pw.w.SHA256, reloads: pw.w.Reload, restart: pw.w.Restart})
		}
	}
	if len(follow) == 0 {
		did = append(did, "nothing was pending after them")
	}
	record(followFiles(ctx, x, follow, true, did))
	return true
}

// putBackDetail puts back the files of the group i of rec (why: why, the check's failure, checked, or
// a write's), and is the detail of the attempt's outcome: what was done, why, and what was put back.
func putBackDetail(r *os.Root, dir string, rec *checksRecord, i int, did []string, why string, checked bool) string {
	back, err := putBackGroup(r, dir, rec, i, "")
	detail := strings.Join(append(append(did, why), back...), "; ")
	switch {
	case err != nil:
		return detail + "; but " + err.Error() + ": mamelet tries again every round; nothing was reloaded or restarted"
	case !checked:
		return detail + "; nothing was reloaded or restarted, and the check did not run: fix the cause, then apply again"
	}
	return detail + "; nothing was reloaded or restarted: fix the declaration (what the check said is wrong with it), then plan again"
}

// putBackGroup puts back the files of the group i of rec, whose check failed (or one of whose writes
// did: Failed says which), in the reverse order of their writing, with what was pending after each.
// Once all are, it records each as put back (with the outcome of the group's grants, detail, or the
// one putBackDetail makes), takes the group out of the record, and says what it did. A file it cannot
// put back leaves the group in the record, with the error (Error), for the next round.
func putBackGroup(r *os.Root, dir string, rec *checksRecord, i int, detail string) ([]string, error) {
	g := &rec.Groups[i]
	a := readAccounts(r)
	var did, failed []string
	var paths []string
	restored := map[string]Before{}
	for _, f := range slices.Backward(g.Files) {
		paths = append(paths, f.Path)
		what, err := putBack(r, dir, f, a)
		if err != nil {
			failed = append(failed, "put back "+f.Path+": "+err.Error())
			continue
		}
		did = append(did, what)
		if st := inspect(r, f.Path, a); st.Error == "" && st.Unsafe == "" && (!st.Exists || !st.Large) && isPrev(f.Prev, st) {
			restored[f.Path] = beforeOf(st)
		}
	}
	slices.Reverse(did)
	slices.Sort(paths)
	if len(failed) > 0 {
		g.Error = strings.Join(failed, "; ")
		saveChecks(dir, *rec)
		return did, errors.New(g.Error)
	}
	if detail == "" {
		v, _ := validatorOf(g.Check)
		detail = fmt.Sprintf("the check %s of %s failed: %s; %s; nothing was reloaded or restarted: fix the declaration (what the check said is wrong with it), then plan again",
			v.Shown, strings.Join(paths, ", "), g.Failed, strings.Join(did, "; "))
	}
	for _, f := range g.Files {
		setPending(dir, f.Path, f.Pending)
		// A write that failed is not a check that failed: the plan shows the change again, without
		// saying it was put back for its check.
		if b, ok := restored[f.Path]; ok && !g.WriteFailed {
			rec.RolledBack[f.Path] = rolledBack{RolledBack: RolledBack{SHA256: f.SHA256, Check: g.Check, Output: g.Failed, Files: paths},
				Restored: b, Grants: g.Grants, Detail: detail}
		}
	}
	rec.Groups = slices.Delete(rec.Groups, i, i+1)
	if err := saveChecks(dir, *rec); err != nil {
		did = append(did, "(the record of the check could not be updated: "+err.Error()+")")
	}
	return did, nil
}

// isPrev reports whether st (what is at the path) is what was there before the write.
func isPrev(prev prevFile, st FileState) bool {
	if !prev.Exists || !st.Exists {
		return prev.Exists == st.Exists
	}
	return st.SHA256 == prev.SHA256 && st.Mode == prev.Mode && st.Owner == prev.Owner && st.Group == prev.Group
}

// putBack puts the file of a group back as it was before mamelet wrote it (f.Prev): the content it
// kept, with its mode and owner, or no file. It does so only while the file is the one mamelet wrote
// (its content, mode and owner): one that is as it was already (not written yet, or put back by an
// attempt that was interrupted) is left so, and one that changed since (someone else wrote it) is
// left as it is (and said so). It never follows a link.
func putBack(r *os.Root, dir string, f checkedFile, a accounts) (string, error) {
	cur := inspect(r, f.Path, a)
	switch {
	case cur.Error != "":
		return "", errors.New(cur.Error)
	case cur.Unsafe != "":
		return f.Path + " is not a regular file any more (" + cur.Unsafe + "): left as it is", nil
	case isPrev(f.Prev, cur):
		return f.Path + " is as it was", nil
	case !cur.Exists || cur.Large || cur.SHA256 != f.SHA256 || cur.Mode != f.Mode || cur.Owner != f.Owner || cur.Group != f.Group:
		return f.Path + " changed since mamelet wrote it: left as it is", nil
	}
	d, err := openDir(r, path.Dir(f.Path))
	if err != nil {
		return "", err
	}
	defer d.Close()
	base := path.Base(f.Path)
	if !f.Prev.Exists {
		if err := d.remove(base); err != nil {
			return "", err
		}
		d.sync()
		return "removed " + f.Path + " (it did not exist)", nil
	}
	content, err := keptContent(dir, f.Prev.SHA256)
	if err != nil {
		return "", err
	}
	uid, okU := idOf(a.users, f.Prev.Owner)
	gid, okG := idOf(a.groups, f.Prev.Group)
	if !okU || !okG {
		return "", fmt.Errorf("its owner %s:%s is not on the node any more", f.Prev.Owner, f.Prev.Group)
	}
	mode, err := fileModeOf(f.Prev.Mode)
	if err != nil {
		return "", err
	}
	if err := replaceIn(d, path.Dir(f.Path), base, bytes.NewReader(content), int64(len(content)), f.Prev.SHA256, mode, uid, gid); err != nil {
		return "", err
	}
	return "put back " + f.Path + " as it was", nil
}

// idOf is the id of a user or a group by its name as mamelet observes it ("#<id>" for an id the node
// has no name for).
func idOf(ids map[string]int, name string) (int, bool) {
	if n, ok := strings.CutPrefix(name, "#"); ok {
		id, err := strconv.Atoi(n)
		return id, err == nil && id >= 0
	}
	id, ok := ids[name]
	return id, ok
}

// fileModeOf is a mode as modeString writes it (with the setuid, setgid and sticky bits).
func fileModeOf(s string) (fs.FileMode, error) {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || len(s) != 4 {
		return 0, fmt.Errorf("mode %q is not four octal digits", s)
	}
	m := fs.FileMode(v & 0o777)
	if v&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if v&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if v&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m, nil
}

// recoverChecks finishes, at the start of a round and whatever the revision, the groups mamelet's
// record has: it stopped after it wrote them and before it finished their check. It runs the check
// again: if it passes, the files stay as written (what follows them is pending, and is done with the
// grant: the plan asks for it, MAM-PLAN-0218); if it fails (or failed before), it puts the files back.
// It changes nothing else, and nothing mamelet did not write.
func recoverChecks(ctx context.Context, s *mamelet.System) {
	dir := stateDir(s)
	if dir == "" {
		return
	}
	rec, err := loadChecks(dir)
	if err != nil || len(rec.Groups) == 0 {
		return
	}
	r, err := openTree(s)
	if err != nil {
		return
	}
	defer r.Close()
	for i := 0; i < len(rec.Groups); {
		g := &rec.Groups[i]
		v, _ := validatorOf(g.Check)
		var paths []string
		for _, f := range g.Files {
			paths = append(paths, f.Path)
		}
		reason := g.Failed
		if g.Failed == "" {
			passed, out, err := runCheck(ctx, s, g.Check)
			if err != nil {
				return // stopping again
			}
			if passed {
				rec.Groups = slices.Delete(rec.Groups, i, i+1)
				saveChecks(dir, rec)
				continue
			}
			g.Failed, reason = out, "when it started again, "+v.Shown+" failed: "+out
			saveChecks(dir, rec)
		}
		detail := fmt.Sprintf("mamelet stopped after it wrote %s and before it finished the check; %s", strings.Join(paths, ", "), reason)
		back, err := putBackGroup(r, dir, &rec, i, "")
		if err != nil {
			i++
			continue
		}
		for _, f := range paths {
			if rb, ok := rec.RolledBack[f]; ok {
				rb.Detail = detail + "; " + strings.Join(back, "; ") + "; nothing was reloaded or restarted: fix the declaration (what the check said is wrong with it), then plan again"
				rec.RolledBack[f] = rb
			}
		}
		saveChecks(dir, rec)
	}
}

// observeChecks adds to the observation what mamelet's record of its checks says of its files: those
// it has not finished (Unchecked), and those it put back while they are still so (RolledBack). A file
// that changed since it was put back is forgotten; one the part does not name now is kept, the first
// maxRolledBackKept by path. A record that cannot be read marks every file observed (Unchecked, with
// the error): the plan blocks those declared with a check this round.
func observeChecks(s *mamelet.System, o *Observed) {
	if len(o.Files) == 0 {
		return
	}
	dir := stateDir(s)
	rec, err := loadChecks(dir)
	if err != nil {
		for i := range o.Files {
			if o.Files[i].Error == "" {
				o.Files[i].Unchecked = &Unchecked{Error: err.Error()}
			}
		}
		return
	}
	for _, g := range rec.Groups {
		for _, f := range g.Files {
			if i := slices.IndexFunc(o.Files, func(st FileState) bool { return st.Path == f.Path }); i >= 0 {
				o.Files[i].Unchecked = &Unchecked{SHA256: f.SHA256, Check: g.Check, Failed: g.Failed, Error: g.Error}
			}
		}
	}
	if len(rec.RolledBack) == 0 {
		return
	}
	keep := map[string]rolledBack{}
	seen := map[string]bool{}
	for i := range o.Files {
		st := &o.Files[i]
		rb, ok := rec.RolledBack[st.Path]
		if !ok {
			continue
		}
		seen[st.Path] = true
		switch {
		case st.Error != "":
			keep[st.Path] = rb // not looked at this time
		case st.Unsafe == "" && rb.Restored.Is(*st):
			keep[st.Path] = rb
			c := rb.RolledBack
			c.Files = slices.Clone(c.Files)
			st.RolledBack = &c
		}
	}
	var unnamed []string
	for p := range rec.RolledBack {
		if !seen[p] {
			unnamed = append(unnamed, p)
		}
	}
	slices.Sort(unnamed)
	for _, p := range unnamed {
		if len(keep) >= maxRolledBackKept {
			break
		}
		keep[p] = rec.RolledBack[p]
	}
	if len(keep) != len(rec.RolledBack) {
		rec.RolledBack = keep
		saveChecks(dir, rec)
	}
}

// maxRolledBackKept bounds the files the record keeps as put back.
const maxRolledBackKept = 2 * MaxNodeFiles
