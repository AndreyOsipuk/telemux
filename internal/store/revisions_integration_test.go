//go:build integration

package store

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

// setupAll - Open + все миграции по порядку + чистые таблицы ревизий.
func setupAll(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, mustDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	files, _ := filepath.Glob("../../migrations/*.sql")
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("миграция %s: %v", f, err)
		}
	}
	if _, err := st.pool.Exec(ctx, `TRUNCATE users, targets, desired_config, observed_state, apply_log, agent_tokens,
		agent_join_tokens, agent_runtime, runtime_user_conns, runtime_log_errors, servers, server_endpoints,
		agent_state, machine_metrics, machine_events, machine_alert_state, machine_apply_alerts, server_render,
		stack_releases, stack_updates, pair_alert_state, runtime_traffic_counters, runtime_traffic_deltas`); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	return st
}

const (
	sec1 = "11111111111111111111111111111111"
	sec2 = "22222222222222222222222222222222"
	sec9 = "99999999999999999999999999999999"
)

func seedTargets(t *testing.T, st *Store, targets ...Target) {
	t.Helper()
	for _, tg := range targets {
		if err := st.UpsertTarget(context.Background(), tg); err != nil {
			t.Fatalf("UpsertTarget %s: %v", tg.Code, err)
		}
	}
}

func telemtTarget(code, kind string) Target {
	return Target{Code: code, Kind: kind, Resources: []string{ResourceTelemtUsers}, Enabled: true}
}

