//go:build integration

package store

import (
	"bytes"
	"context"
	"testing"
)

func TestIntegration_FT5_DataQuotaRevision(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("ps2", "exit"))

	// Пользователь записан без новой колонки, как до включения квот.
	if _, err := st.pool.Exec(ctx, "INSERT INTO users(username, secret) VALUES ('free_1', $1)", sec1); err != nil {
		t.Fatal(err)
	}
	publish := func() Desired {
		t.Helper()
		if _, err := st.PublishTelemtUsers(ctx, 50); err != nil {
			t.Fatal(err)
		}
		d, err := st.LatestDesired(ctx, "ps2", ResourceTelemtUsers)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	unlimited := publish()
	row := ImportRow{Username: "free_1", Secret: sec1, DataQuotaBytes: 3_000_000_000}
	if _, err := st.ReconcileUsers(ctx, []ImportRow{row}, false); err != nil {
		t.Fatal(err)
	}
	limited := publish()
	if limited.Revision <= unlimited.Revision ||
		!bytes.HasSuffix(limited.Content, []byte("\n[access.user_data_quota]\nfree_1 = 3000000000\n")) {
		t.Fatalf("квота не попала в новую ревизию %d", limited.Revision)
	}
	if repeat := publish(); repeat.Revision != limited.Revision {
		t.Fatal("неизменная квота создала новую ревизию")
	}

	row.DataQuotaBytes = 0
	if _, err := st.UpsertUser(ctx, row); err != nil {
		t.Fatal(err)
	}
	cleared := publish()
	if cleared.Revision <= limited.Revision || !bytes.Equal(cleared.Content, unlimited.Content) {
		t.Fatal("снятие квоты не восстановило исходный конфиг")
	}

	row.DataQuotaBytes = 100_000_000
	if _, _, err := st.ImportUsers(ctx, []ImportRow{row}); err != nil {
		t.Fatal(err)
	}
	if imported := publish(); !bytes.HasSuffix(imported.Content, []byte("\n[access.user_data_quota]\nfree_1 = 100000000\n")) {
		t.Fatal("квота потерялась при импорте")
	}
}
