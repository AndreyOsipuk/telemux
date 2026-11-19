package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

func (f *fakeAgents) RecordRuntime(_ context.Context, rep store.RuntimeReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runtimeErr != nil {
		return f.runtimeErr
	}
	f.runtime = append(f.runtime, rep)
	return nil
}

func (f *fakeAgents) PruneLogErrors(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned++
	f.prunedBefore = before
	return 0, nil
}

func (f *fakeAgents) MachineRuntime(_ context.Context, code string) (store.MachineRuntime, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mr, ok := f.snapshots[code]
	if !ok {
		return mr, store.ErrUnknownTarget
	}
	return mr, nil
}

func page[T any](all []T, limit, offset int) []T {
	if offset >= len(all) {
		return []T{}
	}
	return all[offset:min(offset+limit, len(all))]
}

func (f *fakeAgents) Connections(_ context.Context, flt store.ConnFilter, limit, offset int) ([]store.ConnRow, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastConn = flt
	var all []store.ConnRow
	for _, c := range f.conns {
		if (flt.Machine == "" || c.Machine == flt.Machine) && (flt.Product == "" || c.Product == flt.Product) &&
			c.Connections >= flt.MinConnections {
			all = append(all, c)
		}
	}
	return page(all, limit, offset), len(all), nil
}

func (f *fakeAgents) ConnectionsSummary(_ context.Context, limit, offset int) ([]store.ConnSummary, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return page(f.summary, limit, offset), len(f.summary), nil
}

func (f *fakeAgents) LogErrors(_ context.Context, since time.Time, machine string, limit, offset int) ([]store.LogErrorSum, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSince, f.lastMach = since, machine
	return page(f.logErrors, limit, offset), len(f.logErrors), nil
}

func usersJSON(n, ips int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		ipList := make([]string, ips)
		for j := range ipList {
			ipList[j] = fmt.Sprintf(`"10.0.%d.%d"`, j/250, j%250)
		}
		fmt.Fprintf(&b, `{"key":"u_%d","connections":1,"active_ips":[%s]}`, i, strings.Join(ipList, ","))
	}
	return b.String()
}

// runtimeBody - корректный отчёт с периодом журнала, закончившимся в end.
func runtimeBody(end time.Time) string {
	end = end.UTC().Truncate(time.Second)
	return `{"target":"pr2","agent_version":"v1",
 "host":{"cpu_pct":12.5,"mem_pct":40.1,"disk_pct":55,"load1":0.3,"net_rx_mbps":10.2,"net_tx_mbps":11,"uptime_s":12345},
 "telemt":{"version":"3.5.17","api_ok":true,"error":"","users_configured":120,"users_effective":118,"connections_total":340,
   "config_sha256":"ab","keepalive":0,"ack":0,"tls_domain":"x","max_tcp_conns_median":0,
   "users":[{"key":"andrey_1135","connections":3,"active_ips":["1.2.3.4"]}]},
 "mtg":{"version":"x","api_ok":false,"error":"refused","connections_total":0,"users":[]},
 "log_errors":{"period_start":"` + end.Add(-time.Minute).Format(time.RFC3339) + `","period_end":"` + end.Format(time.RFC3339) + `",
   "items":[{"key":"andrey_1135","kind":"limit","count":4},{"key":"","kind":"handshake","count":2}]}}`
}

// runtimeOK - корректный отчёт: период журнала - последняя минута.
var runtimeOK = runtimeBody(time.Now())

