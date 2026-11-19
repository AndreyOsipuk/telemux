//go:build integration

package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func exitTarget(code, relay, addr, socks string) Target {
	return Target{Code: code, Kind: "exit", RelayCode: relay, Resources: []string{}, Enabled: true,
		Settings: map[string]string{SettingAddress: addr, SettingBridgeSocks: socks}}
}

func liveJoinTokens(t *testing.T, st *Store, target string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_join_tokens WHERE target = $1 AND used_at IS NULL`, target).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Машина и join-токен создаются одной транзакцией: занятый код не оставляет
// ни новой машины, ни токена; в базе только хеш токена.
func TestIntegration_CreateTargetWithJoin(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, Target{Code: "pr1", Kind: "relay", Resources: []string{}, Enabled: true})

	res, err := st.CreateTarget(ctx, exitTarget("ps1", "pr1", "203.0.113.5", "socks5://127.0.0.1:10872"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Join == nil || len(res.Join.Token) != 64 {
		t.Fatalf("ждали токен из 64 hex, получили %+v", res.Join)
	}
	if time.Until(res.Join.ExpiresAt) < 59*time.Minute || time.Until(res.Join.ExpiresAt) > 61*time.Minute {
		t.Fatalf("срок токена не час: %v", res.Join.ExpiresAt)
	}
	var stored string
	if err := st.pool.QueryRow(ctx, `SELECT token_sha256 FROM agent_join_tokens WHERE target = 'ps1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == res.Join.Token || stored != tokenHash(res.Join.Token) {
		t.Fatal("в базе должен лежать только sha256 токена")
	}
	got, err := st.TargetByCode(ctx, "ps1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings[SettingAddress] != "203.0.113.5" || got.Settings[SettingBridgeSocks] != "socks5://127.0.0.1:10872" || got.RelayCode != "pr1" {
		t.Fatalf("настройки выхода не сохранились: %+v", got)
	}

	// Занятый код: ни машина не перезаписана, ни токен не выпущен.
	_, err = st.CreateTarget(ctx, exitTarget("ps1", "pr1", "198.51.100.7", "socks5://127.0.0.1:10873"), time.Hour)
	if !errors.Is(err, ErrTargetExists) {
		t.Fatalf("ждали ErrTargetExists, получили %v", err)
	}
	if got, _ := st.TargetByCode(ctx, "ps1"); got.Settings[SettingAddress] != "203.0.113.5" {
		t.Fatal("занятый код перезаписал машину")
	}
	if n := liveJoinTokens(t, st, "ps1"); n != 1 {
		t.Fatalf("ждали 1 живой токен, получили %d", n)
	}

	// Релей не существует или это не релей - машина не создаётся.
	if _, err := st.CreateTarget(ctx, exitTarget("ps2", "nobody", "203.0.113.6", "socks5://127.0.0.1:1"), time.Hour); !errors.Is(err, ErrBadRelay) {
		t.Fatalf("ждали ErrBadRelay, получили %v", err)
	}
	if _, err := st.CreateTarget(ctx, exitTarget("ps3", "ps1", "203.0.113.6", "socks5://127.0.0.1:1"), time.Hour); !errors.Is(err, ErrBadRelay) {
		t.Fatalf("релей - выход: ждали ErrBadRelay, получили %v", err)
	}
	if _, err := st.TargetByCode(ctx, "ps2"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatal("машина с плохим релеем не должна создаться")
	}

	// Новый релей без выходов: машина есть, токена нет (команду не собрать).
	rr, err := st.CreateTarget(ctx, Target{Code: "pr9", Kind: "relay", Resources: []string{ResourceTelemtUsers}, Enabled: true}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Join != nil || len(rr.Exits) != 0 || liveJoinTokens(t, st, "pr9") != 0 {
		t.Fatalf("релей без выходов: токена быть не должно, получили %+v", rr)
	}
}

