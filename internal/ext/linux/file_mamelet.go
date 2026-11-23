package linux

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// progSystemctl is the core's systemctl: a unit is read again after it is written (daemon-reload),
// and the services a file names are restarted if they run (try-restart). linux runs nothing else.
const progSystemctl = "/usr/bin/systemctl"

// observeFiles observes each file of the part, and whether the users and groups it names exist.
func observeFiles(_ context.Context, s *mamelet.System, p Part, o *Observed) {
	if len(p.Files) == 0 {
		return
	}
	r, err := openTree(s)
	if err != nil {
		for _, f := range p.Files {
			o.Files = append(o.Files, FileState{Path: f.Path, Error: err.Error()})
		}
		return
	}
	defer r.Close()
	a := readAccounts(r)
	for _, f := range p.Files {
		if e := CheckPath(f.Path); e != nil {
			o.Files = append(o.Files, FileState{Path: f.Path, Error: "mamelet refuses the path: " + e.Message})
		} else {
			removeStaleTemp(r, f.Path)
			o.Files = append(o.Files, withShadows(r, inspect(r, f.Path, a)))
		}
		owners, groups := slices.Clone(f.Owners), slices.Clone(f.Groups)
		if w := f.Write; w != nil {
			owners, groups = append(owners, w.Owner), append(groups, w.Group)
		}
		for _, u := range owners {
			if o.Users == nil {
				o.Users = map[string]bool{}
			}
			_, o.Users[u] = a.users[u]
		}
		for _, g := range groups {
			if o.Groups == nil {
				o.Groups = map[string]bool{}
			}
			_, o.Groups[g] = a.groups[g]
		}
	}
}

// actFiles carries out the approved writes, each attempt once, from a revision fetched this round:
// those without a check one at a time, in the order of their paths, then the writes of each check
// together (writeGroup).
func actFiles(ctx context.Context, x *mamelet.Act, p Part) bool {
	if !x.Online {
		return false
	}
	changed := false
	groups := map[string][]File{}
	for _, f := range p.Files {
		w := f.Write
		if ctx.Err() != nil {
			break // mamelet is stopping: what is left goes on in a later round
		}
		if w == nil || x.Done(w.Grant.Operation, w.Grant.Attempt) {
			continue
		}
		if w.Check != "" {
			groups[w.Check] = append(groups[w.Check], f)
			continue
		}
		outcome, detail, ran := writeFile(ctx, x, f.Path, *w)
		x.Record(w.Grant.Operation, w.Grant.Attempt, outcome, detail)
		changed = changed || ran
	}
	for _, check := range slices.Sorted(maps.Keys(groups)) {
		if ctx.Err() != nil {
			break
		}
		changed = writeGroup(ctx, x, check, groups[check]) || changed
	}
	return changed
}

// writeFile carries out one approved write. It checks again, whatever the revision says, that the
// path is one Mamela writes (and not mamelet's own state), that the content is the approved one, and
// that nothing at the path or above it is a symbolic link; then it writes the file (unless it is as
// approved already), reloads systemd for a unit, reloads the active services and restarts the running
// services the file names. ran says whether it changed anything.
func writeFile(ctx context.Context, x *mamelet.Act, p string, w Write) (outcome, detail string, ran bool) {
	if outcome, detail := refuseWrite(x, p, w); outcome != "" {
		return outcome, detail, false
	}
	r, err := openTree(x.System)
	if err != nil {
		return noderev.OutcomeFailed, err.Error(), false
	}
	defer r.Close()
	pw, outcome, detail := prepareWrite(r, p, w)
	if outcome != "" {
		return outcome, detail, false
	}
	var did []string
	if pw.write {
		// What follows the write is recorded before it (mamelet killed between the two would forget it
		// otherwise), and put back as it was if the file is not replaced.
		undo, err := pendingBeforeWith(stateDir(x.System), p, w.SHA256, w.Restart, w.Reload)
		if err != nil {
			return noderev.OutcomeFailed, "record what follows the write of " + p + ": " + err.Error() + "; nothing was written", false
		}
		ran = true
		d, err := makeWrite(r, pw)
		if err != nil {
			if !errors.Is(err, errWrittenOther) {
				undo()
			}
			return noderev.OutcomeFailed, err.Error(), ran
		}
		did = d
	} else {
		// Written already (by an attempt that was interrupted before it said so, or by hand): what
		// follows a write still follows.
		did = append(did, p+" was already as approved")
	}
	outcome, detail = followFiles(ctx, x, []after{{path: p, sum: w.SHA256, reloads: w.Reload, restart: w.Restart}}, false, did)
	return outcome, detail, true
}

// refuseWrite says why mamelet does not make an approved write, whatever the revision says (outcome
// "": it may go on): the path is not one Mamela writes, or is mamelet's own state, or the write is not
// a valid one (its content is not the approved one).
func refuseWrite(x *mamelet.Act, p string, w Write) (outcome, detail string) {
	refused := func(why string) (string, string) { return noderev.OutcomeRefused, why }
	if e := CheckPath(p); e != nil {
		return refused("mamelet does not write " + p + ": " + e.Message)
	}
	if x.StateDir != "" && (Under(p, x.StateDir) || Under(x.StateDir, p)) {
		return refused("mamelet does not write " + p + ": it is mamelet's own state")
	}
	if err := checkWrite(w); err != nil {
		return refused(err.Error())
	}
	return "", ""
}

// prepared is an approved write mamelet checked against the node: what is at the path, whether the
// write changes it (the file is not as approved yet), and the ids it gives the file and the
// directories it makes.
type prepared struct {
	path                     string
	w                        Write
	got                      FileState
	write                    bool
	uid, gid, dirUID, dirGID int
}

