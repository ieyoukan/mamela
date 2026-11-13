package store

import (
	"context"
	"fmt"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// RedirectAgents changes only the endpoint of enrolled nodes with an existing revision. It is
// an explicit operator action, never a consequence of evaluating or planning a declaration.
func (s *Store) RedirectAgents(ctx context.Context, url string) (int, error) {
	if !noderev.ValidControlPlane(url) {
		return 0, fmt.Errorf("invalid Control Plane URL")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT node,vmid FROM agents ORDER BY node`)
	if err != nil {
		return 0, err
	}
	var agents []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Node, &a.VMID); err != nil {
			rows.Close()
			return 0, err
		}
		agents = append(agents, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range agents {
		r, err := latestNodeRevision(ctx, tx, a.Node)
		if err != nil {
			return 0, err
		}
		if r == nil || r.VMID != a.VMID || r.ControlPlane == url {
			continue
		}
		r.ControlPlane = url
		r.Serial++
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_revisions(node,serial,content,created_ms) VALUES(?,?,?,?)`, a.Node, r.Serial, r.Content(), s.nowMs()); err != nil {
			return 0, err
		}
		n++
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(at_ms,kind,message) VALUES(?,?,?)`, s.nowMs(), "controlplane-redirect", fmt.Sprintf("redirected %d enrolled agents to %s", n, url)); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
