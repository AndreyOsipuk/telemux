//go:build integration

package store

import (
	"context"
	"testing"
)

// Токен машины: выдаётся один раз, в базе хранится только хеш, старый токен
// после перевыпуска не действует.
func TestIntegration_AgentTokens(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, `TRUNCATE agent_tokens`); err != nil {
		t.Fatal(err)
	}
	seedTargets(t, st, telemtTarget("pr2", "relay"))

	tok, err := st.IssueAgentToken(ctx, "pr2")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 40 {
		t.Fatalf("слишком короткий токен: %q", tok)
	}
	var stored string
	if err := st.pool.QueryRow(ctx, `SELECT token_sha256 FROM agent_tokens WHERE target = 'pr2'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == tok {
		t.Fatal("токен хранится открытым текстом")
	}
	code, err := st.TargetByAgentToken(ctx, tok)
	if err != nil || code != "pr2" {
		t.Fatalf("ждали pr2, получили %q, %v", code, err)
	}
	if _, err := st.TargetByAgentToken(ctx, "nope"); err != ErrBadAgentToken {
		t.Fatalf("чужой токен: ждали ErrBadAgentToken, получили %v", err)
	}

	tok2, err := st.IssueAgentToken(ctx, "pr2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TargetByAgentToken(ctx, tok); err != ErrBadAgentToken {
		t.Fatal("старый токен после перевыпуска должен перестать работать")
	}
	if code, _ := st.TargetByAgentToken(ctx, tok2); code != "pr2" {
		t.Fatal("новый токен не работает")
	}
	if _, err := st.IssueAgentToken(ctx, "nobody"); err != ErrUnknownTarget {
		t.Fatalf("токен для несуществующей машины: ждали ErrUnknownTarget, получили %v", err)
	}
}

// Бандл - последняя ревизия каждого ресурса машины; номер бандла - максимальный.
func TestIntegration_DesiredBundle(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr2", "relay"), telemtTarget("ps2", "exit"))

	if rev, _ := st.MaxRevision(ctx, "pr2"); rev != 0 {
		t.Fatalf("без публикаций ждали 0, получили %d", rev)
	}
	if _, err := st.ReconcileUsers(ctx, []ImportRow{{Username: "alice_1", Secret: sec1}}, true); err != nil {
		t.Fatal(err)
	}
	p1, _ := st.PublishTelemtUsers(ctx, 50)
	if _, err := st.ReconcileUsers(ctx, []ImportRow{{Username: "alice_1", Secret: sec2}}, true); err != nil {
		t.Fatal(err)
	}
	p2, _ := st.PublishTelemtUsers(ctx, 50)

	rev, err := st.MaxRevision(ctx, "pr2")
	if err != nil || rev != p2.Revision {
		t.Fatalf("ждали ревизию %d, получили %d, %v", p2.Revision, rev, err)
	}
	b, err := st.DesiredBundle(ctx, "pr2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Revision != p2.Revision || len(b.Resources) != 1 || b.Resources[0].Revision != p2.Revision || b.Resources[0].Revision == p1.Revision {
		t.Fatalf("ждали один ресурс ревизии %d, получили %+v", p2.Revision, b)
	}
}

// Отчёт машины перезаписывает observed_state, а в журнал пишется только смена
// ревизии или статуса - повтор того же отчёта журнал не засоряет.
func TestIntegration_RecordObserved(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr2", "relay"))

	rep := ObservedReport{Target: "pr2", AgentVersion: "v0.9", Via: "ps2", Resources: []ObservedResource{
		{Resource: ResourceTelemtUsers, Revision: 5, SHA256: "aa", Status: "compared"},
	}}
	for i := 0; i < 3; i++ {
		if err := st.RecordObserved(ctx, rep); err != nil {
			t.Fatal(err)
		}
	}
	rep.Resources[0].Status = "failed"
	rep.Resources[0].Error = "отличается: 1 лишний"
	if err := st.RecordObserved(ctx, rep); err != nil {
		t.Fatal(err)
	}

	var status, errText, via string
	var revision int64
	if err := st.pool.QueryRow(ctx,
		`SELECT status, COALESCE(error, ''), revision, COALESCE(via, '') FROM observed_state WHERE target = 'pr2'`).
		Scan(&status, &errText, &revision, &via); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || errText == "" || revision != 5 || via != "ps2" {
		t.Fatalf("observed_state не обновлён: %s %q %d %s", status, errText, revision, via)
	}
	var logRows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM apply_log WHERE target = 'pr2'`).Scan(&logRows); err != nil {
		t.Fatal(err)
	}
	if logRows != 2 {
		t.Fatalf("в журнале ждали 2 записи (compared, failed), получили %d", logRows)
	}
}