// prepareWrite checks an approved write against the node (outcome "": it may go on): nothing at the
// path or above it is a symbolic link, the path would run instead of no program of the node's
// packages, its owner and group exist, and the file is as the approved plan saw it, or as approved
// already (compare and swap).
func prepareWrite(r *os.Root, p string, w Write) (pw prepared, outcome, detail string) {
	refused := func(why string) (prepared, string, string) { return prepared{}, noderev.OutcomeRefused, why }
	switch prog, err := shadowed(r, p); {
	case err != nil:
		return prepared{}, noderev.OutcomeFailed, err.Error() + "; nothing was written"
	case prog != "":
		return refused("mamelet does not write " + p + ": " + shadowsWhy(p, prog) + "; nothing was written")
	}
	a := readAccounts(r)
	uid, okU := a.users[w.Owner]
	gid, okG := a.groups[w.Group]
	switch {
	case !okU:
		return refused(fmt.Sprintf("user %s does not exist on this node", w.Owner))
	case !okG:
		return refused(fmt.Sprintf("group %s does not exist on this node", w.Group))
	}
	got := inspect(r, p, a)
	switch {
	case got.Error != "":
		return prepared{}, noderev.OutcomeFailed, "observe " + p + ": " + got.Error
	case got.Unsafe != "":
		return refused("mamelet does not write " + p + ": " + got.Unsafe + " (mamelet never follows a symbolic link)")
	}
	// Compare and swap: the file is as the approved plan saw it, or has the approved content already
	// (an attempt that was interrupted, or another apply): a write approved for one content never
	// overwrites another.
	if !(got.Exists && !got.Large && got.SHA256 == w.SHA256) && !w.Before.Is(got) {
		return refused(fmt.Sprintf("%s changed since the plan (it was %s, it is %s); nothing was written (plan again)", p, w.Before, beforeOf(got)))
	}
	pw = prepared{path: p, w: w, got: got, uid: uid, gid: gid,
		write: !(got.Exists && !got.Large && got.SHA256 == w.SHA256 && got.Mode == w.Mode && got.Owner == w.Owner && got.Group == w.Group)}
	if id, ok := a.users["root"]; ok {
		pw.dirUID = id
	}
	if id, ok := a.groups["root"]; ok {
		pw.dirGID = id
	}
	return pw, "", ""
}

// errWrittenOther: the file was replaced, but is not as approved after.
var errWrittenOther = errors.New("not as approved after it was written")

// makeWrite writes the file of a prepared write, and checks that it is as approved after; it says what
// it did. An error that is errWrittenOther says the file was replaced (with what the error says).
func makeWrite(r *os.Root, pw prepared) ([]string, error) {
	p, w := pw.path, pw.w
	if err := writeTo(r, p, w, pw.uid, pw.gid, pw.dirUID, pw.dirGID); err != nil {
		return nil, fmt.Errorf("write %s: %w", p, err)
	}
	after := inspect(r, p, readAccounts(r))
	if after.SHA256 != w.SHA256 || after.Mode != w.Mode || after.Owner != w.Owner || after.Group != w.Group {
		return nil, fmt.Errorf("after writing %s, it is %s %s %s:%s, %w", p, after.SHA256, after.Mode, after.Owner, after.Group, errWrittenOther)
	}
	did := []string{fmt.Sprintf("wrote %s (sha256 %s, %s %s:%s)", p, w.SHA256[:12], w.Mode, w.Owner, w.Group)}
	if len(pw.got.Missing) > 0 {
		did = append(did, "made "+strings.Join(pw.got.Missing, ", "))
	}
	return did, nil
}

// tryRestart restarts a service if it runs (and does nothing if it does not), without holding
// mamelet for longer than mamelet.StartTimeout.
func tryRestart(ctx context.Context, s *mamelet.System, name string) error {
	sctx, cancel := context.WithTimeout(ctx, mamelet.StartTimeout)
	defer cancel()
	args := []string{"try-restart", "--", name}
	out, errOut, code, err := s.Runner.Run(sctx, progSystemctl, args, nil)
	switch {
	case (err != nil || code != 0) && sctx.Err() != nil && ctx.Err() == nil:
		return fmt.Errorf("%s did not finish restarting within %s (systemd keeps starting it)", name, mamelet.StartTimeout)
	case err != nil:
		return err
	case code == 0:
		return nil
	case code == systemctlNotLoaded:
		// The unit is not loaded: there is no unit file for it (yet: the file that makes it may come
		// later in the same round, after an artifact it runs). A service that does not exist does not
		// run, and try-restart restarts only what runs: there is nothing to do.
		return nil
	}
	msg := strings.TrimSpace(string(errOut))
	if msg == "" {
		msg = strings.TrimSpace(string(out))
	}
	return fmt.Errorf("%s %s: exit %d: %s", progSystemctl, strings.Join(args, " "), code, msg)
}

// systemctlNotLoaded is systemctl's exit status for a unit that is not loaded (LSB "program is not
// installed"): "Unit x.service not found."
const systemctlNotLoaded = 5

// fileResults are the recorded outcomes of the part's writes.
func fileResults(p Part, known func(op string, attempt int) (noderev.Result, bool)) []noderev.Result {
	var out []noderev.Result
	for _, f := range p.Files {
		if w := f.Write; w != nil {
			if r, ok := known(w.Grant.Operation, w.Grant.Attempt); ok {
				out = append(out, r)
			}
		}
	}
	return out
}
