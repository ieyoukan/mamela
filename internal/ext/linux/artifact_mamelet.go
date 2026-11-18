package linux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
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

// What mamelet observes of the artifacts and how it installs them. An extracted member's sha256 is
// not the download's, so mamelet keeps a record of what it installed (artifactRecord, in its own
// state directory): an artifact is the declared one when the file at its path is still exactly the
// file mamelet installed there from a download of the declared sha256 (and member).

// recordName is the file of mamelet's record of the artifacts it installed, in its state directory.
const recordName = "linux-artifacts.json"

// artifactRecord is what mamelet installed, by path.
type artifactRecord struct {
	Artifacts map[string]installed `json:"artifacts"`
}

// installed is what mamelet installed at a path: the download (Source), and the sha256 of the file
// it put there.
type installed struct {
	Source    `json:",inline"`
	Installed string `json:"installed"`
}

// stateDir is mamelet's state directory as the extension reaches it ("" when it has none).
func stateDir(s *mamelet.System) string {
	if s.StateDir == "" {
		return ""
	}
	return s.Path(s.StateDir)
}

// loadRecord reads the record (empty when there is none yet).
func loadRecord(dir string) (artifactRecord, error) {
	rec := artifactRecord{Artifacts: map[string]installed{}}
	if dir == "" {
		return rec, errors.New("mamelet has no state directory")
	}
	data, err := os.ReadFile(filepath.Join(dir, recordName))
	if errors.Is(err, fs.ErrNotExist) {
		return rec, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &rec, json.RejectUnknownMembers(true))
	}
	if err != nil {
		return artifactRecord{Artifacts: map[string]installed{}}, fmt.Errorf("the record of the artifacts mamelet installed (%s): %w", recordName, err)
	}
	if rec.Artifacts == nil {
		rec.Artifacts = map[string]installed{}
	}
	return rec, nil
}

// saveRecord replaces the record (saveJSON).
func saveRecord(dir string, rec artifactRecord) error { return saveJSON(dir, recordName, rec) }

