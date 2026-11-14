package store

import (
	"context"
	"encoding/json/v2"
	"errors"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// RecordAdoption records that a person imported an object (of Terraform, the source) to adopt as
// a declared resource: until it is adopted, observing reports the object with these IDs as
// adoptable for it. Importing it again replaces the IDs.
func (s *Store) RecordAdoption(ctx context.Context, provider, resource string, ids map[string]string, source, by string) error {
	if len(ids) == 0 {
		return errors.New("store: an adoption names the object's IDs")
	}
	b, err := json.Marshal(ids, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO adoptions(provider, resource, ids, source, imported_by, recorded_ms) VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(provider, resource) DO UPDATE SET ids = excluded.ids, source = excluded.source, imported_by = excluded.imported_by, recorded_ms = excluded.recorded_ms`,
		provider, resource, string(b), source, by, s.nowMs())
	return err
}

// Adoptions are, by provider, the resources to adopt, in the order of their IDs.
func (s *Store) Adoptions(ctx context.Context) (map[string][]protocol.Adopting, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, resource, ids FROM adoptions ORDER BY provider, resource`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]protocol.Adopting{}
	for rows.Next() {
		var provider, resource, text string
		if err := rows.Scan(&provider, &resource, &text); err != nil {
			return nil, err
		}
		a := protocol.Adopting{Resource: resource}
		if err := json.Unmarshal([]byte(text), &a.IDs); err != nil {
			return nil, err
		}
		out[provider] = append(out[provider], a)
	}
	return out, rows.Err()
}

// ForgetAdoption removes a resource's adoption: it was adopted (it is Mamela's now), or the
// import is withdrawn.
func (s *Store) ForgetAdoption(ctx context.Context, provider, resource string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM adoptions WHERE provider = ? AND resource = ?`, provider, resource)
	return err
}
