//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func telemtRep(target string, users ...RuntimeUser) RuntimeReport {
	return RuntimeReport{Target: target, AgentVersion: "v1",
		Host:   &RuntimeHost{CPUPct: 12.5, MemPct: 40, UptimeS: 100},
		Telemt: &RuntimeTelemt{Version: "3.5.17", APIOK: true, ConnectionsTotal: 9, Users: users}}
}

func userKeys(t *testing.T, st *Store, target string) map[string]int64 {
	t.Helper()
	rows, err := st.pool.Query(context.Background(),
		`SELECT product || ':' || user_key, connections FROM runtime_user_conns WHERE target = $1`, target)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		out[k] = n
	}
	return out
}

// Каждый отчёт заменяет строки пользователей машины целиком: ушедший пропадает,
// чужая машина не задевается, список пользователей в снимок машины не попадает.
func TestIntegration_RecordRuntime_ReplacesUsers(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"), telemtTarget("pr2", "relay"))

	if err := st.RecordRuntime(ctx, telemtRep("pr1",
		RuntimeUser{Key: "a_1", Connections: 2, ActiveIPs: []string{"1.1.1.1"}},
		RuntimeUser{Key: "b_2", Connections: 1})); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRuntime(ctx, telemtRep("pr2", RuntimeUser{Key: "z_9", Connections: 1})); err != nil {
		t.Fatal(err)
	}
	seen := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	rep := telemtRep("pr1", RuntimeUser{Key: "b_2", Connections: 4}, RuntimeUser{Key: "c_3", Connections: 1})
	rep.Mtg = &RuntimeMtg{Version: "2510861", APIOK: true, ConnectionsTotal: 3,
		Users: []RuntimeUser{{Key: "m_1", Connections: 3, BytesIn: 10, BytesOut: 20, LastSeen: &seen, ActiveIPs: []string{"2.2.2.2"}}}}
	if err := st.RecordRuntime(ctx, rep); err != nil {
		t.Fatal(err)
	}

	got := userKeys(t, st, "pr1")
	if len(got) != 3 || got["telemt:b_2"] != 4 || got["telemt:c_3"] != 1 || got["mtg:m_1"] != 3 {
		t.Fatalf("строки pr1 после замены: %v", got)
	}
	if _, ok := got["telemt:a_1"]; ok {
		t.Fatal("ушедший пользователь a_1 должен пропасть")
	}
	if other := userKeys(t, st, "pr2"); len(other) != 1 {
		t.Fatalf("отчёт pr1 не должен трогать pr2: %v", other)
	}
	var hasUsers bool
	if err := st.pool.QueryRow(ctx, `SELECT telemt ? 'users' FROM agent_runtime WHERE target = 'pr1'`).Scan(&hasUsers); err != nil {
		t.Fatal(err)
	}
	if hasUsers {
		t.Fatal("в agent_runtime.telemt не должно быть списка пользователей")
	}

	mr, err := st.MachineRuntime(ctx, "pr1")
	if err != nil {
		t.Fatal(err)
	}
	if mr.SeenAt == nil || mr.AgentVersion != "v1" || mr.Host == nil || mr.Host.CPUPct != 12.5 ||
		mr.Telemt == nil || mr.Telemt.Version != "3.5.17" || mr.Mtg == nil || mr.Mtg.ConnectionsTotal != 3 {
		t.Fatalf("снимок: %+v", mr)
	}

	// Машина без mtg в следующем отчёте: mtg = null, его строки пропали.
	if err := st.RecordRuntime(ctx, telemtRep("pr1")); err != nil {
		t.Fatal(err)
	}
	if mr, _ := st.MachineRuntime(ctx, "pr1"); mr.Mtg != nil || len(userKeys(t, st, "pr1")) != 0 {
		t.Fatalf("после отчёта без mtg и без пользователей: %+v %v", mr.Mtg, userKeys(t, st, "pr1"))
	}
}

