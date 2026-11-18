package linux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/mamelet"
	"github.com/ieyoukan/mamela/internal/noderev"
)

// progAptGet is the core's apt-get, which mamelet runs to install packages: linux runs it to read the
// package lists again after a source is added (apt-get update), with the core's environment and lock
// timeout.
const progAptGet = "/usr/bin/apt-get"

// Bounds of what mamelet waits for.
var (
	// AptKeyTimeout bounds the download of a source's key.
	AptKeyTimeout = 30 * time.Second
	// AptUpdateTimeout bounds apt-get update (apt has its own timeouts for each server).
	AptUpdateTimeout = 10 * time.Minute
)

// maxAptDetail bounds what an outcome quotes of apt's messages.
const maxAptDetail = 1000

// aptListsDir is where apt keeps what apt-get update fetched.
const aptListsDir = "/var/lib/apt/lists"

// observeAptRepos observes each source of the part: its file and its key's two files, which of its
// URIs and suites apt has not read, and which other source files of the node name them too.
func observeAptRepos(_ context.Context, s *mamelet.System, p Part, o *Observed) {
	if len(p.AptRepos) == 0 {
		return
	}
	r, err := openTree(s)
	if err != nil {
		for _, repo := range p.AptRepos {
			o.AptRepos = append(o.AptRepos, AptRepoState{Name: repo.Name, Error: err.Error()})
		}
		return
	}
	defer r.Close()
	a := readAccounts(r)
	others := readAptSourceFiles(r)
	for _, repo := range p.AptRepos {
		st := AptRepoState{Name: repo.Name}
		var problem string
		if !ValidAptRepoName(repo.Name) {
			problem = quote(repo.Name) + " is not the name of an APT source"
		}
		for _, w := range repo.Where {
			if ps := aptWhereProblems(repo.Name, w.URIs, w.Suites); len(ps) > 0 && problem == "" {
				problem = ps[0].message
			}
		}
		if problem != "" {
			st.Error = "mamelet refuses the source: " + problem
		} else {
			st.Sources = inspect(r, AptSourcesPath(repo.Name), a)
			st.KeyArmored = inspect(r, AptKeyPath(repo.Name, true), a)
			st.KeyBinary = inspect(r, AptKeyPath(repo.Name, false), a)
			// Each declaration's URIs and suites by themselves, saying which they are.
			for _, w := range repo.Where {
				st.Where = append(st.Where, AptWhereState{AptWhere: w,
					Conflicts: aptConflicts(others, AptSourcesPath(repo.Name), w.URIs, w.Suites), Unread: aptUnread(r, w.URIs, w.Suites)})
			}
		}
		o.AptRepos = append(o.AptRepos, st)
	}
}

// actAptRepos carries out the approved additions, each attempt once, from a revision fetched this
// round.
func actAptRepos(ctx context.Context, x *mamelet.Act, p Part) bool {
	if !x.Online {
		return false
	}
	changed := false
	for _, r := range p.AptRepos {
		a := r.Add
		if a == nil || x.Done(a.Grant.Operation, a.Grant.Attempt) {
			continue
		}
		outcome, detail, ran := addAptRepo(ctx, x, r.Name, *a)
		x.Record(a.Grant.Operation, a.Grant.Attempt, outcome, detail)
		changed = changed || ran
	}
	return changed
}

