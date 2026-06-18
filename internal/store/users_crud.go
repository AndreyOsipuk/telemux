package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrUserExists — попытка создать юзера с уже занятым username.
var ErrUserExists = errors.New("пользователь с таким username уже существует")

// User — полная строка таблицы users (для CRUD/UI).
type User struct {
	Username     string     `json:"username"`
	Secret       string     `json:"secret"`
	ExpirationAt *time.Time `json:"expiration_at"`
	MaxTCPConns  *int       `json:"max_tcp_conns"`
	Enabled      bool       `json:"enabled"`
	CreatedAt    time.Time  `json:"created_at"`
}

// GenerateSecret — 16 случайных байт в hex (32 симв.) — формат MTProto-секрета telemt.
func GenerateSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CreateUser добавляет юзера (только на master/primary). secret пустой → сгенерируется.
func (s *Store) CreateUser(ctx context.Context, username, secret string, exp *time.Time, maxConns *int) (User, error) {
	if username == "" {
		return User{}, fmt.Errorf("username обязателен")
	}
	if secret == "" {
		var err error
		if secret, err = GenerateSecret(); err != nil {
			return User{}, err
		}
	}
	var u User
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (username, secret, expiration_at, max_tcp_conns, enabled)
		 VALUES ($1, $2, $3, $4, true)
		 RETURNING username, secret, expiration_at, max_tcp_conns, enabled, created_at`,
		username, secret, exp, maxConns).
		Scan(&u.Username, &u.Secret, &u.ExpirationAt, &u.MaxTCPConns, &u.Enabled, &u.CreatedAt)
	if err != nil {
		// 23505 = unique_violation
		if pgErrCode(err) == "23505" {
			return User{}, ErrUserExists
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// DeleteUser удаляет юзера. Возвращает false, если такого не было.
func (s *Store) DeleteUser(ctx context.Context, username string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE username = $1`, username)
	if err != nil {
		return false, fmt.Errorf("delete user: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetExpiration выставляет срок (nil = снять срок). false, если юзера нет.
func (s *Store) SetExpiration(ctx context.Context, username string, exp *time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET expiration_at = $2, updated_at = now() WHERE username = $1`, username, exp)
	if err != nil {
		return false, fmt.Errorf("set expiration: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetEnabled включает/выключает юзера. false, если юзера нет.
func (s *Store) SetEnabled(ctx context.Context, username string, enabled bool) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET enabled = $2, updated_at = now() WHERE username = $1`, username, enabled)
	if err != nil {
		return false, fmt.Errorf("set enabled: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListUsersPage возвращает страницу юзеров + общее число (для UI-пагинатора).
// limit зажимается в [1,100], offset >= 0 (правила проекта по list-эндпоинтам).
func (s *Store) ListUsersPage(ctx context.Context, limit, offset int) ([]User, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT username, secret, expiration_at, max_tcp_conns, enabled, created_at
		   FROM users ORDER BY created_at DESC, username LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	out := make([]User, 0, limit)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.Username, &u.Secret, &u.ExpirationAt, &u.MaxTCPConns, &u.Enabled, &u.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// pgErrCode достаёт SQLSTATE из ошибки pgx/pgconn (или "").
func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
