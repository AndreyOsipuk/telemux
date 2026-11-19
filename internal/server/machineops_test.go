package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// fakeOps - машины этапа C в памяти с теми же ошибками, что у store.
type fakeOps struct {
	mu       sync.Mutex
	targets  map[string]store.Target
	refs     map[string][]int // машина → номера серверов
	writes   int              // сколько раз дошло до записи
	recorded []string         // RecordMetrics
	metErr   error

	metCode          string
	metFrom, metTo   time.Time
	metStep          time.Duration
	points           []store.MetricPoint
	events           []store.MachineEvent
	afterID          int64
	evLimit          int
	pruneM, pruneE   time.Time
	detected         int
	detectSilent     time.Duration
	checkTarget      store.CheckTarget
	checkEndpoints   []store.CheckEndpoint
	checkTruncated   bool
	updateLastPatch  store.TargetPatch
	updateCheckError error
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		targets: map[string]store.Target{
			"pr2": {Code: "pr2", Kind: "relay", Enabled: true, Resources: []string{store.ResourceTelemtUsers}, Settings: map[string]string{}},
			"ps2": {Code: "ps2", Kind: "exit", RelayCode: "pr2", Enabled: true, Resources: []string{},
				Settings: map[string]string{store.SettingAddress: "203.0.113.10", store.SettingBridgeSocks: "socks5://127.0.0.1:10872"}},
			"pan1": {Code: "pan1", Kind: "panel", Enabled: true, Resources: []string{}, Settings: map[string]string{}},
		},
		refs: map[string][]int{},
	}
}

func (f *fakeOps) UpdateTarget(_ context.Context, code string, p store.TargetPatch, check store.SettingsCheck) (store.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	f.updateLastPatch = p
	t, ok := f.targets[code]
	if !ok {
		return t, store.ErrUnknownTarget
	}
	if p.Settings != nil {
		merged := maps.Clone(t.Settings)
		for k, v := range p.Settings {
			if v == "" {
				delete(merged, k)
			} else {
				merged[k] = v
			}
		}
		if check != nil {
			if err := check(t.Kind, merged); err != nil {
				return t, fmt.Errorf("%w: %v", store.ErrBadSettings, err)
			}
		}
		t.Settings = merged
	}
	if p.Enabled != nil {
		t.Enabled = *p.Enabled
	}
	f.targets[code] = t
	return t, nil
}

func (f *fakeOps) DeleteTarget(_ context.Context, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	t, ok := f.targets[code]
	if !ok {
		return store.ErrUnknownTarget
	}
	if t.Enabled {
		return store.ErrMachineEnabled
	}
	if refs := f.refs[code]; len(refs) > 0 {
		return &store.MachineInUseError{Code: code, Servers: refs}
	}
	delete(f.targets, code)
	return nil
}

func (f *fakeOps) RecordMetrics(_ context.Context, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, code)
	return f.metErr
}

func (f *fakeOps) MachineMetrics(_ context.Context, code string, from, to time.Time, step time.Duration) ([]store.MetricPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.targets[code]; !ok {
		return nil, store.ErrUnknownTarget
	}
	f.metCode, f.metFrom, f.metTo, f.metStep = code, from, to, step
	if f.points == nil {
		return []store.MetricPoint{}, nil
	}
	return f.points, nil
}

func (f *fakeOps) MachineEvents(_ context.Context, afterID int64, limit int) ([]store.MachineEvent, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.afterID, f.evLimit = afterID, limit
	var out []store.MachineEvent
	for _, e := range f.events {
		if e.ID > afterID {
			out = append(out, e)
		}
	}
	total := len(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}

func (f *fakeOps) PruneMachineOps(_ context.Context, m, e time.Time) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneM, f.pruneE = m, e
	return 0, 0, nil
}

func (f *fakeOps) DetectMachineEvents(_ context.Context, silent time.Duration) ([]store.MachineEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detected++
	f.detectSilent = silent
	return []store.MachineEvent{{ID: 1, Machine: "ps2", Kind: "down"}}, nil
}

func (f *fakeOps) MachineCheckInfo(_ context.Context, code string) (store.CheckTarget, []store.CheckEndpoint, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.targets[code]
	if !ok {
		return store.CheckTarget{}, nil, false, store.ErrUnknownTarget
	}
	ct := f.checkTarget
	ct.Code, ct.Kind, ct.Enabled = t.Code, t.Kind, t.Enabled
	return ct, f.checkEndpoints, f.checkTruncated, nil
}

