package linux

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// What follows a write (NodeFile, NodeArtifact): systemctl daemon-reload for a unit, then the
// restarts of the services the write names. A write is approved once, and its attempt is recorded
// once; so what did not finish after the file was written (a restart that failed, mamelet stopped
// or killed meanwhile) is kept in mamelet's record of what is pending (pendingName, in its state
// directory), written before the first of them runs and taken out once all of them have. mamelet
// reports it while the file is still the one written (FileState.Pending), and the plan shows the
// file as not in sync, asking to approve the write again (MAM-PLAN-0218): its grant finds the file
// as approved and does only what is left. Nothing is restarted without a grant.

// pendingName is the file of the record, in mamelet's state directory.
const pendingName = "linux-pending.json"

// pendingRecord is what is pending after each path's last write.
type pendingRecord struct {
	Pending map[string]Pending `json:"pending"`
	// Lost: the record before this one could not be read, and was set aside (setAside): what was
	// pending after the writes it named is not known. The next observation marks each file and
	// artifact it finds as such (Pending.Unknown), and clears it.
	Lost bool `json:"lost,omitzero"`
}

// loadPending reads the record (empty when there is none). One whose content cannot be decoded is
// set aside (pendingName + corruptSuffix, for a person to look at) and replaced by one that says it was
// lost: whether the restarts after the writes it named finished is then not known, which is reported
// (the plan asks to approve them again), never forgotten. One that cannot be read (an I/O error) is
// left where it is, and the error says so (this round does not use it). Members it does not know are
// ignored (decodeRecord). The error also says why a record could not be set aside (nothing is then
// written over it). The record returned always has its map.
func loadPending(dir string) (pendingRecord, error) {
	rec := pendingRecord{Pending: map[string]Pending{}}
	if dir == "" {
		return rec, nil
	}
	data, err := readRecord(dir, pendingName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return rec, nil
	case err != nil:
		return rec, fmt.Errorf("mamelet's record of what is pending after its writes (%s) cannot be read: %w", pendingName, err)
	}
	if err := decodeRecord(data, &rec); err != nil {
		lost := pendingRecord{Pending: map[string]Pending{}, Lost: true}
		if aerr := setAside(dir, pendingName); aerr != nil {
			return lost, fmt.Errorf("mamelet's record of what is pending after its writes (%s) cannot be decoded (%v), nor set aside: %w", pendingName, err, aerr)
		}
		return lost, savePending(dir, lost)
	}
	if rec.Pending == nil {
		rec.Pending = map[string]Pending{}
	}
	return rec, nil
}

// corruptSuffix is added to the name of a record of mamelet's state directory that cannot be read:
// it is set aside for a person to look at, never written over (it may say what nothing else does).
const corruptSuffix = ".corrupt"

// setAside renames the record name of mamelet's state directory dir, which cannot be read, to
// name + corruptSuffix (replacing one set aside before).
func setAside(dir, name string) error {
	return os.Rename(filepath.Join(dir, name), filepath.Join(dir, name+corruptSuffix))
}

// setAsideRecords are the records of dir that were set aside and are still there (sorted names).
func setAsideRecords(dir string) []string {
	var out []string
	if dir == "" {
		return nil
	}
	for _, name := range []string{recordName, pendingName, checksName} {
		if fi, err := os.Lstat(filepath.Join(dir, name+corruptSuffix)); err == nil && fi.Mode().IsRegular() {
			out = append(out, name+corruptSuffix)
		}
	}
	return out
}

// setPending records what is pending after the write of p (nil: nothing), and returns what was
// recorded before (nil: nothing), so that a write that fails can put it back.
func setPending(dir, p string, left *Pending) (*Pending, error) {
	if dir == "" {
		return nil, errors.New("mamelet has no state directory")
	}
	rec, err := loadPending(dir)
	if err != nil {
		return nil, err
	}
	cur, had := rec.Pending[p]
	var before *Pending
	if had {
		before = &cur
	}
	switch {
	case left == nil && !had:
		return nil, nil
	case left == nil:
		delete(rec.Pending, p)
	case had && cur.SHA256 == left.SHA256 && cur.Reload == left.Reload && cur.Unknown == left.Unknown && slices.Equal(cur.Restart, left.Restart) &&
		slices.Equal(cur.Reloads, left.Reloads):
		return before, nil
	default:
		rec.Pending[p] = *left
	}
	return before, savePending(dir, rec)
}

