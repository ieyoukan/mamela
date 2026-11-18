package linux

import (
	"context"
	"encoding/json/v2"
	"errors"
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
}

// loadPending reads the record (empty when there is none, or when it cannot be read: what is lost
// then is only what the plan would have said).
func loadPending(dir string) pendingRecord {
	rec := pendingRecord{Pending: map[string]Pending{}}
	if dir == "" {
		return rec
	}
	data, err := os.ReadFile(filepath.Join(dir, pendingName))
	if err != nil || json.Unmarshal(data, &rec, json.RejectUnknownMembers(true)) != nil || rec.Pending == nil {
		return pendingRecord{Pending: map[string]Pending{}}
	}
	return rec
}

// setPending records what is pending after the write of p (nil: nothing).
func setPending(dir, p string, left *Pending) error {
	if dir == "" {
		return errors.New("mamelet has no state directory")
	}
	rec := loadPending(dir)
	cur, had := rec.Pending[p]
	switch {
	case left == nil && !had:
		return nil
	case left == nil:
		delete(rec.Pending, p)
	case had && cur.SHA256 == left.SHA256 && cur.Reload == left.Reload && slices.Equal(cur.Restart, left.Restart):
		return nil
	default:
		rec.Pending[p] = *left
	}
	return savePending(dir, rec)
}

// savePending replaces the record (and removes it when nothing is pending).
func savePending(dir string, rec pendingRecord) error {
	if len(rec.Pending) == 0 {
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
	dir := stateDir(x.System)
	left := Pending{SHA256: sum, Reload: IsUnitFile(p), Restart: slices.Clone(restart)}
	if !left.Reload && len(left.Restart) == 0 {
		setPending(dir, p, nil)
		return noderev.OutcomeDone, strings.Join(did, "; ")
	}
	if err := setPending(dir, p, &left); err != nil {
		did = append(did, "(what is left could not be recorded: "+err.Error()+")")
	}
	stop := func(what string, err error) (string, string) {
		setPending(dir, p, &left)
		return noderev.OutcomeFailed, strings.Join(did, "; ") + "; " + what + ": " + err.Error() +
			"; the file stays as written, and what is left (" + pendingText(left) + ") is pending: the next plan asks to approve it again"
	}
	if left.Reload {
		if _, err := x.System.Run(ctx, progSystemctl, []string{"daemon-reload"}); err != nil {
			return stop("systemctl daemon-reload", err)
		}
		did = append(did, "reloaded systemd")
		left.Reload = false
	}
	for len(left.Restart) > 0 {
		svc := left.Restart[0]
		if err := tryRestart(ctx, x.System, svc); err != nil {
			return stop("restart "+svc, err)
		}
		did = append(did, "restarted "+svc+" if it ran")
		left.Restart = left.Restart[1:]
	}
	if err := setPending(dir, p, nil); err != nil {
		did = append(did, "(the record of what was pending could not be cleared: "+err.Error()+")")
	}
	return noderev.OutcomeDone, strings.Join(did, "; ")
}

// pendingText is what is pending, for a person: "systemctl daemon-reload, restart of nginx, app".
func pendingText(left Pending) string {
	var what []string
	if left.Reload {
		what = append(what, "systemctl daemon-reload")
	}
	if len(left.Restart) > 0 {
		what = append(what, "restart of "+strings.Join(left.Restart, ", "))
	}
	return strings.Join(what, ", ")
}

// observePending reports what is pending after the last write of each file and artifact of the
// observation, while it is still the file written; the record forgets a path the part no longer
// names, and one whose file changed since (a new write follows, with its own restarts).
func observePending(s *mamelet.System, o *Observed) {
	dir := stateDir(s)
	rec := loadPending(dir)
	if len(rec.Pending) == 0 {
		return
	}
	keep := map[string]Pending{}
	visit := func(st *FileState) {
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
	if len(keep) != len(rec.Pending) {
		savePending(dir, pendingRecord{Pending: keep})
	}
}

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
			n == "."+recordName+".tmp" || n == "."+pendingName+".tmp"
		if stale && e.Type()&fs.ModeType == 0 {
			os.Remove(filepath.Join(dir, n))
		}
	}
}