// addAptRepo carries out one approved addition. It checks again, whatever the revision says, the
// source's values and that they render to the approved file, that no other source file names its URIs
// and suites, and that nothing at its files or above them is a symbolic link. It takes the key from the
// node if the approved one is there already (an attempt that was interrupted), and otherwise downloads
// it over https, and keeps it only if its sha256 is the approved one and it is an OpenPGP public key:
// nothing is written before. Then it writes the key, the source's file, removes the key's file of the
// other form, and runs apt-get update. Each step is skipped when it is done already, so an attempt
// that was interrupted is made again from the start. ran says whether it changed anything (or ran apt).
func addAptRepo(ctx context.Context, x *mamelet.Act, name string, a AptRepoAdd) (outcome, detail string, ran bool) {
	refused := func(why string) (string, string, bool) { return noderev.OutcomeRefused, why, false }
	failed := func(why string) (string, string, bool) { return noderev.OutcomeFailed, why, ran }
	if err := checkAptRepoAdd(name, a); err != nil {
		return refused(err.Error())
	}
	r, err := openTree(x.System)
	if err != nil {
		return failed(err.Error())
	}
	defer r.Close()
	acc := readAccounts(r)
	uid, gid := 0, 0
	if id, ok := acc.users["root"]; ok {
		uid = id
	}
	if id, ok := acc.groups["root"]; ok {
		gid = id
	}
	srcPath := AptSourcesPath(name)
	src, asc, gpg := inspect(r, srcPath, acc), inspect(r, AptKeyPath(name, true), acc), inspect(r, AptKeyPath(name, false), acc)
	for _, f := range []FileState{src, asc, gpg} {
		switch {
		case f.Error != "":
			return failed("observe " + f.Path + ": " + f.Error)
		case f.Unsafe != "":
			return refused("mamelet does not write " + f.Path + ": " + f.Unsafe + " (mamelet never follows a symbolic link)")
		}
	}
	if c := aptConflicts(readAptSourceFiles(r), srcPath, a.URIs, a.Suites); len(c) > 0 {
		return refused(fmt.Sprintf("%s names a URI and suite of this source too: apt refuses one source with two keys, and would then read no source at all; nothing was written",
			strings.Join(c, ", ")))
	}

	// Compare and swap: each file is as the approved plan saw it, or as the addition makes it already
	// (an attempt that was interrupted, or another apply; a key's file of the form the key is not in is
	// removed).
	if why := aptFilesChanged(a, name, src, asc, gpg); why != "" {
		return refused(why + "; nothing was written (plan again)")
	}

	// The key, as approved: the one on the node, or downloaded. Nothing is written before it is here.
	var key []byte
	for _, f := range []FileState{asc, gpg} {
		if f.Exists && !f.Large && f.SHA256 == a.KeySHA256 {
			if b, err := r.ReadFile(rel(f.Path)); err == nil && ContentSHA256(string(b)) == a.KeySHA256 {
				key = b
				break
			}
		}
	}
	var did []string
	if key == nil {
		b, refuse, err := downloadAptKey(ctx, x.System, name, a.KeyURL, a.KeySHA256)
		switch {
		case err != nil && refuse:
			return refused(err.Error() + "; nothing was written")
		case err != nil:
			return failed(err.Error() + "; nothing was written")
		}
		key = b
		did = append(did, "downloaded the key from "+a.KeyURL)
	}
	armored, err := aptKeyForm(key)
	if err != nil {
		return refused(fmt.Sprintf("the key from %s %v; nothing was written", a.KeyURL, err))
	}
	keyPath := AptKeyPath(name, armored)
	keyState, other := asc, gpg
	if !armored {
		keyState, other = gpg, asc
	}
	sources := AptSources(a.spec(name), keyPath)
	sum := ContentSHA256(sources)

	write := func(p, content, sum string, state FileState) error {
		if rootFile(state, sum) {
			return nil
		}
		ran = true
		if err := writeTo(r, p, Write{Content: content, SHA256: sum, Mode: "0644"}, uid, gid, uid, gid); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		if after := inspect(r, p, acc); !rootFile(after, sum) {
			return fmt.Errorf("after writing %s, it is %s %s %s:%s, not as approved", p, after.SHA256, after.Mode, after.Owner, after.Group)
		}
		did = append(did, fmt.Sprintf("wrote %s (sha256 %s)", p, sum[:12]))
		return nil
	}
	if err := write(keyPath, string(key), a.KeySHA256, keyState); err != nil {
		return failed(strings.Join(append(did, err.Error()), "; "))
	}
	if err := write(srcPath, sources, sum, src); err != nil {
		return failed(strings.Join(append(did, err.Error()), "; "))
	}
	if other.Exists {
		ran = true
		if err := removeFile(r, other.Path, uid, gid); err != nil {
			return failed(strings.Join(append(did, "remove "+other.Path+": "+err.Error()), "; "))
		}
		did = append(did, "removed "+other.Path)
	}
	if len(did) == 0 {
		did = append(did, "the source and its key were already as approved")
	}
	ran = true
	if err := aptGetUpdate(ctx, x.System); err != nil {
		msg := err.Error()
		if len(msg) > maxAptDetail {
			msg = msg[:maxAptDetail] + "..."
		}
		return failed(strings.Join(did, "; ") + "; apt-get update failed, the files stay (applying again runs it again): " + msg)
	}
	return noderev.OutcomeDone, strings.Join(append(did, "ran apt-get update"), "; "), ran
}

