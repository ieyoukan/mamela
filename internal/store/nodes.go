package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// Errors of enrollment. They are deliberately not specific to the caller (see Enroll).
var ErrEnrollRefused = errors.New("enrollment refused: the token is unknown, used, expired or for another node")

var ErrAgentRevoked = errors.New("the agent registration was revoked or replaced")

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueEnrollToken creates a one-time token with which the agent of VM node (vmid) can enroll
// once, before ttl passes. Only its hash is stored. seed names the cloud-init seed image the token
// is written into, so a retried create can reuse that image while the token is still good.
func (s *Store) IssueEnrollToken(ctx context.Context, node string, vmid int, seed string, ttl time.Duration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.nowMs()
	_, err := s.db.ExecContext(ctx, `INSERT INTO enroll_tokens(token_hash, node, vmid, seed, created_ms, expires_ms) VALUES(?, ?, ?, ?, ?, ?)`,
		hashToken(token), node, vmid, seed, now, now+ttl.Milliseconds())
	return token, err
}

// SetEnrollSeed records the seed image a token was written into (its name depends on the token).
func (s *Store) SetEnrollSeed(ctx context.Context, token, seed string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE enroll_tokens SET seed = ? WHERE token_hash = ?`, seed, hashToken(token))
	return err
}

// ReusableSeed returns the seed image of an unused token for node (vmid) that stays valid for at
// least minLeft, if there is one.
func (s *Store) ReusableSeed(ctx context.Context, node string, vmid int, minLeft time.Duration) (string, bool, error) {
	var seed string
	err := s.db.QueryRowContext(ctx, `SELECT seed FROM enroll_tokens
WHERE node = ? AND vmid = ? AND used_ms = 0 AND seed <> '' AND expires_ms > ? ORDER BY created_ms DESC LIMIT 1`,
		node, vmid, s.nowMs()+minLeft.Milliseconds()).Scan(&seed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return seed, err == nil, err
}

// Enroll uses a token for node (vmid) and records the agent's certificate. The token must exist,
// be unused, unexpired and issued for exactly this node and VMID; it can never be used again.
// Enrolling again (with a new token) replaces the certificate, which revokes the old one.
func (s *Store) Enroll(ctx context.Context, token, node string, vmid int, certSerial string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.nowMs()
	res, err := tx.ExecContext(ctx, `UPDATE enroll_tokens SET used_ms = ?
WHERE token_hash = ? AND node = ? AND vmid = ? AND used_ms = 0 AND expires_ms > ?`, now, hashToken(token), node, vmid, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrEnrollRefused
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(node, vmid, cert_serial, enrolled_ms) VALUES(?, ?, ?, ?)
ON CONFLICT(node) DO UPDATE SET vmid = excluded.vmid, cert_serial = excluded.cert_serial, enrolled_ms = excluded.enrolled_ms`,
		node, vmid, certSerial, now); err != nil {
		return err
	}
	return tx.Commit()
}

// Agent is an enrolled mamelet.
type Agent struct {
	Node       string
	VMID       int
	CertSerial string
	Enrolled   time.Time
}

// Agent returns the enrolled agent of a node.
func (s *Store) Agent(ctx context.Context, node string) (Agent, bool, error) {
	a := Agent{Node: node}
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT vmid, cert_serial, enrolled_ms FROM agents WHERE node = ?`, node).Scan(&a.VMID, &a.CertSerial, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	a.Enrolled = time.UnixMilli(at).UTC()
	return a, err == nil, err
}

// PublishNodeRevision computes the node's next revision from its current one (nil if none) and
// stores it. The serial increases only when the content changes, so publishing the same content
// again is a no-op and mamelet sees no new revision.
func (s *Store) PublishNodeRevision(ctx context.Context, node string, next func(prev *noderev.Revision) (noderev.Revision, error)) (noderev.Revision, error) {
	return s.PublishNodeRevisionForAgent(ctx, node, "", next)
}

// PublishNodeRevisionForAgent rejects an approval for a replaced enrollment in the same
// transaction that grants execution. An empty serial is used for observation-only publication.
func (s *Store) PublishNodeRevisionForAgent(ctx context.Context, node, serial string, next func(prev *noderev.Revision) (noderev.Revision, error)) (noderev.Revision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return noderev.Revision{}, err
	}
	defer tx.Rollback()
	if serial != "" {
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT cert_serial FROM agents WHERE node=?`, node).Scan(&current); err != nil || current != serial {
			return noderev.Revision{}, ErrAgentRevoked
		}
	}
	prev, err := latestNodeRevision(ctx, tx, node)
	if err != nil {
		return noderev.Revision{}, err
	}
	r, err := next(prev)
	if err != nil {
		return noderev.Revision{}, err
	}
	r.Node, r.Schema = node, noderev.Schema
	r.IssuedAt, r.ExpiresAt = time.Time{}, time.Time{}
	if prev != nil {
		r.Serial = prev.Serial
		if bytes.Equal(r.Content(), prev.Content()) {
			return *prev, nil
		}
	}
	r.Serial++
	if _, err := tx.ExecContext(ctx, `INSERT INTO node_revisions(node, serial, content, created_ms) VALUES(?, ?, ?, ?)`,
		node, r.Serial, r.Content(), s.nowMs()); err != nil {
		return noderev.Revision{}, err
	}
	return r, tx.Commit()
}

