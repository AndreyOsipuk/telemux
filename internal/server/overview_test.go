package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
)

func (f *fakeAgents) ConnectionsByUser(_ context.Context, keys []string) ([]store.UserConns, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byUserKeys = slices.Clone(keys)
	out := make([]store.UserConns, 0, len(keys))
	for _, k := range keys {
		u := store.UserConns{Key: k}
		machines := map[string]bool{}
		for _, c := range f.conns {
			if c.Key != k {
				continue
			}
			u.Connections += c.Connections
			if c.Connections > 0 {
				machines[c.Machine] = true
			}
			seen := c.SeenAt
			if u.SeenAt == nil || seen.After(*u.SeenAt) {
				u.SeenAt = &seen
			}
		}
		u.Machines, u.Online = len(machines), u.Connections > 0
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeAgents) FleetTotals(_ context.Context, freshSince, silentBefore time.Time) (store.FleetTotals, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFresh, f.lastSilent = freshSince, silentBefore
	return f.totals, f.totalsErr
}

// overviewServer - мастер с машинами и (если fc не nil) сертификатами.
func overviewServer(fc *fakeCerts) (*Server, *fakeAgents) {
	s, _, fa := agentServer(false)
	if fc != nil {
		s = New(Deps{
			Store: fakeStore{}, Agents: fa, Certs: fc, Node: &fakeNode{}, Version: "v1",
			SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}, BotToken: botToken,
			AdminUser: "admin", AdminPassword: "pw",
		})
	}
	return s, fa
}

func ptrTime(t time.Time) *time.Time { return &t }
func ptrBool(b bool) *bool           { return &b }