// downloadAptKey downloads a source's key with the extension's downloader (download.go: https only,
// a redirect too, at most MaxAptKey bytes, within AptKeyTimeout) into mamelet's state directory, and
// returns it only if its sha256 is want; the temporary file is removed whatever happens. refuse:
// what it got is not the approved key (the next attempt gets the same); otherwise the download failed
// (it may work later).
func downloadAptKey(ctx context.Context, s *mamelet.System, name, url, want string) (key []byte, refuse bool, err error) {
	dir := stateDir(s)
	if dir == "" {
		return nil, true, errors.New("mamelet has no state directory to download the key into")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	dest := filepath.Join(dir, ".linux-apt-key-"+name+".download")
	if err := os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("remove what an earlier download left: %w", err)
	}
	defer os.Remove(dest)
	client := HTTPSClient(s.HTTP)
	defer client.CloseIdleConnections()
	dctx, cancel := context.WithTimeout(ctx, AptKeyTimeout)
	defer cancel()
	if err := Fetch(dctx, client, url, want, dest, MaxAptKey); err != nil {
		var r *Refusal
		return nil, errors.As(err, &r), fmt.Errorf("the key: %w", err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		return nil, false, err
	}
	return b, false, nil
}

// OpenPGP's markers of a public key (RFC 9580). An ASCII-armored key starts with the armor's header
// line (after blank space, if any) and has its end line. A binary key starts with a public-key packet
// (tag 6): its first byte is 0x98, 0x99 or 0x9a in the old packet format (tag 6 with a one-, two- or
// four-byte length) or 0xc6 in the new one. Anything else (an HTML page of an error, a private key, a
// GnuPG keybox, which apt does not read) is refused.
const (
	armorBegin = "-----BEGIN PGP PUBLIC KEY BLOCK-----"
	armorEnd   = "-----END PGP PUBLIC KEY BLOCK-----"
)

var publicKeyPacket = []byte{0x98, 0x99, 0x9a, 0xc6}

// aptKeyForm says whether a key is ASCII-armored (written to .asc) or binary (.gpg), or why it is not
// an OpenPGP public key apt reads.
func aptKeyForm(b []byte) (armored bool, err error) {
	t := bytes.TrimLeft(b, " \t\r\n")
	switch {
	case bytes.HasPrefix(t, []byte(armorBegin)):
		if !bytes.Contains(t, []byte(armorEnd)) {
			return false, errors.New("is an ASCII-armored key without its end line (cut short?)")
		}
		return true, nil
	case len(b) > 0 && slices.Contains(publicKeyPacket, b[0]):
		return false, nil
	}
	head := t
	if len(head) > 24 {
		head = head[:24]
	}
	return false, fmt.Errorf("is not an OpenPGP public key, ASCII-armored (%s) or binary (a public-key packet): it starts with %s", armorBegin, quote(string(head)))
}

// aptGetUpdate runs apt-get update as the core runs apt-get (its environment and lock timeout), within
// AptUpdateTimeout.
func aptGetUpdate(ctx context.Context, s *mamelet.System) error {
	uctx, cancel := context.WithTimeout(ctx, AptUpdateTimeout)
	defer cancel()
	_, err := s.Run(uctx, progAptGet, []string{"-o", "DPkg::Lock::Timeout=300", "update"}, "DEBIAN_FRONTEND=noninteractive")
	if err != nil && uctx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("apt-get update did not finish within %s", AptUpdateTimeout)
	}
	return err
}

