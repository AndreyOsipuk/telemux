package store

import (
	"context"
	"fmt"
	"time"
)

// StreamingStandbys - имена (application_name) реплик, которые сейчас стримят с
// локального primary и присылали отчёт не позже maxAge назад. Patroni выставляет
// application_name = имя участника, оно же код машины в telemux.
//
// Нужна роль с pg_monitor: иначе pg_stat_replication скрывает поля чужих сессий.
// На реплике возвращает пустой список.
func (s *Store) StreamingStandbys(ctx context.Context, maxAge time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT application_name FROM pg_stat_replication
		 WHERE state = 'streaming'
		   AND reply_time > now() - make_interval(secs => $1)
		 ORDER BY application_name`, maxAge.Seconds())
	if err != nil {
		return nil, fmt.Errorf("pg_stat_replication: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("pg_stat_replication: скан: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Fencer - PG-fence для заданных ролей приложений. Реализует role.Fencer.
type Fencer struct {
	Store *Store
	Roles []string // роли приложений (бот), чьи сессии рвутся при fence
}

// SetWriteFence: on - default_transaction_read_only=on для всего кластера PG
// (через postgresql.auto.conf + reload) и обрыв сессий ролей приложений;
// off - сброс параметра.
//
// read-only по умолчанию отсекает libpq-клиентов с target_session_attrs=read-write:
// при переподключении они уйдут на мастер. Сессии Patroni, репликации и самого
// telemux не трогаются.
func (f Fencer) SetWriteFence(ctx context.Context, on bool) error {
	stmt := "ALTER SYSTEM RESET default_transaction_read_only"
	if on {
		stmt = "ALTER SYSTEM SET default_transaction_read_only = on"
	}
	conn, err := f.Store.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("PG-fence: соединение: %w", err)
	}
	defer conn.Release()
	// Своя сессия могла унаследовать read-only после прошлого fence - снимаем для неё.
	// ALTER SYSTEM нельзя в блоке транзакции, поэтому отдельными командами.
	for _, q := range []string{"SET SESSION default_transaction_read_only = off", stmt, "SELECT pg_reload_conf()"} {
		if _, err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("PG-fence %v: %s: %w", on, q, err)
		}
	}
	if on {
		return f.TerminateAppSessions(ctx)
	}
	return nil
}

// TerminateAppSessions рвёт клиентские сессии ролей приложений (кроме своей).
func (f Fencer) TerminateAppSessions(ctx context.Context) error {
	if len(f.Roles) == 0 {
		return nil
	}
	_, err := f.Store.pool.Exec(ctx, `
		SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE backend_type = 'client backend'
		   AND pid <> pg_backend_pid()
		   AND usename = ANY($1)`, f.Roles)
	if err != nil {
		return fmt.Errorf("PG-fence: обрыв сессий %v: %w", f.Roles, err)
	}
	return nil
}

// WriteFenceStatus - включён ли PG-fence в postgresql.auto.conf (для `telemux fence status`).
func (s *Store) WriteFenceStatus(ctx context.Context) (bool, error) {
	var on bool
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(bool_or(setting = 'on'), false) FROM pg_file_settings
		 WHERE name = 'default_transaction_read_only'
		   AND sourcefile LIKE '%postgresql.auto.conf'
		   AND error IS NULL`).Scan(&on)
	if err != nil {
		return false, fmt.Errorf("pg_file_settings: %w", err)
	}
	return on, nil
}
