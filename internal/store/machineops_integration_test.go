//go:build integration

package store

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"
)

// setupOps - все миграции, чистые таблицы и машины: релеи pr1, pr2 (с telemt.users),
// выходы ps1 (у pr1), ps2 (у pr2).
func setupOps(t *testing.T) *Store {
	t.Helper()
	st := setupAll(t)
	seedTargets(t, st,
		telemtTarget("pr1", "relay"), telemtTarget("pr2", "relay"),
		Target{Code: "ps1", Kind: "exit", RelayCode: "pr1", Enabled: true, Resources: []string{ResourceTelemtUsers}},
		Target{Code: "ps2", Kind: "exit", RelayCode: "pr2", Enabled: true, Resources: []string{ResourceTelemtUsers}})
	return st
}

func setSeen(t *testing.T, st *Store, code string, ago time.Duration) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO agent_state (target, seen_at) VALUES ($1, now() - make_interval(secs => $2))
		 ON CONFLICT (target) DO UPDATE SET seen_at = EXCLUDED.seen_at`, code, ago.Seconds()); err != nil {
		t.Fatal(err)
	}
}

func setFailed(t *testing.T, st *Store, code, resource string, rev int64) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO observed_state (target, resource, revision, status, error, seen_at)
		 VALUES ($1, $2, $3, 'failed', 'toml: bad key', now())
		 ON CONFLICT (target, resource) DO UPDATE SET revision = EXCLUDED.revision, status = 'failed', seen_at = now()`,
		code, resource, rev); err != nil {
		t.Fatal(err)
	}
}

func eventKinds(evs []MachineEvent) []string {
	out := []string{}
	for _, e := range evs {
		out = append(out, e.Machine+":"+e.Kind)
	}
	return out
}

// Миграция 0012 идемпотентна; схема видна проверке.
func TestIntegration_MachineOps_Schema(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	setupAll(t).Close()
	if err := st.CheckMachineOpsSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Runtime-отчёт → строка истории (минута, upsert), агрегация по шагу, чистка.
func TestIntegration_MachineOps_Metrics(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	rep := RuntimeReport{Target: "ps1", AgentVersion: "v1",
		Host: &RuntimeHost{CPUPct: 12.5, MemPct: 40, DiskPct: 55, Load1: 0.5, NetRxMbps: 10.2, NetTxMbps: 3},
		Telemt: &RuntimeTelemt{APIOK: true, Users: []RuntimeUser{
			{Key: "a_1", Connections: 3}, {Key: "b_2", Connections: 2}, {Key: "c_3", Connections: 0}}}}
	if err := st.RecordRuntime(ctx, rep); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMetrics(ctx, "ps1"); err != nil {
		t.Fatal(err)
	}
	// Повтор в ту же минуту - та же строка.
	if err := st.RecordMetrics(ctx, "ps1"); err != nil {
		t.Fatal(err)
	}
	var n int
	var at time.Time
	var cpu, rx float64
	var conns int64
	var online int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) OVER (), at, cpu, rx_bps, connections, users_online FROM machine_metrics WHERE code = 'ps1'`).
		Scan(&n, &at, &cpu, &rx, &conns, &online); err != nil {
		t.Fatal(err)
	}
	if n != 1 || at.Second() != 0 || at.Nanosecond() != 0 || cpu != 12.5 || math.Abs(rx-10.2e6) > 1 || conns != 5 || online != 2 {
		t.Fatalf("строка истории: n=%d at=%v cpu=%v rx=%v conns=%d online=%d", n, at, cpu, rx, conns, online)
	}
	// Машина без отчёта - ничего не пишется.
	if err := st.RecordMetrics(ctx, "ps2"); err != nil {
		t.Fatal(err)
	}
	if countRows(t, st, "machine_metrics") != 1 {
		t.Fatal("строка без runtime-снимка")
	}

	// Агрегация: 10 минут трое суток назад, cpu = номер минуты; шаг 5m → 2 точки (2 и 7).
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 10; i++ {
		var host any = i
		if i == 9 {
			host = nil // минута без host: в среднее не входит
		}
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO machine_metrics (code, at, cpu, connections, users_online) VALUES ('ps2', $1, $2, $3, 1)`,
			base.Add(time.Duration(i)*time.Minute), host, i*10); err != nil {
			t.Fatal(err)
		}
	}
	pts, err := st.MachineMetrics(ctx, "ps2", base, base.Add(10*time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || !pts[0].At.Equal(base) || !pts[1].At.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("точки: %+v", pts)
	}
	if pts[0].CPU == nil || *pts[0].CPU != 2 || pts[1].CPU == nil || *pts[1].CPU != 6.5 || pts[0].Connections != 20 || pts[1].UsersOnline != 1 {
		t.Fatalf("средние: %+v %+v", pts[0], pts[1])
	}
	if pts[0].Mem != nil {
		t.Fatal("mem без данных должен быть null")
	}
	// Окно [from, to): минуты 3..5 с шагом 5m - границы шагов от начала отсчёта, не от from.
	pts, err = st.MachineMetrics(ctx, "ps2", base.Add(3*time.Minute), base.Add(6*time.Minute), 5*time.Minute)
	if err != nil || len(pts) != 2 || !pts[0].At.Equal(base) || *pts[0].CPU != 3.5 || *pts[1].CPU != 5 {
		t.Fatalf("окно внутри шагов: %+v %v", pts, err)
	}
	if _, err := st.MachineMetrics(ctx, "nobody", base, base.Add(time.Hour), time.Minute); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("нет машины: %v", err)
	}

	// Чистка: строки ps2 (трое суток назад) старше границы уходят, свежая ps1 - нет.
	m, _, err := st.PruneMachineOps(ctx, base.Add(time.Hour), time.Now().Add(-30*24*time.Hour))
	if err != nil || m != 10 || countRows(t, st, "machine_metrics") != 1 {
		t.Fatalf("чистка: удалено %d, осталось %d, %v", m, countRows(t, st, "machine_metrics"), err)
	}
}