// fakeProber - ответы проверок по имени хоста; block - ждать отмены контекста.
type fakeProber struct {
	block    bool
	mu       sync.Mutex
	tlsSNI   []string
	notAfter time.Time
}

func (p *fakeProber) DialTCP(ctx context.Context, addr string) error {
	if p.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if strings.HasPrefix(addr, "down.") {
		return errors.New("dial tcp: connection refused")
	}
	return nil
}

func (p *fakeProber) TLSHandshake(_ context.Context, addr, sni string) (time.Time, error) {
	p.mu.Lock()
	p.tlsSNI = append(p.tlsSNI, addr+"|"+sni)
	p.mu.Unlock()
	if strings.HasPrefix(sni, "badcert.") {
		return time.Time{}, errors.New("x509: certificate has expired")
	}
	return p.notAfter, nil
}

func opsServer(inRecovery bool) (*Server, *fakeOps, *fakeRevisions, *fakeAgents) {
	s, fr, fa := agentServer(inRecovery)
	fo := newFakeOps()
	s.deps.Ops = fo
	s.deps.Prober = &fakeProber{notAfter: time.Now().Add(30*24*time.Hour + time.Hour)}
	s.routesMachineOps()
	return s, fo, fr, fa
}

// Включение, выключение и настройки: каждый плохой ввод - точный 4xx и код.
func TestMachineOps_UpdateTarget(t *testing.T) {
	s, fo, fr, _ := opsServer(false)
	rec := do(s, "PUT", "/api/v1/targets/ps2", botToken, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("выключение: %d %s", rec.Code, rec.Body.String())
	}
	var got store.Target
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Enabled || got.Code != "ps2" {
		t.Fatalf("ответ выключения: %s", rec.Body.String())
	}
	if fr.published != 0 {
		t.Fatal("выключение не должно публиковать ревизию")
	}
	// Включение - выключенная машина догоняет пользователей новой ревизией.
	if rec := do(s, "PUT", "/api/v1/targets/ps2", botToken, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("включение: %d %s", rec.Code, rec.Body.String())
	}
	if fr.published != 1 {
		t.Fatalf("после включения ждали публикацию telemt.users, публикаций %d", fr.published)
	}
	// Настройки: слияние с текущими.
	rec = do(s, "PUT", "/api/v1/targets/ps2", botToken, `{"settings":{"address":"198.51.100.7"}}`)
	if rec.Code != http.StatusOK || fo.targets["ps2"].Settings[store.SettingAddress] != "198.51.100.7" ||
		fo.targets["ps2"].Settings[store.SettingBridgeSocks] == "" {
		t.Fatalf("смена адреса: %d %s %+v", rec.Code, rec.Body.String(), fo.targets["ps2"].Settings)
	}
	if rec := do(s, "PUT", "/api/v1/targets/pr2", botToken, `{"settings":{"web_domains":""}}`); rec.Code != http.StatusOK {
		t.Fatalf("удаление web_domains релея: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(s, "PUT", "/api/v1/targets/pan1", botToken, `{"settings":{"inbound_id":"7","node_id":"2","port":"2443","node_code":"ps2"}}`); rec.Code != http.StatusOK {
		t.Fatalf("настройки входа панели: %d %s", rec.Code, rec.Body.String())
	}

	for _, c := range []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"пустое тело", "/api/v1/targets/ps2", `{}`, 400, "bad_request"},
		{"не JSON", "/api/v1/targets/ps2", `{"enabled":`, 400, "bad_request"},
		{"опечатка в поле", "/api/v1/targets/ps2", `{"enable":true}`, 400, "bad_request"},
		{"enabled строкой", "/api/v1/targets/ps2", `{"enabled":"yes"}`, 400, "bad_request"},
		{"настройка числом", "/api/v1/targets/ps2", `{"settings":{"address":1}}`, 400, "bad_request"},
		{"пустой ключ", "/api/v1/targets/ps2", `{"settings":{"":"x"}}`, 400, "bad_request"},
		{"чужая настройка выхода", "/api/v1/targets/ps2", `{"settings":{"web_domains":"a.example.com"}}`, 400, "bad_setting"},
		{"неизвестная настройка", "/api/v1/targets/pr2", `{"settings":{"color":"red"}}`, 400, "bad_setting"},
		{"плохой адрес", "/api/v1/targets/ps2", `{"settings":{"address":"127.0.0.1"}}`, 400, "bad_setting"},
		{"плохой socks", "/api/v1/targets/ps2", `{"settings":{"bridge_socks":"http://x"}}`, 400, "bad_setting"},
		{"удаление адреса", "/api/v1/targets/ps2", `{"settings":{"address":""}}`, 400, "bad_setting"},
		{"порт панели", "/api/v1/targets/pan1", `{"settings":{"port":"70000"}}`, 400, "bad_setting"},
		{"node_code панели", "/api/v1/targets/pan1", `{"settings":{"node_code":"Bad Code"}}`, 400, "bad_setting"},
		{"нет машины", "/api/v1/targets/nobody", `{"enabled":false}`, 404, "not_found"},
		{"код не по формату", "/api/v1/targets/BAD", `{"enabled":false}`, 404, "not_found"},
		{"большое тело", "/api/v1/targets/ps2", `{"settings":{"address":"` + strings.Repeat("a", maxTargetPatchBody) + `"}}`, 413, "payload_too_large"},
	} {
		rec := do(s, "PUT", c.path, botToken, c.body)
		if rec.Code != c.status || decodeErr(t, rec).Code != c.code {
			t.Fatalf("%s: ждали %d %s, получили %d %s", c.name, c.status, c.code, rec.Code, rec.Body.String())
		}
	}
}