// sessionGet - GET с сессией панели (вход admin/pw).
func sessionGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	login := httptest.NewRecorder()
	s.Handler().ServeHTTP(login, httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"admin","password":"pw"}`)))
	if login.Code != http.StatusOK {
		t.Fatalf("вход: %d", login.Code)
	}
	req := httptest.NewRequest("GET", path, nil)
	for _, c := range login.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// Статус машины: свежий отчёт - online, API продукта не ответил - degraded,
// отчёт старше порога свежести - stale, агент молчит дольше 10 минут или отчёта
// нет - offline.
func TestRuntimeStatus(t *testing.T) {
	s, _ := overviewServer(nil)
	now := time.Now()
	cases := []struct {
		name string
		m    store.Machine
		want string
	}{
		{"отчёта нет", store.Machine{}, RuntimeOffline},
		{"свежий, API отвечает", store.Machine{RuntimeSeenAt: ptrTime(now.Add(-time.Minute)), TelemtAPIOK: ptrBool(true)}, RuntimeOnline},
		{"свежий, продуктов нет", store.Machine{RuntimeSeenAt: ptrTime(now)}, RuntimeOnline},
		{"telemt не отвечает", store.Machine{RuntimeSeenAt: ptrTime(now), TelemtAPIOK: ptrBool(false)}, RuntimeDegraded},
		{"mtg не отвечает", store.Machine{RuntimeSeenAt: ptrTime(now), TelemtAPIOK: ptrBool(true), MtgAPIOK: ptrBool(false)}, RuntimeDegraded},
		{"старше порога свежести", store.Machine{RuntimeSeenAt: ptrTime(now.Add(-5 * time.Minute)), TelemtAPIOK: ptrBool(false)}, RuntimeStale},
		{"ровно 10 минут - ещё stale", store.Machine{RuntimeSeenAt: ptrTime(now.Add(-store.SilentAfter + time.Second))}, RuntimeStale},
		{"молчит дольше 10 минут", store.Machine{RuntimeSeenAt: ptrTime(now.Add(-store.SilentAfter - time.Second))}, RuntimeOffline},
	}
	for _, c := range cases {
		if got := s.runtimeStatus(c.m, now); got != c.want {
			t.Errorf("%s: ждали %s, получили %s", c.name, c.want, got)
		}
	}
}

type overviewResp struct {
	Machines    struct{ Total, Online, Silent int } `json:"machines"`
	Connections struct {
		Total       int64 `json:"total"`
		UsersOnline int   `json:"users_online"`
	} `json:"connections"`
	Pairs              struct{ Total, OK, Problem int }        `json:"pairs"`
	Certs              *struct{ Total, Expiring, Missing int } `json:"certs"`
	CertWarnDays       int                                     `json:"cert_warn_days"`
	StaleAfterSeconds  int                                     `json:"stale_after_seconds"`
	SilentAfterSeconds int                                     `json:"silent_after_seconds"`
	Data               []store.Machine                         `json:"data"`
	Truncated          bool                                    `json:"truncated"`
}

func getOverview(t *testing.T, s *Server) overviewResp {
	t.Helper()
	rec := do(s, "GET", "/api/v1/overview", botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	var out overviewResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOverview_Body(t *testing.T) {
	fc := newFakeCerts()
	fc.counts = store.CertCounts{Total: 3, Expiring: 1, Missing: 1}
	s, fa := overviewServer(fc)
	now := time.Now()
	fa.totals = store.FleetTotals{Machines: 3, AgentsOnline: 1, AgentsSilent: 1, ConnectionsTotal: 42, UsersOnline: 7}
	fa.machines = []store.Machine{
		{Code: "pr1", Kind: "relay", Enabled: true, RuntimeSeenAt: ptrTime(now), TelemtAPIOK: ptrBool(true), Connections: 40, UsersOnline: 6,
			Host: &store.RuntimeHost{CPUPct: 12, MemPct: 40, DiskPct: 55, Load1: 0.4, UptimeS: 3600}},
		{Code: "ps1", Kind: "exit", Enabled: true, RuntimeSeenAt: ptrTime(now), MtgAPIOK: ptrBool(false)},
		{Code: "ps2", Kind: "exit", Enabled: true},
	}
	ok := func(code string) store.PairMachine {
		return store.PairMachine{Machine: store.Machine{Code: code}, Status: store.StatusOK}
	}
	failed := ok("ps9")
	failed.Status = store.StatusFailed
	off := ok("ps8")
	off.Status = store.StatusDisabled
	r1, r2 := ok("pr1"), ok("pr2")
	fa.pairs = []store.Pair{
		{Relay: &r1, Exits: []store.PairMachine{ok("ps1"), off}}, // выключенная машина пару не портит
		{Relay: &r2, Exits: []store.PairMachine{failed}},
		{Exits: []store.PairMachine{failed}}, // псевдо-пара «без релея» не считается
	}

	out := getOverview(t, s)
	if out.Machines.Total != 3 || out.Machines.Online != 1 || out.Machines.Silent != 1 {
		t.Fatalf("машины: %+v", out.Machines)
	}
	if out.Connections.Total != 42 || out.Connections.UsersOnline != 7 {
		t.Fatalf("подключения: %+v", out.Connections)
	}
	if out.Pairs.Total != 2 || out.Pairs.OK != 1 || out.Pairs.Problem != 1 {
		t.Fatalf("пары: %+v", out.Pairs)
	}
	if out.Certs == nil || out.Certs.Total != 3 || out.Certs.Expiring != 1 || out.Certs.Missing != 1 || out.CertWarnDays != 14 {
		t.Fatalf("сертификаты: %+v %d", out.Certs, out.CertWarnDays)
	}
	if d := time.Until(fc.before); d < 13*24*time.Hour || d > 15*24*time.Hour {
		t.Fatalf("граница «скоро истекает» - через 14 дней, а не %s", d)
	}
	if out.StaleAfterSeconds != 180 || out.SilentAfterSeconds != 600 {
		t.Fatalf("пороги: %d %d", out.StaleAfterSeconds, out.SilentAfterSeconds)
	}
	if d := now.Sub(fa.lastFresh); d < 179*time.Second || d > 185*time.Second {
		t.Fatalf("граница свежести: %s", d)
	}
	if d := now.Sub(fa.lastSilent); d < 599*time.Second || d > 605*time.Second {
		t.Fatalf("граница молчания: %s", d)
	}
	if len(out.Data) != 3 || out.Truncated {
		t.Fatalf("машины списком: %d truncated=%v", len(out.Data), out.Truncated)
	}
	got := map[string]string{}
	for _, m := range out.Data {
		got[m.Code] = m.RuntimeStatus
	}
	if got["pr1"] != RuntimeOnline || got["ps1"] != RuntimeDegraded || got["ps2"] != RuntimeOffline {
		t.Fatalf("статусы: %v", got)
	}
	if h := out.Data[0].Host; h == nil || h.CPUPct != 12 || out.Data[0].Connections != 40 || out.Data[0].UsersOnline != 6 {
		t.Fatalf("runtime-поля машины: %+v", out.Data[0])
	}
	if fa.lastFilter != (store.MachineFilter{}) {
		t.Fatalf("обзор берёт машины без фильтра: %+v", fa.lastFilter)
	}
}

// Флот больше 500 машин: списком - первые 500, truncated: true, итоги - по всем.
func TestOverview_LimitAndTruncated(t *testing.T) {
	s, fa := overviewServer(nil)
	for i := 0; i < overviewMaxMachines+3; i++ {
		fa.machines = append(fa.machines, store.Machine{Code: fmt.Sprintf("m%04d", i), Kind: "exit", Enabled: true})
	}
	fa.totals = store.FleetTotals{Machines: overviewMaxMachines + 3}
	out := getOverview(t, s)
	if len(out.Data) != overviewMaxMachines || !out.Truncated || out.Machines.Total != overviewMaxMachines+3 {
		t.Fatalf("ждали %d машин и truncated, получили %d %v", overviewMaxMachines, len(out.Data), out.Truncated)
	}
	if out.Certs != nil {
		t.Fatalf("сертификаты выключены - certs: null, а не %+v", out.Certs)
	}
}

func TestOverview_EmptyFleet(t *testing.T) {
	s, _ := overviewServer(nil)
	rec := do(s, "GET", "/api/v1/overview", botToken, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("пустой флот: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOverview_Auth(t *testing.T) {
	s, _ := overviewServer(nil)
	for _, p := range []string{"/api/v1/overview", "/api/v1/connections/by-user?user=a_1"} {
		for _, tok := range []string{"", "nope", pr2Token} {
			if rec := do(s, "GET", p, tok, ""); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s с токеном %q → ждали 401, получили %d", p, tok, rec.Code)
			}
		}
		if rec := sessionGet(t, s, p); rec.Code != http.StatusOK {
			t.Fatalf("%s по сессии → ждали 200, получили %d: %s", p, rec.Code, rec.Body.String())
		}
	}
}

// Ошибка базы - не 200 с пустыми итогами: связь с базой - 503, остальное - 500.
func TestOverview_StoreErrors(t *testing.T) {
	s, fa := overviewServer(nil)
	fa.totalsErr = context.DeadlineExceeded
	if rec := do(s, "GET", "/api/v1/overview", botToken, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("таймаут базы → ждали 503, получили %d", rec.Code)
	}
	fa.totalsErr = nil
	fa.fail = errors.New("boom")
	if rec := do(s, "GET", "/api/v1/overview", botToken, ""); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("сбой выборки пар → ждали 500 без деталей, получили %d: %s", rec.Code, rec.Body.String())
	}
	fa.fail = nil
	fc := newFakeCerts()
	fc.countsErr = errors.New("certs boom")
	s2, fa2 := overviewServer(fc)
	_ = fa2
	if rec := do(s2, "GET", "/api/v1/overview", botToken, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("сбой итогов сертификатов → ждали 500, получили %d", rec.Code)
	}
}

// /api/v1/machines и /api/v1/pairs отдают runtime_status машин.
func TestMachines_RuntimeStatus(t *testing.T) {
	s, fa := overviewServer(nil)
	now := time.Now()
	fa.machines = []store.Machine{{Code: "pr1", Kind: "relay", Enabled: true, RuntimeSeenAt: ptrTime(now), TelemtAPIOK: ptrBool(false)}}
	r := store.PairMachine{Machine: fa.machines[0], Status: store.StatusOK}
	fa.pairs = []store.Pair{{Relay: &r, Exits: []store.PairMachine{{Machine: store.Machine{Code: "ps1"}, Status: store.StatusNoAgent}}}}
	if rec := do(s, "GET", "/api/v1/machines", botToken, ""); !strings.Contains(rec.Body.String(), `"runtime_status":"degraded"`) {
		t.Fatalf("machines: %s", rec.Body.String())
	}
	rec := do(s, "GET", "/api/v1/pairs", botToken, "")
	if !strings.Contains(rec.Body.String(), `"runtime_status":"degraded"`) || !strings.Contains(rec.Body.String(), `"runtime_status":"offline"`) {
		t.Fatalf("pairs: %s", rec.Body.String())
	}
}

func TestConnections_ByUser(t *testing.T) {
	s, fa := overviewServer(nil)
	fresh := time.Now().Add(-10 * time.Second)
	fa.conns = []store.ConnRow{
		{Machine: "pr1", Product: "telemt", Key: "a_1", Connections: 3, SeenAt: fresh},
		{Machine: "ps1", Product: "mtg", Key: "a_1", Connections: 2, SeenAt: fresh},
		{Machine: "ps2", Product: "telemt", Key: "a_1", Connections: 0, SeenAt: fresh},
		{Machine: "pr1", Product: "telemt", Key: "b_2", Connections: 0, SeenAt: fresh},
	}
	rec := do(s, "GET", "/api/v1/connections/by-user?user=a_1&user=b_2&user=nobody&user=a_1", botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	var out struct{ Data []store.UserConns }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fa.byUserKeys, []string{"a_1", "b_2", "nobody"}) {
		t.Fatalf("дубли схлопываются, порядок сохраняется: %v", fa.byUserKeys)
	}
	if len(out.Data) != 3 || out.Data[0].Connections != 5 || out.Data[0].Machines != 2 || !out.Data[0].Online ||
		out.Data[1].Online || out.Data[2].Connections != 0 || out.Data[2].SeenAt != nil {
		t.Fatalf("суммы: %+v", out.Data)
	}

	many := strings.Repeat("user=u&", store.MaxConnUsers+1)
	for _, q := range []string{"", "user=", "user=" + strings.Repeat("x", maxRuntimeKeyLen+1), many} {
		if rec := do(s, "GET", "/api/v1/connections/by-user?"+q, botToken, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%.40q → ждали 400, получили %d", q, rec.Code)
		}
	}
}

// Поиск по пользователю в /api/v1/connections - по префиксу ключа.
func TestConnections_UserPrefix(t *testing.T) {
	s, fa := overviewServer(nil)
	fa.conns = []store.ConnRow{
		{Machine: "pr1", Product: "telemt", Key: "andrey_1", Connections: 1},
		{Machine: "pr1", Product: "telemt", Key: "anna_2", Connections: 1},
		{Machine: "pr1", Product: "telemt", Key: "bob_andrey", Connections: 1},
	}
	rec := do(s, "GET", "/api/v1/connections?user=andr", botToken, "")
	var p connPage
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
		t.Fatalf("ждали 200, получили %d", rec.Code)
	}
	if fa.lastConn.User != "andr" || p.Paging.Total != 1 || p.Data[0].Key != "andrey_1" {
		t.Fatalf("префикс: %+v %+v", fa.lastConn, p)
	}
	if rec := do(s, "GET", "/api/v1/connections?user="+strings.Repeat("x", maxRuntimeKeyLen+1), botToken, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("длинный user → ждали 400, получили %d", rec.Code)
	}
}

// /api/v1/certs?machine= - только сертификаты этой машины, total с фильтром.
func TestCerts_ListMachineFilter(t *testing.T) {
	fc := newFakeCerts()
	fc.list = []store.CertInfo{{Machine: "pr1", Domain: "a.example.com"}, {Machine: "pr2", Domain: "b.example.com"}, {Machine: "pr2", Domain: "c.example.com"}}
	s, _, _ := certsServer(false, fc)
	rec := do(s, "GET", "/api/v1/certs?machine=pr2", botToken, "")
	var out struct {
		Data   []store.CertInfo `json:"data"`
		Paging map[string]int   `json:"paging"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("ждали 200, получили %d", rec.Code)
	}
	if fc.lastMach != "pr2" || out.Paging["total"] != 2 || len(out.Data) != 2 || out.Data[0].Domain != "b.example.com" {
		t.Fatalf("фильтр machine: %q %+v", fc.lastMach, out)
	}
}