// Отчёт пишется одной транзакцией: сбой на середине не оставляет полуснимка.
func TestIntegration_RecordRuntime_Rollback(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"))
	if err := st.RecordRuntime(ctx, telemtRep("pr1", RuntimeUser{Key: "a_1", Connections: 1})); err != nil {
		t.Fatal(err)
	}
	// Повтор ключа (сервер такое отсекает 400, здесь - в обход) падает на PK.
	bad := telemtRep("pr1", RuntimeUser{Key: "b_2", Connections: 1}, RuntimeUser{Key: "b_2", Connections: 2})
	bad.Telemt.Version = "9.9.9"
	if err := st.RecordRuntime(ctx, bad); err == nil {
		t.Fatal("ждали ошибку на повторе ключа")
	}
	if got := userKeys(t, st, "pr1"); len(got) != 1 || got["telemt:a_1"] != 1 {
		t.Fatalf("после отката должен остаться прежний снимок: %v", got)
	}
	if mr, _ := st.MachineRuntime(ctx, "pr1"); mr.Telemt.Version != "3.5.17" {
		t.Fatalf("снимок машины не должен обновиться: %+v", mr.Telemt)
	}
}

func TestIntegration_MachineRuntime_UnknownAndEmpty(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, Target{Code: "vega", Kind: "exit", Enabled: true})
	if _, err := st.MachineRuntime(ctx, "nobody"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("ждали ErrUnknownTarget, получили %v", err)
	}
	mr, err := st.MachineRuntime(ctx, "vega")
	if err != nil || mr.SeenAt != nil || mr.Host != nil || mr.Telemt != nil || mr.Mtg != nil {
		t.Fatalf("машина без отчёта: %+v %v", mr, err)
	}
}

// Подключения: фильтры, порядок machine/product/key, total с фильтром, пагинация; сводка.
func TestIntegration_Connections(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"), telemtTarget("ps5", "exit"))
	if err := st.RecordRuntime(ctx, telemtRep("pr1",
		RuntimeUser{Key: "b_2", Connections: 1}, RuntimeUser{Key: "a_1", Connections: 3, ActiveIPs: []string{"1.1.1.1"}})); err != nil {
		t.Fatal(err)
	}
	rep := RuntimeReport{Target: "ps5", Mtg: &RuntimeMtg{APIOK: true, ConnectionsTotal: 7,
		Users: []RuntimeUser{{Key: "m_1", Connections: 5, BytesIn: 100}, {Key: "m_2", Connections: 0, ActiveIPs: []string{"3.3.3.3"}}}}}
	if err := st.RecordRuntime(ctx, rep); err != nil {
		t.Fatal(err)
	}

	all, total, err := st.Connections(ctx, ConnFilter{}, 100, 0)
	if err != nil || total != 4 || len(all) != 4 {
		t.Fatalf("всё: total=%d %+v %v", total, all, err)
	}
	if all[0].Key != "a_1" || all[1].Key != "b_2" || all[2].Machine != "ps5" || all[2].Key != "m_1" ||
		all[2].BytesIn != 100 || all[0].ActiveIPs[0] != "1.1.1.1" || all[1].ActiveIPs == nil {
		t.Fatalf("порядок и поля: %+v", all)
	}
	if rows, total, _ := st.Connections(ctx, ConnFilter{Product: ProductMtg, MinConnections: 1}, 100, 0); total != 1 || rows[0].Key != "m_1" {
		t.Fatalf("mtg с подключениями: total=%d %+v", total, rows)
	}
	if rows, total, _ := st.Connections(ctx, ConnFilter{Machine: "pr1"}, 1, 1); total != 2 || len(rows) != 1 || rows[0].Key != "b_2" {
		t.Fatalf("вторая страница pr1: total=%d %+v", total, rows)
	}

	sum, total, err := st.ConnectionsSummary(ctx, 100, 0)
	if err != nil || total != 2 || len(sum) != 2 {
		t.Fatalf("сводка: total=%d %+v %v", total, sum, err)
	}
	if sum[0].Machine != "pr1" || sum[0].Product != "telemt" || sum[0].ConnectionsTotal != 9 || sum[0].UsersOnline != 2 ||
		sum[1].Machine != "ps5" || sum[1].Product != "mtg" || sum[1].ConnectionsTotal != 7 || sum[1].UsersOnline != 1 {
		t.Fatalf("сводка: %+v", sum)
	}
	if page, total, _ := st.ConnectionsSummary(ctx, 1, 1); total != 2 || len(page) != 1 || page[0].Machine != "ps5" {
		t.Fatalf("вторая страница сводки: %+v", page)
	}
}