// Новый выпуск гасит прежний неиспользованный токен; релей без выходов - 409,
// старый токен при этом остаётся.
func TestIntegration_IssueJoinToken(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st,
		Target{Code: "pr1", Kind: "relay", Resources: []string{}, Enabled: true},
		Target{Code: "pr2", Kind: "relay", Resources: []string{}, Enabled: true},
		exitTarget("ps2", "pr1", "203.0.113.2", "socks5://127.0.0.1:10872"),
		exitTarget("ps1", "pr1", "203.0.113.1", "socks5://127.0.0.1:10871"),
		exitTarget("ps3", "pr1", "", ""), // без адреса - мостом не годится
		Target{Code: "pan1", Kind: "panel", Resources: []string{}, Enabled: true},
	)

	first, err := st.IssueJoinToken(ctx, "ps1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.IssueJoinToken(ctx, "ps1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.JoinTarget(ctx, first.Join.Token); !errors.Is(err, ErrBadJoinToken) {
		t.Fatalf("старый токен после нового выпуска: ждали ErrBadJoinToken, получили %v", err)
	}
	if info, err := st.JoinTarget(ctx, second.Join.Token); err != nil || info.Target.Code != "ps1" {
		t.Fatalf("новый токен не работает: %+v, %v", info, err)
	}
	if n := liveJoinTokens(t, st, "ps1"); n != 1 {
		t.Fatalf("живых токенов %d, ждали 1", n)
	}

	// Релей: выходы с адресом и socks, по коду.
	rel, err := st.IssueJoinToken(ctx, "pr1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.Exits) != 2 || rel.Exits[0].Code != "ps1" || rel.Exits[1].Code != "ps2" {
		t.Fatalf("выходы релея: %+v", rel.Exits)
	}
	if _, err := st.IssueJoinToken(ctx, "pr2", time.Hour); !errors.Is(err, ErrRelayNoExits) {
		t.Fatalf("релей без выходов: ждали ErrRelayNoExits, получили %v", err)
	}
	if _, err := st.IssueJoinToken(ctx, "nobody", time.Hour); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("ждали ErrUnknownTarget, получили %v", err)
	}
	if _, err := st.IssueJoinToken(ctx, "pan1", time.Hour); !errors.Is(err, ErrJoinNotSupported) {
		t.Fatalf("panel: ждали ErrJoinNotSupported, получили %v", err)
	}

	// Просроченный токен не находится.
	old, err := st.IssueJoinToken(ctx, "ps2", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE agent_join_tokens SET expires_at = now() - interval '1 second' WHERE target = 'ps2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.JoinTarget(ctx, old.Join.Token); !errors.Is(err, ErrBadJoinToken) {
		t.Fatalf("просроченный: ждали ErrBadJoinToken, получили %v", err)
	}
	if _, err := st.ExchangeJoin(ctx, old.Join.Token); !errors.Is(err, ErrBadJoinToken) {
		t.Fatalf("обмен просроченного: ждали ErrBadJoinToken, получили %v", err)
	}
}

// Обмен одноразовый и атомарный: токен тратится вместе с выпуском токена
// агента; из параллельных обменов успешен ровно один.
func TestIntegration_ExchangeJoin(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, `TRUNCATE agent_tokens`); err != nil {
		t.Fatal(err)
	}
	seedTargets(t, st,
		Target{Code: "pr1", Kind: "relay", Resources: []string{ResourceTelemtUsers}, Enabled: true},
		exitTarget("ps1", "pr1", "203.0.113.1", "socks5://127.0.0.1:10871"),
	)
	iss, err := st.IssueJoinToken(ctx, "pr1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	results := make(chan JoinExchange, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ex, err := st.ExchangeJoin(ctx, iss.Join.Token)
			if err != nil {
				errs <- err
				return
			}
			results <- ex
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	if len(results) != 1 {
		t.Fatalf("ждали ровно один успешный обмен из %d, получили %d", n, len(results))
	}
	for err := range errs {
		if !errors.Is(err, ErrBadJoinToken) {
			t.Fatalf("неудачный обмен: ждали ErrBadJoinToken, получили %v", err)
		}
	}
	ex := <-results
	if ex.Target.Code != "pr1" || len(ex.Exits) != 1 || ex.Exits[0].Code != "ps1" {
		t.Fatalf("обмен: %+v", ex)
	}
	if code, err := st.TargetByAgentToken(ctx, ex.AgentToken); err != nil || code != "pr1" {
		t.Fatalf("токен агента не работает: %q, %v", code, err)
	}
	if _, err := st.ExchangeJoin(ctx, iss.Join.Token); !errors.Is(err, ErrBadJoinToken) {
		t.Fatalf("повторный обмен: ждали ErrBadJoinToken, получили %v", err)
	}
	if _, err := st.JoinTarget(ctx, iss.Join.Token); !errors.Is(err, ErrBadJoinToken) {
		t.Fatal("использованный токен не должен отдавать скрипт")
	}

	// Релей потерял выходы до обмена: 409, токен не тратится.
	iss2, err := st.IssueJoinToken(ctx, "pr1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM targets WHERE code = 'ps1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ExchangeJoin(ctx, iss2.Join.Token); !errors.Is(err, ErrRelayNoExits) {
		t.Fatalf("ждали ErrRelayNoExits, получили %v", err)
	}
	if n := liveJoinTokens(t, st, "pr1"); n != 1 {
		t.Fatalf("после отказа токен должен остаться живым, живых %d", n)
	}
	// Удаление машины удаляет её токены (ON DELETE CASCADE).
	if _, err := st.pool.Exec(ctx, `DELETE FROM targets WHERE code = 'pr1'`); err != nil {
		t.Fatal(err)
	}
	if n := liveJoinTokens(t, st, "pr1"); n != 0 {
		t.Fatalf("токены удалённой машины остались: %d", n)
	}
}
