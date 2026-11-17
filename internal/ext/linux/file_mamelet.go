package linux

import (
	"context"
	"fmt"
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
			o.Files = append(o.Files, inspect(r, f.Path, a))
		}
		owners, groups := []string{f.Owner}, []string{f.Group}
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

// actFiles carries out the approved writes, each attempt once, from a revision fetched this round.
func actFiles(ctx context.Context, x *mamelet.Act, p Part) bool {
	if !x.Online {
		return false
	}
	changed := false
	for _, f := range p.Files {
		w := f.Write
		if w == nil || x.Done(w.Grant.Operation, w.Grant.Attempt) {
			continue
		}
		outcome, detail, ran := writeFile(ctx, x, f.Path, *w)
		x.Record(w.Grant.Operation, w.Grant.Attempt, outcome, detail)
		changed = changed || ran
	}
	return changed
}

// writeFile carries out one approved write. It checks again, whatever the revision says, that the
// path is one Mamela writes (and not mamelet's own state), that the content is the approved one, and
// that nothing at the path or above it is a symbolic link; then it writes the file (unless it is as
// approved already), reloads systemd for a unit, and restarts the running services the file names.
// ran says whether it changed anything.
func writeFile(ctx context.Context, x *mamelet.Act, p string, w Write) (outcome, detail string, ran bool) {
	refused := func(why string) (string, string, bool) { return noderev.OutcomeRefused, why, false }
	if e := CheckPath(p); e != nil {
		return refused("mamelet does not write " + p + ": " + e.Message)
	}
	if x.StateDir != "" && (Under(p, x.StateDir) || Under(x.StateDir, p)) {
		return refused("mamelet does not write " + p + ": it is mamelet's own state")
	}
	if err := checkWrite(w); err != nil {
		return refused(err.Error())
	}
	r, err := openTree(x.System)
	if err != nil {
		return noderev.OutcomeFailed, err.Error(), false
	}
	defer r.Close()
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
		return noderev.OutcomeFailed, "observe " + p + ": " + got.Error, false
	case got.Unsafe != "":
		return refused("mamelet does not write " + p + ": " + got.Unsafe + " (mamelet never follows a symbolic link)")
	}
	var did []string
	if !(got.Exists && !got.Large && got.SHA256 == w.SHA256 && got.Mode == w.Mode && got.Owner == w.Owner && got.Group == w.Group) {
		dirUID, dirGID := 0, 0
		if id, ok := a.users["root"]; ok {
			dirUID = id
		}
		if id, ok := a.groups["root"]; ok {
			dirGID = id
		}
		ran = true
		if err := writeTo(r, p, w, got.Missing, uid, gid, dirUID, dirGID); err != nil {
			return noderev.OutcomeFailed, "write " + p + ": " + err.Error(), ran
		}
		after := inspect(r, p, a)
		if after.SHA256 != w.SHA256 || after.Mode != w.Mode || after.Owner != w.Owner || after.Group != w.Group {
			return noderev.OutcomeFailed, fmt.Sprintf("after writing %s, it is %s %s %s:%s, not as approved", p, after.SHA256, after.Mode, after.Owner, after.Group), ran
		}
		did = append(did, fmt.Sprintf("wrote %s (sha256 %s, %s %s:%s)", p, w.SHA256[:12], w.Mode, w.Owner, w.Group))
		if len(got.Missing) > 0 {
			did = append(did, "made "+strings.Join(got.Missing, ", "))
		}
	} else {
		// Written already (by an attempt that was interrupted before it said so, or by hand): what
		// follows a write still follows.
		did = append(did, p+" was already as approved")
	}
	ran = true
	if IsUnitFile(p) {
		if _, err := x.System.Run(ctx, progSystemctl, []string{"daemon-reload"}); err != nil {
			return noderev.OutcomeFailed, strings.Join(did, "; ") + "; systemctl daemon-reload: " + err.Error(), ran
		}
		did = append(did, "reloaded systemd")
	}
	for _, svc := range w.Restart {
		if err := tryRestart(ctx, x.System, svc); err != nil {
			return noderev.OutcomeFailed, strings.Join(did, "; ") + "; restart " + svc + ": " + err.Error(), ran
		}
		did = append(did, "restarted "+svc+" if it ran")
	}
	return noderev.OutcomeDone, strings.Join(did, "; "), ran
}

// tryRestart restarts a service if it runs (and does nothing if it does not), without holding
// mamelet for longer than mamelet.StartTimeout.
func tryRestart(ctx context.Context, s *mamelet.System, name string) error {
	sctx, cancel := context.WithTimeout(ctx, mamelet.StartTimeout)
	defer cancel()
	_, err := s.Run(sctx, progSystemctl, []string{"try-restart", "--", name})
	if err != nil && sctx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("%s did not finish restarting within %s (systemd keeps starting it)", name, mamelet.StartTimeout)
	}
	return err
}

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
