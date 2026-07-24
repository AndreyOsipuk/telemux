//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func TestIntegration_ImportUsers_IdempotentPreservesSecret(t *testing.T) {
	st := setup(t) // Open + миграция + TRUNCATE users
	defer st.Close()
	ctx := context.Background()
	exp := time.Date(2027, 6, 13, 16, 14, 8, 0, time.UTC)
	mc := 25

	rows := []ImportRow{
		{Username: "garagefri_3073", Secret: "ca7315aabbccddeeff00112233445566", ExpirationAt: &exp, MaxTCPConns: &mc},
		{Username: "alice_42", Secret: "0011223344556677889900aabbccddee"},
	}
	ins, upd, err := st.ImportUsers(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 2 || upd != 0 {
		t.Fatalf("первый импорт: ждали ins=2 upd=0, получили ins=%d upd=%d", ins, upd)
	}

	// Секрет сохранён ровно как в источнике (обратная совместимость ссылок).
	var secret string
	if err := st.pool.QueryRow(ctx, `SELECT secret FROM users WHERE username='garagefri_3073'`).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if secret != "ca7315aabbccddeeff00112233445566" {
		t.Fatalf("секрет не сохранён: %q", secret)
	}

	// Повторный импорт того же → всё UPDATE, не дубли (idempotent).
	ins, upd, err = st.ImportUsers(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 0 || upd != 2 {
		t.Fatalf("повторный импорт: ждали ins=0 upd=2, получили ins=%d upd=%d", ins, upd)
	}
	var total int
	st.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&total)
	if total != 2 {
		t.Fatalf("после двух импортов ждали 2 строки (без дублей), получили %d", total)
	}
}

func TestIntegration_ImportUsers_BadRowRollsBackAll(t *testing.T) {
	st := setup(t)
	defer st.Close()
	ctx := context.Background()

	// Вторая строка с пустым секретом → весь батч должен откатиться (атомарность).
	rows := []ImportRow{
		{Username: "ok_user", Secret: "aa11"},
		{Username: "bad_user", Secret: ""}, // невалидно
	}
	if _, _, err := st.ImportUsers(ctx, rows); err == nil {
		t.Fatal("ждали ошибку на пустом секрете")
	}
	var total int
	st.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&total)
	if total != 0 {
		t.Fatalf("после отката ждали 0 строк, получили %d (ok_user не должен был сохраниться)", total)
	}
}
