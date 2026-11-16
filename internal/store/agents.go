package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AgentCount is the number of enrolled agents.
func (s *Store) AgentCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents`).Scan(&n)
	return n, err
}

// Agents lists the enrolled agents by node name.
func (s *Store) Agents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node, vmid, cert_serial, enrolled_ms FROM agents ORDER BY node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		var at int64
		if err := rows.Scan(&a.Node, &a.VMID, &a.CertSerial, &at); err != nil {
			return nil, err
		}
		a.Enrolled = time.UnixMilli(at).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// EnrollTokenUsable reports whether Enroll would accept this token for node (vmid) now. It spends
// nothing: Enroll still decides, in its own transaction. It lets the caller refuse a request that
// has no good token before doing any work for it.
func (s *Store) EnrollTokenUsable(ctx context.Context, token, node string, vmid int) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM enroll_tokens
WHERE token_hash = ? AND node = ? AND vmid = ? AND used_ms = 0 AND expires_ms > ?`, hashToken(token), node, vmid, s.nowMs()).Scan(&n)
	return n == 1, err
}

// agentCertKind is the kind of the events that record when an agent's certificate ends.
const agentCertKind = "agent-cert"

// NoteAgentCert records, in the event log, that the certificate issued to a node's agent (at
// enrollment or as a renewal) ends at notAfter. registration is the agent's registration (the
// serial recorded at enrollment). The database holds only that serial, so this is where the
// certificate's end is kept for listings and warnings.
func (s *Store) NoteAgentCert(ctx context.Context, node, registration string, notAfter time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events(at_ms, resource, kind, message) VALUES(?, ?, ?, ?)`,
		s.nowMs(), node, agentCertKind, fmt.Sprintf("registration=%s not_after=%s", registration, notAfter.UTC().Format(time.RFC3339)))
	return err
}

// AgentCertNotAfter is when the latest certificate noted for each enrolled agent's current
// registration ends. A node with no note (enrolled before notes were kept) is not in the map.
func (s *Store) AgentCertNotAfter(ctx context.Context) (map[string]time.Time, error) {
	agents, err := s.Agents(ctx)
	if err != nil {
		return nil, err
	}
	registration := map[string]string{}
	for _, a := range agents {
		registration[a.Node] = a.CertSerial
	}
	rows, err := s.db.QueryContext(ctx, `SELECT resource, message FROM events WHERE kind = ? ORDER BY seq`, agentCertKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var node, msg string
		if err := rows.Scan(&node, &msg); err != nil {
			return nil, err
		}
		reg, notAfter, ok := parseAgentCertNote(msg)
		if !ok || registration[node] != reg {
			continue // not about the registration the node has now (it enrolled again since)
		}
		out[node] = notAfter
	}
	return out, rows.Err()
}

func parseAgentCertNote(msg string) (registration string, notAfter time.Time, ok bool) {
	for _, f := range strings.Fields(msg) {
		k, v, found := strings.Cut(f, "=")
		if !found {
			return "", time.Time{}, false
		}
		switch k {
		case "registration":
			registration = v
		case "not_after":
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return "", time.Time{}, false
			}
			notAfter = t
		}
	}
	return registration, notAfter, registration != "" && !notAfter.IsZero()
}
