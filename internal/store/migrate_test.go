package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// seedAt writes a database as the Mamela of schema version n left it: the first n migrations
// applied and nothing else.
func seedAt(t *testing.T, path string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < n; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("seeding migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
		t.Fatal(err)
	}
}

func openAll(t *testing.T, path string, n int) []error {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := Open(context.Background(), path, time.Now)
			if err == nil {
				err = s.Close()
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

// Several programs of the Control Plane (mamela serve, mamela api, the CLI) start together, after an
// upgrade or when the machine boots: each must find the schema migrated, none may fail on another's
// migration (B6).
func TestConcurrentOpensMigrateOnce(t *testing.T) {
	for round := range 12 {
		for _, from := range []int{0, len(migrations) - 1} {
			path := filepath.Join(t.TempDir(), "state.db")
			if from > 0 {
				seedAt(t, path, from)
			}
			for i, err := range openAll(t, path, 4) {
				if err != nil {
					t.Fatalf("round %d, from schema %d: opener %d: %v", round, from, i, err)
				}
			}
			s, err := Open(context.Background(), path, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			var v int
			if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != len(migrations) {
				t.Fatalf("schema %d (%v), want %d", v, err, len(migrations))
			}
			s.Close()
		}
	}
}

// A transaction that reads, then writes, must wait for another connection that writes in between
// (up to busy_timeout), not fail at once with "database is locked (517)" (B6).
func TestReadThenWriteTransactionWaitsForAnotherWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	a, err := Open(ctx, path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, path, time.Now) // another program on the same database
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	wrote := make(chan error, 1)
	_, err = a.PublishNodeRevision(ctx, "web", func(prev *noderev.Revision) (noderev.Revision, error) {
		// The other program writes while this transaction is between its read and its write.
		go func() { wrote <- b.AppendEvent(ctx, "", "", "other", "written meanwhile") }()
		time.Sleep(200 * time.Millisecond)
		return noderev.Revision{VMID: 1}, nil
	})
	if err != nil {
		t.Fatalf("the read-then-write transaction: %v", err)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the other writer: %v", err)
	}
}

// Before it migrates a database that has data, Mamela keeps a copy of it as it was, readable by its
// owner only, and says where (B6).
func TestMigrationKeepsABackupFirst(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	from := len(migrations) - 1
	seedAt(t, path, from)
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO events(at_ms, kind, message) VALUES(1, 'before', 'written before the upgrade')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := Open(ctx, path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	backup := fmt.Sprintf("%s.pre-migrate-%d", path, from)
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatalf("no backup before the migration: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the backup is mode %o, want 600", info.Mode().Perm())
	}
	b, err := sql.Open("sqlite", "file:"+backup+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var v, n int
	if err := b.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != from {
		t.Fatalf("the backup is schema %d (%v), want %d: it must be the database as it was", v, err, from)
	}
	if err := b.QueryRow(`SELECT COUNT(*) FROM events WHERE message = 'written before the upgrade'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the backup lacks the data (%d, %v)", n, err)
	}

	// A new database, and one that is current, need none.
	for name, prepare := range map[string]func(string){"new": func(string) {}, "current": func(p string) {
		s, err := Open(ctx, p, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}} {
		p := filepath.Join(t.TempDir(), name+".db")
		prepare(p)
		s, err := Open(ctx, p, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		if m, _ := filepath.Glob(p + ".pre-migrate-*"); len(m) != 0 {
			t.Errorf("%s database: a backup was made: %v", name, m)
		}
	}
}

// Concurrent openers during an upgrade still leave one backup, of the old schema.
func TestConcurrentUpgradeLeavesOneOldBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	from := len(migrations) - 1
	seedAt(t, path, from)
	for i, err := range openAll(t, path, 4) {
		if err != nil {
			t.Fatalf("opener %d: %v", i, err)
		}
	}
	m, _ := filepath.Glob(path + ".pre-migrate-*")
	if len(m) != 1 || m[0] != fmt.Sprintf("%s.pre-migrate-%d", path, from) {
		t.Fatalf("backups %v", m)
	}
	b, err := sql.Open("sqlite", "file:"+m[0]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var v int
	if err := b.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != from {
		t.Fatalf("the backup is schema %d (%v), want %d", v, err, from)
	}
}

// If the copy cannot be kept, the database is not changed: the migration is refused (MAM-DB-0002).
func TestMigrationIsRefusedWhenTheCopyCannotBeKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	from := len(migrations) - 1
	seedAt(t, path, from)
	// Something in the way of the copy's name (a directory that is not empty).
	blocker := fmt.Sprintf("%s.pre-migrate-%d", path, from)
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), path, time.Now)
	if err == nil || !strings.Contains(err.Error(), "MAM-DB-0002") {
		t.Fatalf("got %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != from {
		t.Fatalf("the database was changed anyway: schema %d (%v)", v, err)
	}
	if m, _ := filepath.Glob(path + ".pre-migrate-tmp-*"); len(m) != 0 {
		t.Errorf("a temporary copy was left: %v", m)
	}
}