// NodeRevision returns the node's current revision (without a validity period).
func (s *Store) NodeRevision(ctx context.Context, node string) (*noderev.Revision, error) {
	return latestNodeRevision(ctx, s.db, node)
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func latestNodeRevision(ctx context.Context, q queryer, node string) (*noderev.Revision, error) {
	var content []byte
	err := q.QueryRowContext(ctx, `SELECT content FROM node_revisions WHERE node = ? ORDER BY serial DESC LIMIT 1`, node).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r noderev.Revision
	if err := json.Unmarshal(content, &r, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("stored revision of %s: %w", node, err)
	}
	return &r, nil
}

// NodeReport is the latest report of a node and when it arrived (Control Plane time).
type NodeReport struct {
	Report   noderev.Report
	Received time.Time
}

// PutNodeReport stores a node's latest report, received now.
func (s *Store) PutNodeReport(ctx context.Context, r noderev.Report) error {
	data, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO node_reports(node, received_ms, report) VALUES(?, ?, ?)
ON CONFLICT(node) DO UPDATE SET received_ms = excluded.received_ms, report = excluded.report`, r.Node, s.nowMs(), data)
	return err
}

// PutAuthenticatedNodeReport checks enrollment in the same statement as the write. A request
// authenticated just before deletion cannot recreate an old report after registration is revoked.
func (s *Store) PutAuthenticatedNodeReport(ctx context.Context, r noderev.Report, serial string) error {
	data, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO node_reports(node,received_ms,report)
SELECT node,?,? FROM agents WHERE node=? AND vmid=? AND cert_serial=?
ON CONFLICT(node) DO UPDATE SET received_ms=excluded.received_ms,report=excluded.report`, s.nowMs(), data, r.Node, r.VMID, serial)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrAgentRevoked
	}
	return nil
}

// NodeReports returns the latest report of every node.
func (s *Store) NodeReports(ctx context.Context) ([]NodeReport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT received_ms, report FROM node_reports ORDER BY node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeReport
	for rows.Next() {
		var at int64
		var data []byte
		if err := rows.Scan(&at, &data); err != nil {
			return nil, err
		}
		var nr NodeReport
		if err := json.Unmarshal(data, &nr.Report, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		nr.Received = time.UnixMilli(at).UTC()
		out = append(out, nr)
	}
	return out, rows.Err()
}

// NodeReportOf returns the latest report of one node.
func (s *Store) NodeReportOf(ctx context.Context, node string) (NodeReport, bool, error) {
	var at int64
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT received_ms, report FROM node_reports WHERE node = ?`, node).Scan(&at, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeReport{}, false, nil
	}
	if err != nil {
		return NodeReport{}, false, err
	}
	var nr NodeReport
	if err := json.Unmarshal(data, &nr.Report, json.RejectUnknownMembers(true)); err != nil {
		return NodeReport{}, false, err
	}
	nr.Received = time.UnixMilli(at).UTC()
	return nr, true, nil
}

// ForgetVM removes everything Mamela recorded for a destroyed VM: its VMID reservation, its
// agent's enrollment (so the certificate is no longer accepted), revisions, report and tokens.
func (s *Store) ForgetVM(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM vmids WHERE name = ?`, `DELETE FROM agents WHERE node = ?`, `DELETE FROM node_revisions WHERE node = ?`,
		`DELETE FROM node_reports WHERE node = ?`, `DELETE FROM enroll_tokens WHERE node = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}
