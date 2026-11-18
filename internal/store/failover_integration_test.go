//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const appRole = "telemux_test_app"

// appConn - сессия «приложения» (как бот) под отдельной ролью.
func appConn(t *testing.T, st *Store) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	cfg := st.pool.Config().ConnConfig.Copy()
	cfg.User, cfg.Password = appRole, appRole
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("подключение роли приложения: %v", err)
	}
	return c
}

func setupFence(t *testing.T) (*Store, Fencer) {
	t.Helper()
	st := setup(t)
	ctx := context.Background()
	for _, q := range []string{
		`DO $$ BEGIN
		   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'telemux_test_app') THEN
		     CREATE ROLE telemux_test_app LOGIN PASSWORD 'telemux_test_app';
		   END IF;
		 END $$`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON users TO telemux_test_app`,
		`GRANT USAGE ON SEQUENCE users_id_seq TO telemux_test_app`,
	} {
		if _, err := st.pool.Exec(ctx, q); err != nil {
			t.Fatalf("подготовка роли: %v", err)
		}
	}
	f := Fencer{Store: st, Roles: []string{appRole}}
	t.Cleanup(func() {
		// Тест не должен оставить кластер read-only для следующих пакетов.
		_ = f.SetWriteFence(context.Background(), false)
		st.Close()
	})
	return st, f
}

// waitReadOnly ждёт, пока новая сессия приложения увидит default_transaction_read_only
// (pg_reload_conf асинхронен).
func waitReadOnly(t *testing.T, st *Store, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := appConn(t, st)
		var got string
		err := c.QueryRow(context.Background(), "SHOW default_transaction_read_only").Scan(&got)
		c.Close(context.Background())
		if err == nil && got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("default_transaction_read_only: ждали %s, получили %q (err %v)", want, got, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func insertAsApp(c *pgx.Conn, name string) error {
	_, err := c.Exec(context.Background(), `INSERT INTO users (username, secret) VALUES ($1, 'ee')`, name)
	return err
}

func TestIntegration_WriteFence(t *testing.T) {
	st, f := setupFence(t)
	ctx := context.Background()

	// Открытая до fence сессия приложения (как висящее соединение бота).
	old := appConn(t, st)
	defer old.Close(ctx)
	if err := insertAsApp(old, "before_fence"); err != nil {
		t.Fatalf("до fence запись должна проходить: %v", err)
	}

	if err := f.SetWriteFence(ctx, true); err != nil {
		t.Fatalf("включение fence: %v", err)
	}
	on, err := st.WriteFenceStatus(ctx)
	if err != nil || !on {
		t.Fatalf("WriteFenceStatus после on: %v %v", on, err)
	}
	waitReadOnly(t, st, "on")

	// Новая сессия приложения: запись отвергается с 25006 (read_only_sql_transaction).
	c := appConn(t, st)
	err = insertAsApp(c, "during_fence")
	c.Close(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("запись под fence: ждали 25006, получили %v", err)
	}

	// Старая сессия приложения разорвана.
	if err := old.Ping(ctx); err == nil {
		t.Fatal("сессия приложения, открытая до fence, должна быть разорвана")
	}
	// Сессии telemux живы.
	if err := st.pool.Ping(ctx); err != nil {
		t.Fatalf("пул telemux не должен пострадать: %v", err)
	}

	// Повторное включение и обрыв - идемпотентны.
	if err := f.SetWriteFence(ctx, true); err != nil {
		t.Fatalf("повторное включение: %v", err)
	}
	if err := f.TerminateAppSessions(ctx); err != nil {
		t.Fatalf("обрыв без сессий: %v", err)
	}

	// Снятие fence (сессия telemux при этом сама унаследовала read-only - должно работать).
	if err := f.SetWriteFence(ctx, false); err != nil {
		t.Fatalf("снятие fence: %v", err)
	}
	waitReadOnly(t, st, "off")
	on, err = st.WriteFenceStatus(ctx)
	if err != nil || on {
		t.Fatalf("WriteFenceStatus после off: %v %v", on, err)
	}
	c = appConn(t, st)
	defer c.Close(ctx)
	if err := insertAsApp(c, "after_fence"); err != nil {
		t.Fatalf("после снятия fence запись должна проходить: %v", err)
	}
}

func TestIntegration_StreamingStandbysEmptyOnSinglePG(t *testing.T) {
	st := setup(t)
	defer st.Close()
	names, err := st.StreamingStandbys(context.Background(), 15*time.Second)
	if err != nil {
		t.Fatalf("pg_stat_replication: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("у одиночного PG реплик нет, получили %v", names)
	}
}
