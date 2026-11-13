package store

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"reflect"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// ExplicitContext binds recovery to the original provider contract and object. A current object
// with the same display name is never substituted for the confirmed one.
type ExplicitContext struct {
	Observed    protocol.Observed   `json:"observed"`
	Providers   string              `json:"providers"`
	Creations   []map[string]string `json:"creations"`
	AgentSerial string              `json:"agentSerial"`
}

type Explicit struct {
	ID, Provider, Resource, Op, Outcome string
	Change                              protocol.Change
	Context                             ExplicitContext
}

func (s *Store) StartObservedExplicit(ctx context.Context, id, provider, op, by, providers string, observed protocol.Observed, change protocol.Change) error {
	created, err := s.Creations(ctx, provider, observed.ID)
	if err != nil {
		return err
	}
	x := ExplicitContext{Observed: observed, Providers: providers, Creations: created}
	if observed.Node != nil {
		a, ok, err := s.Agent(ctx, observed.Name)
		if err != nil {
			return err
		}
		if ok {
			if int64(a.VMID) != observed.Node.ID {
				return fmt.Errorf("node %s has another enrolled identity", observed.Name)
			}
			x.AgentSerial = a.CertSerial
		}
	}
	b, err := json.Marshal(x, json.Deterministic(true))
	if err != nil {
		return err
	}
	c, err := json.Marshal(change, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO explicit_operations(id,provider,resource,op,change,confirmed_by,started_ms,context)
VALUES(?,?,?,?,?,?,?,?)`, id, provider, observed.ID, op, string(c), by, s.nowMs(), b)
	return err
}

func (s *Store) Explicit(ctx context.Context, id string) (Explicit, error) {
	e := Explicit{ID: id}
	var change, binding []byte
	err := s.db.QueryRowContext(ctx, `SELECT provider,resource,op,outcome,change,context FROM explicit_operations WHERE id=?`, id).
		Scan(&e.Provider, &e.Resource, &e.Op, &e.Outcome, &change, &binding)
	if err != nil {
		return e, notFound(err)
	}
	if len(binding) == 0 {
		return e, fmt.Errorf("MAM-ACT-0204: this older operation has no identity for automatic recovery; inspect it manually")
	}
	if err := json.Unmarshal(change, &e.Change, json.RejectUnknownMembers(true)); err != nil {
		return e, err
	}
	err = json.Unmarshal(binding, &e.Context, json.RejectUnknownMembers(true))
	return e, err
}

// CompleteExplicit commits the outcome and, only after absence was observed, revokes the old
// identity atomically. Audit operations/events remain. Repeating completion after a crash is safe.
func (s *Store) CompleteExplicit(ctx context.Context, e Explicit, outcome, detail string, absent bool) error {
	if absent && outcome != "done" {
		return fmt.Errorf("cannot revoke a node before completion is confirmed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if absent {
		rows, err := tx.QueryContext(ctx, `SELECT ids FROM provider_creations WHERE provider=? AND resource=? ORDER BY recorded_ms,ids`, e.Provider, e.Resource)
		if err != nil {
			return err
		}
		var current []map[string]string
		for rows.Next() {
			var b []byte
			if err = rows.Scan(&b); err != nil {
				rows.Close()
				return err
			}
			var ids map[string]string
			if err = json.Unmarshal(b, &ids); err != nil {
				rows.Close()
				return err
			}
			current = append(current, ids)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		// Empty means a previous completion already committed. A changed record requires fresh
		// investigation, never removal of a newer object's ownership or certificate.
		if len(current) > 0 && !reflect.DeepEqual(current, e.Context.Creations) {
			return fmt.Errorf("MAM-ACT-0205: ownership of %s changed during the operation; retained its registration", e.Resource)
		}
		if n := e.Context.Observed.Node; n != nil && len(current) > 0 {
			name := e.Context.Observed.Name
			var serial string
			var vmid int64
			if err := tx.QueryRowContext(ctx, `SELECT vmid,cert_serial FROM agents WHERE node=?`, name).Scan(&vmid, &serial); err == nil {
				if vmid != n.ID || serial != e.Context.AgentSerial {
					return fmt.Errorf("MAM-ACT-0205: node %s enrolled again; retained its new registration", name)
				}
			} else if notFound(err) != ErrNotFound {
				return err
			}
			rev, err := latestNodeRevision(ctx, tx, name)
			if err != nil {
				return err
			}
			if rev != nil && int64(rev.VMID) != n.ID {
				return fmt.Errorf("MAM-ACT-0205: node %s has a newer identity", name)
			}
			for _, q := range []string{`DELETE FROM agents WHERE node=?`, `DELETE FROM node_revisions WHERE node=?`, `DELETE FROM node_reports WHERE node=?`} {
				if _, err := tx.ExecContext(ctx, q, name); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM enroll_tokens WHERE node=? AND vmid=?`, name, n.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM vmids WHERE name=? AND vmid=?`, name, n.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM provider_creations WHERE provider=? AND resource=?`, e.Provider, e.Resource); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE explicit_operations SET outcome=?,detail=?,finished_ms=? WHERE id=?`, outcome, detail, s.nowMs(), e.ID); err != nil {
		return err
	}
	return tx.Commit()
}