func TestIntegration_Publish_NewRevisionOnlyOnChange(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("ps2", "exit"), telemtTarget("pr2", "relay"))
	if _, err := st.ReconcileUsers(ctx, []ImportRow{{Username: "alice_1", Secret: sec1}}, true); err != nil {
		t.Fatal(err)
	}

	first, err := st.PublishTelemtUsers(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == 0 || len(first.Changed) != 2 {
		t.Fatalf("первая публикация: ждали ревизию и 2 машины, получили %+v", first)
	}

	again, err := st.PublishTelemtUsers(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != first.Revision || len(again.Changed) != 0 {
		t.Fatalf("без изменений не должно быть новой ревизии: было %+v, стало %+v", first, again)
	}

	if _, err := st.ReconcileUsers(ctx, []ImportRow{{Username: "alice_1", Secret: sec1}, {Username: "bob_2", Secret: sec2}}, true); err != nil {
		t.Fatal(err)
	}
	next, err := st.PublishTelemtUsers(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision <= first.Revision || len(next.Changed) != 2 {
		t.Fatalf("после изменения ждали новую ревизию на 2 машинах, получили %+v", next)
	}

	d, err := st.LatestDesired(ctx, "pr2", ResourceTelemtUsers)
	if err != nil {
		t.Fatal(err)
	}
	if d.Revision != next.Revision || d.SHA256 != telemtcfg.SHA256(d.Content) {
		t.Fatalf("желаемое для pr2 не совпадает с публикацией: %+v", d)
	}
	parsed := telemtcfg.ParseUsersToml(string(d.Content))
	if len(parsed) != 2 {
		t.Fatalf("в желаемом конфиге ждали 2 пользователей, получили %d", len(parsed))
	}
}

// Машина без ресурса telemt и выключенная машина ревизий не получают.
func TestIntegration_Publish_SkipsTargetsWithoutResource(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	off := telemtTarget("pr9", "relay")
	off.Enabled = false
	seedTargets(t, st,
		telemtTarget("ps2", "exit"),
		Target{Code: "ps3", Kind: "exit", Resources: nil, Enabled: true},
		off,
	)
	if _, err := st.ReconcileUsers(ctx, []ImportRow{{Username: "alice_1", Secret: sec1}}, true); err != nil {
		t.Fatal(err)
	}
	res, err := st.PublishTelemtUsers(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0] != "ps2" {
		t.Fatalf("ревизию должна получить только ps2, получили %v", res.Changed)
	}
	if _, err := st.LatestDesired(ctx, "ps3", ResourceTelemtUsers); err != ErrNoDesired {
		t.Fatalf("у ps3 не должно быть желаемого, err=%v", err)
	}
}

// Машина, добавленная позже, получает текущее состояние при следующей публикации,
// хотя содержимое у остальных не менялось.
func TestIntegration_Publish_NewTargetGetsCurrentState(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("ps2", "exit"))
	if _, err := st.ReconcileUsers(ctx, []ImportRow{{Username: "alice_1", Secret: sec1}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PublishTelemtUsers(ctx, 50); err != nil {
		t.Fatal(err)
	}
	seedTargets(t, st, telemtTarget("pr2", "relay"))
	res, err := st.PublishTelemtUsers(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0] != "pr2" {
		t.Fatalf("ждали ревизию только для новой pr2, получили %v", res.Changed)
	}
}

// Старый секрет после смены ключа доезжает до рендера.
func TestIntegration_Publish_RendersPreviousSecret(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("ps2", "exit"))
	revoke := time.Date(2026, 11, 18, 12, 10, 0, 0, time.UTC)
	if _, err := st.ReconcileUsers(ctx, []ImportRow{{
		Username: "alice_1", Secret: sec1, PreviousSecret: sec9, PreviousSecretExpiresAt: &revoke,
	}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PublishTelemtUsers(ctx, 50); err != nil {
		t.Fatal(err)
	}
	d, err := st.LatestDesired(ctx, "ps2", ResourceTelemtUsers)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range telemtcfg.ParseUsersToml(string(d.Content)) {
		if u.Username == "alice_1_legacy" && u.Secret == sec9 && u.ExpirationAt != nil && u.ExpirationAt.Equal(revoke) {
			found = true
		}
	}
	if !found {
		t.Fatalf("нет записи старого секрета:\n%s", d.Content)
	}
}

// Отключённые пользователи в конфиг не попадают.
func TestIntegration_Publish_DisabledUsersExcluded(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("ps2", "exit"))
	if _, err := st.ReconcileUsers(ctx, []ImportRow{{Username: "alice_1", Secret: sec1}, {Username: "bob_2", Secret: sec2}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetEnabled(ctx, "bob_2", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PublishTelemtUsers(ctx, 50); err != nil {
		t.Fatal(err)
	}
	d, _ := st.LatestDesired(ctx, "ps2", ResourceTelemtUsers)
	for _, u := range telemtcfg.ParseUsersToml(string(d.Content)) {
		if u.Username == "bob_2" {
			t.Fatal("отключённый пользователь попал в конфиг")
		}
	}
}

// Параллельные публикации не создают двух ревизий с одинаковым содержимым подряд.
func TestIntegration_Publish_ConcurrentIsSerialized(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("ps2", "exit"))
	if _, err := st.ReconcileUsers(ctx, []ImportRow{{Username: "alice_1", Secret: sec1}}, true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.PublishTelemtUsers(ctx, 50); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM desired_config WHERE target = 'ps2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ждали одну ревизию на одинаковое содержимое, получили %d", n)
	}
}

// Статус ревизии: применено, только когда машина отчиталась о ревизии не меньше
// нужной и со статусом applied.
func TestIntegration_RevisionStatus(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("ps2", "exit"), telemtTarget("pr2", "relay"))
	if _, err := st.ReconcileUsers(ctx, []ImportRow{{Username: "alice_1", Secret: sec1}}, true); err != nil {
		t.Fatal(err)
	}
	pub, err := st.PublishTelemtUsers(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}

	status, err := st.RevisionStatus(ctx, pub.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 2 || status[0].Applied || status[1].Applied {
		t.Fatalf("без отчётов ничего не применено, получили %+v", status)
	}

	d, _ := st.LatestDesired(ctx, "ps2", ResourceTelemtUsers)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO observed_state (target, resource, revision, sha256, status, seen_at)
		 VALUES ('ps2', $1, $2, $3, 'applied', now()), ('pr2', $1, $4, 'old', 'applied', now())`,
		ResourceTelemtUsers, pub.Revision, d.SHA256, pub.Revision-1); err != nil {
		t.Fatal(err)
	}
	status, err = st.RevisionStatus(ctx, pub.Revision)
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]TargetStatus{}
	for _, s := range status {
		byCode[s.Code] = s
	}
	if !byCode["ps2"].Applied || byCode["pr2"].Applied {
		t.Fatalf("ps2 применила, pr2 отстаёт - получили %+v", status)
	}

	if _, err := st.RevisionStatus(ctx, pub.Revision+1000); err != ErrUnknownRevision {
		t.Fatalf("несуществующая ревизия: ждали ErrUnknownRevision, получили %v", err)
	}
}

// Точечное изменение: создать, потом изменить - второй раз не «создан».
func TestIntegration_UpsertUser(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	created, err := st.UpsertUser(ctx, ImportRow{Username: "alice_1", Secret: sec1})
	if err != nil || !created {
		t.Fatalf("первый upsert: created=%v err=%v", created, err)
	}
	created, err = st.UpsertUser(ctx, ImportRow{Username: "alice_1", Secret: sec2})
	if err != nil || created {
		t.Fatalf("второй upsert: created=%v err=%v", created, err)
	}
	var secret string
	if err := st.pool.QueryRow(ctx, `SELECT secret FROM users WHERE username = 'alice_1'`).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if secret != sec2 {
		t.Fatalf("секрет не обновлён: %s", secret)
	}
}

// Ресурс, который готовит бот (конфиг mtg машины): новая ревизия только при
// изменении содержимого; машина без этого ресурса или неизвестная - ошибка.
func TestIntegration_PublishResource(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st,
		Target{Code: "pr2", Kind: "relay", Resources: []string{ResourceTelemtUsers, ResourceMtgConfig}, Enabled: true},
		Target{Code: "ps2", Kind: "exit", Resources: []string{ResourceTelemtUsers}, Enabled: true},
	)
	cfg := []byte("[secrets]\n\"alice_1\" = \"ee11\"\n")
	r1, err := st.PublishResource(ctx, "pr2", ResourceMtgConfig, cfg)
	if err != nil || r1.Revision == 0 || len(r1.Changed) != 1 {
		t.Fatalf("первая публикация: %+v, %v", r1, err)
	}
	r2, err := st.PublishResource(ctx, "pr2", ResourceMtgConfig, cfg)
	if err != nil || r2.Revision != r1.Revision || len(r2.Changed) != 0 {
		t.Fatalf("то же содержимое не должно давать новую ревизию: %+v, %v", r2, err)
	}
	r3, err := st.PublishResource(ctx, "pr2", ResourceMtgConfig, append(cfg, '\n'))
	if err != nil || r3.Revision <= r1.Revision {
		t.Fatalf("изменение должно дать новую ревизию: %+v, %v", r3, err)
	}
	if _, err := st.PublishResource(ctx, "ps2", ResourceMtgConfig, cfg); err != ErrResourceNotAssigned {
		t.Fatalf("машина без ресурса: ждали ErrResourceNotAssigned, получили %v", err)
	}
	if _, err := st.PublishResource(ctx, "nobody", ResourceMtgConfig, cfg); err != ErrUnknownTarget {
		t.Fatalf("неизвестная машина: ждали ErrUnknownTarget, получили %v", err)
	}
	b, _ := st.DesiredBundle(ctx, "pr2")
	if len(b.Resources) != 1 || b.Resources[0].Resource != ResourceMtgConfig || b.Revision != r3.Revision {
		t.Fatalf("в бандле pr2 ждали mtg.config ревизии %d, получили %+v", r3.Revision, b)
	}
}

// Вход мастер-панели как машина вида panel: настройки входа, желаемые клиенты и
// отчёт синхронизатора попадают в выборку работы.
func TestIntegration_PanelWork(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, Target{Code: "xui-ps7", Kind: "panel", Resources: []string{ResourceXuiClients}, Enabled: true,
		Settings: map[string]string{"inbound_id": "7", "node_id": "3", "port": "8767", "node_code": "ps7"}})
	if _, err := st.PublishResource(ctx, "xui-ps7", ResourceXuiClients, []byte(`[{"email":"a_1_ps7","secret":"aa","subId":"mtpx-x","enable":true}]`)); err != nil {
		t.Fatal(err)
	}
	work, err := st.PanelWork(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(work) != 1 || work[0].Code != "xui-ps7" || work[0].Settings["inbound_id"] != "7" || work[0].Desired.Revision == 0 || work[0].ObservedRevision != nil {
		t.Fatalf("неверная выборка: %+v", work)
	}
	if err := st.RecordObserved(ctx, ObservedReport{Target: "xui-ps7", Resources: []ObservedResource{
		{Resource: ResourceXuiClients, Revision: work[0].Desired.Revision, SHA256: "d", Status: "applied"}}}); err != nil {
		t.Fatal(err)
	}
	work, _ = st.PanelWork(ctx)
	if work[0].ObservedRevision == nil || *work[0].ObservedRevision != work[0].Desired.Revision || work[0].ObservedStatus != "applied" || work[0].SeenAt == nil {
		t.Fatalf("отчёт не попал в выборку: %+v", work[0])
	}
}

// Основной конфиг telemt публикуется по машине своими ревизиями и не трогает
// telemt.users: публикация пользователей не порождает ревизию telemt.config и наоборот.
func TestIntegration_PublishTelemtConfig(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st,
		Target{Code: "pr2", Kind: "relay", Resources: []string{ResourceTelemtUsers, ResourceTelemtConfig}, Enabled: true},
		Target{Code: "ps2", Kind: "exit", Resources: []string{ResourceTelemtUsers, ResourceTelemtConfig}, Enabled: true},
		Target{Code: "ps4", Kind: "exit", Resources: []string{ResourceTelemtUsers}, Enabled: true},
	)
	if !PublishableResources[ResourceTelemtConfig] {
		t.Fatal("telemt.config должен публиковаться извне")
	}
	if _, err := st.UpsertUser(ctx, ImportRow{Username: "alice_1", Secret: strings.Repeat("a", 32)}); err != nil {
		t.Fatal(err)
	}
	users, err := st.PublishTelemtUsers(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	pr2 := []byte("[server]\nport = 443\n\n[[server.listeners]]\nannounce_ip = \"198.51.100.2\"\n\n[access]\ninclude = \"/etc/telemt/users.toml\"\n")
	ps2 := []byte(strings.Replace(string(pr2), "198.51.100.2", "198.51.100.12", 1))
	r1, err := st.PublishResource(ctx, "pr2", ResourceTelemtConfig, pr2)
	if err != nil || r1.Revision <= users.Revision {
		t.Fatalf("публикация pr2: %+v, %v", r1, err)
	}
	r2, err := st.PublishResource(ctx, "ps2", ResourceTelemtConfig, ps2)
	if err != nil || r2.Revision <= r1.Revision || len(r2.Changed) != 1 || r2.Changed[0] != "ps2" {
		t.Fatalf("публикация ps2 - своя ревизия: %+v, %v", r2, err)
	}
	if again, _ := st.PublishResource(ctx, "pr2", ResourceTelemtConfig, pr2); again.Revision != r1.Revision || len(again.Changed) != 0 {
		t.Fatalf("тот же текст дал новую ревизию: %+v", again)
	}
	if _, err := st.PublishResource(ctx, "ps4", ResourceTelemtConfig, ps2); err != ErrResourceNotAssigned {
		t.Fatalf("машина без telemt.config: ждали ErrResourceNotAssigned, получили %v", err)
	}
	// Повторная публикация тех же пользователей не плодит ревизий и не трогает telemt.config.
	if u2, _ := st.PublishTelemtUsers(ctx, 0); len(u2.Changed) != 0 {
		t.Fatalf("пользователи не менялись, а ревизия новая: %+v", u2)
	}
	b, _ := st.DesiredBundle(ctx, "pr2")
	got := map[string]Desired{}
	for _, d := range b.Resources {
		got[d.Resource] = d
	}
	if len(got) != 2 || string(got[ResourceTelemtConfig].Content) != string(pr2) || got[ResourceTelemtUsers].Revision != users.Revision {
		t.Fatalf("в бандле pr2 ждали оба ресурса со своими ревизиями, получили %+v", b)
	}
	if b.Revision != r1.Revision {
		t.Fatalf("ревизия бандла pr2 - максимальная из его ресурсов: ждали %d, получили %d", r1.Revision, b.Revision)
	}
}

// Повторное назначение второму процессу сохраняет все параметры существующего релея.
func TestIntegration_AssignSecondaryMtg(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, Target{Code: "pr1", Kind: "relay", Resources: []string{ResourceTelemtUsers, ResourceMtgConfig}, Enabled: true, Settings: map[string]string{"address": "203.0.113.20"}})
	if _, err := st.PublishResource(ctx, "pr1", ResourceMtgSecondaryConfig, []byte("secondary")); err != ErrResourceNotAssigned {
		t.Fatalf("второй ресурс до назначения: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := st.AssignSecondaryMtg(ctx, "pr1"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM targets WHERE code='pr1' AND enabled AND cardinality(resources)=3 AND settings->>'address'='203.0.113.20' AND $1=ANY(resources)", ResourceTelemtUsers).Scan(&count); err != nil || count != 1 {
		t.Fatalf("назначение изменило релей: %d %v", count, err)
	}
	for _, resource := range []string{ResourceMtgConfig, ResourceMtgSecondaryConfig} {
		if _, err := st.PublishResource(ctx, "pr1", resource, []byte(resource)); err != nil {
			t.Fatal(err)
		}
	}
	b, err := st.DesiredBundle(ctx, "pr1")
	if err != nil || len(b.Resources) != 2 {
		t.Fatalf("нет двух ресурсов: %+v %v", b, err)
	}
	for _, r := range b.Resources {
		if string(r.Content) != r.Resource {
			t.Fatal("два конфига перекрыли друг друга")
		}
	}
}
