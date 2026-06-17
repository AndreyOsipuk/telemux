//go:build integration

// Интеграционный тест store против реального PostgreSQL.
// Запуск: TELEMUX_TEST_DSN=postgres://... go test -tags=integration ./internal/store
package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func mustDSN(t *testing.T) string {
	dsn := os.Getenv("TELEMUX_TEST_DSN")
	if dsn == "" {
		t.Skip("TELEMUX_TEST_DSN не задан — пропускаю интеграционный тест")
	}
	return dsn
}

func setup(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, mustDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Применяем схему (миграция идемпотентна — IF NOT EXISTS).
	mig, err := os.ReadFile("../../migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("чтение миграции: %v", err)
	}
	if _, err := st.pool.Exec(ctx, string(mig)); err != nil {
		t.Fatalf("применение миграции: %v", err)
	}
	if _, err := st.pool.Exec(ctx, "TRUNCATE users"); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	return st
}

func TestIntegration_IsInRecovery(t *testing.T) {
	st := setup(t)
	defer st.Close()
	rec, err := st.IsInRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec {
		t.Fatal("тестовый PG — primary, ждали pg_is_in_recovery()=false")
	}
}

func TestIntegration_ListDesired(t *testing.T) {
	st := setup(t)
	defer st.Close()
	ctx := context.Background()

	exp := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	_, err := st.pool.Exec(ctx, `
		INSERT INTO users (username, secret, expiration_at, max_tcp_conns, enabled) VALUES
		  ('sub_1', 'eeaaa', $1, 8, true),
		  ('sub_2', 'eebbb', NULL, NULL, true),
		  ('sub_off', 'eeccc', $1, 4, false)`, exp)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	des, err := st.ListDesired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(des) != 2 {
		t.Fatalf("ждали 2 активных (sub_off исключён), получили %d: %+v", len(des), des)
	}
	// ORDER BY username → sub_1, sub_2
	u1 := des[0]
	if u1.Username != "sub_1" || u1.Secret != "eeaaa" || u1.MaxTCPConns == nil || *u1.MaxTCPConns != 8 {
		t.Fatalf("sub_1 неверно: %+v", u1)
	}
	if u1.ExpirationRFC3339 == nil || *u1.ExpirationRFC3339 != "2026-07-01T00:00:00Z" {
		t.Fatalf("sub_1 expiration неверно: %v", u1.ExpirationRFC3339)
	}
	u2 := des[1]
	if u2.Username != "sub_2" || u2.ExpirationRFC3339 != nil || u2.MaxTCPConns != nil {
		t.Fatalf("sub_2 (NULL-поля) неверно: %+v", u2)
	}
}