// saveJSON replaces the file name of mamelet's state directory dir with v, atomically: a new file
// (readable by mamelet only), synced, renamed over the old one.
func saveJSON(dir, name string, v any) error {
	data, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// observeArtifacts observes each artifact of the part (with where it came from, when it is still
// the file mamelet installed), and whether the users and groups it names exist.
func observeArtifacts(_ context.Context, s *mamelet.System, p Part, o *Observed) {
	if len(p.Artifacts) == 0 {
		return
	}
	r, err := openTree(s)
	if err != nil {
		for _, a := range p.Artifacts {
			o.Artifacts = append(o.Artifacts, ArtifactState{FileState: FileState{Path: a.Path, Error: err.Error()}})
		}
		return
	}
	defer r.Close()
	acc := readAccounts(r)
	// A record that cannot be read is taken as empty: every artifact is then reported as one mamelet
	// did not install, and the plan asks to install it again (which writes a new record).
	rec, _ := loadRecord(stateDir(s))
	for _, a := range p.Artifacts {
		st := ArtifactState{FileState: FileState{Path: a.Path}}
		if e := CheckPath(a.Path); e != nil {
			st.Error = "mamelet refuses the path: " + e.Message
		} else {
			removeStaleTemp(r, a.Path)
			st.FileState = inspectUpTo(r, a.Path, acc, MaxArtifact)
			if in, ok := rec.Artifacts[a.Path]; ok && st.Exists && st.Unsafe == "" && st.Error == "" && !st.Large && st.SHA256 == in.Installed {
				from := in.Source
				st.From = &from
			}
		}
		o.Artifacts = append(o.Artifacts, st)
		owners, groups := slices.Clone(a.Owners), slices.Clone(a.Groups)
		if in := a.Install; in != nil {
			owners, groups = append(owners, in.Owner), append(groups, in.Group)
		}
		for _, u := range owners {
			if o.Users == nil {
				o.Users = map[string]bool{}
			}
			_, o.Users[u] = acc.users[u]
		}
		for _, g := range groups {
			if o.Groups == nil {
				o.Groups = map[string]bool{}
			}
			_, o.Groups[g] = acc.groups[g]
		}
	}
}

// actArtifacts carries out the approved installations, each attempt once, from a revision fetched
// this round.
func actArtifacts(ctx context.Context, x *mamelet.Act, p Part) bool {
	if !x.Online {
		return false
	}
	changed := false
	for _, a := range p.Artifacts {
		in := a.Install
		if ctx.Err() != nil {
			break // mamelet is stopping: what is left goes on in a later round
		}
		if in == nil || x.Done(in.Grant.Operation, in.Grant.Attempt) {
			continue
		}
		outcome, detail, ran := installArtifact(ctx, x, a.Path, *in)
		x.Record(in.Grant.Operation, in.Grant.Attempt, outcome, detail)
		changed = changed || ran
	}
	return changed
}

// downloadName is the temporary file of a download for the artifact at p, in mamelet's state
// directory: the same for every attempt, so that one that was interrupted leaves one at most, and the
// next removes it and starts over.
func downloadName(p, suffix string) string {
	sum := sha256.Sum256([]byte(p))
	return ".linux-artifact-" + hex.EncodeToString(sum[:8]) + suffix
}

// installArtifact carries out one approved installation. It checks again, whatever the revision
// says, that the path is one Mamela writes (and not mamelet's own state), that the installation is
// well formed, and that nothing at the path or above it is a symbolic link. Then, unless the file
// there is still the one it installed from that download, it downloads the URL into its state
// directory, checks the sha256 before anything else, takes the member out of it when one is named,
// installs it (atomically, never through a link), checks the file it installed and records it. It
// reloads systemd for a unit, and restarts the running services the artifact names. ran says
// whether it changed anything.
func installArtifact(ctx context.Context, x *mamelet.Act, p string, in Install) (outcome, detail string, ran bool) {
	refused := func(why string) (string, string, bool) { return noderev.OutcomeRefused, why, false }
	if e := CheckPath(p); e != nil {
		return refused("mamelet does not install " + p + ": " + e.Message)
	}
	if x.StateDir != "" && (Under(p, x.StateDir) || Under(x.StateDir, p)) {
		return refused("mamelet does not install " + p + ": it is mamelet's own state")
	}
	if err := checkInstall(in); err != nil {
		return refused(err.Error())
	}
	dir := stateDir(x.System)
	if dir == "" {
		return refused("mamelet has no state directory to download into and to record what it installs in")
	}
	r, err := openTree(x.System)
	if err != nil {
		return noderev.OutcomeFailed, err.Error(), false
	}
	defer r.Close()
	acc := readAccounts(r)
	uid, okU := acc.users[in.Owner]
	gid, okG := acc.groups[in.Group]
	switch {
	case !okU:
		return refused(fmt.Sprintf("user %s does not exist on this node", in.Owner))
	case !okG:
		return refused(fmt.Sprintf("group %s does not exist on this node", in.Group))
	}
	got := inspectUpTo(r, p, acc, MaxArtifact)
	switch {
	case got.Error != "":
		return noderev.OutcomeFailed, "observe " + p + ": " + got.Error, false
	case got.Unsafe != "":
		return refused("mamelet does not install " + p + ": " + got.Unsafe + " (mamelet never follows a symbolic link)")
	}
	rec, recErr := loadRecord(dir)
	prev, recorded := rec.Artifacts[p]
	same := recErr == nil && recorded && prev.SHA256 == in.SHA256 && prev.Extract == in.Extract &&
		got.Exists && !got.Large && got.SHA256 == prev.Installed
	// Compare and swap: the path is as the approved plan saw it, or has the approved installation
	// already (an attempt that was interrupted, or another apply).
	if !same && !in.Before.Is(got) {
		return refused(fmt.Sprintf("%s changed since the plan (it was %s, it is %s); nothing was installed (plan again)", p, in.Before, beforeOf(got)))
	}
	dirUID, dirGID := 0, 0
	if id, ok := acc.users["root"]; ok {
		dirUID = id
	}
	if id, ok := acc.groups["root"]; ok {
		dirGID = id
	}
	var did []string
	sum := prev.Installed // the sha256 of the file at p once it is installed
	switch {
	case same && got.Mode == in.Mode && got.Owner == in.Owner && got.Group == in.Group:
		// Installed already (by an attempt that was interrupted before it said so): what follows an
		// installation still follows.
		did = append(did, p+" was already as approved")
	case same:
		// Only the mode or the owner differs: the installed file is put in place again with them,
		// without downloading it (what is copied must still be what mamelet installed).
		ran = true
		src, fi, _, err := openRegular(r, p)
		if err != nil {
			return noderev.OutcomeFailed, "open " + p + ": " + err.Error(), ran
		}
		err = writeFrom(r, p, src, fi.Size(), prev.Installed, in.Mode, uid, gid, dirUID, dirGID)
		src.Close()
		if err != nil {
			return noderev.OutcomeFailed, "install " + p + " again: " + err.Error(), ran
		}
		did = append(did, fmt.Sprintf("set %s to %s %s:%s", p, in.Mode, in.Owner, in.Group))
	default:
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return noderev.OutcomeFailed, err.Error(), false
		}
		ran = true
		var err error
		sum, err = fetchArtifact(ctx, x.System, dir, p, in, func(file, sum string) error {
			src, err := os.Open(file)
			if err != nil {
				return err
			}
			defer src.Close()
			fi, err := src.Stat()
			if err != nil {
				return err
			}
			return writeFrom(r, p, src, fi.Size(), sum, in.Mode, uid, gid, dirUID, dirGID)
		})
		if err != nil {
			var no *Refusal
			if errors.As(err, &no) {
				return noderev.OutcomeRefused, err.Error(), ran
			}
			return noderev.OutcomeFailed, err.Error(), ran
		}
		member := ""
		if in.Extract != "" {
			member = ", member " + in.Extract
		}
		did = append(did, fmt.Sprintf("installed %s from %s (sha256 %s%s; %s %s:%s)", p, in.URL, in.SHA256[:12], member, in.Mode, in.Owner, in.Group))
		if len(got.Missing) > 0 {
			did = append(did, "made "+strings.Join(got.Missing, ", "))
		}
	}
	after := inspectUpTo(r, p, acc, MaxArtifact)
	if after.SHA256 != sum || after.Mode != in.Mode || after.Owner != in.Owner || after.Group != in.Group {
		return noderev.OutcomeFailed, fmt.Sprintf("after installing %s, it is %s %s %s:%s, not as approved", p, after.SHA256, after.Mode, after.Owner, after.Group), ran
	}
	// The record says what is at p now, and where it came from (the approved URL: one of the same
	// sha256 is the same download).
	now := installed{Source: Source{URL: in.URL, SHA256: in.SHA256, Extract: in.Extract}, Installed: sum}
	if recErr != nil || !recorded || prev != now {
		rec.Artifacts[p] = now
		if err := saveRecord(dir, rec); err != nil {
			return noderev.OutcomeFailed, strings.Join(did, "; ") + "; but the record of it could not be written: " + err.Error(), ran
		}
	}
	outcome, detail = followUp(ctx, x, p, sum, in.Restart, did)
	return outcome, detail, true
}

