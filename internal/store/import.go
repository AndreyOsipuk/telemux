package store

import (
	"context"
	"fmt"
	"time"
)

// MaxImportBatch — верхний предел импорта за раз (защита от OOM/таймаута).
const MaxImportBatch = 50000

// ImportRow — одна запись для импорта (с секретом — для обратной совместимости).
type ImportRow struct {
	Username     string
	Secret       string
	ExpirationAt *time.Time
	MaxTCPConns  *int
}

// ImportUsers идемпотентно загружает юзеров (upsert по username) в ОДНОЙ транзакции.
// Секрет/срок/лимит перезаписываются значениями из источника — это путь обратной
// совместимости (секреты совпадают с теми, что на ноде → старые ссылки работают).
// Любая ошибка строки откатывает весь импорт (атомарность). Только на master/primary.
// Возвращает (inserted, updated).
func (s *Store) ImportUsers(ctx context.Context, rows []ImportRow) (int, int, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	if len(rows) > MaxImportBatch {
		return 0, 0, fmt.Errorf("слишком большой импорт: %d > %d", len(rows), MaxImportBatch)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin import tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op после Commit

	var inserted, updated int
	for _, r := range rows {
		if r.Username == "" || r.Secret == "" {
			return 0, 0, fmt.Errorf("импорт: пустой username/secret у %q", r.Username)
		}
		// xmax=0 у вставки → INSERT; иначе UPDATE. Так различаем без отдельного SELECT.
		var wasInsert bool
		err := tx.QueryRow(ctx,
			`INSERT INTO users (username, secret, expiration_at, max_tcp_conns, enabled)
			 VALUES ($1, $2, $3, $4, true)
			 ON CONFLICT (username) DO UPDATE
			   SET secret = EXCLUDED.secret,
			       expiration_at = EXCLUDED.expiration_at,
			       max_tcp_conns = EXCLUDED.max_tcp_conns,
			       updated_at = now()
			 RETURNING (xmax = 0)`,
			r.Username, r.Secret, r.ExpirationAt, r.MaxTCPConns).Scan(&wasInsert)
		if err != nil {
			return 0, 0, fmt.Errorf("импорт %q: %w", r.Username, err)
		}
		if wasInsert {
			inserted++
		} else {
			updated++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit import: %w", err)
	}
	return inserted, updated, nil
}

// ReconcileResult — итог bulk-реконсиляции (telemux PG ← полный набор от бота).
type ReconcileResult struct {
	Inserted int  `json:"inserted"`
	Updated  int  `json:"updated"`
	Deleted  int  `json:"deleted"`
	Aborted  bool `json:"aborted"` // сработал guard массового сноса
}

// ShrinkGuardThreshold — доля удаляемых, выше которой реконсиляция блокируется
// (подозрение на потерю данных в источнике). Зеркалит shrink-protection бота (20%).
const ShrinkGuardThreshold = 0.20

// ReconcileUsers приводит telemux-PG к ПОЛНОМУ набору desired (от бота) в одной tx:
// upsert всех переданных + удаление тех, кого в наборе нет. Идемпотентно.
//
// Guard массового сноса: пустой набор или удаление > ShrinkGuardThreshold при ≥10
// текущих юзерах → Aborted=true, изменения откатываются (если не force). Зеркалит
// shrink-protection generateUsersConfig — защищает от пустой/битой выборки источника.
func (s *Store) ReconcileUsers(ctx context.Context, rows []ImportRow, force bool) (ReconcileResult, error) {
	var res ReconcileResult
	if len(rows) > MaxImportBatch {
		return res, fmt.Errorf("слишком большой набор: %d > %d", len(rows), MaxImportBatch)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin reconcile tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var current int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&current); err != nil {
		return res, fmt.Errorf("count current: %w", err)
	}

	keep := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Username == "" || r.Secret == "" {
			return res, fmt.Errorf("reconcile: пустой username/secret у %q", r.Username)
		}
		keep = append(keep, r.Username)
		var wasInsert bool
		err := tx.QueryRow(ctx,
			`INSERT INTO users (username, secret, expiration_at, max_tcp_conns, enabled)
			 VALUES ($1, $2, $3, $4, true)
			 ON CONFLICT (username) DO UPDATE
			   SET secret = EXCLUDED.secret, expiration_at = EXCLUDED.expiration_at,
			       max_tcp_conns = EXCLUDED.max_tcp_conns, updated_at = now()
			 RETURNING (xmax = 0)`,
			r.Username, r.Secret, r.ExpirationAt, r.MaxTCPConns).Scan(&wasInsert)
		if err != nil {
			return res, fmt.Errorf("reconcile upsert %q: %w", r.Username, err)
		}
		if wasInsert {
			res.Inserted++
		} else {
			res.Updated++
		}
	}

	// Сколько удалили бы (текущие, кого нет в keep).
	var toDelete int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE NOT (username = ANY($1))`, keep).Scan(&toDelete); err != nil {
		return res, fmt.Errorf("count to-delete: %w", err)
	}

	// Guard массового сноса.
	massDelete := len(rows) == 0 ||
		(current >= 10 && float64(toDelete)/float64(current) > ShrinkGuardThreshold)
	if massDelete && !force {
		res.Aborted = true
		return res, nil // tx откатится в defer — НИЧЕГО не меняем
	}

	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE NOT (username = ANY($1))`, keep)
	if err != nil {
		return res, fmt.Errorf("reconcile delete: %w", err)
	}
	res.Deleted = int(tag.RowsAffected())

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit reconcile: %w", err)
	}
	return res, nil
}
