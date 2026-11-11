package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Program is a provider program a plan was made with.
type Program struct {
	Provider string // the provider's name
	Version  string // its version (from its Pome)
	Source   string // where it came from (the Pome's source path)
	Digest   string // "sha256:..." of the program
	Data     []byte
	// Catalogs are the catalog files its Pome ships, by name (ADR 0008).
	Catalogs map[string][]byte
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
		for _, name := range slices.Sorted(maps.Keys(p.Catalogs)) {
			// Catalogs are stored as the programs are: by their digest.
			data := p.Catalogs[name]
			d := programDigest(data)
			if _, err := tx.ExecContext(ctx, `INSERT INTO provider_programs(digest, data, created_ms) VALUES(?, ?, ?) ON CONFLICT(digest) DO NOTHING`,
				d, data, s.nowMs()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO plan_catalogs(plan_digest, provider, name, catalog_digest) VALUES(?, ?, ?, ?)
ON CONFLICT(plan_digest, provider, name) DO NOTHING`, planDigest, p.Provider, name, d); err != nil {
				return err
			}
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].Catalogs, err = s.planCatalogs(ctx, planDigest, out[i].Provider); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) planCatalogs(ctx context.Context, planDigest, provider string) (map[string][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pc.name, pc.catalog_digest, p.data FROM plan_catalogs pc
JOIN provider_programs p ON p.digest = pc.catalog_digest WHERE pc.plan_digest = ? AND pc.provider = ? ORDER BY pc.name`, planDigest, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var name, digest string
		var data []byte
		if err := rows.Scan(&name, &digest, &data); err != nil {
			return nil, err
		}
		if got := programDigest(data); got != digest {
			return nil, fmt.Errorf("store: the stored catalog %s of %s is %s, not %s", name, provider, got, digest)
		}
		out[name] = data
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

// StartExplicit records, before anything is sent, an explicit operation a person confirmed (ADR
// 0006). The change is the provider's, as shown.
func (s *Store) StartExplicit(ctx context.Context, id, provider, resource, op string, change []byte, confirmedBy string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO explicit_operations(id, provider, resource, op, change, confirmed_by, started_ms)
VALUES(?, ?, ?, ?, ?, ?, ?)`, id, provider, resource, op, string(change), confirmedBy, s.nowMs())
	return err
}

// FinishExplicit records how an explicit operation ended ("done", "failed", "partly-done",
// "unknown").
func (s *Store) FinishExplicit(ctx context.Context, id, outcome, detail string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE explicit_operations SET outcome = ?, detail = ?, finished_ms = ? WHERE id = ? AND outcome = 'started'`,
		outcome, detail, s.nowMs(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("store: explicit operation %s is not started", id)
	}
	return nil
}

// ExplicitOutcome is how an explicit operation ended ("started" while it has not).
func (s *Store) ExplicitOutcome(ctx context.Context, id string) (string, error) {
	var outcome string
	err := s.db.QueryRowContext(ctx, `SELECT outcome FROM explicit_operations WHERE id = ?`, id).Scan(&outcome)
	return outcome, notFound(err)
}

// ForgetCreations removes Mamela's records of creating a resource of a provider once its object is
// deleted (ADR 0006): what remains with that resource's IDs is no longer Mamela's. A node's number
// is kept, so that no number is ever given to two machines (a machine of the same name made again
// gets its number back).
func (s *Store) ForgetCreations(ctx context.Context, provider, resource string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM provider_creations WHERE provider = ? AND resource = ?`, provider, resource)
	return err
}