// removeFile removes the regular file at p, walking to its directory one directory at a time (no
// link followed).
func removeFile(r *os.Root, p string, dirUID, dirGID int) error {
	dir, err := walkDirs(r, p, dirUID, dirGID)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Remove(path.Base(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// aptResults are the recorded outcomes of the part's additions.
func aptRepoResults(p Part, known func(op string, attempt int) (noderev.Result, bool)) []noderev.Result {
	var out []noderev.Result
	for _, r := range p.AptRepos {
		if a := r.Add; a != nil {
			if res, ok := known(a.Grant.Operation, a.Grant.Attempt); ok {
				out = append(out, res)
			}
		}
	}
	return out
}

// aptUnread are the URIs and suites ("<uri> <suite>") apt has no release file of: no apt-get update
// has read them since they were added (or it failed for them). apt keeps the release file of each in
// its lists directory under the name aptListsName gives.
func aptUnread(r *os.Root, uris, suites []string) []string {
	var out []string
	for _, u := range uris {
		for _, s := range suites {
			found := false
			for _, f := range []string{"InRelease", "Release"} {
				if fi, err := r.Lstat(rel(aptListsDir + "/" + aptListsName(u, s, f))); err == nil && fi.Mode().IsRegular() {
					found = true
					break
				}
			}
			if !found {
				out = append(out, u+" "+s)
			}
		}
	}
	return out
}

// aptListsName is the name apt gives in its lists directory to the file of a source's URI and suite
// (apt-pkg's URItoFileName of debReleaseIndex's MetaIndexURI): the URI ends with "/"; the file is under
// dists/<suite>/, or under the suite itself for a flat repository (a suite that ends with "/", the URI
// itself for "/"); without the scheme, each character apt quotes written as %xx, and "/" as "_".
func aptListsName(uri, suite, file string) string {
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	switch {
	case suite == "/":
	case strings.HasSuffix(suite, "/"):
		uri += suite
	default:
		uri += "dists/" + suite + "/"
	}
	u := (uri + file)[strings.Index(uri, "://")+3:]
	const bad = `\|{}[]<>"^~_=!@#$%^&*`
	var b strings.Builder
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch {
		case c == '/':
			b.WriteByte('_')
		case c <= 0x20 || c >= 0x7f || strings.IndexByte(bad, c) >= 0:
			fmt.Fprintf(&b, "%%%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// aptEntry is one URI and suite that a source file of the node names.
type aptEntry struct{ file, uri, suite string }

// readAptSourceFiles reads the URIs and suites of the node's source files (sources.list, and the
// .list and .sources files of sources.list.d) that apt reads. A file it cannot read is left out.
func readAptSourceFiles(r *os.Root) []aptEntry {
	read := func(p string) string {
		f, err := r.OpenFile(rel(p), os.O_RDONLY|openNonBlock, 0)
		if err != nil {
			return ""
		}
		defer f.Close()
		if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
			return ""
		}
		b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
		return string(b)
	}
	out := oneLineEntries("/etc/apt/sources.list", read("/etc/apt/sources.list"))
	const dir = "/etc/apt/sources.list.d"
	entries, _ := fs.ReadDir(r.FS(), rel(dir))
	for _, e := range entries {
		p := dir + "/" + e.Name()
		switch {
		case strings.HasSuffix(e.Name(), ".list"):
			out = append(out, oneLineEntries(p, read(p))...)
		case strings.HasSuffix(e.Name(), ".sources"):
			out = append(out, deb822Entries(p, read(p))...)
		}
	}
	return out
}

// oneLineEntries reads a source file of the one-line format ("deb [options] uri suite components").
func oneLineEntries(file, text string) []aptEntry {
	var out []aptEntry
	for _, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) < 3 || (f[0] != "deb" && f[0] != "deb-src") {
			continue
		}
		f = f[1:]
		if strings.HasPrefix(f[0], "[") {
			for len(f) > 0 && !strings.HasSuffix(f[0], "]") {
				f = f[1:]
			}
			if len(f) > 0 {
				f = f[1:]
			}
		}
		if len(f) >= 2 {
			out = append(out, aptEntry{file, f[0], f[1]})
		}
	}
	return out
}

// deb822Entries reads a source file of the deb822 format: each stanza's URIs and Suites (a stanza with
// "Enabled: no" is left out).
func deb822Entries(file, text string) []aptEntry {
	var out []aptEntry
	fields, last := map[string]string{}, ""
	flush := func() {
		if !strings.EqualFold(fields["enabled"], "no") {
			for _, u := range strings.Fields(fields["uris"]) {
				for _, s := range strings.Fields(fields["suites"]) {
					out = append(out, aptEntry{file, u, s})
				}
			}
		}
		fields, last = map[string]string{}, ""
	}
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
		case strings.TrimSpace(line) == "":
			flush()
		case line[0] == ' ' || line[0] == '\t':
			if last != "" {
				fields[last] += " " + strings.TrimSpace(line)
			}
		default:
			k, v, ok := strings.Cut(line, ":")
			if ok {
				last = strings.ToLower(strings.TrimSpace(k))
				fields[last] = strings.TrimSpace(v)
			}
		}
	}
	flush()
	return out
}

// aptConflicts are the source files other than own that name one of the URIs (with or without its
// last "/") and suites.
func aptConflicts(entries []aptEntry, own string, uris, suites []string) []string {
	var out []string
	for _, e := range entries {
		if e.file == own || slices.Contains(out, e.file) || !slices.Contains(suites, e.suite) {
			continue
		}
		if slices.ContainsFunc(uris, func(u string) bool { return strings.TrimSuffix(u, "/") == strings.TrimSuffix(e.uri, "/") }) {
			out = append(out, e.file)
		}
	}
	return out
}

// aptFilesChanged says which of a source's files is neither as the approved plan saw it nor as the
// addition makes it ("" none).
func aptFilesChanged(a AptRepoAdd, name string, src, asc, gpg FileState) string {
	armored, binary := aptSourcesSHA256(a.spec(name))
	is := func(f FileState, sums ...string) bool { return f.Exists && !f.Large && slices.Contains(sums, f.SHA256) }
	for _, f := range []struct {
		state    FileState
		plan     Before
		approved bool
	}{
		{src, a.Before.Sources, is(src, armored, binary)},
		{asc, a.Before.KeyArmored, is(asc, a.KeySHA256) || !asc.Exists},
		{gpg, a.Before.KeyBinary, is(gpg, a.KeySHA256) || !gpg.Exists},
	} {
		if !f.approved && !f.plan.Is(f.state) {
			return fmt.Sprintf("%s changed since the plan (it was %s, it is %s)", f.state.Path, f.plan, beforeOf(f.state))
		}
	}
	return ""
}