// Ошибки журнала: сумма за окно, порядок count desc, чистка старше 24 ч (задачей мастера).
func TestIntegration_LogErrors(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"), telemtTarget("pr2", "relay"))
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO runtime_log_errors (target, user_key, kind, count, period_start, period_end)
		 VALUES ('pr1', 'old_1', 'limit', 100, $1, $2)`, now.Add(-26*time.Hour), now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	report := func(target string, start time.Time, items ...LogErrorItem) {
		t.Helper()
		rep := telemtRep(target)
		rep.LogErrors = &RuntimeLogErrors{PeriodStart: start, PeriodEnd: start.Add(time.Minute), Items: items}
		if err := st.RecordRuntime(ctx, rep); err != nil {
			t.Fatal(err)
		}
	}
	report("pr1", now.Add(-3*time.Minute), LogErrorItem{Key: "a_1", Kind: "limit", Count: 4}, LogErrorItem{Key: "", Kind: "handshake", Count: 2})
	report("pr1", now.Add(-2*time.Minute), LogErrorItem{Key: "a_1", Kind: "limit", Count: 3})
	report("pr2", now.Add(-2*time.Minute), LogErrorItem{Key: "b_2", Kind: "auth", Count: 5})

	if _, err := st.PruneLogErrors(ctx, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var old int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM runtime_log_errors WHERE user_key = 'old_1'`).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if old != 0 {
		t.Fatal("счётчики старше 24 ч должны чиститься")
	}

	rows, total, err := st.LogErrors(ctx, now.Add(-time.Hour), "", 100, 0)
	if err != nil || total != 3 {
		t.Fatalf("total=%d %v", total, err)
	}
	want := []LogErrorSum{{"pr1", "a_1", "limit", 7}, {"pr2", "b_2", "auth", 5}, {"pr1", "", "handshake", 2}}
	for i, w := range want {
		if rows[i] != w {
			t.Fatalf("строка %d: ждали %+v, получили %+v", i, w, rows[i])
		}
	}
	// Окно позже первого периода и фильтр машины.
	if rows, total, _ := st.LogErrors(ctx, now.Add(-90*time.Second), "pr1", 100, 0); total != 1 || rows[0].Count != 3 {
		t.Fatalf("окно и машина: total=%d %+v", total, rows)
	}
	if rows, total, _ := st.LogErrors(ctx, now.Add(-time.Hour), "", 1, 2); total != 3 || len(rows) != 1 || rows[0].Kind != "handshake" {
		t.Fatalf("третья страница: %+v", rows)
	}
}

// Список машин: фильтры с total, версии из runtime, sha256 и желаемая ревизия ресурса.
func TestIntegration_MachinesFiltersAndRuntime(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, `TRUNCATE agent_state`); err != nil {
		t.Fatal(err)
	}
	seedTargets(t, st,
		telemtTarget("pr1", "relay"),
		Target{Code: "pr2", Kind: "relay", Resources: []string{ResourceTelemtUsers}, Enabled: false},
		Target{Code: "ps5", Kind: "exit", Resources: []string{ResourceMtgConfig}, Enabled: true},
		Target{Code: "vega", Kind: "exit", Enabled: true},
	)
	pub, err := st.PublishResource(ctx, "ps5", ResourceMtgConfig, []byte("[secrets]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObserved(ctx, ObservedReport{Target: "ps5", AgentVersion: "v1", Resources: []ObservedResource{
		{Resource: ResourceMtgConfig, Revision: pub.Revision - 1, SHA256: "old", Status: "applied"},
		{Resource: ResourceTelemtUsers, Revision: 0, SHA256: "x", Status: "compared"},
	}}); err != nil {
		t.Fatal(err)
	}
	rep := RuntimeReport{Target: "ps5", Mtg: &RuntimeMtg{Version: "2510861", APIOK: true}}
	if err := st.RecordRuntime(ctx, rep); err != nil {
		t.Fatal(err)
	}

	tr, fa := true, false
	cases := []struct {
		f     MachineFilter
		total int
		first string
	}{
		{MachineFilter{Resource: ResourceTelemtUsers}, 2, "pr1"},
		{MachineFilter{Resource: ResourceTelemtUsers, Enabled: &tr}, 1, "pr1"},
		{MachineFilter{Enabled: &fa}, 1, "pr2"},
		{MachineFilter{Kind: "exit"}, 2, "ps5"},
		{MachineFilter{Resource: ResourceXuiClients}, 0, ""},
	}
	for _, c := range cases {
		list, total, err := st.Machines(ctx, c.f, 100, 0)
		if err != nil || total != c.total || len(list) != c.total || (c.total > 0 && list[0].Code != c.first) {
			t.Fatalf("%+v: total=%d %+v %v", c.f, total, list, err)
		}
	}
	list, _, err := st.Machines(ctx, MachineFilter{Kind: "exit"}, 1, 0)
	if err != nil || len(list) != 1 {
		t.Fatal(err)
	}
	m := list[0]
	if m.MtgVersion != "2510861" || m.TelemtVersion != "" || m.RuntimeSeenAt == nil || len(m.Observed) != 2 {
		t.Fatalf("ps5: %+v", m)
	}
	byRes := map[string]MachineResource{}
	for _, o := range m.Observed {
		byRes[o.Resource] = o
	}
	if o := byRes[ResourceMtgConfig]; o.SHA256 != "old" || o.DesiredRevision == nil || *o.DesiredRevision != pub.Revision {
		t.Fatalf("mtg.config: ждали sha256 и желаемую ревизию %d, получили %+v", pub.Revision, o)
	}
	if o := byRes[ResourceTelemtUsers]; o.DesiredRevision != nil {
		t.Fatalf("ресурс без публикации → desired_revision null, получили %v", *o.DesiredRevision)
	}
	if page, total, _ := st.Machines(ctx, MachineFilter{Kind: "exit"}, 1, 1); total != 2 || page[0].Code != "vega" || page[0].RuntimeSeenAt != nil {
		t.Fatalf("вторая страница: %+v", page)
	}
}

