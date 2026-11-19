//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// setupTLS - все миграции (включая 0009) и чистые таблицы сертификатов.
func setupTLS(t *testing.T) *Store {
	t.Helper()
	st := setupAll(t)
	if _, err := st.pool.Exec(context.Background(), `TRUNCATE tls_certs, acme_account`); err != nil {
		t.Fatalf("TRUNCATE tls: %v", err)
	}
	return st
}

func relayWeb(code, domains string, assigned bool) Target {
	t := Target{Code: code, Kind: "relay", Enabled: true, Resources: []string{ResourceTelemtUsers},
		Settings: map[string]string{SettingWebDomains: domains}}
	if assigned {
		t.Resources = append(t.Resources, ResourceTLSWeb)
	}
	return t
}

// Миграция 0009 идемпотентна: повторный прогон не падает.
func TestIntegration_TLS_MigrationIdempotent(t *testing.T) {
	st := setupTLS(t)
	defer st.Close()
	setupTLS(t).Close() // второй прогон всех миграций поверх готовой схемы
}

// web_domains проверяются при записи машины: звёздочка - ошибка, список - в каноническом виде.
func TestIntegration_TLS_UpsertTargetValidatesDomains(t *testing.T) {
	st := setupTLS(t)
	defer st.Close()
	ctx := context.Background()
	err := st.UpsertTarget(ctx, relayWeb("pr1", "*.example.com", true))
	if !errors.Is(err, ErrBadSettings) {
		t.Fatalf("звёздочка: ждали ErrBadSettings, получили %v", err)
	}
	if err := st.UpsertTarget(ctx, relayWeb("pr1", "Web1.example.com, web5.example.com", true)); err != nil {
		t.Fatal(err)
	}
	tg, err := st.TargetByCode(ctx, "pr1")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Settings[SettingWebDomains] != "web1.example.com,web5.example.com" {
		t.Fatalf("домены не в каноническом виде: %q", tg.Settings[SettingWebDomains])
	}
}

// Домены включённых релеев и признак назначения tls.web.
func TestIntegration_TLS_Targets(t *testing.T) {
	st := setupTLS(t)
	defer st.Close()
	ctx := context.Background()
	off := relayWeb("pr3", "web3.example.com", true)
	off.Enabled = false
	seedTargets(t, st, relayWeb("pr1", "web1.example.com,web5.example.com", true), relayWeb("pr2", "web2.example.com", false), off,
		Target{Code: "ps1", Kind: "exit", Enabled: true, Resources: []string{}})
	// Мусор, записанный в обход проверки, не роняет цикл: машина помечается.
	if _, err := st.pool.Exec(ctx, `INSERT INTO targets (code, kind, resources, enabled, settings)
		VALUES ('pr4', 'relay', '{}', true, '{"web_domains": "*.example.net"}')`); err != nil {
		t.Fatal(err)
	}
	list, err := st.TLSTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("ждали 3 включённых релея с доменами, получили %+v", list)
	}
	if list[0].Code != "pr1" || !list[0].Assigned || strings.Join(list[0].Domains, ",") != "web1.example.com,web5.example.com" {
		t.Fatalf("pr1: %+v", list[0])
	}
	if list[1].Code != "pr2" || list[1].Assigned {
		t.Fatalf("pr2 без tls.web: %+v", list[1])
	}
	if list[2].Code != "pr4" || list[2].Invalid == "" || len(list[2].Domains) != 0 {
		t.Fatalf("pr4 с мусором должен быть помечен: %+v", list[2])
	}
}