// Удаление: включённую - 409 machine_enabled, с сервером - 409 machine_in_use с номерами.
func TestMachineOps_DeleteTarget(t *testing.T) {
	s, fo, _, _ := opsServer(false)
	rec := do(s, "DELETE", "/api/v1/targets/ps2", botToken, "")
	if rec.Code != http.StatusConflict || decodeErr(t, rec).Code != "machine_enabled" {
		t.Fatalf("включённая: %d %s", rec.Code, rec.Body.String())
	}
	do(s, "PUT", "/api/v1/targets/ps2", botToken, `{"enabled":false}`)
	fo.refs["ps2"] = []int{3, 7}
	rec = do(s, "DELETE", "/api/v1/targets/ps2", botToken, "")
	e := decodeErr(t, rec)
	if rec.Code != http.StatusConflict || e.Code != "machine_in_use" || !strings.Contains(e.Message, "3, 7") {
		t.Fatalf("с серверами: %d %s", rec.Code, rec.Body.String())
	}
	delete(fo.refs, "ps2")
	if rec := do(s, "DELETE", "/api/v1/targets/ps2", botToken, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("удаление: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(s, "DELETE", "/api/v1/targets/ps2", botToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("повторное удаление: %d", rec.Code)
	}
	if rec := do(s, "DELETE", "/api/v1/targets/BAD", botToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("код не по формату: %d", rec.Code)
	}
}

// Доступ: без токена - 401; на реплике запись и проверка - 409 not_master без
// обращения к хранилищу, чтение работает.
func TestMachineOps_AuthAndMaster(t *testing.T) {
	s, _, _, _ := opsServer(false)
	for _, c := range [][3]string{
		{"PUT", "/api/v1/targets/ps2", `{"enabled":false}`},
		{"DELETE", "/api/v1/targets/ps2", ""},
		{"POST", "/api/v1/machines/ps2/check", ""},
		{"GET", "/api/v1/machines/ps2/metrics", ""},
		{"GET", "/api/v1/events", ""},
	} {
		if rec := do(s, c[0], c[1], "", c[2]); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s без токена: %d", c[0], c[1], rec.Code)
		}
	}
	replica, fo, _, _ := opsServer(true)
	for _, c := range [][3]string{
		{"PUT", "/api/v1/targets/ps2", `{"enabled":false}`},
		{"DELETE", "/api/v1/targets/ps2", ""},
		{"POST", "/api/v1/machines/ps2/check", ""},
	} {
		rec := do(replica, c[0], c[1], botToken, c[2])
		if rec.Code != http.StatusConflict || decodeErr(t, rec).Code != "not_master" {
			t.Fatalf("%s %s на реплике: %d %s", c[0], c[1], rec.Code, rec.Body.String())
		}
	}
	if fo.writes != 0 {
		t.Fatal("запись на реплике дошла до хранилища")
	}
	for _, p := range []string{"/api/v1/machines/ps2/metrics", "/api/v1/events"} {
		if rec := do(replica, "GET", p, botToken, ""); rec.Code != http.StatusOK {
			t.Fatalf("чтение %s на реплике: %d", p, rec.Code)
		}
	}
}

