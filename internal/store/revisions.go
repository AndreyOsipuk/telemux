package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

// ResourceTelemtUsers - включаемый файл telemt со списком пользователей.
const ResourceTelemtUsers = "telemt.users"

// Ключ advisory-lock публикации: публикации сериализуются, чтобы две
// параллельные не записали одинаковое содержимое двумя ревизиями.
const publishLockKey int64 = 0x74656c656d757801 // "telemux" + 1

var (
	// ErrNoDesired - у машины нет желаемого состояния для ресурса.
	ErrNoDesired = errors.New("нет желаемого состояния")
	// ErrUnknownRevision - такой ревизии не публиковалось.
	ErrUnknownRevision = errors.New("неизвестная ревизия")
)

// Target - машина флота.
type Target struct {
	Code      string   `json:"code"`
	Kind      string   `json:"kind"` // exit | relay
	RelayCode string   `json:"relay_code,omitempty"`
	Resources []string `json:"resources"`
	Enabled   bool     `json:"enabled"`
}

// UpsertTarget добавляет машину или обновляет её описание.
func (s *Store) UpsertTarget(ctx context.Context, t Target) error {
	if t.Resources == nil {
		t.Resources = []string{}
	}
	var relay *string
	if t.RelayCode != "" {
		relay = &t.RelayCode
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO targets (code, kind, relay_code, resources, enabled)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (code) DO UPDATE
		   SET kind = EXCLUDED.kind, relay_code = EXCLUDED.relay_code,
		       resources = EXCLUDED.resources, enabled = EXCLUDED.enabled, updated_at = now()`,
		t.Code, t.Kind, relay, t.Resources, t.Enabled)
	if err != nil {
		return fmt.Errorf("upsert target %q: %w", t.Code, err)
	}
	return nil
}

// PublishResult - итог публикации.
type PublishResult struct {
	Revision int64    `json:"revision"` // последняя ревизия ресурса (новая или прежняя)
	Changed  []string `json:"changed"`  // машины, получившие новую ревизию
}

// PublishTelemtUsers собирает users.toml из включённых пользователей и пишет
// новую ревизию тем машинам с ресурсом telemt.users, у которых последнее
// желаемое отличается по хешу. Одна публикация - один номер ревизии.
// Только на primary.
func (s *Store) PublishTelemtUsers(ctx context.Context, defaultMaxConns int) (PublishResult, error) {
	res := PublishResult{Changed: []string{}}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin publish: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, publishLockKey); err != nil {
		return res, fmt.Errorf("publish lock: %w", err)
	}

	users, err := renderUsers(ctx, tx)
	if err != nil {
		return res, err
	}
	content := telemtcfg.RenderUsersToml(users, defaultMaxConns)
	sha := telemtcfg.SHA256(content)

	// Машины с ресурсом и их последний хеш (одним запросом, без N+1).
	rows, err := tx.Query(ctx,
		`SELECT t.code, d.sha256, d.revision
		   FROM targets t
		   LEFT JOIN LATERAL (
		     SELECT sha256, revision FROM desired_config
		      WHERE target = t.code AND resource = $1
		      ORDER BY revision DESC LIMIT 1) d ON true
		  WHERE t.enabled AND $1 = ANY(t.resources)
		  ORDER BY t.code`, ResourceTelemtUsers)
	if err != nil {
		return res, fmt.Errorf("targets for publish: %w", err)
	}
	var stale []string
	for rows.Next() {
		var code string
		var lastSHA *string
		var lastRev *int64
		if err := rows.Scan(&code, &lastSHA, &lastRev); err != nil {
			rows.Close()
			return res, fmt.Errorf("scan target: %w", err)
		}
		if lastSHA == nil || *lastSHA != sha {
			stale = append(stale, code)
		} else if lastRev != nil && *lastRev > res.Revision {
			res.Revision = *lastRev
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("targets for publish: %w", err)
	}

	if len(stale) > 0 {
		var rev int64
		if err := tx.QueryRow(ctx, `SELECT nextval('config_revision_seq')`).Scan(&rev); err != nil {
			return res, fmt.Errorf("next revision: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO desired_config (target, resource, revision, sha256, content)
			 SELECT code, $2, $3, $4, $5 FROM unnest($1::text[]) AS code`,
			stale, ResourceTelemtUsers, rev, sha, content); err != nil {
			return res, fmt.Errorf("insert desired: %w", err)
		}
		res.Revision = rev
		res.Changed = stale
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit publish: %w", err)
	}
	return res, nil
}

