package store

import (
	"context"
	"database/sql"
	"errors"
)

// GitopsPlan is a plan gitops (ADR 0010) made for a pull request: of its head (Merged false), or
// of the commit it was merged as (Merged true).
type GitopsPlan struct {
	Digest   string
	Repo     string
	PR       int
	Dir      string
	SHA      string
	Merged   bool
	Revision string // the declaration's digest
	Changes  int    // how many changes it makes
	Applied  bool
	Outcome  string
}

// SaveGitopsPlan records a plan gitops posted.
func (s *Store) SaveGitopsPlan(ctx context.Context, p GitopsPlan) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO gitops_plans(plan_digest, repo, pr, dir, sha, merged, revision, changes, posted_ms)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(plan_digest) DO NOTHING`,
		p.Digest, p.Repo, p.PR, p.Dir, p.SHA, boolInt(p.Merged), p.Revision, p.Changes, s.nowMs())
	return err
}

// GitopsPlan returns a plan gitops posted, by digest.
func (s *Store) GitopsPlan(ctx context.Context, digest string) (GitopsPlan, bool, error) {
	rows, err := s.gitopsPlans(ctx, `WHERE plan_digest = ?`, digest)
	if err != nil || len(rows) == 0 {
		return GitopsPlan{}, false, err
	}
	return rows[0], true, nil
}

// GitopsPlans returns the plans gitops posted for a pull request, newest first.
func (s *Store) GitopsPlans(ctx context.Context, repo string, pr int) ([]GitopsPlan, error) {
	return s.gitopsPlans(ctx, `WHERE repo = ? AND pr = ? ORDER BY posted_ms DESC, plan_digest`, repo, pr)
}

func (s *Store) gitopsPlans(ctx context.Context, where string, args ...any) ([]GitopsPlan, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT plan_digest, repo, pr, dir, sha, merged, revision, changes, applied_ms, outcome FROM gitops_plans `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GitopsPlan
	for rows.Next() {
		var p GitopsPlan
		var merged int
		var applied int64
		if err := rows.Scan(&p.Digest, &p.Repo, &p.PR, &p.Dir, &p.SHA, &merged, &p.Revision, &p.Changes, &applied, &p.Outcome); err != nil {
			return nil, err
		}
		p.Merged, p.Applied = merged != 0, applied != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// GitopsApplied records that gitops applied a plan, and how it went.
func (s *Store) GitopsApplied(ctx context.Context, digest, outcome string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE gitops_plans SET applied_ms = ?, outcome = ? WHERE plan_digest = ?`, s.nowMs(), outcome, digest)
	return err
}

// GitopsSee marks something gitops handled (a comment, a merge, a head it planned) and says
// whether it was new: only the first caller gets true.
func (s *Store) GitopsSee(ctx context.Context, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO gitops_seen(key, seen_ms) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`, key, s.nowMs())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// GitopsSeen says whether gitops handled something.
func (s *Store) GitopsSeen(ctx context.Context, key string) (bool, error) {
	var ms int64
	err := s.db.QueryRowContext(ctx, `SELECT seen_ms FROM gitops_seen WHERE key = ?`, key).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