// savePending replaces the record (and removes it when nothing is pending).
func savePending(dir string, rec pendingRecord) error {
	if len(rec.Pending) == 0 && !rec.Lost {
		if err := os.Remove(filepath.Join(dir, pendingName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return saveJSON(dir, pendingName, rec)
}

// followUp reloads systemd when p is a unit and restarts the services of restart (each only if it
// runs), after the file at p (whose sha256 is sum) was made as approved; did is what was done
// before, which the detail goes on from. What is left is recorded before it starts, and taken out
// once it all has.
func followUp(ctx context.Context, x *mamelet.Act, p, sum string, restart []string, did []string) (outcome, detail string) {
	return followFiles(ctx, x, []after{{path: p, sum: sum, restart: restart}}, false, did)
}

// after is what follows the write of one file: its path, the sha256 written, and the services its
// declaration reloads and restarts.
type after struct {
	path    string
	sum     string
	reloads []string
	restart []string
}

// left is all that follows the write: nothing of it is done yet.
func (a after) left() Pending {
	return Pending{SHA256: a.sum, Reload: IsUnitFile(a.path), Reloads: slices.Clone(a.reloads), Restart: slices.Clone(a.restart)}
}

func (l Pending) empty() bool { return !l.Reload && len(l.Reloads) == 0 && len(l.Restart) == 0 }

// followFiles is what follows the writes of files (one, or a check group's, whose check passed):
// systemctl daemon-reload if one of them is a unit, then each service their declarations reload,
// once, if it is active (it must still be active after), then each service they restart, once, if it
// runs (try-restart; with confirm, one that ran must still be active after). did is what was done
// before, which the detail goes on from. What is left of each file is recorded before it starts
// (pendingBefore did, before the write), and taken out once it is done: a step that fails stops what
// follows, which stays pending (the next plan asks to approve it again); a service found not active
// after its reload or restart is reported (outcome failed), not left pending (it was done).
func followFiles(ctx context.Context, x *mamelet.Act, files []after, confirm bool, did []string) (outcome, detail string) {
	dir := stateDir(x.System)
	lefts := make([]Pending, len(files))
	nothing := true
	for i, f := range files {
		lefts[i] = f.left()
		nothing = nothing && lefts[i].empty()
	}
	record := func() error {
		var first error
		for i, f := range files {
			left := &lefts[i]
			if left.empty() {
				left = nil
			}
			if _, err := setPending(dir, f.path, left); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
	if nothing {
		record()
		return noderev.OutcomeDone, strings.Join(did, "; ")
	}
	if err := record(); err != nil {
		did = append(did, "(what is left could not be recorded: "+err.Error()+")")
	}
	stop := func(what string, err error) (string, string) {
		record()
		var all Pending
		for _, l := range lefts {
			all.Reload = all.Reload || l.Reload
			all.Reloads = appendUnits(all.Reloads, l.Reloads...)
			all.Restart = appendUnits(all.Restart, l.Restart...)
		}
		stays := "the file stays as written"
		if len(files) > 1 {
			stays = "the files stay as written"
		}
		return noderev.OutcomeFailed, strings.Join(did, "; ") + "; " + what + ": " + err.Error() +
			"; " + stays + ", and what is left (" + pendingText(all) + ") is pending: the next plan asks to approve it again"
	}
	var problems []string
	if slices.ContainsFunc(lefts, func(l Pending) bool { return l.Reload }) {
		if _, err := x.System.Run(ctx, progSystemctl, []string{"daemon-reload"}); err != nil {
			return stop("systemctl daemon-reload", err)
		}
		did = append(did, "reloaded systemd")
		for i := range lefts {
			lefts[i].Reload = false
		}
	}
	done := func(svc string, list func(l *Pending) *[]string) {
		for i := range lefts {
			l := list(&lefts[i])
			*l = slices.DeleteFunc(*l, func(s string) bool { return unitOf(s) == unitOf(svc) })
		}
	}
	var reloads, restarts []string
	for _, l := range lefts {
		reloads, restarts = appendUnits(reloads, l.Reloads...), appendUnits(restarts, l.Restart...)
	}
	for _, svc := range reloads {
		reloaded, err := reloadService(ctx, x.System, svc)
		var inactive *notActiveError
		switch {
		case errors.As(err, &inactive):
			problems = append(problems, err.Error())
		case err != nil:
			return stop("reload "+svc, err)
		case reloaded:
			did = append(did, "reloaded "+svc)
		default:
			did = append(did, svc+" is not active: not reloaded")
		}
		done(svc, func(l *Pending) *[]string { return &l.Reloads })
	}
	for _, svc := range restarts {
		ran := false
		if confirm {
			if st, err := x.System.Service(ctx, svc); err == nil {
				ran = st.Running
			}
		}
		if err := tryRestart(ctx, x.System, svc); err != nil {
			return stop("restart "+svc, err)
		}
		did = append(did, "restarted "+svc+" if it ran")
		if ran {
			if st, err := x.System.Service(ctx, svc); err != nil || !st.Running {
				problems = append(problems, (&notActiveError{svc, "restart"}).Error())
			}
		}
		done(svc, func(l *Pending) *[]string { return &l.Restart })
	}
	for i := range lefts {
		lefts[i] = Pending{SHA256: files[i].sum}
	}
	if err := record(); err != nil {
		did = append(did, "(the record of what was pending could not be cleared: "+err.Error()+")")
	}
	if len(problems) > 0 {
		return noderev.OutcomeFailed, strings.Join(did, "; ") + "; but " + strings.Join(problems, "; ")
	}
	return noderev.OutcomeDone, strings.Join(did, "; ")
}

// appendUnits appends to l the services of more it does not name yet (with or without ".service").
func appendUnits(l []string, more ...string) []string {
	for _, svc := range more {
		if !slices.ContainsFunc(l, func(s string) bool { return unitOf(s) == unitOf(svc) }) {
			l = append(l, svc)
		}
	}
	return l
}

// notActiveError: a service was active, and is not after mamelet reloaded or restarted it.
type notActiveError struct{ service, what string }

func (e *notActiveError) Error() string {
	return fmt.Sprintf("%s is not active after its %s (journalctl -u %s on the node says why)", e.service, e.what, e.service)
}

// reloadService reloads a service if it is active (reloaded false: it is not, and nothing was done),
// and checks that it is still active after (a *notActiveError when it is not), without holding
// mamelet for longer than mamelet.StartTimeout.
func reloadService(ctx context.Context, s *mamelet.System, name string) (reloaded bool, err error) {
	st, err := s.Service(ctx, name)
	if err != nil {
		return false, err
	}
	if !st.Running {
		return false, nil
	}
	sctx, cancel := context.WithTimeout(ctx, mamelet.StartTimeout)
	defer cancel()
	args := []string{"reload", "--", name}
	out, errOut, code, err := s.Runner.Run(sctx, progSystemctl, args, nil)
	switch {
	case (err != nil || code != 0) && sctx.Err() != nil && ctx.Err() == nil:
		return false, fmt.Errorf("%s did not finish reloading within %s", name, mamelet.StartTimeout)
	case err != nil:
		return false, err
	case code != 0:
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return false, fmt.Errorf("%s %s: exit %d: %s", progSystemctl, strings.Join(args, " "), code, msg)
	}
	if st, err := s.Service(ctx, name); err != nil || !st.Running {
		return true, &notActiveError{name, "reload"}
	}
	return true, nil
}

// pendingBefore records, before a write of p (whose sha256 will be sum) that restarts restart, what
// will be left once it is written: mamelet killed (or the node's power lost) between the write and
// followUp then leaves the restarts recorded, not forgotten (an entry of a sha256 the file does not
// have yet is not reported: observePending). undo puts back what was recorded before, for a write
// that failed before the file was replaced. Nothing is recorded for a write nothing follows.
func pendingBefore(dir, p, sum string, restart []string) (undo func(), err error) {
	return pendingBeforeWith(dir, p, sum, restart, nil)
}

// pendingBeforeWith is pendingBefore of a write that also reloads the services of reloads.
func pendingBeforeWith(dir, p, sum string, restart, reloads []string) (undo func(), err error) {
	left := after{path: p, sum: sum, reloads: reloads, restart: restart}.left()
	if left.empty() {
		return func() {}, nil
	}
	before, err := setPending(dir, p, &left)
	if err != nil {
		return func() {}, err
	}
	return func() { setPending(dir, p, before) }, nil
}

// pendingText is what is pending, for a person: "systemctl daemon-reload, reload of nginx, restart of app".
func pendingText(left Pending) string {
	var what []string
	if left.Reload {
		what = append(what, "systemctl daemon-reload")
	}
	if len(left.Reloads) > 0 {
		what = append(what, "reload of "+strings.Join(left.Reloads, ", "))
	}
	if len(left.Restart) > 0 {
		what = append(what, "restart of "+strings.Join(left.Restart, ", "))
	}
	return strings.Join(what, ", ")
}

// observePending reports what is pending after the last write of each file and artifact of the
// observation, while it is still the file written; the record forgets one whose file changed since (a
// new write follows, with its own restarts). A path the part no longer names is kept (up to
// maxPendingKept, by path), not reported: a file taken out of the declaration and put back is still
// the one written, and what was to follow its write (the restart of a running service) still has not
// happened, so the plan asks to approve it again then.
func observePending(s *mamelet.System, o *Observed) {
	dir := stateDir(s)
	rec, err := loadPending(dir)
	if err != nil {
		// Not read this time (or neither decoded nor set aside): whether what follows each write
		// finished is not known this round. Nothing is written over the record.
		rec.Lost = true
	}
	if rec.Lost {
		// What was pending was lost: each file and artifact found is marked as not known (while it is
		// still the file it is now), for the plan to ask to approve what follows its write again.
		mark := func(st *FileState) {
			if _, ok := rec.Pending[st.Path]; !ok && st.Error == "" && st.Unsafe == "" && st.Exists && st.SHA256 != "" {
				rec.Pending[st.Path] = Pending{SHA256: st.SHA256, Reload: IsUnitFile(st.Path), Unknown: true}
			}
		}
		for i := range o.Files {
			mark(&o.Files[i])
		}
		for i := range o.Artifacts {
			mark(&o.Artifacts[i].FileState)
		}
		if err == nil {
			rec.Lost = false
			savePending(dir, rec)
		}
	}
	if len(rec.Pending) == 0 {
		return
	}
	keep := map[string]Pending{}
	seen := map[string]bool{}
	visit := func(st *FileState) {
		seen[st.Path] = true
		left, ok := rec.Pending[st.Path]
		if !ok {
			return
		}
		switch {
		case st.Error != "":
			keep[st.Path] = left // not looked at this time
		case st.Exists && st.SHA256 == left.SHA256:
			keep[st.Path] = left
			l := left
			st.Pending = &l
		}
	}
	for i := range o.Files {
		visit(&o.Files[i])
	}
	for i := range o.Artifacts {
		visit(&o.Artifacts[i].FileState)
	}
	// What the part does not name now is kept as it is, the first maxPendingKept by path.
	var unnamed []string
	for path := range rec.Pending {
		if !seen[path] {
			unnamed = append(unnamed, path)
		}
	}
	slices.Sort(unnamed)
	for _, path := range unnamed {
		if len(keep) >= maxPendingKept {
			break
		}
		keep[path] = rec.Pending[path]
	}
	if len(keep) != len(rec.Pending) && err == nil {
		savePending(dir, pendingRecord{Pending: keep})
	}
}

// maxPendingKept bounds the record of what is pending (files and artifacts a node has at most, with
// room for those taken out of the declaration).
const maxPendingKept = 2 * (MaxNodeFiles + MaxNodeArtifacts)

// cleanStateDir removes the temporary files of the extension that an interrupted attempt left in
// mamelet's state directory (a download, a member taken out of one, a record being replaced): only
// files of those names, which no attempt is using between rounds.
func cleanStateDir(s *mamelet.System) {
	dir := stateDir(s)
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		stale := strings.HasPrefix(n, ".linux-artifact-") || strings.HasPrefix(n, ".linux-apt-key-") ||
			n == "."+recordName+".tmp" || n == "."+pendingName+".tmp" || n == "."+checksName+".tmp"
		if stale && e.Type()&fs.ModeType == 0 {
			os.Remove(filepath.Join(dir, n))
		}
	}
}