func logSum(t *testing.T, st *Store, target string) int64 {
	t.Helper()
	var n int64
	if err := st.pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(count), 0) FROM runtime_log_errors WHERE target = $1`, target).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Повтор того же отчёта (агент не получил ответ и прислал снова, мост переслал
// дважды) не удваивает счётчики журнала.
func TestIntegration_LogErrors_RepeatDoesNotDouble(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"))
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	rep := telemtRep("pr1")
	rep.LogErrors = &RuntimeLogErrors{PeriodStart: start, PeriodEnd: start.Add(time.Minute),
		Items: []LogErrorItem{{Key: "a_1", Kind: "limit", Count: 4}, {Key: "", Kind: "handshake", Count: 2}}}
	for i := 0; i < 3; i++ {
		if err := st.RecordRuntime(ctx, rep); err != nil {
			t.Fatal(err)
		}
	}
	if n := logSum(t, st, "pr1"); n != 6 {
		t.Fatalf("трижды один отчёт: сумма должна быть 6, получили %d", n)
	}
	rows, total, err := st.LogErrors(ctx, start.Add(-time.Hour), "pr1", 100, 0)
	if err != nil || total != 2 || rows[0].Count != 4 {
		t.Fatalf("суммы: total=%d %+v %v", total, rows, err)
	}
	// Следующий период - дописывается.
	rep.LogErrors = &RuntimeLogErrors{PeriodStart: start.Add(time.Minute), PeriodEnd: start.Add(2 * time.Minute),
		Items: []LogErrorItem{{Key: "a_1", Kind: "limit", Count: 1}}}
	if err := st.RecordRuntime(ctx, rep); err != nil {
		t.Fatal(err)
	}
	if n := logSum(t, st, "pr1"); n != 7 {
		t.Fatalf("новый период должен добавиться: ждали 7, получили %d", n)
	}
}

// Чистка старше суток - отдельный вызов (задача мастера), отчёт её не делает.
func TestIntegration_PruneLogErrors(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"))
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO runtime_log_errors (target, user_key, kind, count, period_start, period_end)
		 VALUES ('pr1', 'old_1', 'limit', 100, $1, $2), ('pr1', 'new_1', 'limit', 1, $3, $4)`,
		now.Add(-26*time.Hour), now.Add(-25*time.Hour), now.Add(-2*time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRuntime(ctx, telemtRep("pr1")); err != nil {
		t.Fatal(err)
	}
	if n := logSum(t, st, "pr1"); n != 101 {
		t.Fatalf("отчёт не должен чистить счётчики: ждали 101, получили %d", n)
	}
	n, err := st.PruneLogErrors(ctx, now.Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("ждали удаление одной старой строки: %d %v", n, err)
	}
	if n := logSum(t, st, "pr1"); n != 1 {
		t.Fatalf("после чистки ждали 1, получили %d", n)
	}
}