// Проверка связи: TCP ко всем точкам, TLS с SNI=host для web, ошибки сети - в ответе.
func TestMachineOps_Check(t *testing.T) {
	s, fo, _, _ := opsServer(false)
	seen := time.Now().Add(-30 * time.Second)
	fo.checkTarget = store.CheckTarget{SeenAt: &seen}
	fo.checkEndpoints = []store.CheckEndpoint{
		{ServerNo: 1, ServerName: "Финляндия", EndpointID: 10, Protocol: "ee", Host: "ok.example.com", Port: 2091, ViaRelay: true},
		{ServerNo: 1, ServerName: "Финляндия", EndpointID: 11, Protocol: "dd", Host: "down.example.com", Port: 2091, ViaRelay: true},
		{ServerNo: 2, ServerName: "Нидерланды", EndpointID: 12, Protocol: "web", Host: "web.example.com", Port: 8443, ViaRelay: true},
		{ServerNo: 2, ServerName: "Нидерланды", EndpointID: 13, Protocol: "web", Host: "badcert.example.com", ViaRelay: true},
	}
	rec := do(s, "POST", "/api/v1/machines/pr2/check", botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("проверка: %d %s", rec.Code, rec.Body.String())
	}
	var res machineCheck
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || !res.Agent.Fresh || res.Agent.Silent || res.Agent.AgeSeconds == nil || len(res.Endpoints) != 4 {
		t.Fatalf("сводка: %+v", res)
	}
	ok, down, web, bad := res.Endpoints[0], res.Endpoints[1], res.Endpoints[2], res.Endpoints[3]
	if !ok.TCP.OK || ok.TLS != nil {
		t.Fatalf("ee: %+v", ok)
	}
	if down.TCP.OK || !strings.Contains(down.TCP.Error, "refused") {
		t.Fatalf("недоступная точка: %+v", down)
	}
	if web.Port != 443 || !web.TCP.OK || web.TLS == nil || !web.TLS.OK || web.TLS.DaysLeft == nil || *web.TLS.DaysLeft != 30 {
		t.Fatalf("web: %+v %+v", web, web.TLS)
	}
	if bad.TLS == nil || bad.TLS.OK || !strings.Contains(bad.TLS.Error, "expired") {
		t.Fatalf("web с плохим сертификатом: %+v", bad.TLS)
	}
	p := s.deps.Prober.(*fakeProber)
	if len(p.tlsSNI) != 2 || !strings.Contains(strings.Join(p.tlsSNI, ","), "web.example.com:443|web.example.com") {
		t.Fatalf("TLS: адрес и SNI: %v", p.tlsSNI)
	}

	// Агент не отчитывался: не свежий и молчит.
	fo.checkTarget = store.CheckTarget{}
	fo.checkEndpoints = nil
	rec = do(s, "POST", "/api/v1/machines/pr2/check", botToken, "")
	res = machineCheck{}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || res.OK || res.Agent.Fresh || !res.Agent.Silent || res.Endpoints == nil {
		t.Fatalf("без агента: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(s, "POST", "/api/v1/machines/nobody/check", botToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("нет машины: %d", rec.Code)
	}
}

// Общий бюджет: зависшие точки обрываются по таймауту, до остальных очередь не
// доходит - ответ всё равно 200 с ошибками, а не 5xx.
func TestMachineOps_CheckBudget(t *testing.T) {
	oldPer, oldBudget := checkPerEndpoint, checkBudget
	checkPerEndpoint, checkBudget = 100*time.Millisecond, 150*time.Millisecond
	defer func() { checkPerEndpoint, checkBudget = oldPer, oldBudget }()
	s, fo, _, _ := opsServer(false)
	s.deps.Prober = &fakeProber{block: true}
	for i := range 3 * checkParallel {
		fo.checkEndpoints = append(fo.checkEndpoints, store.CheckEndpoint{ServerNo: 1, EndpointID: int64(i), Protocol: "dd", Host: "hang.example.com", Port: 443})
	}
	start := time.Now()
	rec := do(s, "POST", "/api/v1/machines/pr2/check", botToken, "")
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("проверка вышла за бюджет: %v", el)
	}
	var res machineCheck
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &res) != nil || len(res.Endpoints) != 3*checkParallel {
		t.Fatalf("ответ: %d %s", rec.Code, rec.Body.String())
	}
	skipped := 0
	for _, e := range res.Endpoints {
		if e.TCP.OK || e.TCP.Error == "" {
			t.Fatalf("зависшая точка без ошибки: %+v", e)
		}
		if strings.Contains(e.TCP.Error, "бюджет") {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatal("ждали точки, до которых не дошла очередь")
	}
}

