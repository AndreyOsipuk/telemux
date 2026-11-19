//go:build integration

package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndreyOsipuk/telemux/internal/servercfg"
)

// setupServers - все миграции, чистые таблицы и машины: релеи pr1, pr2, выходы ps1, ps2.
func setupServers(t *testing.T) *Store {
	t.Helper()
	st := setupAll(t)
	seedTargets(t, st,
		Target{Code: "pr1", Kind: "relay", Enabled: true, Resources: []string{}},
		Target{Code: "pr2", Kind: "relay", Enabled: true, Resources: []string{}},
		Target{Code: "ps1", Kind: "exit", RelayCode: "pr1", Enabled: true, Resources: []string{}},
		Target{Code: "ps2", Kind: "exit", RelayCode: "pr2", Enabled: true, Resources: []string{}})
	return st
}

func sp(s string) *string { return &s }

func telemtServer(no int, relay, node string) ServerWrite {
	return ServerWrite{
		No: no, Name: "Финляндия", Emoji: "🇫🇮", Country: "fi", Engine: servercfg.EngineTelemt,
		RelayCode: sp(relay), NodeCode: sp(node), RelayOnly: true,
		TLSDomain: "www.example.com", ExtraTLSDomains: []string{"a.example.com", "B.example.com"},
		Settings: map[string]string{"port": "443", "ipv6": "true"},
		Endpoints: []EndpointWrite{
			{Protocol: "ee", Host: "pr1.example.net", TLSDomain: "www.example.com"},
			{Protocol: "dd", Host: "pr1.example.net"},
			{Protocol: "web", Host: "web1.example.net"},
		},
	}
}

