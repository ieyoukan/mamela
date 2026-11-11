package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// Program is a provider program a plan was made with.
type Program struct {
	Provider string // the provider's name
	Version  string // its version (from its Pome)
	Source   string // where it came from (the Pome's source path)
	Digest   string // "sha256:..." of the program
	Data     []byte
}

// SavePlanPrograms stores the provider programs of a plan, content-addressed. Applying the plan
// runs exactly these bytes, wherever its Pome cache is then.
func (s *Store) SavePlanPrograms(ctx context.Context, planDigest string, programs []Program) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range programs {
		if got := programDigest(p.Data); got != p.Digest {
			return fmt.Errorf("store: program of %s is %s, not %s", p.Provider, got, p.Digest)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO provider_programs(digest, data, created_ms) VALUES(?, ?, ?) ON CONFLICT(digest) DO NOTHING`,
			p.Digest, p.Data, s.nowMs()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_programs(plan_digest, provider, version, source, program_digest) VALUES(?, ?, ?, ?, ?)
ON CONFLICT(plan_digest, provider) DO NOTHING`, planDigest, p.Provider, p.Version, p.Source, p.Digest); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PlanPrograms returns the provider programs of a plan, checked against their digests.
func (s *Store) PlanPrograms(ctx context.Context, planDigest string) ([]Program, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pp.provider, pp.version, pp.source, pp.program_digest, p.data
FROM plan_programs pp JOIN provider_programs p ON p.digest = pp.program_digest WHERE pp.plan_digest = ? ORDER BY pp.provider`, planDigest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Program
	for rows.Next() {
		var p Program
		if err := rows.Scan(&p.Provider, &p.Version, &p.Source, &p.Digest, &p.Data); err != nil {
			return nil, err
		}
		if got := programDigest(p.Data); got != p.Digest {
			return nil, fmt.Errorf("store: the stored program of %s is %s, not %s", p.Provider, got, p.Digest)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func programDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ErrPermitUsed means a permit was already issued: each is used once, across processes.
var ErrPermitUsed = errors.New("the permit was already issued")

// UsePermit records that a permit is issued, once.
func (s *Store) UsePermit(ctx context.Context, id, operationID, planDigest string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO permits(id, operation_id, plan_digest, issued_ms) VALUES(?, ?, ?, ?)`,
		id, operationID, planDigest, s.nowMs())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("%w: %s", ErrPermitUsed, id)
	}
	return err
}

// RecordCreation records, before a creating change is sent, that Mamela creates the object with
// these IDs for the resource through the provider.
func (s *Store) RecordCreation(ctx context.Context, provider, resource string, ids map[string]string, planDigest, operationID string) error {
	b, err := json.Marshal(ids, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO provider_creations(provider, resource, ids, plan_digest, operation_id, recorded_ms)
VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(provider, resource, ids) DO NOTHING`, provider, resource, string(b), planDigest, operationID, s.nowMs())
	return err
}

// Creations are the IDs of the objects Mamela set out to create for a resource through a provider.
func (s *Store) Creations(ctx context.Context, provider, resource string) ([]map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ids FROM provider_creations WHERE provider = ? AND resource = ? ORDER BY recorded_ms`, provider, resource)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		var ids map[string]string
		if err := json.Unmarshal([]byte(text), &ids); err != nil {
			return nil, err
		}
		out = append(out, ids)
	}
	return out, rows.Err()
}

// FirstNodeNumber is the first number Mamela gives a node: above every Proxmox VMID (at most
// 999999999), so that the two never meet.
const FirstNodeNumber = 1_000_000_000

// NodeNumber returns the node's number, giving it the next one the first time.
func (s *Store) NodeNumber(ctx context.Context, node string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int64
	err = tx.QueryRowContext(ctx, `SELECT number FROM node_numbers WHERE node = ?`, node).Scan(&n)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number) + 1, ?) FROM node_numbers`, FirstNodeNumber).Scan(&n); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO node_numbers(node, number, created_ms) VALUES(?, ?, ?)`, node, n, s.nowMs()); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