// История нагрузки: окно и шаг по умолчанию, плохой ввод - точный 400, нет машины - 404.
func TestMachineOps_Metrics(t *testing.T) {
	s, fo, _, _ := opsServer(false)
	cpu := 12.5
	fo.points = []store.MetricPoint{{At: time.Now().UTC().Truncate(time.Minute), CPU: &cpu, Connections: 3}}
	rec := do(s, "GET", "/api/v1/machines/ps2/metrics", botToken, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"step":"5m"`) || !strings.Contains(rec.Body.String(), `"cpu":12.5`) {
		t.Fatalf("по умолчанию: %d %s", rec.Code, rec.Body.String())
	}
	if fo.metStep != 5*time.Minute || fo.metTo.Sub(fo.metFrom) != 24*time.Hour || time.Since(fo.metTo) > time.Minute {
		t.Fatalf("окно по умолчанию: %v..%v шаг %v", fo.metFrom, fo.metTo, fo.metStep)
	}
	rec = do(s, "GET", "/api/v1/machines/ps2/metrics?from=2026-11-18T00:00:00Z&to=2026-11-19T00:00:00Z&step=1m", botToken, "")
	if rec.Code != http.StatusOK || fo.metStep != time.Minute || !fo.metFrom.Equal(time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("окно из запроса: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct {
		query  string
		status int
		code   string
	}{
		{"?step=10m", 400, "bad_request"},
		{"?from=yesterday", 400, "bad_request"},
		{"?to=2026-11-19", 400, "bad_request"},
		{"?from=2026-11-19T00:00:00Z&to=2026-11-19T00:00:00Z", 400, "bad_request"},
		{"?from=2026-11-19T01:00:00Z&to=2026-11-19T00:00:00Z", 400, "bad_request"},
		{"?from=2026-11-16T00:00:00Z&to=2026-11-19T00:00:00Z&step=1m", 400, "too_many_points"},
		{"?from=2025-11-19T00:00:00Z&to=2026-11-19T00:00:00Z&step=1h", 400, "too_many_points"},
	} {
		rec := do(s, "GET", "/api/v1/machines/ps2/metrics"+c.query, botToken, "")
		if rec.Code != c.status || decodeErr(t, rec).Code != c.code {
			t.Fatalf("%s: ждали %d %s, получили %d %s", c.query, c.status, c.code, rec.Code, rec.Body.String())
		}
	}
	// Ровно 2000 точек - можно.
	if rec := do(s, "GET", "/api/v1/machines/ps2/metrics?from=2026-11-18T00:00:00Z&to=2026-11-19T09:20:00Z&step=1m", botToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("2000 точек: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(s, "GET", "/api/v1/machines/nobody/metrics", botToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("нет машины: %d", rec.Code)
	}
}

// Лента событий: курсор after_id, limit по умолчанию 100, больше 500 - 400.
func TestMachineOps_Events(t *testing.T) {
	s, fo, _, _ := opsServer(false)
	for i := int64(1); i <= 5; i++ {
		fo.events = append(fo.events, store.MachineEvent{ID: i, At: time.Now().UTC(), Machine: "ps2", Kind: "down"})
	}
	rec := do(s, "GET", "/api/v1/events", botToken, "")
	if rec.Code != http.StatusOK || fo.evLimit != 100 || fo.afterID != 0 || !strings.Contains(rec.Body.String(), `"total":5`) {
		t.Fatalf("по умолчанию: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(s, "GET", "/api/v1/events?after_id=3&limit=1", botToken, "")
	var body struct {
		Data   []store.MachineEvent `json:"data"`
		Paging map[string]int       `json:"paging"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("курсор: %d %s", rec.Code, rec.Body.String())
	}
	if len(body.Data) != 1 || body.Data[0].ID != 4 || body.Paging["total"] != 2 || body.Paging["limit"] != 1 {
		t.Fatalf("курсор: %+v", body)
	}
	for _, q := range []string{"?limit=501", "?limit=0", "?limit=x", "?after_id=-1", "?after_id=x"} {
		rec := do(s, "GET", "/api/v1/events"+q, botToken, "")
		if rec.Code != http.StatusBadRequest || decodeErr(t, rec).Code != "bad_request" {
			t.Fatalf("%s: ждали 400, получили %d %s", q, rec.Code, rec.Body.String())
		}
	}
}

