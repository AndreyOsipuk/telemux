package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrBadAgentToken - токен не принадлежит ни одной машине.
	ErrBadAgentToken = errors.New("неизвестный токен агента")
	// ErrUnknownTarget - машины нет в targets.
	ErrUnknownTarget = errors.New("неизвестная машина")
)

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueAgentToken выпускает новый токен машины; прежний перестаёт действовать.
// Токен возвращается один раз, в базе остаётся только его хеш.
func (s *Store) IssueAgentToken(ctx context.Context, target string) (string, error) {
	return issueAgentToken(ctx, s.pool, target)
}

// execer - пул или открытая транзакция.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// issueAgentToken - выпуск токена агента через пул или внутри транзакции.
func issueAgentToken(ctx context.Context, db execer, target string) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	tag, err := db.Exec(ctx,
		`INSERT INTO agent_tokens (target, token_sha256)
		 SELECT code, $2 FROM targets WHERE code = $1
		 ON CONFLICT (target) DO UPDATE SET token_sha256 = EXCLUDED.token_sha256, created_at = now()`,
		target, tokenHash(token))
	if err != nil {
		return "", fmt.Errorf("выпуск токена %q: %w", target, err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrUnknownTarget
	}
	return token, nil
}

// randomToken - 32 случайных байта в hex.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("генерация токена: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// TargetByAgentToken - машина, которой принадлежит токен.
func (s *Store) TargetByAgentToken(ctx context.Context, token string) (string, error) {
	var code string
	err := s.pool.QueryRow(ctx,
		`SELECT a.target FROM agent_tokens a JOIN targets t ON t.code = a.target
		  WHERE a.token_sha256 = $1 AND t.enabled`, tokenHash(token)).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrBadAgentToken
	}
	if err != nil {
		return "", fmt.Errorf("поиск токена: %w", err)
	}
	return code, nil
}

// MaxRevision - последняя ревизия любого ресурса машины (0 - публикаций не было).
// Дешёвый запрос для long-poll.
func (s *Store) MaxRevision(ctx context.Context, target string) (int64, error) {
	var rev int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(max(revision), 0) FROM desired_config WHERE target = $1`, target).Scan(&rev); err != nil {
		return 0, fmt.Errorf("max revision %q: %w", target, err)
	}
	return rev, nil
}

// Bundle - желаемое состояние машины: последняя ревизия каждого ресурса.
type Bundle struct {
	Target    string    `json:"target"`
	Revision  int64     `json:"revision"` // максимальная ревизия среди ресурсов
	Resources []Desired `json:"resources"`
}

// DesiredBundle собирает бандл машины одним запросом.
func (s *Store) DesiredBundle(ctx context.Context, target string) (Bundle, error) {
	b := Bundle{Target: target, Resources: []Desired{}}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (resource) resource, revision, sha256, content
		   FROM desired_config WHERE target = $1
		  ORDER BY resource, revision DESC`, target)
	if err != nil {
		return b, fmt.Errorf("bundle %q: %w", target, err)
	}
	defer rows.Close()
	for rows.Next() {
		d := Desired{Target: target}
		if err := rows.Scan(&d.Resource, &d.Revision, &d.SHA256, &d.Content); err != nil {
			return b, fmt.Errorf("scan bundle: %w", err)
		}
		if d.Revision > b.Revision {
			b.Revision = d.Revision
		}
		b.Resources = append(b.Resources, d)
	}
	return b, rows.Err()
}

// ObservedResource - отчёт агента по одному ресурсу.
type ObservedResource struct {
	Resource string `json:"resource"`
	Revision int64  `json:"revision"`
	SHA256   string `json:"sha256"`
	Status   string `json:"status"` // applied | failed | pending | compared
	Error    string `json:"error,omitempty"`
}

