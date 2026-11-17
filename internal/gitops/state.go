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
// what it last did with the head of the branch. Mamela's state (plans, approvals, the journal) is
// the Control Plane's, asked through its API.
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
	// 2 (0.3.2): what a poll has to remember between polls, and what `mamelacd status` shows.
	// Only columns and tables are added: rows of 1 stay as they are (the new columns are empty).
	`
-- the shape of a plan (a hash of what it says), to tell the same change coming back (A7a)
ALTER TABLE plans ADD COLUMN shape TEXT NOT NULL DEFAULT '';
-- the result comment of an apply that could not be posted yet (the next poll posts it)
ALTER TABLE plans ADD COLUMN report_pr INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plans ADD COLUMN report_body TEXT NOT NULL DEFAULT '';
-- the head's plans undo a drift (the world changed under a declaration that was in sync): B7
ALTER TABLE heads ADD COLUMN drift INTEGER NOT NULL DEFAULT 0;
-- the last poll, for status and the heartbeat (one row)
CREATE TABLE heartbeat (
  id            INTEGER PRIMARY KEY CHECK (id = 1),
  pid           INTEGER NOT NULL,
  version       TEXT NOT NULL,
  started_ms    INTEGER NOT NULL,  -- when this mamelacd started
  poll_start_ms INTEGER NOT NULL,  -- the last poll's start
  poll_end_ms   INTEGER NOT NULL,  -- its end (0: it is running)
  result        TEXT NOT NULL,     -- "ok", "error", "running"
  detail        TEXT NOT NULL,     -- the error, in one line
  duration_ms   INTEGER NOT NULL,
  next_ms       INTEGER NOT NULL,  -- when the next poll is due
  interval_ms   INTEGER NOT NULL,
  polls         INTEGER NOT NULL,
  failing       INTEGER NOT NULL,  -- polls that failed in a row
  failing_since INTEGER NOT NULL
);
-- GitHub's numeric id of each author and approver (a login can be taken by somebody else after a
-- rename; the id cannot): pinned at first sight (B11)
CREATE TABLE principals (
  login       TEXT PRIMARY KEY,
  id          INTEGER NOT NULL,
  pinned_ms   INTEGER NOT NULL,
  conflict_id INTEGER NOT NULL DEFAULT 0,  -- another account that acted under the login
  conflict_ms INTEGER NOT NULL DEFAULT 0
);
`,
}

// OpenStore opens (creating if needed) mamelacd's database.
func OpenStore(ctx context.Context, path string, clock func() time.Time) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
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
			// Another mamelacd command (status, once) may have made this migration first: that is fine.
			var now int
			if db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&now) == nil && now > i {
				continue
			}
			db.Close()
			return nil, fmt.Errorf("mamelacd: migration %d: %w", i+1, err)
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Now is the store's clock.
func (s *Store) Now() time.Time { return s.clock() }

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
	// Shape is a hash of what the plan says (see planShape): the same change has the same shape.
	Shape string
	// AppliedMs is when it was applied (the store's clock, milliseconds; 0: not yet).
	AppliedMs int64
}

// SavePlan records a plan. The same plan (digest) is recorded once.
func (s *Store) SavePlan(ctx context.Context, p Plan) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO plans(digest, repo, pr, dir, sha, at_head, revision, changes, waiting, blocked, posted_ms, shape)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(digest) DO NOTHING`,
		p.Digest, p.Repo, p.PR, p.Dir, p.SHA, boolInt(p.AtHead), p.Revision, p.Changes, p.Waiting, p.Blocked, s.nowMs(), p.Shape)
	return err
}

const planColumns = `digest, repo, pr, dir, sha, at_head, revision, changes, waiting, blocked, applied_ms, outcome, shape`

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
		if err := rows.Scan(&p.Digest, &p.Repo, &p.PR, &p.Dir, &p.SHA, &atHead, &p.Revision, &p.Changes, &p.Waiting, &p.Blocked, &applied, &p.Outcome, &p.Shape); err != nil {
			return nil, err
		}
		p.AtHead, p.Applied, p.AppliedMs = atHead != 0, applied != 0, applied
		out = append(out, p)
	}
	return out, rows.Err()
}

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

// FailedApplies counts the plans of a declaration at a commit whose apply failed.
func (s *Store) FailedApplies(ctx context.Context, repo, dir, sha string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plans WHERE repo = ? AND dir = ? AND sha = ? AND outcome = ?`, repo, dir, sha, outcomeFailed).Scan(&n)
	return n, err
}