func TestRuntime_PostAccepted(t *testing.T) {
	s, _, fa := agentServer(false)
	if rec := do(s, "POST", "/v1/runtime", pr2Token, runtimeOK); rec.Code != http.StatusNoContent {
		t.Fatalf("ждали 204, получили %d: %s", rec.Code, rec.Body.String())
	}
	if len(fa.runtime) != 1 || fa.runtime[0].Telemt.Users[0].Key != "andrey_1135" || fa.runtime[0].Mtg.APIOK ||
		len(fa.runtime[0].LogErrors.Items) != 2 {
		t.Fatalf("отчёт сохранён не так: %+v", fa.runtime)
	}
	// Машина без telemt и mtg: только host.
	if rec := do(s, "POST", "/v1/runtime", pr2Token, `{"target":"pr2","host":{"cpu_pct":1},"telemt":null,"mtg":null,"log_errors":null}`); rec.Code != http.StatusNoContent {
		t.Fatalf("отчёт без продуктов → ждали 204, получили %d", rec.Code)
	}
}

func TestRuntime_PostAuthAndTarget(t *testing.T) {
	s, _, fa := agentServer(false)
	if rec := do(s, "POST", "/v1/runtime", "", runtimeOK); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена → ждали 401, получили %d", rec.Code)
	}
	if rec := do(s, "POST", "/v1/runtime", "token-ps2", runtimeOK); rec.Code != http.StatusForbidden {
		t.Fatalf("отчёт за чужую машину → ждали 403, получили %d", rec.Code)
	}
	if len(fa.runtime) != 0 {
		t.Fatal("отклонённый отчёт не должен сохраняться")
	}
}

func TestRuntime_PostLimits(t *testing.T) {
	s, _, fa := agentServer(false)
	bad := map[string]string{
		"битый JSON":            `{"target":`,
		"5001 в telemt":         `{"target":"pr2","telemt":{"users":[` + usersJSON(5001, 0) + `]}}`,
		"5001 в mtg":            `{"target":"pr2","mtg":{"users":[` + usersJSON(5001, 0) + `]}}`,
		"21 адрес":              `{"target":"pr2","telemt":{"users":[` + usersJSON(1, 21) + `]}}`,
		"пустой ключ":           `{"target":"pr2","telemt":{"users":[{"key":"","connections":1}]}}`,
		"повтор ключа":          `{"target":"pr2","mtg":{"users":[{"key":"a","connections":1},{"key":"a","connections":2}]}}`,
		"минус подключения":     `{"target":"pr2","telemt":{"users":[{"key":"a","connections":-1}]}}`,
		"неизвестный вид":       `{"target":"pr2","log_errors":{"period_start":"2026-11-19T10:00:00Z","period_end":"2026-11-19T10:01:00Z","items":[{"key":"a","kind":"boom","count":1}]}}`,
		"конец раньше начала":   `{"target":"pr2","log_errors":{"period_start":"2026-11-19T10:01:00Z","period_end":"2026-11-19T10:00:00Z","items":[]}}`,
		"без периода":           `{"target":"pr2","log_errors":{"items":[]}}`,
		"длинная ошибка":        `{"target":"pr2","mtg":{"error":"` + strings.Repeat("x", 1025) + `"}}`,
		"5001 строка журнала":   `{"target":"pr2","log_errors":{"period_start":"2026-11-19T10:00:00Z","period_end":"2026-11-19T10:01:00Z","items":[` + logItems(5001) + `]}}`,
		"повтор строки журнала": `{"target":"pr2","log_errors":{"period_start":"2026-11-19T10:00:00Z","period_end":"2026-11-19T10:01:00Z","items":[{"key":"a","kind":"limit","count":1},{"key":"a","kind":"limit","count":2}]}}`,
	}
	for name, body := range bad {
		rec := do(s, "POST", "/v1/runtime", pr2Token, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → ждали 400, получили %d: %s", name, rec.Code, rec.Body.String())
		}
		decodeErr(t, rec)
	}
	// Ровно на границе - принимается.
	edge := `{"target":"pr2","telemt":{"users":[` + usersJSON(5000, 0) + `]},"mtg":{"users":[` + usersJSON(1, 20) + `]}}`
	if rec := do(s, "POST", "/v1/runtime", pr2Token, edge); rec.Code != http.StatusNoContent {
		t.Fatalf("5000 пользователей и 20 адресов → ждали 204, получили %d: %s", rec.Code, rec.Body.String())
	}
	// Тело больше 2 МБ - 413.
	huge := `{"target":"pr2","agent_version":"` + strings.Repeat("x", 2<<20) + `"}`
	rec := do(s, "POST", "/v1/runtime", pr2Token, huge)
	if rec.Code != http.StatusRequestEntityTooLarge || decodeErr(t, rec).Code != "payload_too_large" {
		t.Fatalf("тело больше 2 МБ → ждали 413 payload_too_large, получили %d: %s", rec.Code, rec.Body.String())
	}
	if len(fa.runtime) != 1 {
		t.Fatalf("сохраниться должен только отчёт на границе, сохранено %d", len(fa.runtime))
	}
}