// Runtime-отчёт пишет строку истории нагрузки; сбой истории не валит отчёт.
func TestMachineOps_RuntimeRecordsMetrics(t *testing.T) {
	s, fo, _, fa := opsServer(false)
	if rec := do(s, "POST", "/v1/runtime", pr2Token, runtimeOK); rec.Code != http.StatusNoContent {
		t.Fatalf("отчёт: %d %s", rec.Code, rec.Body.String())
	}
	if len(fo.recorded) != 1 || fo.recorded[0] != "pr2" {
		t.Fatalf("история не записана: %v", fo.recorded)
	}
	fo.metErr = errors.New("база")
	if rec := do(s, "POST", "/v1/runtime", pr2Token, runtimeOK); rec.Code != http.StatusNoContent {
		t.Fatalf("сбой истории не должен валить отчёт: %d", rec.Code)
	}
	if len(fa.runtime) != 2 {
		t.Fatalf("снимков %d, ждали 2", len(fa.runtime))
	}
}

// Выключенная машина: новых ревизий не получает (ожидание и 304), но отчитывается.
func TestMachineOps_DisabledAgent(t *testing.T) {
	s, _, fr, fa := opsServer(false)
	fa.disabled = map[string]bool{"pr2": true}
	fr.rev = 5
	start := time.Now()
	rec := do(s, "GET", "/v1/desired?target=pr2&rev=1&wait=150ms", pr2Token, "")
	if rec.Code != http.StatusNotModified {
		t.Fatalf("выключенная машина получила бандл: %d %s", rec.Code, rec.Body.String())
	}
	if el := time.Since(start); el < 140*time.Millisecond {
		t.Fatalf("304 без паузы (%v): агент крутил бы опрос", el)
	}
	body := `{"target":"pr2","resources":[{"resource":"telemt.users","revision":3,"status":"applied"}]}`
	if rec := do(s, "POST", "/v1/observed", pr2Token, body); rec.Code != http.StatusNoContent {
		t.Fatalf("отчёт выключенной машины: %d", rec.Code)
	}
	if rec := do(s, "POST", "/v1/runtime", pr2Token, runtimeOK); rec.Code != http.StatusNoContent {
		t.Fatalf("runtime выключенной машины: %d", rec.Code)
	}
	// Включённая машина того же агента получает бандл сразу.
	fa.disabled = nil
	if rec := do(s, "GET", "/v1/desired?target=pr2&rev=1&wait=5s", pr2Token, ""); rec.Code != http.StatusOK {
		t.Fatalf("включённая машина: %d", rec.Code)
	}
}

// Детектор тревог и чистка работают только на мастере, с порогом silent_after и сроками хранения.
func TestMachineOps_LoopsOnlyOnMaster(t *testing.T) {
	replica, fo, _, fa := opsServer(true)
	replica.alertsOnce(context.Background())
	replica.pruneRuntime(context.Background())
	if fo.detected != 0 || !fo.pruneM.IsZero() || fa.pruned != 0 {
		t.Fatal("реплика не должна писать тревоги и чистить")
	}
	s, fo, _, _ := opsServer(false)
	s.alertsOnce(context.Background())
	if fo.detected != 1 || fo.detectSilent != store.SilentAfter {
		t.Fatalf("детектор: %d проходов, порог %v", fo.detected, fo.detectSilent)
	}
	s.pruneRuntime(context.Background())
	if d := time.Since(fo.pruneM); d < 8*24*time.Hour-time.Minute || d > 8*24*time.Hour+time.Minute {
		t.Fatalf("история нагрузки чистится не по 8 дням: %v", d)
	}
	if d := time.Since(fo.pruneE); d < 30*24*time.Hour-time.Minute || d > 30*24*time.Hour+time.Minute {
		t.Fatalf("события чистятся не по 30 дням: %v", d)
	}
}

