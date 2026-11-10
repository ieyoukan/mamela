package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
)

// K3sToken returns the join token of the k3s cluster whose server is the given node, making it
// (32 random bytes) the first time. It is given only to the cluster's nodes, in their revisions.
func (s *Store) K3sToken(ctx context.Context, server string) (string, error) {
	var token string
	err := s.db.QueryRowContext(ctx, `SELECT token FROM k3s_tokens WHERE server = ?`, server).Scan(&token)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO k3s_tokens(server, token, created_ms) VALUES(?, ?, ?) ON CONFLICT(server) DO NOTHING`,
		server, hex.EncodeToString(b), s.nowMs()); err != nil {
		return "", err
	}
	err = s.db.QueryRowContext(ctx, `SELECT token FROM k3s_tokens WHERE server = ?`, server).Scan(&token)
	return token, err
}
