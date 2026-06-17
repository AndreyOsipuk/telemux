// Package store — доступ к локальному PostgreSQL telemux (pgx).
//
// Реализует role.RecoveryChecker (pg_is_in_recovery) и sync.DesiredSource
// (чтение desired-юзеров). На реплике запросы read-only — этого достаточно
// (desired только читаем; пишет лишь master в primary).
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// Store — пул соединений к локальному PG.
type Store struct {
	pool *pgxpool.Pool
}

// Open открывает пул к локальному PG (dsn вида postgres://user:pass@127.0.0.1:5432/telemux).
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PG: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close закрывает пул.
func (s *Store) Close() { s.pool.Close() }

// IsInRecovery: true → standby (replica), false → primary (master).
func (s *Store) IsInRecovery(ctx context.Context) (bool, error) {
	var inRecovery bool
	err := s.pool.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery)
	if err != nil {
		return false, fmt.Errorf("pg_is_in_recovery: %w", err)
	}
	return inRecovery, nil
}

// ListDesired возвращает активных юзеров (desired-состояние) из таблицы users.
func (s *Store) ListDesired(ctx context.Context) ([]telemtsync.DesiredUser, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT username, secret, expiration_at, max_tcp_conns
		   FROM users WHERE enabled ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("запрос users: %w", err)
	}
	defer rows.Close()

	var out []telemtsync.DesiredUser
	for rows.Next() {
		var (
			username string
			secret   string
			expAt    *time.Time
			maxConns *int
		)
		if err := rows.Scan(&username, &secret, &expAt, &maxConns); err != nil {
			return nil, fmt.Errorf("скан users: %w", err)
		}
		var expStr *string
		if expAt != nil {
			s := expAt.UTC().Format(time.RFC3339)
			expStr = &s
		}
		out = append(out, telemtsync.DesiredUser{
			Username:          username,
			Secret:            secret,
			ExpirationRFC3339: expStr,
			MaxTCPConns:       maxConns,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("итерация users: %w", err)
	}
	return out, nil
}
