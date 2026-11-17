package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A database from a newer Mamela is refused, with a stable code, which Mamela it needs, and how to
// go back to this one (B6).
func TestNewerSchemaIsRefusedWithAHint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(ctx, path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	newer := len(migrations) + 3
	if _, err := s.db.ExecContext(ctx, `UPDATE meta SET value = 'abc123def456' WHERE key = 'migrated_by'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", newer)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, err = Open(ctx, path, time.Now)
	var ns *NewerSchemaError
	if !errors.As(err, &ns) || ns.Found != newer || ns.Supported != len(migrations) {
		t.Fatalf("got %v", err)
	}
	for _, want := range []string{"MAM-DB-0001", fmt.Sprintf("schema %d", newer), "abc123def456",
		fmt.Sprintf("%s.pre-migrate-%d", path, len(migrations)), "hint:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q:\n%v", want, err)
		}
	}
}