func logItems(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"key":"u_%d","kind":"limit","count":1}`, i)
	}
	return strings.Join(items, ",")
}

// На реплике отчёт не пишется: 409 not_master, агент пойдёт к следующему адресу.
func TestRuntime_PostReplica(t *testing.T) {
	s, _, _ := agentServer(true)
	if rec := do(s, "POST", "/v1/runtime", pr2Token, runtimeOK); rec.Code != http.StatusConflict {
		t.Fatalf("реплика → ждали 409, получили %d", rec.Code)
	}
}

// Сбой базы - 5xx (это не ошибка агента).
func TestRuntime_PostStoreFailure(t *testing.T) {
	s, _, fa := agentServer(false)
	fa.runtimeErr = context.DeadlineExceeded
	if rec := do(s, "POST", "/v1/runtime", pr2Token, runtimeOK); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("база недоступна → ждали 503, получили %d", rec.Code)
	}
}

func TestRuntime_MachineSnapshot(t *testing.T) {
	s, _, fa := agentServer(false)
	fresh := time.Now().Add(-30 * time.Second)
	old := time.Now().Add(-181 * time.Second)
	fa.snapshots = map[string]store.MachineRuntime{
		"pr2": {Code: "pr2", SeenAt: &fresh, AgentVersion: "v1", Host: &store.RuntimeHost{CPUPct: 5},
			Telemt: &store.RuntimeTelemt{Version: "3.5.17", APIOK: true, Users: []store.RuntimeUser{{Key: "a"}}}},
		"ps2":  {Code: "ps2", SeenAt: &old},
		"vega": {Code: "vega"},
	}
	get := func(code string) (int, map[string]any) {
		rec := do(s, "GET", "/api/v1/machines/"+code+"/runtime", botToken, "")
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	code, body := get("pr2")
	if code != http.StatusOK || body["stale"] != false || body["telemt"].(map[string]any)["version"] != "3.5.17" {
		t.Fatalf("свежий снимок: %d %v", code, body)
	}
	if _, has := body["telemt"].(map[string]any)["users"]; has {
		t.Fatal("в снимке машины не должно быть списка пользователей")
	}
	if body["mtg"] != nil {
		t.Fatalf("нет mtg на машине → null, получили %v", body["mtg"])
	}
	if code, body := get("ps2"); code != http.StatusOK || body["stale"] != true {
		t.Fatalf("снимок старше 180 с → stale, получили %d %v", code, body)
	}
	if code, body := get("vega"); code != http.StatusOK || body["stale"] != true || body["seen_at"] != nil {
		t.Fatalf("отчёта ещё не было → seen_at null и stale, получили %d %v", code, body)
	}
	if code, _ := get("nobody"); code != http.StatusNotFound {
		t.Fatalf("неизвестная машина → ждали 404, получили %d", code)
	}
	if rec := do(s, "GET", "/api/v1/machines/pr2/runtime", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена → ждали 401, получили %d", rec.Code)
	}
}

type connPage struct {
	Data   []store.ConnRow                    `json:"data"`
	Paging struct{ Total, Limit, Offset int } `json:"paging"`
}

func TestRuntime_Connections(t *testing.T) {
	s, _, fa := agentServer(false)
	fresh, old := time.Now().Add(-10*time.Second), time.Now().Add(-10*time.Minute)
	fa.conns = []store.ConnRow{
		{Machine: "pr1", Product: "telemt", Key: "a_1", Connections: 3, ActiveIPs: []string{"1.1.1.1"}, SeenAt: fresh},
		{Machine: "pr1", Product: "telemt", Key: "b_2", Connections: 1, ActiveIPs: []string{}, SeenAt: fresh},
		{Machine: "ps5", Product: "mtg", Key: "c_3", Connections: 5, ActiveIPs: []string{}, BytesIn: 10, SeenAt: old},
	}
	list := func(q string) connPage {
		t.Helper()
		rec := do(s, "GET", "/api/v1/connections?"+q, botToken, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: ждали 200, получили %d: %s", q, rec.Code, rec.Body.String())
		}
		var p connPage
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := list("")
	if p.Paging.Total != 3 || p.Paging.Limit != 20 || len(p.Data) != 3 || p.Data[0].Stale || !p.Data[2].Stale {
		t.Fatalf("весь список: %+v", p)
	}
	if p := list("product=telemt&min_connections=2"); p.Paging.Total != 1 || p.Data[0].Key != "a_1" {
		t.Fatalf("фильтр product+min_connections: %+v", p)
	}
	if p := list("machine=ps5"); p.Paging.Total != 1 || fa.lastConn.Machine != "ps5" {
		t.Fatalf("фильтр machine: %+v", p)
	}
	if p := list("limit=1&offset=1"); p.Paging.Total != 3 || len(p.Data) != 1 || p.Data[0].Key != "b_2" || p.Paging.Offset != 1 {
		t.Fatalf("вторая страница: %+v", p)
	}
	for _, q := range []string{"limit=101", "limit=0", "offset=-1", "product=xray", "min_connections=-1", "min_connections=x"} {
		if rec := do(s, "GET", "/api/v1/connections?"+q, botToken, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → ждали 400, получили %d", q, rec.Code)
		}
	}
}

func TestRuntime_Summary(t *testing.T) {
	s, _, fa := agentServer(false)
	fresh, old := time.Now(), time.Now().Add(-time.Hour)
	fa.summary = []store.ConnSummary{
		{Machine: "pr1", Product: "telemt", ConnectionsTotal: 10, UsersOnline: 4, SeenAt: fresh},
		{Machine: "ps5", Product: "mtg", ConnectionsTotal: 2, UsersOnline: 1, SeenAt: old},
	}
	rec := do(s, "GET", "/api/v1/connections/summary?limit=1&offset=1", botToken, "")
	var p struct {
		Data   []store.ConnSummary                `json:"data"`
		Paging struct{ Total, Limit, Offset int } `json:"paging"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	if p.Paging.Total != 2 || len(p.Data) != 1 || p.Data[0].Machine != "ps5" || !p.Data[0].Stale {
		t.Fatalf("сводка: %+v", p)
	}
	if rec := do(s, "GET", "/api/v1/connections/summary?limit=101", botToken, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=101 → ждали 400, получили %d", rec.Code)
	}
}