// API продукта не ответил (api_ok=false): его строки подключений не стираются
// (станут stale по времени), строки другого продукта обновляются как обычно,
// в сводке строка продукта есть - с api_ok=false и причиной.
func TestIntegration_RecordRuntime_APIDownKeepsProductRows(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"))
	ok := telemtRep("pr1", RuntimeUser{Key: "a_1", Connections: 2}, RuntimeUser{Key: "b_2", Connections: 3})
	ok.Mtg = &RuntimeMtg{APIOK: true, ConnectionsTotal: 1, Users: []RuntimeUser{{Key: "m_1", Connections: 1}}}
	if err := st.RecordRuntime(ctx, ok); err != nil {
		t.Fatal(err)
	}
	var firstSeen time.Time
	if err := st.pool.QueryRow(ctx, `SELECT seen_at FROM runtime_user_conns WHERE target='pr1' AND user_key='a_1'`).Scan(&firstSeen); err != nil {
		t.Fatal(err)
	}

	down := RuntimeReport{Target: "pr1", Host: &RuntimeHost{CPUPct: 1},
		Telemt: &RuntimeTelemt{Version: "3.5.17", APIOK: false, Error: "/v1/users: HTTP 502"},
		Mtg:    &RuntimeMtg{APIOK: true, ConnectionsTotal: 4, Users: []RuntimeUser{{Key: "m_2", Connections: 4}}}}
	if err := st.RecordRuntime(ctx, down); err != nil {
		t.Fatal(err)
	}
	got := userKeys(t, st, "pr1")
	if len(got) != 3 || got["telemt:a_1"] != 2 || got["telemt:b_2"] != 3 || got["mtg:m_2"] != 4 {
		t.Fatalf("telemt без API - строки прежние, mtg - заменены: %v", got)
	}
	var seen time.Time
	if err := st.pool.QueryRow(ctx, `SELECT seen_at FROM runtime_user_conns WHERE target='pr1' AND user_key='a_1'`).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if !seen.Equal(firstSeen) {
		t.Fatalf("строки продукта без API не должны освежаться (иначе не станут stale): было %v, стало %v", firstSeen, seen)
	}

	sum, total, err := st.ConnectionsSummary(ctx, 100, 0)
	if err != nil || total != 2 {
		t.Fatalf("сводка: total=%d %+v %v", total, sum, err)
	}
	var tm, mg ConnSummary
	for _, r := range sum {
		if r.Product == ProductTelemt {
			tm = r
		} else {
			mg = r
		}
	}
	if tm.Machine != "pr1" || tm.APIOK || tm.Error != "/v1/users: HTTP 502" || tm.UsersOnline != 2 || tm.ConnectionsTotal != 5 {
		t.Fatalf("telemt без API: строка есть, api_ok=false, error, данные последнего удачного: %+v", tm)
	}
	if !mg.APIOK || mg.Error != "" || mg.ConnectionsTotal != 4 || mg.UsersOnline != 1 {
		t.Fatalf("mtg: %+v", mg)
	}

	// API вернулся - строки telemt заменяются.
	if err := st.RecordRuntime(ctx, telemtRep("pr1", RuntimeUser{Key: "c_3", Connections: 1})); err != nil {
		t.Fatal(err)
	}
	if got := userKeys(t, st, "pr1"); len(got) != 1 || got["telemt:c_3"] != 1 {
		t.Fatalf("после восстановления API (и без mtg в отчёте): %v", got)
	}
}

// Мастер на старте проверяет таблицы миграции 0007 в своей схеме.
func TestIntegration_CheckRuntimeSchema(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	if err := st.CheckRuntimeSchema(ctx); err != nil {
		t.Fatalf("на полной схеме проверка должна пройти: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS rtcheck_empty`); err != nil {
		t.Fatal(err)
	}
	dsn := mustDSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	// Схема без таблиц 0007: таблицы в public не должны засчитываться.
	empty, err := Open(ctx, dsn+sep+"search_path=rtcheck_empty")
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	err = empty.CheckRuntimeSchema(ctx)
	if err == nil || !strings.Contains(err.Error(), "0007") || !strings.Contains(err.Error(), "agent_runtime") {
		t.Fatalf("ждали понятную ошибку про миграцию 0007, получили %v", err)
	}
}
