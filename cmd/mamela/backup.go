package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/internal/cp"
	"github.com/ieyoukan/mamela/internal/store"
	"github.com/ieyoukan/mamela/internal/version"
)

// backupSchema names the layout of a backup archive.
const backupSchema = "mamela-backup/v1"

// backupManifest lists what a backup holds, with each file's size and sha256, so that restore can
// tell a damaged or edited archive before it writes anything.
type backupManifest struct {
	Schema  string                `json:"schema"`
	Created string                `json:"created"`
	Mamela  string                `json:"mamela"`
	Agents  int                   `json:"agents"` // how many mamelets were registered in the database
	Files   map[string]backupFile `json:"files"`
}

type backupFile struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

const (
	backupDB           = "state.db"
	backupManifestName = "manifest.json"
	// maxBackupFile bounds a file read from an archive (restore reads the archive into memory).
	maxBackupFile = 2 << 30
)

func backupKeyName(name string) string { return "cp/" + name }

// runBackup writes the two things only this Control Plane has, and which cannot be made again: its
// database (what Mamela made, which nodes are registered, the revisions they accepted) and the
// keys in controlplane.state_dir (the CA every mamelet is pinned to, the key that signs revisions,
// the server certificate). Not the site, the providers' file and their tokens (the operator's own,
// and a provider can issue a token again), and not mamelacd, which is a separate program with its
// own state and a GitHub token (docs/recovery.md).
func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("mamela backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	db := flags.String("db", envOr("MAMELA_DB", defaultDB), "state database (SQLite)")
	sitePath := addSiteFlag(flags)
	out := flags.String("out", "", "the archive to write (mode 0600; it must not exist). It holds the CA's private key (required)")
	flags.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela backup --out <file> [flags]\n\n"+
			"Saves the state database (a consistent copy, while the Control Plane runs) and the Control Plane's keys\n"+
			"(controlplane.state_dir: the CA, the revision signing key, the server certificate) into one archive.\n"+
			"Run it as the user that owns them (sudo -u mamela-cp on a Control Plane host). The archive is secret\n"+
			"like the keys in it: it holds the CA's private key, and the database may hold manifests and tokens\n"+
			"of the declarations. Keep it off the host, encrypted. docs/recovery.md says what else to keep.\n\nFlags:\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *out == "" {
		flags.Usage()
		return 2
	}
	site, err := loadSite(*sitePath)
	if err == nil && (site == nil || !site.ControlPlane.Enabled()) {
		err = errors.New("the site has no [controlplane] section (see mamela.site.example.toml)")
	}
	if err != nil {
		return report(stderr, err)
	}
	dir, err := keyDir(site)
	if err != nil {
		return report(stderr, err)
	}
	if _, err := os.Lstat(*out); err == nil {
		return report(stderr, told{fmt.Sprintf("MAM-BAK-0001: %s exists; a backup is never written over another file\n  hint: choose a new name (a date in it helps)", *out)})
	}
	if _, err := os.Stat(*db); err != nil {
		return report(stderr, told{fmt.Sprintf("MAM-BAK-0002: there is no database at %s: nothing to back up\n  hint: --db (or MAMELA_DB) names the Control Plane's state.db", *db)})
	}
	st, err := openStore(ctx, *db)
	if err != nil {
		return report(stderr, err)
	}
	defer st.Close()
	agents, err := st.AgentCount(ctx)
	if err != nil {
		return report(stderr, err)
	}
	// Keys that do not go with the database are not a backup of anything: it would restore into the
	// state this check exists to stop.
	if err := cp.CheckKeys(dir, agents); err != nil {
		return report(stderr, told{err.Error()})
	}
	if missing := cp.MissingKeys(dir); len(missing) > 0 {
		return report(stderr, told{fmt.Sprintf("MAM-BAK-0002: %s lacks %s: the Control Plane has not made its keys yet, so there is nothing to back up\n"+
			"  hint: they are made when mamela serve first starts (controlplane.state_dir)", dir, strings.Join(missing, ", "))})
	}

	files := map[string]bundleFile{}
	for _, name := range cp.Files {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) && (name == "server.pem" || name == "server-key.pem") {
			continue // the server certificate is issued again from the CA when it is missing
		} else if err != nil {
			return report(stderr, err)
		}
		files[backupKeyName(name)] = bundleFile{mode: 0o600, data: b}
	}
	// The database last, so that it is the newest of all (the keys do not change; the database does).
	tmp, err := os.MkdirTemp("", "mamela-backup-")
	if err != nil {
		return report(stderr, err)
	}
	defer os.RemoveAll(tmp)
	if err := st.CopyTo(ctx, filepath.Join(tmp, backupDB)); err != nil {
		return report(stderr, fmt.Errorf("copy the database: %w", err))
	}
	dbBytes, err := os.ReadFile(filepath.Join(tmp, backupDB))
	if err != nil {
		return report(stderr, err)
	}
	files[backupDB] = bundleFile{mode: 0o600, data: dbBytes}

	m := backupManifest{Schema: backupSchema, Created: wallClock().Format("2006-01-02T15:04:05Z"), Mamela: version.Current(), Agents: agents, Files: map[string]backupFile{}}
	for name, f := range files {
		sum := sha256.Sum256(f.data)
		m.Files[name] = backupFile{Size: int64(len(f.data)), SHA256: hex.EncodeToString(sum[:])}
	}
	mb, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return report(stderr, err)
	}
	files[backupManifestName] = bundleFile{mode: 0o600, data: mb}

	partial := *out + ".partial"
	if err := os.Remove(partial); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return report(stderr, err)
	}
	if err := writeBundle(partial, files); err != nil {
		return report(stderr, err)
	}
	if err := os.Rename(partial, *out); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "wrote %s (mode 0600): the database (%d registered mamelets) and %d key files, as of %s\n\n", *out, agents, len(files)-2, m.Created)
	fmt.Fprintf(stdout, "It holds the CA's private key. Move it off this host and encrypt it; do not leave it where this host's users can read it.\n")
	fmt.Fprintf(stdout, "It is not a copy of the site, the providers' file or their tokens, nor of mamelacd (docs/recovery.md).\n")
	fmt.Fprintf(stdout, "The database changes with every apply: make a backup after the changes that matter, and on a schedule.\n")
	return 0
}