// fetchArtifact downloads the installation's URL into mamelet's state directory, checks its sha256,
// takes the member out of it when one is named, and gives install the file to install and its
// sha256; it returns that sha256. The temporary files are removed whatever happens (and those an
// interrupted attempt left, before it starts).
func fetchArtifact(ctx context.Context, s *mamelet.System, dir, p string, in Install, install func(file, sum string) error) (string, error) {
	download, member := filepath.Join(dir, downloadName(p, ".download")), filepath.Join(dir, downloadName(p, ".member"))
	for _, f := range []string{download, member} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("remove what an earlier download left: %w", err)
		}
		defer os.Remove(f)
	}
	client := HTTPSClient(s.HTTP)
	defer client.CloseIdleConnections()
	if err := Fetch(ctx, client, in.URL, in.SHA256, download, MaxArtifact); err != nil {
		return "", err
	}
	file, sum := download, in.SHA256
	if in.Extract != "" {
		var err error
		if sum, err = ExtractMember(download, in.Extract, member, MaxArtifact); err != nil {
			return "", err
		}
		// The download is not needed any more: only the member is installed.
		os.Remove(download)
		file = member
	}
	if err := install(file, sum); err != nil {
		return "", fmt.Errorf("install %s: %w", p, err)
	}
	return sum, nil
}

// artifactResults are the recorded outcomes of the part's installations.
func artifactResults(p Part, known func(op string, attempt int) (noderev.Result, bool)) []noderev.Result {
	var out []noderev.Result
	for _, a := range p.Artifacts {
		if in := a.Install; in != nil {
			if r, ok := known(in.Grant.Operation, in.Grant.Attempt); ok {
				out = append(out, r)
			}
		}
	}
	return out
}
