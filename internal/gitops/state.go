package gitops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver ("sqlite")
)

// Store is mamelacd's own database: the plans it posted, what it handled (comments, heads), and
// what it last did with the head of the branch. It holds none of Mamela's state: plans, approvals
// and the operation journal are the Control Plane's (asked through its API).
type Store struct {
	db    *sql.DB
	clock func() time.Time
}

var storeMigrations = []string{
	`
CREATE TABLE plans (
  digest     TEXT PRIMARY KEY,
  repo       TEXT NOT NULL,
  pr         INTEGER NOT NULL,     -- the pull request it was posted on (0: none)
  dir        TEXT NOT NULL,
  sha        TEXT NOT NULL,        -- the commit planned
  at_head    INTEGER NOT NULL,     -- planned as the head of the branch (not a pull request's head)
  revision   TEXT NOT NULL,        -- the declaration's digest
  changes    INTEGER NOT NULL,
  waiting    INTEGER NOT NULL,
  blocked    INTEGER NOT NULL,
  posted_ms  INTEGER NOT NULL,
  applied_ms INTEGER NOT NULL DEFAULT 0,
  outcome    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX plans_by_pr ON plans(repo, pr, dir);
CREATE INDEX plans_by_revision ON plans(repo, dir, revision);
CREATE TABLE seen (
  key     TEXT PRIMARY KEY,
  seen_ms INTEGER NOT NULL
);
CREATE TABLE heads (
  repo       TEXT NOT NULL,
  dir        TEXT NOT NULL,
  sha        TEXT NOT NULL,
  revision   TEXT NOT NULL,
  state      TEXT NOT NULL,
  digest     TEXT NOT NULL,
  updated_ms INTEGER NOT NULL,
  PRIMARY KEY (repo, dir)
);
`,
}

// OpenStore opens (creating if needed) mamelacd's database.
func OpenStore(ctx context.Context, path string, clock func() time.Time) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, clock: clock}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > len(storeMigrations) {
		db.Close()
		return nil, fmt.Errorf("mamelacd: database schema %d is newer than this mamelacd (%d)", version, len(storeMigrations))
	}
	for i := version; i < len(storeMigrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err == nil {
			if _, err = tx.ExecContext(ctx, storeMigrations[i]); err == nil {
				if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err == nil {
					err = tx.Commit()
				}
			}
			if err != nil {
				tx.Rollback()
			}
		}
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("mamelacd: migration %d: %w", i+1, err)
		}
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) nowMs() int64 { return s.clock().UnixMilli() }

// Plan is a plan mamelacd posted.
type Plan struct {
	Digest   string
	Repo     string
	PR       int
	Dir      string
	SHA      string
	AtHead   bool
	Revision string
	Changes  int
	Waiting  int
	Blocked  int
	Applied  bool
	Outcome  string
}

// SavePlan records a plan. The same plan (digest) is recorded once.
func (s *Store) SavePlan(ctx context.Context, p Plan) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO plans(digest, repo, pr, dir, sha, at_head, revision, changes, waiting, blocked, posted_ms)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(digest) DO NOTHING`,
		p.Digest, p.Repo, p.PR, p.Dir, p.SHA, boolInt(p.AtHead), p.Revision, p.Changes, p.Waiting, p.Blocked, s.nowMs())
	return err
}

const planColumns = `digest, repo, pr, dir, sha, at_head, revision, changes, waiting, blocked, applied_ms, outcome`

func (s *Store) plans(ctx context.Context, where string, args ...any) ([]Plan, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+planColumns+` FROM plans `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		var p Plan
		var atHead int
		var applied int64
		if err := rows.Scan(&p.Digest, &p.Repo, &p.PR, &p.Dir, &p.SHA, &atHead, &p.Revision, &p.Changes, &p.Waiting, &p.Blocked, &applied, &p.Outcome); err != nil {
			return nil, err
		}
		p.AtHead, p.Applied = atHead != 0, applied != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// Plan returns a plan by its digest.
func (s *Store) Plan(ctx context.Context, digest string) (Plan, bool, error) {
	ps, err := s.plans(ctx, `WHERE digest = ?`, digest)
	if err != nil || len(ps) == 0 {
		return Plan{}, false, err
	}
	return ps[0], true, nil
}

// LatestPlan returns the newest plan of a declaration posted on a pull request (0: on none).
func (s *Store) LatestPlan(ctx context.Context, repo string, pr int, dir string) (Plan, bool, error) {
	ps, err := s.plans(ctx, `WHERE repo = ? AND pr = ? AND dir = ? ORDER BY posted_ms DESC, rowid DESC LIMIT 1`, repo, pr, dir)
	if err != nil || len(ps) == 0 {
		return Plan{}, false, err
	}
	return ps[0], true, nil
}

// Unapplied returns the plans of one declaration (by its digest) not applied yet, the newest first.
func (s *Store) Unapplied(ctx context.Context, repo, dir, revision string) ([]Plan, error) {
	return s.plans(ctx, `WHERE repo = ? AND dir = ? AND revision = ? AND applied_ms = 0 AND changes > 0 ORDER BY posted_ms DESC, rowid DESC`, repo, dir, revision)
}

// Applied records that a plan was applied, and how it went.
func (s *Store) Applied(ctx context.Context, digest, outcome string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plans SET applied_ms = ?, outcome = ? WHERE digest = ?`, s.nowMs(), outcome, digest)
	return err
}

// See marks something handled (a comment, a head planned) and says whether it was new: only the
// first caller gets true.
func (s *Store) See(ctx context.Context, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO seen(key, seen_ms) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`, key, s.nowMs())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Seen says whether something was handled.
func (s *Store) Seen(ctx context.Context, key string) (bool, error) {
	var ms int64
	err := s.db.QueryRowContext(ctx, `SELECT seen_ms FROM seen WHERE key = ?`, key).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Head is what mamelacd last did with a declaration at the head of the branch.
type Head struct {
	Dir      string
	SHA      string
	Revision string
	State    string // HeadPending, HeadWaiting, ...
	Digest   string // the last plan
}

// States of a declaration at the head of the branch.
const (
	HeadInSync    = "in-sync"   // nothing to change
	HeadPending   = "pending"   // changes, waiting for an approval of the plan
	HeadWaiting   = "waiting"   // waiting for observation or dependencies: planned again each poll
	HeadBlocked   = "blocked"   // refused: planned again each poll
	HeadFailed    = "failed"    // the apply failed: investigate
	HeadError     = "error"     // could not be evaluated: another commit is needed
	HeadUnplanned = "unplanned" // evaluated, but could not be planned: planned again each poll
	HeadUntrusted = "untrusted" // not merged from a trusted author's pull request: not planned
)

// Head returns the last state of a declaration at the head of the branch.
func (s *Store) Head(ctx context.Context, repo, dir string) (Head, bool, error) {
	h := Head{Dir: dir}
	err := s.db.QueryRowContext(ctx, `SELECT sha, revision, state, digest FROM heads WHERE repo = ? AND dir = ?`, repo, dir).
		Scan(&h.SHA, &h.Revision, &h.State, &h.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return h, false, nil
	}
	return h, err == nil, err
}

// SetHead records the state of a declaration at the head of the branch.
func (s *Store) SetHead(ctx context.Context, repo string, h Head) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO heads(repo, dir, sha, revision, state, digest, updated_ms) VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, dir) DO UPDATE SET sha = excluded.sha, revision = excluded.revision, state = excluded.state, digest = excluded.digest, updated_ms = excluded.updated_ms`,
		repo, h.Dir, h.SHA, h.Revision, h.State, h.Digest, s.nowMs())
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