// Детектор: одно событие на переход down/up, apply_failed - одно на ревизию,
// выключенные и машины без агента не участвуют.
func TestIntegration_MachineOps_Detector(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st,
		Target{Code: "ps3", Kind: "exit", RelayCode: "pr1", Enabled: false, Resources: []string{}},
		Target{Code: "ps4", Kind: "exit", RelayCode: "pr1", Enabled: true, Resources: []string{}})
	setSeen(t, st, "pr1", 10*time.Second)
	setSeen(t, st, "ps1", 10*time.Second)
	setSeen(t, st, "ps2", 20*time.Minute) // молчит
	setSeen(t, st, "ps3", 20*time.Minute) // молчит, но выключена
	// pr2 и ps4 - агента нет: не молчат.

	detect := func() []string {
		t.Helper()
		evs, err := st.DetectMachineEvents(ctx, SilentAfter)
		if err != nil {
			t.Fatal(err)
		}
		return eventKinds(evs)
	}
	if got := detect(); !slices.Equal(got, []string{"ps2:down"}) {
		t.Fatalf("первый проход: %v", got)
	}
	if got := detect(); len(got) != 0 {
		t.Fatalf("повторный проход без перехода: %v", got)
	}
	setSeen(t, st, "ps2", 0)
	if got := detect(); !slices.Equal(got, []string{"ps2:up"}) {
		t.Fatalf("вернулась: %v", got)
	}
	if got := detect(); len(got) != 0 {
		t.Fatalf("после up без перехода: %v", got)
	}
	// Свежий runtime-отчёт тоже считается связью (берётся позднейший отчёт).
	setSeen(t, st, "ps1", 30*time.Minute)
	if err := st.RecordRuntime(ctx, RuntimeReport{Target: "ps1"}); err != nil {
		t.Fatal(err)
	}
	if got := detect(); len(got) != 0 {
		t.Fatalf("runtime свежий - не down: %v", got)
	}

	// apply_failed: одно событие на ревизию; новая ревизия - новое событие.
	setFailed(t, st, "ps1", ResourceTelemtUsers, 5)
	setFailed(t, st, "ps3", ResourceTelemtUsers, 5) // выключена
	if got := detect(); !slices.Equal(got, []string{"ps1:apply_failed"}) {
		t.Fatalf("провал применения: %v", got)
	}
	if got := detect(); len(got) != 0 {
		t.Fatalf("тот же провал повторно: %v", got)
	}
	setFailed(t, st, "ps1", ResourceTelemtUsers, 6)
	if got := detect(); !slices.Equal(got, []string{"ps1:apply_failed"}) {
		t.Fatalf("провал новой ревизии: %v", got)
	}
	evs, total, err := st.MachineEvents(ctx, 0, 100)
	if err != nil || total != 4 || len(evs) != 4 {
		t.Fatalf("лента: %d/%d %v", len(evs), total, err)
	}
	if evs[2].Detail != "telemt.users, ревизия 5: toml: bad key" || evs[0].Detail == "" {
		t.Fatalf("тексты событий: %+v", evs)
	}
	page, total, err := st.MachineEvents(ctx, evs[1].ID, 1)
	if err != nil || total != 2 || len(page) != 1 || page[0].ID != evs[2].ID {
		t.Fatalf("курсор: %+v total=%d %v", page, total, err)
	}

	// Выключение сбрасывает состояние: выключенная молчащая - без событий; после
	// включения она впервые увидена молчащей - down.
	setSeen(t, st, "ps2", 20*time.Minute)
	off, on := false, true
	if _, err := st.UpdateTarget(ctx, "ps2", TargetPatch{Enabled: &off}, nil); err != nil {
		t.Fatal(err)
	}
	if got := detect(); len(got) != 0 {
		t.Fatalf("выключенная машина в тревогах: %v", got)
	}
	if _, err := st.UpdateTarget(ctx, "ps2", TargetPatch{Enabled: &on}, nil); err != nil {
		t.Fatal(err)
	}
	if got := detect(); !slices.Equal(got, []string{"ps2:down"}) {
		t.Fatalf("включили молчащую: %v", got)
	}

	// Чистка событий старше границы.
	if _, e, err := st.PruneMachineOps(ctx, time.Now().Add(-8*24*time.Hour), time.Now().Add(time.Minute)); err != nil || e != 5 {
		t.Fatalf("чистка событий: %d %v", e, err)
	}
}

