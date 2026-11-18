package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Retention is how long a plan that nobody approved or applied is kept. mamela api plans the head of
// a branch again and again; each plan keeps its observation, and nothing else ever deletes them.
// What was approved or applied is never deleted: it is the audit of what was authorized and done.
const Retention = 30 * 24 * time.Hour

// Pruned counts what Prune deleted.
type Pruned struct {
	Plans, Snapshots, Revisions, Policies, Programs, Events int
	// NodeRevisions are the revisions of nodes older than the cutoff that are not a node's newest.
	NodeRevisions int
}

// pruneEvery is how often PruneIfDue prunes.
const pruneEvery = time.Hour

// PruneIfDue prunes what is older than retention, unless it did within the last hour (any program
// of the Control Plane: the time is kept in the database). It reports whether it did.
func (s *Store) PruneIfDue(ctx context.Context, retention time.Duration) (Pruned, bool, error) {
	var last int64
	var text string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'pruned_ms'`).Scan(&text)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Pruned{}, false, err
	}
	if err == nil {
		last, _ = strconv.ParseInt(text, 10, 64)
	}
	now := s.clock()
	if now.UnixMilli()-last < pruneEvery.Milliseconds() && last <= now.UnixMilli() {
		return Pruned{}, false, nil
	}
	n, err := s.Prune(ctx, now.Add(-retention))
	if err != nil {
		return Pruned{}, false, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('pruned_ms', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.FormatInt(now.UnixMilli(), 10))
	return n, true, err
}

// Prune deletes the plans made before the cutoff that were never approved and never had an operation
// journaled, with their events and provider programs, and then the inputs only they used: their
// revisions, observations and policies, and any observation older than the cutoff that no plan
// uses (apply saves the one it observes). It deletes the revisions of nodes made before the cutoff
// that are not a node's newest: a node is offered only its newest (its serial never goes down), and
// what was granted and done is in the operations and events. Everything else stays.
func (s *Store) Prune(ctx context.Context, before time.Time) (Pruned, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Pruned{}, err
	}
	defer tx.Rollback()
	cut := before.UnixMilli()
	var n Pruned

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS prune_plans(digest TEXT PRIMARY KEY, revision TEXT, snapshot TEXT, policy TEXT)`); err != nil {
		return n, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM prune_plans`); err != nil {
		return n, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO prune_plans SELECT p.digest, p.revision_digest, p.snapshot_digest, p.policy_digest FROM plans p
WHERE p.created_ms < ? AND NOT EXISTS (SELECT 1 FROM approvals a WHERE a.plan_digest = p.digest)
  AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.plan_digest = p.digest)`, cut); err != nil {
		return n, err
	}
	step := func(count *int, query string, args ...any) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		*count += int(c)
		return nil
	}
	var discard int
	steps := []struct {
		count *int
		query string
		args  []any
	}{
		{&n.Events, `DELETE FROM events WHERE plan_digest IN (SELECT digest FROM prune_plans)`, nil},
		{&discard, `DELETE FROM plan_catalogs WHERE plan_digest IN (SELECT digest FROM prune_plans)`, nil},
		{&discard, `DELETE FROM plan_programs WHERE plan_digest IN (SELECT digest FROM prune_plans)`, nil},
		{&n.Plans, `DELETE FROM plans WHERE digest IN (SELECT digest FROM prune_plans)`, nil},
		{&n.Revisions, `DELETE FROM revisions WHERE digest IN (SELECT revision FROM prune_plans)
AND digest NOT IN (SELECT revision_digest FROM plans)`, nil},
		{&n.Policies, `DELETE FROM policies WHERE digest IN (SELECT policy FROM prune_plans)
AND digest NOT IN (SELECT policy_digest FROM plans)`, nil},
		{&n.Snapshots, `DELETE FROM snapshots WHERE (digest IN (SELECT snapshot FROM prune_plans) OR created_ms < ?)
AND digest NOT IN (SELECT snapshot_digest FROM plans)`, []any{cut}},
		{&n.Programs, `DELETE FROM provider_programs WHERE created_ms < ? AND digest NOT IN (SELECT program_digest FROM plan_programs)
AND digest NOT IN (SELECT catalog_digest FROM plan_catalogs)`, []any{cut}},
		{&n.NodeRevisions, `DELETE FROM node_revisions WHERE created_ms < ?
AND serial < (SELECT MAX(r.serial) FROM node_revisions r WHERE r.node = node_revisions.node)`, []any{cut}},
	}
	for _, st := range steps {
		if err := step(st.count, st.query, st.args...); err != nil {
			return n, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE prune_plans`); err != nil {
		return n, err
	}
	return n, tx.Commit()
}
