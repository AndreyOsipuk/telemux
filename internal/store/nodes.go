package store

import (
	"context"
	"fmt"
	"time"
)

// Node — нода кластера (строка таблицы nodes).
type Node struct {
	Code         string     `json:"code"`
	Name         string     `json:"name"`
	Address      string     `json:"address"`
	TelemtAPIURL string     `json:"telemt_api_url"`
	Role         string     `json:"role"`
	Enabled      bool       `json:"enabled"`
	LastSeenAt   *time.Time `json:"last_seen_at"`
}

// UpsertNode регистрирует/обновляет ноду по code (heartbeat). Только на master (primary PG).
func (s *Store) UpsertNode(ctx context.Context, n Node) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO nodes (code, name, address, telemt_api_url, role, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (code) DO UPDATE SET
			name = EXCLUDED.name,
			address = EXCLUDED.address,
			telemt_api_url = EXCLUDED.telemt_api_url,
			role = EXCLUDED.role,
			last_seen_at = now()`,
		n.Code, n.Name, n.Address, n.TelemtAPIURL, n.Role)
	if err != nil {
		return fmt.Errorf("upsert node %s: %w", n.Code, err)
	}
	return nil
}

// ListNodes возвращает все ноды (для веб-морды).
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT code, name, address, telemt_api_url, role, enabled, last_seen_at
		   FROM nodes ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.Code, &n.Name, &n.Address, &n.TelemtAPIURL, &n.Role, &n.Enabled, &n.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CreateJoinToken сохраняет одноразовый токен добавления ноды со сроком ttl.
func (s *Store) CreateJoinToken(ctx context.Context, token string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO join_tokens (token, expires_at) VALUES ($1, now() + make_interval(secs => $2))`,
		token, int(ttl.Seconds()))
	if err != nil {
		return fmt.Errorf("create join token: %w", err)
	}
	return nil
}

// ConsumeJoinToken атомарно помечает токен использованным, если он валиден
// (существует, не использован, не истёк). Возвращает true при успехе.
func (s *Store) ConsumeJoinToken(ctx context.Context, token string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE join_tokens SET used_at = now()
		   WHERE token = $1 AND used_at IS NULL AND expires_at > now()`, token)
	if err != nil {
		return false, fmt.Errorf("consume join token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