func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Миграция 0010 идемпотентна; схема видна проверке.
func TestIntegration_Servers_Schema(t *testing.T) {
	st := setupServers(t)
	defer st.Close()
	setupAll(t).Close()
	if err := st.CheckServersSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Создание, чтение, замена, удаление; значения по умолчанию точек входа и нормализация.
func TestIntegration_Servers_CRUD(t *testing.T) {
	st := setupServers(t)
	defer st.Close()
	ctx := context.Background()

	created, err := st.CreateServer(ctx, telemtServer(1, "pr1", "ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "active" || created.Settings["ipv6"] != "1" || len(created.Endpoints) != 3 {
		t.Fatalf("созданный сервер: %+v", created)
	}
	ep := created.Endpoints[0]
	if ep.Port != 443 || !ep.ViaRelay || !ep.Enabled || ep.SortOrder != 100 {
		t.Fatalf("значения по умолчанию точки входа: %+v", ep)
	}
	if created.ExtraTLSDomains[1] != "b.example.com" {
		t.Fatalf("домены не в нижнем регистре: %v", created.ExtraTLSDomains)
	}
	if created.Live == nil || created.Live.Relay == nil || created.Live.Relay.Code != "pr1" || created.Live.Relay.SeenAt != nil {
		t.Fatalf("живые цифры без агента: %+v", created.Live)
	}

	if _, err := st.CreateServer(ctx, telemtServer(1, "pr1", "ps1")); !errors.Is(err, ErrServerExists) {
		t.Fatalf("номер занят: ждали ErrServerExists, получили %v", err)
	}

	w := telemtServer(1, "pr2", "ps2")
	w.Name = "Нидерланды"
	w.State = "maintenance"
	w.Endpoints = w.Endpoints[:1]
	replaced, err := st.ReplaceServer(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Name != "Нидерланды" || *replaced.RelayCode != "pr2" || len(replaced.Endpoints) != 1 || replaced.State != "maintenance" {
		t.Fatalf("замена: %+v", replaced)
	}
	got, err := st.GetServer(ctx, 1)
	if err != nil || len(got.Endpoints) != 1 || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("чтение после замены: %+v %v", got, err)
	}

	if _, err := st.GetServer(ctx, 42); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("нет сервера: %v", err)
	}
	if _, err := st.ReplaceServer(ctx, telemtServer(42, "pr1", "ps1")); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("замена несуществующего: %v", err)
	}

	// Удаление: активный - нельзя, выключенный - можно, точки входа уходят каскадом.
	if err := st.DeleteServer(ctx, 1); !errors.Is(err, ErrServerNotDisabled) {
		t.Fatalf("удаление невыключенного: ждали ErrServerNotDisabled, получили %v", err)
	}
	if countRows(t, st, "servers") != 1 {
		t.Fatal("сервер пропал после отказа в удалении")
	}
	w.State = "disabled"
	if _, err := st.ReplaceServer(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServer(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if countRows(t, st, "servers") != 0 || countRows(t, st, "server_endpoints") != 0 {
		t.Fatal("после удаления остались строки")
	}
	if err := st.DeleteServer(ctx, 1); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("повторное удаление: %v", err)
	}
}

// Движок после создания не меняется - ни заменой, ни импортом.
func TestIntegration_Servers_EngineImmutable(t *testing.T) {
	st := setupServers(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateServer(ctx, telemtServer(1, "pr1", "ps1")); err != nil {
		t.Fatal(err)
	}
	w := telemtServer(1, "pr1", "ps1")
	w.Engine = servercfg.EngineMtg
	w.Settings = map[string]string{"BIND_PORT": "8767"}
	if _, err := st.ReplaceServer(ctx, w); !errors.Is(err, ErrEngineImmutable) {
		t.Fatalf("замена: ждали ErrEngineImmutable, получили %v", err)
	}
	if _, _, err := st.ImportServers(ctx, []ServerWrite{w}); !errors.Is(err, ErrEngineImmutable) {
		t.Fatalf("импорт: ждали ErrEngineImmutable, получили %v", err)
	}
	got, _ := st.GetServer(ctx, 1)
	if got.Engine != servercfg.EngineTelemt {
		t.Fatalf("движок изменился: %s", got.Engine)
	}
}

// Неизвестная машина или машина не того вида - ошибка до записи, строк нет.
func TestIntegration_Servers_UnknownMachine(t *testing.T) {
	st := setupServers(t)
	defer st.Close()
	ctx := context.Background()
	for name, w := range map[string]ServerWrite{
		"нет релея":         telemtServer(1, "pr9", "ps1"),
		"релей - это выход": telemtServer(1, "ps1", "ps2"),
		"нет выхода":        telemtServer(1, "pr1", "ps9"),
		"выход - это релей": telemtServer(1, "pr1", "pr2"),
	} {
		if _, err := st.CreateServer(ctx, w); !errors.Is(err, ErrUnknownMachine) {
			t.Fatalf("%s: ждали ErrUnknownMachine, получили %v", name, err)
		}
	}
	if countRows(t, st, "servers") != 0 || countRows(t, st, "server_endpoints") != 0 {
		t.Fatal("после отказа появились строки")
	}
	// Машины необязательны: сервер без машин сохраняется.
	w := telemtServer(2, "", "")
	w.RelayCode, w.NodeCode = nil, sp(" ")
	srv, err := st.CreateServer(ctx, w)
	if err != nil || srv.RelayCode != nil || srv.NodeCode != nil || srv.Live.Relay != nil {
		t.Fatalf("сервер без машин: %+v %v", srv, err)
	}
}

// Машину, на которую ссылается сервер, не удалить (FK RESTRICT).
func TestIntegration_Servers_MachineRestrict(t *testing.T) {
	st := setupServers(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateServer(ctx, telemtServer(1, "pr1", "ps1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM targets WHERE code = 'ps1'`); err == nil {
		t.Fatal("машина сервера удалилась")
	}
}

// Импорт: upsert по номеру, N-я запись плохая - 0 изменений, лимит батча.
func TestIntegration_Servers_Import(t *testing.T) {
	st := setupServers(t)
	defer st.Close()
	ctx := context.Background()

	mtg := ServerWrite{No: 5, Name: "Польша", Engine: servercfg.EngineMtg, RelayCode: sp("pr1"), NodeCode: sp("ps1"),
		Settings:  map[string]string{"RELAY_BIND": "0.0.0.0:2095,0.0.0.0:2096", "RELAY_HOT_RELOAD": "1", "RELAY_MODE": "local-mtg"},
		Endpoints: []EndpointWrite{{Protocol: "dd", Host: "pr1.example.net", Port: ip(2095)}}}
	ins, upd, err := st.ImportServers(ctx, []ServerWrite{telemtServer(1, "pr1", "ps1"), mtg})
	if err != nil || ins != 2 || upd != 0 {
		t.Fatalf("первый импорт: %d %d %v", ins, upd, err)
	}
	// Повтор идемпотентен: всё - обновления, точки входа не удваиваются.
	ins, upd, err = st.ImportServers(ctx, []ServerWrite{telemtServer(1, "pr1", "ps1"), mtg})
	if err != nil || ins != 0 || upd != 2 {
		t.Fatalf("повторный импорт: %d %d %v", ins, upd, err)
	}
	if n := countRows(t, st, "server_endpoints"); n != 4 {
		t.Fatalf("точек входа %d, ждали 4", n)
	}

	// Третья запись ссылается на несуществующую машину - не меняется ничего.
	changed := telemtServer(1, "pr2", "ps2")
	changed.Name = "Изменено"
	bad := telemtServer(3, "pr9", "ps1")
	if _, _, err := st.ImportServers(ctx, []ServerWrite{changed, telemtServer(2, "pr1", "ps1"), bad}); !errors.Is(err, ErrUnknownMachine) {
		t.Fatalf("плохая запись: ждали ErrUnknownMachine, получили %v", err)
	}
	// Плохая точка входа в последней записи - ошибка до транзакции.
	badEp := telemtServer(4, "pr1", "ps1")
	badEp.Endpoints = append(badEp.Endpoints, EndpointWrite{Protocol: "ee", Host: "x.example.net"})
	if _, _, err := st.ImportServers(ctx, []ServerWrite{changed, badEp}); !errors.Is(err, ErrBadEndpoint) {
		t.Fatalf("ee без tls_domain: ждали ErrBadEndpoint, получили %v", err)
	}
	if countRows(t, st, "servers") != 2 {
		t.Fatal("после отказа импорта число серверов изменилось")
	}
	if got, _ := st.GetServer(ctx, 1); got.Name != "Финляндия" || *got.RelayCode != "pr1" {
		t.Fatalf("после отказа импорта сервер изменился: %+v", got)
	}

	// Повтор номера в одном импорте и лимит батча.
	if _, _, err := st.ImportServers(ctx, []ServerWrite{telemtServer(7, "pr1", "ps1"), telemtServer(7, "pr1", "ps1")}); !errors.Is(err, ErrBadServer) {
		t.Fatalf("повтор номера: %v", err)
	}
	many := make([]ServerWrite, MaxServersImport+1)
	for i := range many {
		many[i] = telemtServer(1, "pr1", "ps1")
	}
	if _, _, err := st.ImportServers(ctx, many); !errors.Is(err, ErrBatchTooLarge) {
		t.Fatalf("лимит батча: ждали ErrBatchTooLarge, получили %v", err)
	}
}

func ip(n int) *int { return &n }

// queryCounter - трассировщик pgx: считает запросы к базе.
type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Страница: фильтры, total, точки входа и живые цифры всех серверов - фиксированным
// числом запросов, не зависящим от числа серверов (без N+1).
func TestIntegration_Servers_ListNoNPlusOne(t *testing.T) {
	st := setupServers(t)
	defer st.Close()
	ctx := context.Background()
	var items []ServerWrite
	for no := 1; no <= 12; no++ {
		w := telemtServer(no, "pr1", "ps1")
		if no%2 == 0 {
			w = telemtServer(no, "pr2", "ps2")
			w.Engine = servercfg.EngineMtg
			w.Settings = map[string]string{}
		}
		if no == 12 {
			w.State = "test"
		}
		items = append(items, w)
	}
	if _, _, err := st.ImportServers(ctx, items); err != nil {
		t.Fatal(err)
	}
	// Отчёт агента pr1: telemt API ответил - подключения из снимка; pr2: mtg API не ответил.
	if err := st.RecordRuntime(ctx, RuntimeReport{Target: "pr1", Telemt: &RuntimeTelemt{APIOK: true, ConnectionsTotal: 7,
		Users: []RuntimeUser{{Key: "a_1", Connections: 4}, {Key: "b_2", Connections: 3}, {Key: "c_3"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRuntime(ctx, RuntimeReport{Target: "pr2", Mtg: &RuntimeMtg{APIOK: true,
		Users: []RuntimeUser{{Key: "a_1", Connections: 2}}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRuntime(ctx, RuntimeReport{Target: "pr2", Mtg: &RuntimeMtg{APIOK: false, Error: "HTTP 502"}}); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(mustDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	counter := &queryCounter{}
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	traced := &Store{pool: pool}
	defer traced.Close()

	list, total, err := traced.ListServers(ctx, "", "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 12 || len(list) != 12 {
		t.Fatalf("страница: total %d, строк %d", total, len(list))
	}
	if q := counter.n.Load(); q != 4 {
		t.Fatalf("запросов на страницу %d, ждали 4 (count, серверы, точки входа, живые цифры)", q)
	}
	for _, srv := range list {
		if len(srv.Endpoints) != 3 {
			t.Fatalf("сервер %d: точек входа %d", srv.No, len(srv.Endpoints))
		}
	}
	one := list[0] // telemt на pr1
	if r := one.Live.Relay; r.SeenAt == nil || !r.APIOK || r.Connections != 7 || r.UsersOnline != 2 {
		t.Fatalf("живые цифры telemt pr1: %+v", r)
	}
	if n := one.Live.Node; n.SeenAt != nil || n.Connections != 0 {
		t.Fatalf("выход без агента: %+v", n)
	}
	two := list[1] // mtg на pr2: API не ответил - сумма строк прошлого удачного снимка
	if r := two.Live.Relay; r.APIOK || r.Connections != 2 || r.UsersOnline != 1 {
		t.Fatalf("живые цифры mtg pr2: %+v", r)
	}

	mtgPage, total, err := st.ListServers(ctx, servercfg.EngineMtg, "", 2, 1)
	if err != nil || total != 6 || len(mtgPage) != 2 || mtgPage[0].No != 4 {
		t.Fatalf("фильтр engine и offset: total %d, %+v, %v", total, mtgPage, err)
	}
	testPage, total, err := st.ListServers(ctx, "", "test", 20, 0)
	if err != nil || total != 1 || testPage[0].No != 12 {
		t.Fatalf("фильтр state: %d %v", total, err)
	}
	empty, total, err := st.ListServers(ctx, "", "disabled", 20, 0)
	if err != nil || total != 0 || empty == nil {
		t.Fatalf("пустая страница: %v %d %v", empty, total, err)
	}
}