// renderUsers читает включённых пользователей для рендера в той же транзакции.
func renderUsers(ctx context.Context, tx pgx.Tx) ([]telemtcfg.RenderUser, error) {
	rows, err := tx.Query(ctx,
		`SELECT username, secret, expiration_at, max_tcp_conns,
		        COALESCE(previous_secret, ''), previous_secret_expires_at
		   FROM users WHERE enabled`)
	if err != nil {
		return nil, fmt.Errorf("users for render: %w", err)
	}
	defer rows.Close()
	var out []telemtcfg.RenderUser
	for rows.Next() {
		var u telemtcfg.RenderUser
		if err := rows.Scan(&u.Key, &u.Secret, &u.ExpiresAt, &u.MaxTCPConns, &u.PreviousSecret, &u.PreviousSecretExpiresAt); err != nil {
			return nil, fmt.Errorf("scan user for render: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Desired - желаемое состояние ресурса машины.
type Desired struct {
	Target   string `json:"-"`
	Resource string `json:"resource"`
	Revision int64  `json:"revision"`
	SHA256   string `json:"sha256"`
	Content  []byte `json:"content"` // в JSON - base64
}

// LatestDesired - последняя ревизия ресурса машины.
func (s *Store) LatestDesired(ctx context.Context, target, resource string) (Desired, error) {
	d := Desired{Target: target, Resource: resource}
	err := s.pool.QueryRow(ctx,
		`SELECT revision, sha256, content FROM desired_config
		  WHERE target = $1 AND resource = $2 ORDER BY revision DESC LIMIT 1`,
		target, resource).Scan(&d.Revision, &d.SHA256, &d.Content)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNoDesired
	}
	if err != nil {
		return d, fmt.Errorf("latest desired %s/%s: %w", target, resource, err)
	}
	return d, nil
}

// TargetStatus - дошла ли ревизия до машины.
type TargetStatus struct {
	Code             string     `json:"code"`
	DesiredRevision  int64      `json:"desired_revision"`  // ревизия ресурса машины, действующая на момент запрошенной
	ObservedRevision *int64     `json:"observed_revision"` // о какой ревизии машина отчиталась
	Status           string     `json:"status"`            // applied | failed | pending | compared | unknown
	SeenAt           *time.Time `json:"seen_at"`
	Applied          bool       `json:"applied"`
}

// RevisionStatus - по каждой машине с ресурсом telemt.users: применено ли
// состояние не старее ревизии rev. Машины, которым ресурс назначен позже rev,
// не учитываются.
func (s *Store) RevisionStatus(ctx context.Context, rev int64) ([]TargetStatus, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM desired_config WHERE revision = $1)`, rev).Scan(&exists); err != nil {
		return nil, fmt.Errorf("revision exists: %w", err)
	}
	if !exists {
		return nil, ErrUnknownRevision
	}
	rows, err := s.pool.Query(ctx,
		`SELECT d.target, d.revision, o.revision, COALESCE(o.status, 'unknown'), o.seen_at
		   FROM (SELECT target, max(revision) AS revision FROM desired_config
		          WHERE resource = $1 AND revision <= $2 GROUP BY target) d
		   LEFT JOIN observed_state o ON o.target = d.target AND o.resource = $1
		  ORDER BY d.target`, ResourceTelemtUsers, rev)
	if err != nil {
		return nil, fmt.Errorf("revision status: %w", err)
	}
	defer rows.Close()
	var out []TargetStatus
	for rows.Next() {
		var t TargetStatus
		if err := rows.Scan(&t.Code, &t.DesiredRevision, &t.ObservedRevision, &t.Status, &t.SeenAt); err != nil {
			return nil, fmt.Errorf("scan status: %w", err)
		}
		t.Applied = t.Status == "applied" && t.ObservedRevision != nil && *t.ObservedRevision >= t.DesiredRevision
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpsertUser создаёт или обновляет одного пользователя (точечное изменение от
// бота: смена ключа, продление). Возвращает true, если пользователь создан.
func (s *Store) UpsertUser(ctx context.Context, r ImportRow) (bool, error) {
	if r.Username == "" || r.Secret == "" {
		return false, fmt.Errorf("upsert user: пустой username/secret у %q", r.Username)
	}
	var inserted bool
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (username, secret, expiration_at, max_tcp_conns, enabled,
		                    previous_secret, previous_secret_expires_at)
		 VALUES ($1, $2, $3, $4, true, NULLIF($5, ''), $6)
		 ON CONFLICT (username) DO UPDATE
		   SET secret = EXCLUDED.secret, expiration_at = EXCLUDED.expiration_at,
		       max_tcp_conns = EXCLUDED.max_tcp_conns, enabled = true,
		       previous_secret = EXCLUDED.previous_secret,
		       previous_secret_expires_at = EXCLUDED.previous_secret_expires_at,
		       updated_at = now()
		 RETURNING (xmax = 0)`,
		r.Username, r.Secret, r.ExpirationAt, r.MaxTCPConns, r.PreviousSecret, r.PreviousSecretExpiresAt).Scan(&inserted)
	if err != nil {
		return false, fmt.Errorf("upsert user %q: %w", r.Username, err)
	}
	return inserted, nil
}
