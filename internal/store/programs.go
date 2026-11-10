package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