// Ответы и ошибки ручек этапа C совпадают со спекой.
func TestContract_MachineOps(t *testing.T) {
	_, router := loadSpec(t)
	s, fo, _, _ := opsServer(false)
	seen := time.Now().Add(-10 * time.Second)
	fo.checkTarget = store.CheckTarget{SeenAt: &seen}
	fo.checkEndpoints = []store.CheckEndpoint{
		{ServerNo: 1, ServerName: "Финляндия", EndpointID: 10, Protocol: "ee", Host: "ok.example.com", Port: 2091, ViaRelay: true},
		{ServerNo: 1, ServerName: "Финляндия", EndpointID: 11, Protocol: "web", Host: "badcert.example.com", ViaRelay: true},
		{ServerNo: 1, ServerName: "Финляндия", EndpointID: 12, Protocol: "web", Host: "web.example.com", ViaRelay: true},
	}
	cpu := 1.5
	fo.points = []store.MetricPoint{{At: time.Now().UTC(), CPU: &cpu}}
	fo.events = []store.MachineEvent{{ID: 1, At: time.Now().UTC(), Machine: "ps2", Kind: "apply_failed", Detail: "telemt.users, ревизия 3"}}
	checkContract(t, router, s, "PUT", "/api/v1/targets/ps2", `{"enabled":false}`, http.StatusOK)
	checkContract(t, router, s, "PUT", "/api/v1/targets/ps2", `{"settings":{"color":"red"}}`, http.StatusBadRequest)
	checkContract(t, router, s, "PUT", "/api/v1/targets/nobody", `{"enabled":false}`, http.StatusNotFound)
	checkContract(t, router, s, "POST", "/api/v1/machines/ps2/check", "", http.StatusOK)
	checkContract(t, router, s, "POST", "/api/v1/machines/nobody/check", "", http.StatusNotFound)
	checkContract(t, router, s, "GET", "/api/v1/machines/ps2/metrics?step=1h", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/machines/ps2/metrics?from=2026-11-01T00:00:00Z&to=2026-11-19T00:00:00Z&step=1m", "", http.StatusBadRequest)
	checkContract(t, router, s, "GET", "/api/v1/machines/nobody/metrics", "", http.StatusNotFound)
	checkContract(t, router, s, "GET", "/api/v1/events?after_id=0&limit=10", "", http.StatusOK)
	checkContract(t, router, s, "DELETE", "/api/v1/targets/pr2", "", http.StatusConflict)
	fo.refs["ps2"] = []int{1}
	checkContract(t, router, s, "DELETE", "/api/v1/targets/ps2", "", http.StatusConflict)
	delete(fo.refs, "ps2")
	checkContract(t, router, s, "DELETE", "/api/v1/targets/ps2", "", http.StatusNoContent)
	checkContract(t, router, s, "DELETE", "/api/v1/targets/ps2", "", http.StatusNotFound)
}

// Проверка настроек машины по виду - как при добавлении.
func TestCheckTargetSettings(t *testing.T) {
	for _, c := range []struct {
		kind  string
		patch map[string]string
		ok    bool
	}{
		{"exit", map[string]string{"address": "203.0.113.10", "bridge_socks": "socks5://127.0.0.1:1080"}, true},
		{"exit", map[string]string{"address": "203.0.113.010"}, false},
		{"exit", map[string]string{"bridge_socks": "socks5://127.0.0.1:0"}, false},
		{"exit", map[string]string{"bridge_socks": ""}, false},
		{"relay", map[string]string{"web_domains": "a.example.com"}, true},
		{"relay", map[string]string{"web_domains": ""}, true},
		{"relay", map[string]string{"address": "203.0.113.10"}, true},
		{"relay", map[string]string{"address": ""}, true},
		{"relay", map[string]string{"bridge_socks": "socks5://127.0.0.1:1080"}, false},
		{"panel", map[string]string{"inbound_id": "0"}, false},
		{"panel", map[string]string{"node_id": "x"}, false},
		{"panel", map[string]string{"port": "443", "node_code": "ps1"}, true},
		{"other", map[string]string{"x": "1"}, false},
	} {
		if err := checkTargetSettings(c.kind, c.patch); (err == nil) != c.ok {
			t.Fatalf("%s %v: ждали ok=%v, получили %v", c.kind, c.patch, c.ok, err)
		}
	}
}