// Выпуск, ошибка и повтор: ошибка не стирает прежний сертификат.
func TestIntegration_TLS_SaveAndError(t *testing.T) {
	st := setupTLS(t)
	defer st.Close()
	ctx := context.Background()
	na := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.RecordTLSError(ctx, "web1.example.com", "лимит"); err != nil {
		t.Fatal(err)
	}
	states, err := st.TLSCertStates(ctx, []string{"web1.example.com", "web2.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	s1 := states["web1.example.com"]
	if s1.HasCert || s1.LastError != "лимит" || s1.LastTryAt == nil || s1.NotAfter != nil {
		t.Fatalf("ошибка без сертификата: %+v", s1)
	}
	if _, ok := states["web2.example.com"]; ok {
		t.Fatal("домена без строк в таблице быть не должно")
	}

	if err := st.SaveTLSCert(ctx, "web1.example.com", []byte("CERT"), []byte("KEY"), na); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordTLSError(ctx, "web1.example.com", "сбой продления"); err != nil {
		t.Fatal(err)
	}
	states, err = st.TLSCertStates(ctx, []string{"web1.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	s1 = states["web1.example.com"]
	if !s1.HasCert || s1.NotAfter == nil || !s1.NotAfter.Equal(na) || s1.IssuedAt == nil || s1.LastError != "сбой продления" {
		t.Fatalf("ошибка продления стёрла сертификат: %+v", s1)
	}
	if err := st.SaveTLSCert(ctx, "web1.example.com", []byte("CERT2"), []byte("KEY2"), na.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	states, _ = st.TLSCertStates(ctx, []string{"web1.example.com"})
	if states["web1.example.com"].LastError != "" {
		t.Fatalf("успешный выпуск должен сбросить ошибку: %+v", states["web1.example.com"])
	}
}

// tls.web публикуется только машине с назначенным ресурсом; без изменений - без новой ревизии.
func TestIntegration_TLS_PublishOnlyAssignedAndOnChange(t *testing.T) {
	st := setupTLS(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, relayWeb("pr1", "web1.example.com,web5.example.com", true), relayWeb("pr2", "web1.example.com", false))

	// Сертификатов ещё нет - публиковать нечего.
	res, err := st.PublishTLSWeb(ctx, "pr1")
	if err != nil || len(res.Changed) != 0 {
		t.Fatalf("без сертификатов ждали пустую публикацию, получили %+v, %v", res, err)
	}

	na := time.Now().Add(80 * 24 * time.Hour)
	if err := st.SaveTLSCert(ctx, "web1.example.com", []byte("CERT1"), []byte("KEY1"), na); err != nil {
		t.Fatal(err)
	}
	first, err := st.PublishTLSWeb(ctx, "pr1")
	if err != nil || len(first.Changed) != 1 || first.Revision == 0 {
		t.Fatalf("первая публикация: %+v, %v", first, err)
	}
	d, err := st.LatestDesired(ctx, "pr1", ResourceTLSWeb)
	if err != nil {
		t.Fatal(err)
	}
	var content map[string]TLSWebEntry
	if err := json.Unmarshal(d.Content, &content); err != nil {
		t.Fatalf("содержимое tls.web не JSON: %v", err)
	}
	if len(content) != 1 || content["web1.example.com"].Cert != "CERT1" || content["web1.example.com"].Key != "KEY1" {
		t.Fatalf("в ресурсе только домены с сертификатом: %+v", content)
	}

	again, err := st.PublishTLSWeb(ctx, "pr1")
	if err != nil || len(again.Changed) != 0 || again.Revision != first.Revision {
		t.Fatalf("без изменений новой ревизии быть не должно: %+v, %v", again, err)
	}

	if _, err := st.PublishTLSWeb(ctx, "pr2"); !errors.Is(err, ErrResourceNotAssigned) {
		t.Fatalf("машине без tls.web: ждали ErrResourceNotAssigned, получили %v", err)
	}
	if _, err := st.LatestDesired(ctx, "pr2", ResourceTLSWeb); !errors.Is(err, ErrNoDesired) {
		t.Fatalf("машине без tls.web ресурс опубликован: %v", err)
	}
	if _, err := st.PublishTLSWeb(ctx, "nobody"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("неизвестная машина: %v", err)
	}

	// Продление - новая ревизия.
	if err := st.SaveTLSCert(ctx, "web5.example.com", []byte("CERT5"), []byte("KEY5"), na); err != nil {
		t.Fatal(err)
	}
	next, err := st.PublishTLSWeb(ctx, "pr1")
	if err != nil || len(next.Changed) != 1 || next.Revision <= first.Revision {
		t.Fatalf("новый сертификат - новая ревизия: %+v, %v", next, err)
	}
}

// Список сертификатов: строка на (релей, домен), постранично, без ключей.
func TestIntegration_TLS_ListCerts(t *testing.T) {
	st := setupTLS(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, relayWeb("pr1", "web1.example.com,web5.example.com", true), relayWeb("pr2", "web2.example.com", false))
	na := time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.SaveTLSCert(ctx, "web5.example.com", []byte("CERT"), []byte("SECRET-KEY"), na); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordTLSError(ctx, "web2.example.com", "нет зоны"); err != nil {
		t.Fatal(err)
	}
	page1, total, err := st.ListCerts(ctx, "", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(page1) != 2 {
		t.Fatalf("ждали total=3 и 2 строки, получили %d и %+v", total, page1)
	}
	if page1[0].Machine != "pr1" || page1[0].Domain != "web1.example.com" || page1[0].NotAfter != nil || !page1[0].Assigned {
		t.Fatalf("первая строка: %+v", page1[0])
	}
	if page1[1].Domain != "web5.example.com" || page1[1].NotAfter == nil || !page1[1].NotAfter.Equal(na) {
		t.Fatalf("вторая строка: %+v", page1[1])
	}
	page2, _, err := st.ListCerts(ctx, "", 2, 2)
	if err != nil || len(page2) != 1 || page2[0].Machine != "pr2" || page2[0].LastError != "нет зоны" || page2[0].Assigned {
		t.Fatalf("вторая страница: %+v, %v", page2, err)
	}
	raw, _ := json.Marshal(append(page1, page2...))
	if strings.Contains(string(raw), "SECRET-KEY") || strings.Contains(string(raw), "CERT") {
		t.Fatalf("в списке не должно быть ни ключей, ни сертификатов: %s", raw)
	}
}

// Фильтр по машине и итоги для обзора: истекающие (меньше границы, в том числе
// уже истёкшие) и ещё не выпущенные.
func TestIntegration_TLS_ListCertsMachineAndCounts(t *testing.T) {
	st := setupTLS(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, relayWeb("pr1", "web1.example.com,web5.example.com", true),
		relayWeb("pr2", "web2.example.com,web3.example.com", false))
	now := time.Now().UTC().Truncate(time.Second)
	for d, na := range map[string]time.Time{
		"web1.example.com": now.Add(60 * 24 * time.Hour), // свежий
		"web5.example.com": now.Add(5 * 24 * time.Hour),  // истекает
		"web2.example.com": now.Add(-24 * time.Hour),     // уже истёк
	} {
		if err := st.SaveTLSCert(ctx, d, []byte("C"), []byte("K"), na); err != nil {
			t.Fatal(err)
		}
	}
	list, total, err := st.ListCerts(ctx, "pr2", 100, 0)
	if err != nil || total != 2 || len(list) != 2 || list[0].Domain != "web2.example.com" || list[1].Machine != "pr2" {
		t.Fatalf("фильтр pr2: total=%d %+v %v", total, list, err)
	}
	if _, total, _ := st.ListCerts(ctx, "nobody", 100, 0); total != 0 {
		t.Fatalf("неизвестная машина - пусто, а не %d", total)
	}
	c, err := st.CertCounts(ctx, now.Add(14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c != (CertCounts{Total: 4, Expiring: 2, Missing: 1}) {
		t.Fatalf("итоги: %+v", c)
	}
}

// Аккаунт ACME хранится в базе отдельно для каждого каталога ACME.
func TestIntegration_TLS_ACMEAccount(t *testing.T) {
	st := setupTLS(t)
	defer st.Close()
	ctx := context.Background()
	const dir = "https://acme.example.net/directory"
	if _, err := st.LoadACMEAccount(ctx, dir); !errors.Is(err, ErrNoACMEAccount) {
		t.Fatalf("ждали ErrNoACMEAccount, получили %v", err)
	}
	acc := ACMEAccount{Directory: dir, Email: "ops@example.com", KeyPEM: []byte("KEY")}
	if err := st.SaveACMEAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	acc.Registration = []byte(`{"uri":"https://acme.example.net/acct/1"}`)
	if err := st.SaveACMEAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadACMEAccount(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != acc.Email || string(got.KeyPEM) != "KEY" || !strings.Contains(string(got.Registration), "acct/1") {
		t.Fatalf("аккаунт: %+v", got)
	}
}
