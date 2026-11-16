package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/version"
)

// NewerSchemaError (MAM-DB-0001) is the refusal to open a database that a newer Mamela migrated: this
// Mamela does not know its tables, and would damage them or fail again and again.
type NewerSchemaError struct {
	Path       string
	Found      int    // the database's schema version
	Supported  int    // the newest schema this Mamela has
	MigratedBy string // the Mamela that last migrated it, when the database says
}

func (e *NewerSchemaError) Error() string {
	by := ""
	if e.MigratedBy != "" {
		by = fmt.Sprintf(", last migrated by Mamela %s", e.MigratedBy)
	}
	return fmt.Sprintf("MAM-DB-0001: the database %s has schema %d, newer than the schema %d this Mamela (%s) knows%s\n"+
		"  hint: run that Mamela, or a newer one, on this database. To go back to this Mamela instead, stop every Mamela program, "+
		"set the database aside (with its -wal and -shm files), and put %s, the copy made when the database was migrated, in its place",
		e.Path, e.Found, e.Supported, version.Current(), by, backupPath(e.Path, e.Supported))
}

// backupPath is where the copy of a database at schema version v is kept before it is migrated.
func backupPath(db string, v int) string { return fmt.Sprintf("%s.pre-migrate-%d", db, v) }

// enableWAL puts the database in WAL mode (it stays so). Programs that open a new database together
// find the switch busy, and SQLite does not wait for it as it does for other locks: it is tried again.
func (s *Store) enableWAL(ctx context.Context) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var mode string
		err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode)
		if err == nil {
			return nil
		}
		if !isBusy(err) || time.Now().After(deadline) {
			return fmt.Errorf("store: switching the database to WAL mode: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func isBusy(err error) bool {
	return strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked")
}

// migrate brings the database to the newest schema. Programs of the Control Plane start together,
// so it must be safe against another doing the same: the version is read again inside a write
// transaction (BEGIN IMMEDIATE; the one that waits finds the work done), and a database with data is
// copied before it is changed, to <db>.pre-migrate-<old version> (mode 0600).
func (s *Store) migrate(ctx context.Context) error {
	for {
		v0, err := schemaVersion(ctx, s.db)
		if err != nil {
			return err
		}
		switch {
		case v0 > len(migrations):
			return s.newerSchema(ctx, v0)
		case v0 == len(migrations):
			return nil
		}
		// The copy is made first, from a read of the database that is not older than v0. If another
		// program migrates before this one gets the write lock, the copy is discarded below: its
		// schema would not be v0, and that program made its own.
		var copyPath string
		if v0 > 0 {
			if copyPath, err = s.copyBeforeMigrating(ctx); err != nil {
				return err
			}
		}
		done, err := s.migrateFrom(ctx, v0, copyPath)
		if err != nil {
			if copyPath != "" {
				os.Remove(copyPath)
			}
			return err
		}
		if done {
			return nil
		}
	}
}

func schemaVersion(ctx context.Context, q queryer) (int, error) {
	var v int
	err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// migrateFrom applies the migrations after version v0 in one transaction: all of them, or none. It
// reports false, having done nothing, when the database is no longer at v0.
func (s *Store) migrateFrom(ctx context.Context, v0 int, copyPath string) (bool, error) {
	tx, err := s.beginForMigration(ctx)
	if err != nil {
		return false, fmt.Errorf("store: waiting to migrate the database: %w", err)
	}
	defer tx.Rollback()
	v, err := schemaVersion(ctx, tx)
	if err != nil {
		return false, err
	}
	if v != v0 {
		if copyPath != "" {
			os.Remove(copyPath)
		}
		return false, nil
	}
	if copyPath != "" {
		// Keep it only now that this program holds the write lock and the schema is as it was copied.
		if err := os.Rename(copyPath, backupPath(s.path, v0)); err != nil {
			return false, fmt.Errorf("MAM-DB-0002: keeping the copy of the database made before migrating it: %w", err)
		}
	}
	for i := v0; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return false, fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('migrated_by', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		version.Current()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// beginForMigration takes the write lock for the migration. Another program may hold it for the whole
// of its own migration (a slow disk, a large database), which can outlast busy_timeout, so it waits
// longer than any other transaction does.
func (s *Store) beginForMigration(ctx context.Context) (*sql.Tx, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		tx, err := s.db.BeginTx(ctx, nil)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return tx, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// copyBeforeMigrating writes a consistent copy of the database to a new file of its own (mode 0600:
// the database holds tokens and keys' records), and returns its path.
func (s *Store) copyBeforeMigrating(ctx context.Context) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".pre-migrate-tmp-*")
	if err != nil {
		return "", fmt.Errorf("MAM-DB-0002: cannot keep a copy of the database before migrating it: %w\n"+
			"  hint: make the directory of the database writable, or copy the database by hand and migrate it afterwards", err)
	}
	name := f.Name()
	f.Close()
	// VACUUM INTO accepts a file that exists if it is empty.
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, name); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("MAM-DB-0002: cannot keep a copy of the database before migrating it: %w", err)
	}
	return name, nil
}

// newerSchema says who migrated a database whose schema this Mamela does not know.
func (s *Store) newerSchema(ctx context.Context, found int) error {
	e := &NewerSchemaError{Path: s.path, Found: found, Supported: len(migrations)}
	// The table is there only if the Mamela that migrated it kept it; a database that does not say is
	// still refused, without the name.
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'migrated_by'`).Scan(&e.MigratedBy)
	return e
}