func TestRuntime_LogErrors(t *testing.T) {
	s, _, fa := agentServer(false)
	fa.logErrors = []store.LogErrorSum{{Machine: "pr1", Key: "a_1", Kind: "limit", Count: 9}}
	since := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	rec := do(s, "GET", "/api/v1/log-errors?machine=pr1&since="+url.QueryEscape(since.Format(time.RFC3339)), botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	if !fa.lastSince.Equal(since) || fa.lastMach != "pr1" {
		t.Fatalf("since/machine не дошли до хранилища: %v %q", fa.lastSince, fa.lastMach)
	}
	var p struct {
		Data   []store.LogErrorSum                `json:"data"`
		Paging struct{ Total, Limit, Offset int } `json:"paging"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &p) != nil || p.Paging.Total != 1 || p.Data[0].Count != 9 {
		t.Fatalf("ответ: %s", rec.Body.String())
	}
	tooOld := time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	for _, q := range []string{"", "since=вчера", "since=" + url.QueryEscape(tooOld),
		"since=" + url.QueryEscape(since.Format(time.RFC3339)) + "&limit=101"} {
		if rec := do(s, "GET", "/api/v1/log-errors?"+q, botToken, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%q → ждали 400, получили %d", q, rec.Code)
		}
	}
}

// logBody - отчёт только с журналом за период [start, end] и одной строкой count.
func logBody(start, end time.Time, count string) string {
	return `{"target":"pr2","log_errors":{"period_start":"` + start.UTC().Format(time.RFC3339) +
		`","period_end":"` + end.UTC().Format(time.RFC3339) +
		`","items":[{"key":"a_1","kind":"limit","count":` + count + `}]}}`
}

// Период журнала и счётчик проверяются на разумность: иначе один кривой отчёт
// (часы агента убежали, мусор в счётчике) навсегда портит суммы за сутки.
func TestRuntime_PostLogPeriodSanity(t *testing.T) {
	s, _, fa := agentServer(false)
	now := time.Now()
	bad := map[string]struct{ body, code string }{
		"период в будущем":      {logBody(now.Add(9*time.Minute), now.Add(10*time.Minute), "1"), "log_period_invalid"},
		"период слишком старый": {logBody(now.Add(-3*time.Hour), now.Add(-2*time.Hour), "1"), "log_period_invalid"},
		"период длиннее окна":   {logBody(now.Add(-3*time.Hour), now, "1"), "log_period_invalid"},
		"счётчик больше 1e9":    {logBody(now.Add(-time.Minute), now, "1000000001"), "log_count_invalid"},
	}
	for name, c := range bad {
		rec := do(s, "POST", "/v1/runtime", pr2Token, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → ждали 400, получили %d: %s", name, rec.Code, rec.Body.String())
		}
		if got := decodeErr(t, rec).Code; got != c.code {
			t.Fatalf("%s → ждали код %s, получили %s", name, c.code, got)
		}
	}
	// На границах - принимается: час окна, часы агента чуть впереди, счётчик ровно 1e9.
	for name, body := range map[string]string{
		"час окна":        logBody(now.Add(-time.Hour), now, "1"),
		"часы впереди":    logBody(now.Add(time.Minute), now.Add(2*time.Minute), "1"),
		"счётчик 1e9":     logBody(now.Add(-time.Minute), now, "1000000000"),
		"конец час назад": logBody(now.Add(-61*time.Minute), now.Add(-60*time.Minute), "1"),
	} {
		if rec := do(s, "POST", "/v1/runtime", pr2Token, body); rec.Code != http.StatusNoContent {
			t.Fatalf("%s → ждали 204, получили %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	if len(fa.runtime) != 4 {
		t.Fatalf("сохраниться должны только 4 отчёта на границе, сохранено %d", len(fa.runtime))
	}
}

// Нагрузка машины проверяется на разумность: проценты 0..100, остальное не меньше 0.
func TestRuntime_PostHostSanity(t *testing.T) {
	s, _, fa := agentServer(false)
	for name, host := range map[string]string{
		"cpu больше 100":    `{"cpu_pct":100.1}`,
		"mem меньше 0":      `{"mem_pct":-1}`,
		"disk больше 100":   `{"disk_pct":250}`,
		"load1 меньше 0":    `{"load1":-0.5}`,
		"rx меньше 0":       `{"net_rx_mbps":-3}`,
		"tx меньше 0":       `{"net_tx_mbps":-3}`,
		"uptime меньше 0":   `{"uptime_s":-10}`,
		"rx 1e300 и больше": `{"net_rx_mbps":1e300}`,
	} {
		rec := do(s, "POST", "/v1/runtime", pr2Token, `{"target":"pr2","host":`+host+`}`)
		if rec.Code != http.StatusBadRequest || decodeErr(t, rec).Code != "host_invalid" {
			t.Fatalf("%s → ждали 400 host_invalid, получили %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	edge := `{"target":"pr2","host":{"cpu_pct":100,"mem_pct":0,"disk_pct":100,"load1":0,"net_rx_mbps":0,"net_tx_mbps":0,"uptime_s":0}}`
	if rec := do(s, "POST", "/v1/runtime", pr2Token, edge); rec.Code != http.StatusNoContent {
		t.Fatalf("границы → ждали 204, получили %d: %s", rec.Code, rec.Body.String())
	}
	if len(fa.runtime) != 1 {
		t.Fatalf("сохранено %d", len(fa.runtime))
	}
}

// Порог устаревания настраивается (по умолчанию - три периода отчёта).
func TestRuntime_StaleAfterConfigurable(t *testing.T) {
	_, fr, fa := agentServer(false)
	s := New(Deps{
		Store: fakeStore{}, Users: fr.users, Revisions: fr, Agents: fa, Node: &fakeNode{},
		BotToken: botToken, AdminUser: "admin", AdminPassword: "pw", RuntimeStaleAfter: 2 * time.Hour,
	})
	seen := time.Now().Add(-time.Hour)
	fa.snapshots = map[string]store.MachineRuntime{"pr2": {Code: "pr2", SeenAt: &seen}}
	rec := do(s, "GET", "/api/v1/machines/pr2/runtime", botToken, "")
	var body map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body["stale"] != false {
		t.Fatalf("порог 2 ч, снимок час назад → не stale: %d %s", rec.Code, rec.Body.String())
	}
	if d := runtimeStaleDefault(30 * time.Second); d != 90*time.Second {
		t.Fatalf("по умолчанию - три периода отчёта: %v", d)
	}
}

// Сводка отдаёт api_ok и error продукта: бот по ним решает идти по SSH.
func TestRuntime_SummaryAPIState(t *testing.T) {
	s, _, fa := agentServer(false)
	fa.summary = []store.ConnSummary{
		{Machine: "pr1", Product: "telemt", ConnectionsTotal: 10, UsersOnline: 4, SeenAt: time.Now(), APIOK: true},
		{Machine: "pr2", Product: "telemt", ConnectionsTotal: 3, UsersOnline: 2, SeenAt: time.Now(), APIOK: false, Error: "HTTP 502"},
	}
	rec := do(s, "GET", "/api/v1/connections/summary", botToken, "")
	var p struct {
		Data []map[string]any `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &p) != nil || len(p.Data) != 2 {
		t.Fatalf("ждали 200 и две строки: %d %s", rec.Code, rec.Body.String())
	}
	if p.Data[0]["api_ok"] != true || p.Data[0]["error"] != nil {
		t.Fatalf("удачный продукт: api_ok=true без error, получили %v", p.Data[0])
	}
	if p.Data[1]["api_ok"] != false || p.Data[1]["error"] != "HTTP 502" || p.Data[1]["users_online"] != float64(2) {
		t.Fatalf("API продукта не ответил: строка есть, api_ok=false и error, получили %v", p.Data[1])
	}
}

// Чистка старых счётчиков журнала - периодическая задача мастера, не часть отчёта.
func TestRuntime_PruneOnlyOnMaster(t *testing.T) {
	s, _, fa := agentServer(false)
	s.pruneRuntime(context.Background())
	if fa.pruned != 1 || time.Since(fa.prunedBefore) < 24*time.Hour-time.Minute {
		t.Fatalf("на мастере чистка старше 24 ч: вызовов %d, граница %v", fa.pruned, fa.prunedBefore)
	}
	r, _, fr := agentServer(true)
	r.pruneRuntime(context.Background())
	if fr.pruned != 0 {
		t.Fatal("на реплике чистить нельзя (и нечем: база только для чтения)")
	}
	if rec := do(s, "POST", "/v1/runtime", pr2Token, runtimeOK); rec.Code != http.StatusNoContent || fa.pruned != 1 {
		t.Fatalf("отчёт не должен запускать чистку: %d, вызовов %d", rec.Code, fa.pruned)
	}
}