// Applied records that a plan was applied, and how it went.
func (s *Store) Applied(ctx context.Context, digest, outcome string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plans SET applied_ms = ?, outcome = ? WHERE digest = ?`, s.nowMs(), outcome, digest)
	return err
}

// AppliesOf returns the plans of a declaration at a commit that were applied (whatever came of it),
// the newest first.
func (s *Store) AppliesOf(ctx context.Context, repo, dir, sha string) ([]Plan, error) {
	return s.plans(ctx, `WHERE repo = ? AND dir = ? AND sha = ? AND applied_ms > 0 ORDER BY applied_ms DESC, rowid DESC LIMIT 50`, repo, dir, sha)
}

// SaveReport keeps the comment on pull request pr that reports an apply, until it is posted.
func (s *Store) SaveReport(ctx context.Context, digest string, pr int, body string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plans SET report_pr = ?, report_body = ? WHERE digest = ?`, pr, body, digest)
	return err
}

// ClearReport forgets a report that was posted.
func (s *Store) ClearReport(ctx context.Context, digest string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plans SET report_pr = 0, report_body = '' WHERE digest = ?`, digest)
	return err
}

// Report is a comment that reports an apply and could not be posted yet.
type Report struct {
	Digest string
	PR     int
	Body   string
}

// PendingReports returns the reports to post, the oldest first.
func (s *Store) PendingReports(ctx context.Context, repo string) ([]Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT digest, report_pr, report_body FROM plans WHERE repo = ? AND report_pr > 0 ORDER BY applied_ms, rowid`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Report
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.Digest, &r.PR, &r.Body); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
	// Drift: the plans of this head undo a drift (the declaration was in sync and the world changed
	// under it), not the phases of a change a push approved: they are approved only with self_heal,
	// or on the host.
	Drift bool
	// UpdatedMs is when the state was recorded (the store's clock, milliseconds).
	UpdatedMs int64
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
	// HeadEvalRetry: the evaluator was unavailable for now (MAM-EVAL-0001, 0102, a full disk): the
	// declaration is evaluated again after the retry interval (it is not the declaration's fault).
	HeadEvalRetry = "eval-retry"
	// HeadStopped: the same change came back (stale, or applied and still planned) too many times in
	// a row: not applied again until another commit, or a person's approval on the host (MAM-GIT-0101).
	HeadStopped = "stopped"
)

// Head returns the last state of a declaration at the head of the branch.
func (s *Store) Head(ctx context.Context, repo, dir string) (Head, bool, error) {
	h := Head{Dir: dir}
	var drift int
	err := s.db.QueryRowContext(ctx, `SELECT sha, revision, state, digest, drift, updated_ms FROM heads WHERE repo = ? AND dir = ?`, repo, dir).
		Scan(&h.SHA, &h.Revision, &h.State, &h.Digest, &drift, &h.UpdatedMs)
	h.Drift = drift != 0
	if errors.Is(err, sql.ErrNoRows) {
		return h, false, nil
	}
	return h, err == nil, err
}

// SetHead records the state of a declaration at the head of the branch.
func (s *Store) SetHead(ctx context.Context, repo string, h Head) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO heads(repo, dir, sha, revision, state, digest, drift, updated_ms) VALUES(?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, dir) DO UPDATE SET sha = excluded.sha, revision = excluded.revision, state = excluded.state, digest = excluded.digest, drift = excluded.drift, updated_ms = excluded.updated_ms`,
		repo, h.Dir, h.SHA, h.Revision, h.State, h.Digest, boolInt(h.Drift), s.nowMs())
	return err
}

// Heads returns the last state of every declaration of the repository, by directory.
func (s *Store) Heads(ctx context.Context, repo string) ([]Head, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT dir, sha, revision, state, digest, drift, updated_ms FROM heads WHERE repo = ? ORDER BY dir`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Head
	for rows.Next() {
		var h Head
		var drift int
		if err := rows.Scan(&h.Dir, &h.SHA, &h.Revision, &h.State, &h.Digest, &drift, &h.UpdatedMs); err != nil {
			return nil, err
		}
		h.Drift = drift != 0
		out = append(out, h)
	}
	return out, rows.Err()
}