// Удаление: включённую - нельзя, с сервером или выходами - machine_in_use, иначе
// машина уходит со всем своим, события остаются.
func TestIntegration_MachineOps_Delete(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	if err := st.DeleteTarget(ctx, "ps1"); !errors.Is(err, ErrMachineEnabled) {
		t.Fatalf("включённая: %v", err)
	}
	if err := st.DeleteTarget(ctx, "nobody"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("нет машины: %v", err)
	}
	off := false
	for _, c := range []string{"ps1", "pr1"} {
		if _, err := st.UpdateTarget(ctx, c, TargetPatch{Enabled: &off}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateServer(ctx, telemtServer(3, "pr2", "ps1")); err != nil {
		t.Fatal(err)
	}
	err := st.DeleteTarget(ctx, "ps1")
	var inUse *MachineInUseError
	if !errors.Is(err, ErrMachineInUse) || !errors.As(err, &inUse) || !slices.Equal(inUse.Servers, []int{3}) {
		t.Fatalf("на машине сервер: %v", err)
	}
	if err := st.DeleteTarget(ctx, "pr1"); !errors.As(err, &inUse) || !slices.Equal(inUse.Exits, []string{"ps1"}) {
		t.Fatalf("у релея выход: %v", err)
	}

	// Всё, что относится к машине.
	if _, err := st.IssueAgentToken(ctx, "ps1"); err != nil {
		t.Fatal(err)
	}
	setSeen(t, st, "ps1", 20*time.Minute)
	setFailed(t, st, "ps1", ResourceTelemtUsers, 1)
	if err := st.RecordRuntime(ctx, RuntimeReport{Target: "ps1", Host: &RuntimeHost{CPUPct: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMetrics(ctx, "ps1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO desired_config (target, resource, revision, sha256) VALUES ('ps1', 'telemt.users', 1, 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO apply_log (target, resource, status) VALUES ('ps1', 'telemt.users', 'failed')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO machine_events (machine, kind) VALUES ('ps1', 'down')`); err != nil {
		t.Fatal(err)
	}

	w := telemtServer(3, "pr2", "ps2")
	if _, err := st.ReplaceServer(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTarget(ctx, "ps1"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM agent_tokens WHERE target = 'ps1'`,
		`SELECT count(*) FROM desired_config WHERE target = 'ps1'`,
		`SELECT count(*) FROM observed_state WHERE target = 'ps1'`,
		`SELECT count(*) FROM apply_log WHERE target = 'ps1'`,
		`SELECT count(*) FROM agent_state WHERE target = 'ps1'`,
		`SELECT count(*) FROM agent_runtime WHERE target = 'ps1'`,
		`SELECT count(*) FROM machine_metrics WHERE code = 'ps1'`,
		`SELECT count(*) FROM targets WHERE code = 'ps1'`,
	} {
		var n int
		if err := st.pool.QueryRow(ctx, q).Scan(&n); err != nil || n != 0 {
			t.Fatalf("после удаления %s: %d %v", q, n, err)
		}
	}
	if countRows(t, st, "machine_events") != 1 {
		t.Fatal("события удалённой машины должны остаться")
	}
	// Релей без выходов удаляется.
	if err := st.DeleteTarget(ctx, "pr1"); err != nil {
		t.Fatal(err)
	}
}

// Выключенная машина не получает новых ревизий, но токен её агента действует;
// после включения она догоняет пользователей.
func TestIntegration_MachineOps_DisabledGetsNoRevision(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	tok, err := st.IssueAgentToken(ctx, "ps2")
	if err != nil {
		t.Fatal(err)
	}
	off, on := false, true
	if _, err := st.UpdateTarget(ctx, "ps2", TargetPatch{Enabled: &off}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileUsers(ctx, []ImportRow{{Username: "alice_1", Secret: sec1}}, true); err != nil {
		t.Fatal(err)
	}
	pub, err := st.PublishTelemtUsers(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pub.Changed, "ps2") || !slices.Contains(pub.Changed, "ps1") {
		t.Fatalf("публикация: %v", pub.Changed)
	}
	if rev, err := st.MaxRevision(ctx, "ps2"); err != nil || rev != 0 {
		t.Fatalf("у выключенной машины ревизия %d %v", rev, err)
	}
	if _, err := st.PublishResource(ctx, "ps2", ResourceTelemtConfig, []byte("x")); !errors.Is(err, ErrMachineDisabled) {
		t.Fatalf("ресурс выключенной машине: %v", err)
	}
	code, enabled, err := st.AgentTargetByToken(ctx, tok)
	if err != nil || code != "ps2" || enabled {
		t.Fatalf("токен выключенной: %q %v %v", code, enabled, err)
	}
	if _, err := st.TargetByAgentToken(ctx, tok); !errors.Is(err, ErrBadAgentToken) {
		t.Fatalf("TargetByAgentToken выключенной: %v", err)
	}
	if _, err := st.UpdateTarget(ctx, "ps2", TargetPatch{Enabled: &on}, nil); err != nil {
		t.Fatal(err)
	}
	pub, err = st.PublishTelemtUsers(ctx, 50)
	if err != nil || !slices.Equal(pub.Changed, []string{"ps2"}) {
		t.Fatalf("после включения: %v %v", pub.Changed, err)
	}
}

// Настройки: слияние, удаление ключа, отказ проверки ничего не меняет.
func TestIntegration_MachineOps_UpdateSettings(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	got, err := st.UpdateTarget(ctx, "pr1", TargetPatch{Settings: map[string]string{SettingWebDomains: "B.example.com, a.example.com"}}, nil)
	if err != nil || got.Settings[SettingWebDomains] != "b.example.com,a.example.com" {
		t.Fatalf("web_domains: %+v %v", got.Settings, err)
	}
	reject := func(string, map[string]string) error { return errors.New("нельзя") }
	if _, err := st.UpdateTarget(ctx, "pr1", TargetPatch{Settings: map[string]string{"x": "1"}}, reject); !errors.Is(err, ErrBadSettings) {
		t.Fatalf("отказ проверки: %v", err)
	}
	if _, err := st.UpdateTarget(ctx, "ps1", TargetPatch{Settings: map[string]string{SettingWebDomains: "a.example.com"}}, nil); !errors.Is(err, ErrBadSettings) {
		t.Fatalf("web_domains выходу: %v", err)
	}
	cur, _ := st.TargetByCode(ctx, "pr1")
	if len(cur.Settings) != 1 || !cur.Enabled {
		t.Fatalf("после отказа: %+v", cur)
	}
	got, err = st.UpdateTarget(ctx, "pr1", TargetPatch{Settings: map[string]string{SettingWebDomains: ""}}, nil)
	if err != nil || len(got.Settings) != 0 {
		t.Fatalf("удаление ключа: %+v %v", got.Settings, err)
	}
	if _, err := st.UpdateTarget(ctx, "nobody", TargetPatch{}, nil); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("нет машины: %v", err)
	}
}

// Точки входа для проверки связи: через релей - на релее, напрямую - на машине за
// рубежом; выключенные точки и серверы - нет.
func TestIntegration_MachineOps_CheckInfo(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	no, yes := false, true
	w := telemtServer(1, "pr1", "ps1")
	w.Endpoints = []EndpointWrite{
		{Protocol: "ee", Host: "pr1.example.net", TLSDomain: "www.example.com"},
		{Protocol: "dd", Host: "ps1.example.net", ViaRelay: &no},
		{Protocol: "web", Host: "web1.example.net"},
		{Protocol: "dd", Host: "off.example.net", Enabled: &no},
	}
	if _, err := st.CreateServer(ctx, w); err != nil {
		t.Fatal(err)
	}
	off := telemtServer(2, "pr1", "ps1")
	off.State = "disabled"
	off.Endpoints = []EndpointWrite{{Protocol: "dd", Host: "disabled.example.net", ViaRelay: &yes}}
	if _, err := st.CreateServer(ctx, off); err != nil {
		t.Fatal(err)
	}
	setSeen(t, st, "pr1", time.Minute)

	ct, eps, truncated, err := st.MachineCheckInfo(ctx, "pr1")
	if err != nil || truncated || ct.Kind != "relay" || !ct.Enabled || ct.SeenAt == nil {
		t.Fatalf("релей: %+v %v", ct, err)
	}
	var hosts []string
	for _, e := range eps {
		hosts = append(hosts, e.Host)
	}
	if !slices.Equal(hosts, []string{"pr1.example.net", "web1.example.net"}) {
		t.Fatalf("точки релея: %v", hosts)
	}
	_, eps, _, err = st.MachineCheckInfo(ctx, "ps1")
	if err != nil || len(eps) != 1 || eps[0].Host != "ps1.example.net" || eps[0].ViaRelay || eps[0].ServerNo != 1 {
		t.Fatalf("точки выхода: %+v %v", eps, err)
	}
	ct, eps, _, err = st.MachineCheckInfo(ctx, "ps2")
	if err != nil || len(eps) != 0 || ct.SeenAt != nil {
		t.Fatalf("машина без серверов и агента: %+v %+v %v", ct, eps, err)
	}
	if _, _, _, err := st.MachineCheckInfo(ctx, "nobody"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("нет машины: %v", err)
	}
}