// ObservedCheck - результат локальной проверки машины (мост на выходе и т.п.).
type ObservedCheck struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ObservedReport - отчёт агента машины. Resources может быть пустым: машина без
// ресурсов (выход без telemt и mtg) отчитывается только о себе и о проверках.
type ObservedReport struct {
	Target       string             `json:"target"`
	AgentVersion string             `json:"agent_version"`
	Via          string             `json:"via,omitempty"`
	Resources    []ObservedResource `json:"resources"`
	Checks       []ObservedCheck    `json:"checks,omitempty"`
}

// RecordObserved сохраняет отчёт: observed_state перезаписывается, в apply_log
// попадает только смена ревизии, статуса или ошибки (повторы журнал не засоряют).
func (s *Store) RecordObserved(ctx context.Context, rep ObservedReport) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin observed: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, r := range rep.Resources {
		var changed bool
		err := tx.QueryRow(ctx,
			`WITH prev AS (
			   SELECT revision, status, COALESCE(error, '') AS error FROM observed_state
			    WHERE target = $1 AND resource = $2)
			 INSERT INTO observed_state (target, resource, revision, sha256, status, error, agent_version, via, seen_at)
			 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, NULLIF($8, ''), now())
			 ON CONFLICT (target, resource) DO UPDATE
			   SET revision = EXCLUDED.revision, sha256 = EXCLUDED.sha256, status = EXCLUDED.status,
			       error = EXCLUDED.error, agent_version = EXCLUDED.agent_version, via = EXCLUDED.via,
			       seen_at = EXCLUDED.seen_at
			 RETURNING NOT EXISTS (
			   SELECT 1 FROM prev WHERE prev.revision = $3 AND prev.status = $5 AND prev.error = $6)`,
			rep.Target, r.Resource, r.Revision, r.SHA256, r.Status, r.Error, rep.AgentVersion, rep.Via).Scan(&changed)
		if err != nil {
			return fmt.Errorf("observed %s/%s: %w", rep.Target, r.Resource, err)
		}
		if changed {
			if _, err := tx.Exec(ctx,
				`INSERT INTO apply_log (target, resource, revision, status, error) VALUES ($1, $2, $3, $4, NULLIF($5, ''))`,
				rep.Target, r.Resource, r.Revision, r.Status, r.Error); err != nil {
				return fmt.Errorf("apply_log %s/%s: %w", rep.Target, r.Resource, err)
			}
		}
	}
	if err := recordAgentState(ctx, tx, rep); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit observed: %w", err)
	}
	return nil
}

// recordAgentState обновляет строку машины в agent_state; смену результата
// проверки (появилась, пройдена ↔ не пройдена, другая причина) пишет в apply_log
// как ресурс "check:<имя>".
func recordAgentState(ctx context.Context, tx pgx.Tx, rep ObservedReport) error {
	checks := rep.Checks
	if checks == nil {
		checks = []ObservedCheck{}
	}
	var prev []ObservedCheck
	err := tx.QueryRow(ctx, `SELECT checks FROM agent_state WHERE target = $1 FOR UPDATE`, rep.Target).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("agent_state %s: %w", rep.Target, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_state (target, agent_version, via, checks, seen_at)
		 VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4, now())
		 ON CONFLICT (target) DO UPDATE
		   SET agent_version = EXCLUDED.agent_version, via = EXCLUDED.via,
		       checks = EXCLUDED.checks, seen_at = EXCLUDED.seen_at`,
		rep.Target, rep.AgentVersion, rep.Via, checks); err != nil {
		return fmt.Errorf("agent_state %s: %w", rep.Target, err)
	}
	was := make(map[string]ObservedCheck, len(prev))
	for _, c := range prev {
		was[c.Name] = c
	}
	for _, c := range checks {
		if p, ok := was[c.Name]; ok && p.OK == c.OK && p.Error == c.Error {
			continue
		}
		status := "ok"
		if !c.OK {
			status = "failed"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO apply_log (target, resource, status, error) VALUES ($1, $2, $3, NULLIF($4, ''))`,
			rep.Target, "check:"+c.Name, status, c.Error); err != nil {
			return fmt.Errorf("apply_log %s/check:%s: %w", rep.Target, c.Name, err)
		}
	}
	return nil
}