// Heartbeat is what the last poll left, for `mamelacd status` (and nothing else reads it).
type Heartbeat struct {
	PID          int
	Version      string
	StartedMs    int64
	PollStartMs  int64
	PollEndMs    int64  // 0: the poll is running
	Result       string // "ok", "error", "running"
	Detail       string // the error, in one line
	DurationMs   int64
	NextMs       int64 // when the next poll is due
	IntervalMs   int64
	Polls        int64
	Failing      int64 // polls that failed in a row
	FailingSince int64
}

// SetHeartbeat records the last poll.
func (s *Store) SetHeartbeat(ctx context.Context, h Heartbeat) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO heartbeat(id, pid, version, started_ms, poll_start_ms, poll_end_ms, result, detail, duration_ms, next_ms, interval_ms, polls, failing, failing_since)
VALUES(1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET pid = excluded.pid, version = excluded.version, started_ms = excluded.started_ms, poll_start_ms = excluded.poll_start_ms,
  poll_end_ms = excluded.poll_end_ms, result = excluded.result, detail = excluded.detail, duration_ms = excluded.duration_ms, next_ms = excluded.next_ms,
  interval_ms = excluded.interval_ms, polls = excluded.polls, failing = excluded.failing, failing_since = excluded.failing_since`,
		h.PID, h.Version, h.StartedMs, h.PollStartMs, h.PollEndMs, h.Result, h.Detail, h.DurationMs, h.NextMs, h.IntervalMs, h.Polls, h.Failing, h.FailingSince)
	return err
}

// LastHeartbeat returns the last poll's record (false: no poll has recorded one).
func (s *Store) LastHeartbeat(ctx context.Context) (Heartbeat, bool, error) {
	var h Heartbeat
	err := s.db.QueryRowContext(ctx, `SELECT pid, version, started_ms, poll_start_ms, poll_end_ms, result, detail, duration_ms, next_ms, interval_ms, polls, failing, failing_since FROM heartbeat WHERE id = 1`).
		Scan(&h.PID, &h.Version, &h.StartedMs, &h.PollStartMs, &h.PollEndMs, &h.Result, &h.Detail, &h.DurationMs, &h.NextMs, &h.IntervalMs, &h.Polls, &h.Failing, &h.FailingSince)
	if errors.Is(err, sql.ErrNoRows) {
		return h, false, nil
	}
	return h, err == nil, err
}

// Principal is a configured author or approver, with the GitHub account (numeric id) it was pinned to.
type Principal struct {
	Login      string
	ID         int64
	PinnedMs   int64
	ConflictID int64 // another account that acted under the login (0: none)
	ConflictMs int64
}

// Principals returns the pinned accounts.
func (s *Store) Principals(ctx context.Context) ([]Principal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT login, id, pinned_ms, conflict_id, conflict_ms FROM principals ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		var p Principal
		if err := rows.Scan(&p.Login, &p.ID, &p.PinnedMs, &p.ConflictID, &p.ConflictMs); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Pin records the account a login meant at first sight. The first pin stays.
func (s *Store) Pin(ctx context.Context, login string, id int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO principals(login, id, pinned_ms) VALUES(?, ?, ?) ON CONFLICT(login) DO NOTHING`, login, id, s.nowMs())
	return err
}

// Unpin forgets the pin of a login (the next poll pins it again) and says whether there was one.
func (s *Store) Unpin(ctx context.Context, login string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM principals WHERE login = ?`, login)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// NoteConflict records that another account (id) acted under a pinned login.
func (s *Store) NoteConflict(ctx context.Context, login string, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE principals SET conflict_id = ?, conflict_ms = ? WHERE login = ?`, id, s.nowMs(), login)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
