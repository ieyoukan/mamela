// Package store persists Mamela's state in SQLite: immutable revisions, snapshots, policies and
// plans (addressed by digest), approvals, the operation journal and the event log.
//
// One Control Plane process owns the database (WAL mode, single writer). Do not share the file
// between hosts or put it on network storage.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver ("sqlite")

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/plan"
	"github.com/ieyoukan/mamela/internal/policy"
	"github.com/ieyoukan/mamela/internal/state"
	"github.com/ieyoukan/mamela/internal/version"
)

// Clock returns the current time. Tests inject a fake one.
type Clock func() time.Time

// Store is the SQLite-backed state store.
type Store struct {
	db    *sql.DB
	clock Clock
}

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

var migrations = []string{
	// 1: initial schema.
	`
CREATE TABLE revisions (
  digest     TEXT PRIMARY KEY,
  canonical  BLOB NOT NULL,
  created_ms INTEGER NOT NULL
);
CREATE TABLE snapshots (
  digest     TEXT PRIMARY KEY,
  canonical  BLOB NOT NULL,
  created_ms INTEGER NOT NULL
);
CREATE TABLE policies (
  digest     TEXT PRIMARY KEY,
  canonical  BLOB NOT NULL
);
CREATE TABLE plans (
  digest          TEXT PRIMARY KEY,
  revision_digest TEXT NOT NULL REFERENCES revisions(digest),
  snapshot_digest TEXT NOT NULL REFERENCES snapshots(digest),
  policy_digest   TEXT NOT NULL REFERENCES policies(digest),
  canonical       BLOB NOT NULL,
  created_ms      INTEGER NOT NULL
);
CREATE TABLE approvals (
  plan_digest TEXT PRIMARY KEY REFERENCES plans(digest),
  approver    TEXT NOT NULL,
  approved_ms INTEGER NOT NULL,
  expires_ms  INTEGER NOT NULL
);
CREATE TABLE operations (
  id          TEXT PRIMARY KEY,
  plan_digest TEXT NOT NULL REFERENCES plans(digest),
  resource    TEXT NOT NULL,
  op          TEXT NOT NULL,
  stage       TEXT NOT NULL CHECK (stage IN ('Intended','Started','Succeeded','Failed','Unknown')),
  attempt     INTEGER NOT NULL DEFAULT 0,
  owner       TEXT NOT NULL DEFAULT '',
  lease_ms    INTEGER NOT NULL DEFAULT 0,
  detail      TEXT NOT NULL DEFAULT '',
  updated_ms  INTEGER NOT NULL
);
CREATE INDEX operations_by_plan ON operations(plan_digest);
CREATE TABLE events (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  at_ms       INTEGER NOT NULL,
  plan_digest TEXT NOT NULL DEFAULT '',
  resource    TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL,
  message     TEXT NOT NULL
);
`,
	// 2: VMIDs reserved for named VMs, recorded before the VM is created.
	`
CREATE TABLE vmids (
  name         TEXT PRIMARY KEY,
  vmid         INTEGER NOT NULL UNIQUE,
  allocated_ms INTEGER NOT NULL
);
`,
	// 3: mamelet (the node agent): enrollment, revisions and reports.
	`
CREATE TABLE enroll_tokens (
  token_hash TEXT PRIMARY KEY,
  node       TEXT NOT NULL,
  vmid       INTEGER NOT NULL,
  seed       TEXT NOT NULL,
  created_ms INTEGER NOT NULL,
  expires_ms INTEGER NOT NULL,
  used_ms    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE agents (
  node        TEXT PRIMARY KEY,
  vmid        INTEGER NOT NULL,
  cert_serial TEXT NOT NULL,
  enrolled_ms INTEGER NOT NULL
);
CREATE TABLE node_revisions (
  node       TEXT NOT NULL,
  serial     INTEGER NOT NULL,
  content    BLOB NOT NULL,
  created_ms INTEGER NOT NULL,
  PRIMARY KEY (node, serial)
);
CREATE TABLE node_reports (
  node        TEXT PRIMARY KEY,
  received_ms INTEGER NOT NULL,
  report      BLOB NOT NULL
);
`,
	// 4: which Mamela made each plan (a plan recomputes only with the planner that made it).
	`ALTER TABLE plans ADD COLUMN made_by TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE plans ADD COLUMN providers_digest TEXT NOT NULL DEFAULT '';`,
	// 6: provider programs (content-addressed), which programs each plan was made with, and the
	// permits issued (each once).
	`
CREATE TABLE provider_programs (
  digest     TEXT PRIMARY KEY,
  data       BLOB NOT NULL,
  created_ms INTEGER NOT NULL
);
CREATE TABLE plan_programs (
  plan_digest    TEXT NOT NULL REFERENCES plans(digest),
  provider       TEXT NOT NULL,
  version        TEXT NOT NULL,
  source         TEXT NOT NULL,
  program_digest TEXT NOT NULL REFERENCES provider_programs(digest),
  PRIMARY KEY (plan_digest, provider)
);
CREATE TABLE permits (
  id           TEXT PRIMARY KEY,
  operation_id TEXT NOT NULL,
  plan_digest  TEXT NOT NULL,
  issued_ms    INTEGER NOT NULL
);
`,
	// 7: the join token of each k3s cluster, by its server node.
	`
CREATE TABLE k3s_tokens (
  server     TEXT PRIMARY KEY,
  token      TEXT NOT NULL,
  created_ms INTEGER NOT NULL
);
`,
	// 8: what Mamela set out to create through a provider, recorded before the create is sent: a
	// provider's object is Mamela's only if it matches one of these (its marker alone is not enough).
	`
CREATE TABLE provider_creations (
  provider     TEXT NOT NULL,
  resource     TEXT NOT NULL,
  ids          TEXT NOT NULL,
  plan_digest  TEXT NOT NULL,
  operation_id TEXT NOT NULL,
  recorded_ms  INTEGER NOT NULL,
  PRIMARY KEY (provider, resource, ids)
);
`,
	// 9: the numbers Mamela gives the nodes of providers whose machines have no number of their
	// own before they exist (an EC2 instance): mamelet's identity in revisions and enrollment.
	`
CREATE TABLE node_numbers (
  node       TEXT PRIMARY KEY,
  number     INTEGER NOT NULL UNIQUE,
  created_ms INTEGER NOT NULL
);
`,
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(ctx context.Context, path string, clock Clock) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // a single writer; serializes access within the process
	s := &Store{db: db, clock: clock}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// CopyTo writes a consistent copy of the whole database to path (which must not exist), while the
// database stays usable: for moving the Control Plane to its host (ADR 0003).
func (s *Store) CopyTo(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema %d is newer than this Mamela (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) nowMs() int64 { return s.clock().UnixMilli() }

// PutRevision stores a graph (idempotent) and returns its digest.
func (s *Store) PutRevision(ctx context.Context, g *graph.Graph) (string, error) {
	d := g.Digest()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO revisions(digest, canonical, created_ms) VALUES(?, ?, ?) ON CONFLICT(digest) DO NOTHING`,
		d, g.Canonical(), s.nowMs())
	return d, err
}

// Revision loads a graph by digest and checks that its content still has that digest.
func (s *Store) Revision(ctx context.Context, digest string) (*graph.Graph, error) {
	var b []byte
	if err := s.db.QueryRowContext(ctx, `SELECT canonical FROM revisions WHERE digest = ?`, digest).Scan(&b); err != nil {
		return nil, notFound(err)
	}
	g, err := graph.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("store: revision %s is corrupt: %w", digest, err)
	}
	if g.Digest() != digest {
		return nil, fmt.Errorf("store: revision %s does not match its content (%s)", digest, g.Digest())
	}
	return g, nil
}

// PutSnapshot stores a snapshot (idempotent) and returns its digest.
func (s *Store) PutSnapshot(ctx context.Context, snap state.Snapshot) (string, error) {
	d := snap.Digest()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO snapshots(digest, canonical, created_ms) VALUES(?, ?, ?) ON CONFLICT(digest) DO NOTHING`,
		d, snap.Canonical(), s.nowMs())
	return d, err
}

// Snapshot loads a snapshot by digest.
func (s *Store) Snapshot(ctx context.Context, digest string) (state.Snapshot, error) {
	var b []byte
	if err := s.db.QueryRowContext(ctx, `SELECT canonical FROM snapshots WHERE digest = ?`, digest).Scan(&b); err != nil {
		return state.Snapshot{}, notFound(err)
	}
	snap, err := state.Decode(b)
	if err != nil {
		return state.Snapshot{}, err
	}
	if snap.Digest() != digest {
		return state.Snapshot{}, fmt.Errorf("store: snapshot %s does not match its content", digest)
	}
	return snap, nil
}

// PutPolicy stores a policy (idempotent) and returns its digest.
func (s *Store) PutPolicy(ctx context.Context, p policy.Policy) (string, error) {
	d := p.Digest()
	_, err := s.db.ExecContext(ctx, `INSERT INTO policies(digest, canonical) VALUES(?, ?) ON CONFLICT(digest) DO NOTHING`,
		d, p.Canonical())
	return d, err
}

// Policy loads a policy by digest.
func (s *Store) Policy(ctx context.Context, digest string) (policy.Policy, error) {
	var b []byte
	if err := s.db.QueryRowContext(ctx, `SELECT canonical FROM policies WHERE digest = ?`, digest).Scan(&b); err != nil {
		return policy.Policy{}, notFound(err)
	}
	p, err := policy.Decode(b)
	if err != nil {
		return policy.Policy{}, err
	}
	if p.Digest() != digest {
		return policy.Policy{}, fmt.Errorf("store: policy %s does not match its content", digest)
	}
	return p, nil
}

// SavePlan stores the plan together with everything needed to recompute it.
func (s *Store) SavePlan(ctx context.Context, g *graph.Graph, snap state.Snapshot, pol policy.Policy, p *plan.Plan) error {
	if _, err := s.PutRevision(ctx, g); err != nil {
		return err
	}
	if _, err := s.PutSnapshot(ctx, snap); err != nil {
		return err
	}
	if _, err := s.PutPolicy(ctx, pol); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO plans(digest, revision_digest, snapshot_digest, policy_digest, canonical, created_ms, made_by, providers_digest)
VALUES(?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(digest) DO NOTHING`,
		p.Digest, p.Inputs.RevisionDigest, p.Inputs.SnapshotDigest, p.Inputs.PolicyDigest, p.Canonical(), s.nowMs(), version.Current(),
		p.Inputs.ProvidersDigest)
	return err
}

// PlanInputs are the stored inputs of a plan.
type PlanInputs struct {
	Revision *graph.Graph
	Snapshot state.Snapshot
	Policy   policy.Policy
	// Providers is the digest of the providers' contracts the plan was made for.
	Providers string
}

// LoadPlan recomputes a stored plan from its stored inputs and verifies that the result has the
// stored digest. A plan is never trusted as stored text: it is always re-derived.
func (s *Store) LoadPlan(ctx context.Context, digest string) (*plan.Plan, PlanInputs, error) {
	var rev, snapD, polD, madeBy, providers string
	var canonical []byte
	err := s.db.QueryRowContext(ctx, `SELECT revision_digest, snapshot_digest, policy_digest, canonical, made_by, providers_digest FROM plans WHERE digest = ?`, digest).
		Scan(&rev, &snapD, &polD, &canonical, &madeBy, &providers)
	if err != nil {
		return nil, PlanInputs{}, notFound(err)
	}
	g, err := s.Revision(ctx, rev)
	if err != nil {
		return nil, PlanInputs{}, err
	}
	snap, err := s.Snapshot(ctx, snapD)
	if err != nil {
		return nil, PlanInputs{}, err
	}
	pol, err := s.Policy(ctx, polD)
	if err != nil {
		return nil, PlanInputs{}, err
	}
	p := plan.BuildFor(g, snap, pol, providers)
	if p.Digest != digest || string(p.Canonical()) != string(canonical) {
		why := "its stored record or inputs were changed"
		if cur := version.Current(); madeBy != "" && madeBy != cur {
			why = fmt.Sprintf("it was made by Mamela %s and this is %s, whose planner differs; make the plan again with this version", madeBy, cur)
		}
		return nil, PlanInputs{}, fmt.Errorf("%w: plan %s recomputes to %s (%s)", ErrPlanMismatch, digest, p.Digest, why)
	}
	return p, PlanInputs{Revision: g, Snapshot: snap, Policy: pol, Providers: providers}, nil
}

// ErrPlanMismatch means a stored plan no longer recomputes to itself (tampering or a planner change).
var ErrPlanMismatch = errors.New("stored plan does not match its recomputation")

// Approval is a person's approval of one exact plan.
type Approval struct {
	PlanDigest string
	Approver   string
	ApprovedAt time.Time
	ExpiresAt  time.Time
}

// Approve records an approval of the plan, valid for ttl. Approving again replaces it.
func (s *Store) Approve(ctx context.Context, planDigest, approver string, ttl time.Duration) (Approval, error) {
	now := s.clock()
	a := Approval{PlanDigest: planDigest, Approver: approver, ApprovedAt: now, ExpiresAt: now.Add(ttl)}
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals(plan_digest, approver, approved_ms, expires_ms) VALUES(?, ?, ?, ?)
ON CONFLICT(plan_digest) DO UPDATE SET approver = excluded.approver, approved_ms = excluded.approved_ms, expires_ms = excluded.expires_ms`,
		planDigest, approver, now.UnixMilli(), a.ExpiresAt.UnixMilli())
	return a, err
}

// ValidApproval returns the approval of the plan if it exists and has not expired.
func (s *Store) ValidApproval(ctx context.Context, planDigest string) (*Approval, error) {
	var a Approval
	var approved, expires int64
	err := s.db.QueryRowContext(ctx, `SELECT approver, approved_ms, expires_ms FROM approvals WHERE plan_digest = ?`, planDigest).
		Scan(&a.Approver, &approved, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.PlanDigest = planDigest
	a.ApprovedAt, a.ExpiresAt = time.UnixMilli(approved).UTC(), time.UnixMilli(expires).UTC()
	if !s.clock().Before(a.ExpiresAt) {
		return nil, nil
	}
	return &a, nil
}

// Stage is the state of an operation in the journal.
type Stage string

const (
	StageIntended  Stage = "Intended"  // recorded, not yet attempted
	StageStarted   Stage = "Started"   // claimed by a worker holding a lease; outcome not yet known
	StageSucceeded Stage = "Succeeded" // done (reported or confirmed by observation)
	StageFailed    Stage = "Failed"    // attempted and known not to have taken effect
	StageUnknown   Stage = "Unknown"   // attempted, outcome unknown: re-observe before anything else
)

// Operation is one journal entry.
type Operation struct {
	ID         string
	PlanDigest string
	Resource   string
	Op         string
	Stage      Stage
	Attempt    int
	Owner      string
	LeaseUntil time.Time
	Detail     string
}

// Intend records an operation as Intended if it is not in the journal yet, and returns the
// current entry either way. Recording intent always happens before the side effect.
func (s *Store) Intend(ctx context.Context, id, planDigest, resource, op string) (Operation, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO operations(id, plan_digest, resource, op, stage, updated_ms)
VALUES(?, ?, ?, ?, 'Intended', ?) ON CONFLICT(id) DO NOTHING`, id, planDigest, resource, op, s.nowMs())
	if err != nil {
		return Operation{}, err
	}
	return s.Operation(ctx, id)
}

// Claim moves an operation to Started under a lease held by owner, and increments the attempt.
// It succeeds only from Intended, Failed or Unknown, or from Started whose lease has expired
// (a worker that died). It returns false if someone else holds it or it already succeeded.
func (s *Store) Claim(ctx context.Context, id, owner string, lease time.Duration) (bool, error) {
	now := s.clock()
	res, err := s.db.ExecContext(ctx, `UPDATE operations
SET stage = 'Started', owner = ?, lease_ms = ?, attempt = attempt + 1, updated_ms = ?
WHERE id = ? AND (stage IN ('Intended', 'Failed', 'Unknown') OR (stage = 'Started' AND lease_ms <= ?))`,
		owner, now.Add(lease).UnixMilli(), now.UnixMilli(), id, now.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Finish records the outcome of a claimed operation. It only applies while owner holds the claim,
// so a worker whose lease was taken over cannot overwrite the new owner's result.
func (s *Store) Finish(ctx context.Context, id, owner string, stage Stage, detail string) error {
	switch stage {
	case StageSucceeded, StageFailed, StageUnknown:
	default:
		return fmt.Errorf("store: cannot finish an operation as %s", stage)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE operations SET stage = ?, detail = ?, lease_ms = 0, updated_ms = ?
WHERE id = ? AND stage = 'Started' AND owner = ?`, string(stage), detail, s.nowMs(), id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("store: operation %s is no longer claimed by %s", id, owner)
	}
	return nil
}

// Confirm marks an operation Succeeded because observation shows its effect, without running it.
// Used after a crash or a lost response.
func (s *Store) Confirm(ctx context.Context, id, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET stage = 'Succeeded', detail = ?, lease_ms = 0, updated_ms = ?
WHERE id = ? AND stage <> 'Succeeded' AND NOT (stage = 'Started' AND lease_ms > ?)`, detail, s.nowMs(), id, s.nowMs())
	return err
}

// Operation loads one journal entry.
func (s *Store) Operation(ctx context.Context, id string) (Operation, error) {
	var o Operation
	var lease int64
	var stage string
	err := s.db.QueryRowContext(ctx, `SELECT id, plan_digest, resource, op, stage, attempt, owner, lease_ms, detail FROM operations WHERE id = ?`, id).
		Scan(&o.ID, &o.PlanDigest, &o.Resource, &o.Op, &stage, &o.Attempt, &o.Owner, &lease, &o.Detail)
	if err != nil {
		return Operation{}, notFound(err)
	}
	o.Stage = Stage(stage)
	if lease > 0 {
		o.LeaseUntil = time.UnixMilli(lease).UTC()
	}
	return o, nil
}

// Operations lists the journal entries of a plan in a stable order.
func (s *Store) Operations(ctx context.Context, planDigest string) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM operations WHERE plan_digest = ? ORDER BY resource, op, id`, planDigest)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Operation, 0, len(ids))
	for _, id := range ids {
		o, err := s.Operation(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// Event is one entry of the append-only event log.
type Event struct {
	Seq        int64
	At         time.Time
	PlanDigest string
	Resource   string
	Kind       string
	Message    string
}

// AppendEvent appends to the event log.
func (s *Store) AppendEvent(ctx context.Context, planDigest, resource, kind, message string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events(at_ms, plan_digest, resource, kind, message) VALUES(?, ?, ?, ?, ?)`,
		s.nowMs(), planDigest, resource, kind, message)
	return err
}

// Events returns the events of a plan in order.
func (s *Store) Events(ctx context.Context, planDigest string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at_ms, plan_digest, resource, kind, message FROM events WHERE plan_digest = ? ORDER BY seq`, planDigest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.Seq, &at, &e.PlanDigest, &e.Resource, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// VMID returns the VMID reserved for the VM name, if any.
func (s *Store) VMID(ctx context.Context, name string) (int, bool, error) {
	var vmid int
	err := s.db.QueryRowContext(ctx, `SELECT vmid FROM vmids WHERE name = ?`, name).Scan(&vmid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return vmid, err == nil, err
}

// AllocateVMID returns the VMID reserved for name, reserving one first if there is none: the lowest
// ID in [min, max] that no other name holds and that free reports unused in the cluster.
//
// The reservation is written before the VM is created and never changes afterwards, so a create
// that is retried after a timeout or a crash targets the same VMID and cannot make a second VM.
func (s *Store) AllocateVMID(ctx context.Context, name string, min, max int, free func(context.Context, int) (bool, error)) (int, error) {
	if vmid, ok, err := s.VMID(ctx, name); err != nil || ok {
		return vmid, err
	}
	taken := map[int]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT vmid FROM vmids WHERE vmid BETWEEN ? AND ?`, min, max)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return 0, err
		}
		taken[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for v := min; v <= max; v++ {
		if taken[v] {
			continue
		}
		ok, err := free(ctx, v)
		if err != nil {
			return 0, fmt.Errorf("check whether VMID %d is free: %w", v, err)
		}
		if !ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO vmids(name, vmid, allocated_ms) VALUES(?, ?, ?)`, name, v, s.nowMs()); err != nil {
			return 0, err
		}
		return v, nil
	}
	return 0, fmt.Errorf("no free VMID in %d..%d for %s", min, max, name)
}