// readBackup reads and checks an archive: every entry is a regular file with a name it may have,
// the manifest is there, and every file is what the manifest says.
func readBackup(path string) (backupManifest, map[string][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return backupManifest{}, nil, err
	}
	defer f.Close()
	bad := func(format string, a ...any) (backupManifest, map[string][]byte, error) {
		return backupManifest{}, nil, told{"MAM-BAK-0002: " + path + " is not a backup that can be restored: " + fmt.Sprintf(format, a...) +
			"\n  hint: use the archive mamela backup wrote, unchanged; if it was damaged in transfer, copy it again or use an older one"}
	}
	allowed := map[string]bool{backupDB: true, backupManifestName: true}
	for _, n := range cp.Files {
		allowed[backupKeyName(n)] = true
	}
	files := map[string][]byte{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return bad("%v", err)
		}
		if h.Typeflag != tar.TypeReg || !allowed[h.Name] || h.Size > maxBackupFile {
			return bad("an entry %q that a backup does not have", h.Name)
		}
		if _, dup := files[h.Name]; dup {
			return bad("%q is in it twice", h.Name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil || int64(len(b)) != h.Size {
			return bad("%q is cut short", h.Name)
		}
		files[h.Name] = b
	}
	mb, ok := files[backupManifestName]
	if !ok {
		return bad("it has no %s", backupManifestName)
	}
	var m backupManifest
	if err := json.Unmarshal(mb, &m, json.RejectUnknownMembers(true)); err != nil || m.Schema != backupSchema {
		return bad("its manifest is not %s", backupSchema)
	}
	delete(files, backupManifestName)
	for name, b := range files {
		want, listed := m.Files[name]
		sum := sha256.Sum256(b)
		if !listed || want.Size != int64(len(b)) || want.SHA256 != hex.EncodeToString(sum[:]) {
			return bad("%q is not what the manifest says", name)
		}
	}
	for name := range m.Files {
		if _, ok := files[name]; !ok {
			return bad("%q is in the manifest and not in the archive", name)
		}
	}
	for _, need := range []string{backupDB, backupKeyName("ca.pem"), backupKeyName("ca-key.pem"), backupKeyName("revision-key.pem")} {
		if _, ok := files[need]; !ok {
			return bad("it lacks %s", need)
		}
	}
	return m, files, nil
}

// runRestore puts a backup back where the site and --db say. It writes nothing until the archive has
// been checked end to end, refuses to write over a Control Plane that is there (or, with
// --replace, moves what is there aside, never deleting it), and writes the keys before the database.
func runRestore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("mamela restore", flag.ContinueOnError)
	flags.SetOutput(stderr)
	db := flags.String("db", envOr("MAMELA_DB", defaultDB), "where to put the state database")
	sitePath := addSiteFlag(flags)
	in := flags.String("in", "", "the archive mamela backup wrote (required)")
	replace := flags.Bool("replace", false, "if a database or keys are there, move them aside (name.before-restore-<time>) instead of refusing")
	flags.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela restore --in <file> [flags]\n\n"+
			"Puts back the database and the Control Plane's keys from a backup: the keys into controlplane.state_dir (mode 0600,\n"+
			"directory 0700) and the database at --db. Stop mamela serve first, and run it as the user that owns them\n"+
			"(sudo -u mamela-cp on a Control Plane host). The archive is checked end to end before anything is written.\n"+
			"Nothing is overwritten: what is there makes it stop (MAM-BAK-0003), or with --replace is moved aside.\n\nFlags:\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *in == "" {
		flags.Usage()
		return 2
	}
	site, err := loadSite(*sitePath)
	if err == nil && (site == nil || !site.ControlPlane.Enabled()) {
		err = errors.New("the site has no [controlplane] section (see mamela.site.example.toml)")
	}
	if err != nil {
		return report(stderr, err)
	}
	dir, err := keyDir(site)
	if err != nil {
		return report(stderr, err)
	}
	m, files, err := readBackup(*in)
	if err != nil {
		return report(stderr, err)
	}

	// The database must be one this Mamela can open: a newer schema is refused here, not at start.
	check, err := os.MkdirTemp("", "mamela-restore-")
	if err != nil {
		return report(stderr, err)
	}
	defer os.RemoveAll(check)
	probe := filepath.Join(check, backupDB)
	if err := os.WriteFile(probe, files[backupDB], 0o600); err != nil {
		return report(stderr, err)
	}
	ps, err := store.Open(ctx, probe, wallClock)
	if err != nil {
		return report(stderr, told{fmt.Sprintf("MAM-BAK-0004: the database in the backup cannot be opened by this Mamela: %v\n"+
			"  hint: restore with a Mamela at least as new as the one that made the backup (%s)", err, m.Mamela)})
	}
	agents, aerr := ps.AgentCount(ctx)
	ps.Close()
	if aerr != nil {
		return report(stderr, aerr)
	}

	// What is there already.
	var targets []string // the paths a restore writes
	for _, n := range cp.Files {
		if _, ok := files[backupKeyName(n)]; ok {
			targets = append(targets, filepath.Join(dir, n))
		}
	}
	dbFiles := []string{*db, *db + "-wal", *db + "-shm"} // a leftover WAL would be applied to the restored database
	var existing []string
	for _, p := range append(slices.Clone(targets), dbFiles...) {
		if _, err := os.Lstat(p); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) > 0 && !*replace {
		return report(stderr, told{"MAM-BAK-0003: a Control Plane is there already; nothing was changed:\n  " + strings.Join(existing, "\n  ") +
			"\n  hint: restore on a host that has none of it, or move these away yourself, or use --replace, which moves them aside (name.before-restore-<time>) and keeps them"})
	}
	if len(existing) > 0 {
		stamp := wallClock().Format("20060102T150405Z")
		for _, p := range existing {
			if err := os.Rename(p, p+".before-restore-"+stamp); err != nil {
				return report(stderr, err)
			}
			fmt.Fprintf(stdout, "moved %s aside (%s.before-restore-%s)\n", p, filepath.Base(p), stamp)
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return report(stderr, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return report(stderr, err)
	}
	if err := os.MkdirAll(filepath.Dir(*db), 0o700); err != nil {
		return report(stderr, err)
	}
	for _, n := range cp.Files {
		b, ok := files[backupKeyName(n)]
		if !ok {
			continue
		}
		if err := writeFileAtomic(filepath.Join(dir, n), b); err != nil {
			return report(stderr, err)
		}
	}
	// The database last: if this stops half way, there are keys and no database (the Control Plane
	// starts empty and says so, MAM-PKI-0203) and not a database without its keys.
	if err := writeFileAtomic(*db, files[backupDB]); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "restored from %s (made %s by %s): the database at %s (%d registered mamelets) and the keys in %s\n\n", *in, m.Created, m.Mamela, *db, agents, dir)
	fmt.Fprintf(stdout, "Next: start mamela serve, and check, in this order (docs/recovery.md):\n"+
		"  1. its log has no MAM-BAK-0101 (a node that accepted a newer revision than this database holds) and no MAM-PKI-0201/0203\n"+
		"  2. mamela controlplane agents lists the nodes, none ended\n"+
		"  3. mamela plan <declaration> shows the nodes reporting, and no object of Mamela's as Foreign\n"+
		"Everything the database learned after the backup was made is gone: apply and approvals since then are not in it.\n")
	return 0
}

// writeFileAtomic writes data to path (mode 0600) by writing a file next to it and renaming it over,
// so that a reader sees the old file or the whole new one.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".restoring"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
