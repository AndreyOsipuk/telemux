package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/render"
	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

// fakeRender - сборка в памяти: строки сравнения и источники по серверам.
type fakeRender struct {
	mu      sync.Mutex
	input   store.RenderInput
	saved   []store.RenderRow
	rows    map[int][]store.RenderStatus
	sources map[int]string
	servers []store.Server // для TelemuxServersOfMachine
}

func (f *fakeRender) RenderInput(context.Context) (store.RenderInput, error) { return f.input, nil }

func (f *fakeRender) SaveRender(_ context.Context, rows []store.RenderRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = rows
	return nil
}

func (f *fakeRender) ListRender(_ context.Context, no int) ([]store.RenderStatus, error) {
	if _, ok := f.sources[no]; !ok {
		return nil, fmt.Errorf("%w: %d", store.ErrServerNotFound, no)
	}
	return f.rows[no], nil
}

func (f *fakeRender) SetConfigSource(_ context.Context, no int, source string, force bool) (store.Server, error) {
	if _, ok := f.sources[no]; !ok {
		return store.Server{}, fmt.Errorf("%w: %d", store.ErrServerNotFound, no)
	}
	if source == store.ConfigSourceTelemux && !force {
		rows := f.rows[no]
		if len(rows) == 0 {
			return store.Server{}, store.ErrRenderMismatch
		}
		for _, r := range rows {
			if !r.Equal || r.Error != "" {
				return store.Server{}, store.ErrRenderMismatch
			}
		}
	}
	f.sources[no] = source
	return store.Server{No: no, Name: "Финляндия", Engine: "telemt", State: "active", ConfigSource: source,
		ExtraTLSDomains: []string{}, Settings: map[string]string{}, Endpoints: []store.Endpoint{}}, nil
}

func (f *fakeRender) TelemuxServersOfMachine(_ context.Context, target string) ([]store.Server, error) {
	var out []store.Server
	for _, srv := range f.servers {
		if srv.ConfigSource == store.ConfigSourceTelemux && ((srv.RelayCode != nil && *srv.RelayCode == target) ||
			(srv.NodeCode != nil && *srv.NodeCode == target)) {
			out = append(out, srv)
		}
	}
	return out, nil
}

func strp(s string) *string { return &s }

func renderServer(inRecovery bool) (*Server, *fakeRender, *fakeRevisions) {
	fu := newFakeUsers(0)
	fr := &fakeRevisions{users: fu, status: map[int64][]store.TargetStatus{}}
	frd := &fakeRender{sources: map[int]string{1: "bot", 2: "bot"}, rows: map[int][]store.RenderStatus{}}
	s := New(Deps{
		Store: fakeStore{inRecovery: inRecovery}, Node: &fakeNode{}, Version: "v1",
		SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}, BotToken: botToken,
		AdminUser: "admin", AdminPassword: "pw",
		Users: fu, Revisions: fr, Servers: newFakeServers(), Render: frd,
	})
	return s, frd, fr
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("ответ ошибки не JSON: %s", body)
	}
	return e.Code
}

// На telemux - только если сборка совпала по всем машинам; force - всегда; обратно - всегда.
func TestRender_ConfigSource(t *testing.T) {
	s, frd, _ := renderServer(false)
	put := func(no int, body string) (int, []byte) {
		rec := do(s, "PUT", fmt.Sprintf("/api/v1/servers/%d/config-source", no), botToken, body)
		return rec.Code, rec.Body.Bytes()
	}
	// Сравнения ещё не было.
	if code, body := put(1, `{"source":"telemux"}`); code != http.StatusConflict || errCode(t, body) != "render_mismatch" {
		t.Fatalf("без сравнения: %d %s", code, body)
	}
	frd.rows[1] = []store.RenderStatus{
		{RenderRow: store.RenderRow{Target: "pr1", Resource: "telemt.config", Equal: true}},
		{RenderRow: store.RenderRow{Target: "ps1", Resource: "telemt.config", Equal: false}},
	}
	if code, body := put(1, `{"source":"telemux"}`); code != http.StatusConflict || errCode(t, body) != "render_mismatch" {
		t.Fatalf("расхождение: %d %s", code, body)
	}
	if frd.sources[1] != "bot" {
		t.Fatal("при расхождении источник сменился")
	}
	if code, body := put(1, `{"source":"telemux","force":true}`); code != http.StatusOK {
		t.Fatalf("force: %d %s", code, body)
	}
	if frd.sources[1] != "telemux" {
		t.Fatal("force не переключил")
	}
	if code, body := put(1, `{"source":"bot"}`); code != http.StatusOK || frd.sources[1] != "bot" {
		t.Fatalf("обратно на bot: %d %s", code, body)
	}
	frd.rows[2] = []store.RenderStatus{{RenderRow: store.RenderRow{Target: "pr1", Resource: "telemt.config", Equal: true}}}
	if code, body := put(2, `{"source":"telemux"}`); code != http.StatusOK || frd.sources[2] != "telemux" {
		t.Fatalf("совпало: %d %s", code, body)
	}
	for _, c := range []struct {
		no     int
		body   string
		status int
	}{
		{1, `{"source":"panel"}`, http.StatusBadRequest},
		{1, `{`, http.StatusBadRequest},
		{9, `{"source":"bot"}`, http.StatusNotFound},
	} {
		if code, body := put(c.no, c.body); code != c.status {
			t.Fatalf("%d %s: ждали %d, получили %d %s", c.no, c.body, c.status, code, body)
		}
	}
	if rec := do(s, "PUT", "/api/v1/servers/abc/config-source", botToken, `{"source":"bot"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("номер не число: %d", rec.Code)
	}
	if rec := do(s, "PUT", "/api/v1/servers/1/config-source", "", `{"source":"bot"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", rec.Code)
	}
}

// Запись источника - только на мастере.
func TestRender_ConfigSourceReplica(t *testing.T) {
	s, frd, _ := renderServer(true)
	frd.rows[1] = []store.RenderStatus{{RenderRow: store.RenderRow{Target: "pr1", Resource: "telemt.config", Equal: true}}}
	rec := do(s, "PUT", "/api/v1/servers/1/config-source", botToken, `{"source":"telemux","force":true}`)
	if rec.Code != http.StatusConflict || frd.sources[1] != "bot" {
		t.Fatalf("реплика: %d %s", rec.Code, rec.Body.String())
	}
	// Чтение статуса на реплике работает.
	if rec := do(s, "GET", "/api/v1/servers/1/render", botToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("статус на реплике: %d", rec.Code)
	}
}

// Статус: diff без секретов, ошибка - null или текст.
func TestRender_Status(t *testing.T) {
	s, frd, _ := renderServer(false)
	cur := "debug = false\n\n[secrets]\n\"alice_1\" = \"ee0123456789abcdef0123456789abcdef\"\n"
	cand := "debug = false\n\n[secrets]\n\"alice_1\" = \"eefedcba9876543210fedcba9876543210\"\n"
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	frd.rows[1] = []store.RenderStatus{
		{RenderRow: store.RenderRow{Target: "ps1", Resource: "mtg.config", CandidateSHA: strp("c"), Candidate: &cand,
			CurrentSHA: strp("a"), RenderedAt: at}, Current: &cur},
		{RenderRow: store.RenderRow{Target: "pr1", Resource: "mtg.config", Error: "no_address: у машины pr1 не задан адрес", RenderedAt: at}},
	}
	rec := do(s, "GET", "/api/v1/servers/1/render", botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("статус: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "0123456789abcdef") || strings.Contains(body, "fedcba98") {
		t.Fatalf("секрет в ответе: %s", body)
	}
	var resp struct {
		Data []renderItem `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Data) != 2 {
		t.Fatalf("ответ: %v %s", err, body)
	}
	d := resp.Data[0]
	if d.Error != nil || d.Equal || !strings.Contains(d.Diff, "+\"alice_1\" = <secret changed>") || !strings.Contains(d.Diff, "-\"alice_1\" = <secret>") {
		t.Fatalf("строка ps1: %+v", d)
	}
	if e := resp.Data[1]; e.Error == nil || !strings.HasPrefix(*e.Error, "no_address") || e.Diff != "" || e.CandidateSHA != nil {
		t.Fatalf("строка pr1: %+v", e)
	}
	if rec := do(s, "GET", "/api/v1/servers/9/render", botToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("нет сервера: %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/v1/servers/1/render", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", rec.Code)
	}
}

// Ресурс, который собирает сервер на telemux, бот не публикует: 409 managed_by_telemux.
func TestRender_ManagedByTelemux(t *testing.T) {
	s, frd, fr := renderServer(false)
	frd.servers = []store.Server{
		{No: 1, Engine: "telemt", State: "active", ConfigSource: "telemux", RelayCode: strp("pr2"), NodeCode: strp("ps9")},
		{No: 2, Engine: "mtg", State: "active", ConfigSource: "telemux", RelayCode: strp("pr3"), NodeCode: strp("ps3"),
			Settings: map[string]string{}},
	}
	rec := do(s, "PUT", "/api/v1/targets/pr2/resources/telemt.config", botToken, "include = \"/etc/telemt/users.toml\"\n")
	if rec.Code != http.StatusConflict || errCode(t, rec.Body.Bytes()) != "managed_by_telemux" {
		t.Fatalf("telemt.config релея telemux-сервера: %d %s", rec.Code, rec.Body.String())
	}
	// mtg.config у pr2 никто из telemux не собирает; у pr3 - mtg без local-mtg, релей не наш.
	for _, path := range []string{"/api/v1/targets/pr2/resources/mtg.config", "/api/v1/targets/pr3/resources/mtg.config"} {
		if rec := do(s, "PUT", path, botToken, "[secrets]\n"); rec.Code != http.StatusOK {
			t.Fatalf("%s: ждали 200, получили %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := do(s, "PUT", "/api/v1/targets/ps3/resources/mtg.config", botToken, "[secrets]\n"); rec.Code != http.StatusConflict {
		t.Fatalf("mtg.config выхода telemux-сервера: %d", rec.Code)
	}
	if fr.published != 2 {
		t.Fatalf("опубликовано %d, ждали 2", fr.published)
	}
}

// Цикл: telemux-сервер публикует при расхождении, bot - только сравнивает; ресурс
// не назначен, нет адреса, общая машина - ошибки без публикации.
func TestRender_Loop(t *testing.T) {
	s, frd, fr := renderServer(false)
	tm := func(no int, source, relay, node string) store.Server {
		return store.Server{No: no, Engine: "telemt", State: "active", ConfigSource: source, TLSDomain: "www.example.com",
			RelayCode: strp(relay), NodeCode: strp(node), Settings: map[string]string{}}
	}
	machine := func(code, kind, addr string, res ...string) store.RenderMachine {
		return store.RenderMachine{Code: code, Kind: kind, Address: addr, Enabled: true, Resources: res}
	}
	frd.input = store.RenderInput{
		Servers: []store.Server{tm(1, "telemux", "pr1", "ps1"), tm(2, "bot", "pr2", "ps2"), tm(3, "telemux", "pr3", "ps3"),
			tm(4, "telemux", "pr4", "ps4"), tm(5, "telemux", "pr4", "ps5"), tm(6, "telemux", "pr6", "off1")},
		Machines: map[string]store.RenderMachine{
			"pr1": machine("pr1", "relay", "198.51.100.1", "telemt.config"),
			"ps1": machine("ps1", "exit", "192.0.2.1", "telemt.config"),
			"pr2": machine("pr2", "relay", "198.51.100.2", "telemt.config"),
			"ps2": machine("ps2", "exit", "192.0.2.2", "telemt.config"),
			"pr3": machine("pr3", "relay", "", "telemt.config"),
			"ps3": machine("ps3", "exit", "192.0.2.3"),
			"pr4": machine("pr4", "relay", "198.51.100.4", "telemt.config"),
			"ps4": machine("ps4", "exit", "192.0.2.4", "telemt.config"),
			"ps5": machine("ps5", "exit", "192.0.2.5", "telemt.config"),
			// pr6 выключена в базе; off1 выключили между чтением и публикацией.
			"pr6":  {Code: "pr6", Kind: "relay", Address: "198.51.100.6", Enabled: false, Resources: []string{"telemt.config"}},
			"off1": machine("off1", "exit", "192.0.2.6", "telemt.config"),
		},
		Current: map[string]string{},
	}
	// ps1 уже совпадает - публиковать нечего.
	srv1 := frd.input.Servers[0]
	ps1, err := s.renderJob(srv1, render.Job{Target: "ps1", Kind: "exit", Resource: "telemt.config"}, frd.input.Machines, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	frd.input.Current[store.CurrentKey("ps1", "telemt.config")] = telemtcfg.SHA256(ps1)

	s.renderOnce(context.Background())
	byKey := map[string]store.RenderRow{}
	for _, r := range frd.saved {
		byKey[fmt.Sprintf("%d/%s", r.ServerNo, r.Target)] = r
	}
	if len(frd.saved) != 12 {
		t.Fatalf("строк %d, ждали 12: %+v", len(frd.saved), frd.saved)
	}
	check := func(key string, equal bool, errPrefix string) {
		t.Helper()
		r := byKey[key]
		if r.Equal != equal || !strings.HasPrefix(r.Error, errPrefix) || (errPrefix == "" && r.Error != "") {
			t.Fatalf("%s: equal=%v error=%q", key, r.Equal, r.Error)
		}
	}
	check("1/pr1", true, "") // опубликован
	check("1/ps1", true, "") // уже совпадал
	check("2/pr2", false, "")
	check("2/ps2", false, "")
	check("3/pr3", false, "no_address")
	check("3/ps3", false, "not_assigned")
	check("4/pr4", false, "shared_machine")
	check("5/pr4", false, "shared_machine")
	check("4/ps4", true, "")
	check("5/ps5", true, "")
	check("6/pr6", false, "machine_disabled")
	check("6/off1", false, "machine_disabled")
	if byKey["3/ps3"].Candidate == nil {
		t.Fatal("не назначенный ресурс: кандидат должен собраться для diff")
	}
	// Опубликованы pr1, ps4, ps5: bot-сервер, ошибки и уже совпавший - нет.
	if fr.published != 3 {
		t.Fatalf("публикаций %d, ждали 3", fr.published)
	}
}

// Ответы ручек сборки совпадают со спекой.
func TestContract_Render(t *testing.T) {
	_, router := loadSpec(t)
	s, frd, _ := renderServer(false)
	cand, cur := "a = 1\n", "a = 2\n"
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	frd.rows[1] = []store.RenderStatus{
		{RenderRow: store.RenderRow{Target: "ps1", Resource: "telemt.config", CandidateSHA: strp("c"), Candidate: &cand,
			CurrentSHA: strp("a"), RenderedAt: at}, Current: &cur},
		{RenderRow: store.RenderRow{Target: "pr1", Resource: "telemt.config", Error: "no_address: нет адреса", RenderedAt: at}},
	}
	checkContract(t, router, s, "GET", "/api/v1/servers/1/render", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/servers/9/render", "", http.StatusNotFound)
	checkContract(t, router, s, "PUT", "/api/v1/servers/1/config-source", `{"source":"telemux"}`, http.StatusConflict)
	checkContract(t, router, s, "PUT", "/api/v1/servers/1/config-source", `{"source":"telemux","force":true}`, http.StatusOK)
	checkContract(t, router, s, "PUT", "/api/v1/servers/1/config-source", `{"source":"bot"}`, http.StatusOK)
}

// Два local-mtg одного релея выпускают независимые ревизии для разных процессов.
// При смене источника только одного из них бот по-прежнему может вести соседний.
func TestRender_SecondaryMtgRelay(t *testing.T) {
	s, frd, fr := renderServer(false)
	const secondary = "mtg.secondary.config"
	main := store.Server{No: 5, Engine: "mtg", TLSDomain: "five.example.com", State: "active",
		ConfigSource: store.ConfigSourceBot, NodeCode: strp("ps5"), RelayCode: strp("pr1"),
		Settings: map[string]string{"RELAY_MODE": "local-mtg"}}
	second := store.Server{No: 6, Engine: "mtg", TLSDomain: "six.example.com", State: "active",
		ConfigSource: store.ConfigSourceTelemux, NodeCode: strp("ps6"), RelayCode: strp("pr1"),
		Settings: map[string]string{"RELAY_MODE": "local-mtg", "TELEMUX_RELAY_RESOURCE": secondary,
			"RELAY_PROMETHEUS_BIND": "127.0.0.1:3130"}}
	frd.servers = []store.Server{main, second}
	in := store.RenderInput{Servers: frd.servers, Machines: map[string]store.RenderMachine{
		"pr1": {Code: "pr1", Enabled: true, Resources: []string{"mtg.config", secondary}},
		"ps5": {Code: "ps5", Enabled: true, Resources: []string{"mtg.config"}},
		"ps6": {Code: "ps6", Enabled: true, Resources: []string{"mtg.config"}},
	}, Current: map[string]string{}}
	want := map[string]string{"pr1/mtg.config": "127.0.0.1:3129", "pr1/" + secondary: "127.0.0.1:3130",
		"ps5/mtg.config": "127.0.0.1:3129", "ps6/mtg.config": "127.0.0.1:3129"}
	ctx := context.Background()
	rows, published := s.renderAll(ctx, in, time.Now())
	if !published || len(rows) != len(want) || fr.published != 2 {
		t.Fatalf("строк %d, публикаций %d", len(rows), fr.published)
	}
	for _, r := range rows {
		key := r.Target + "/" + r.Resource
		if bind, ok := want[key]; !ok || r.Error != "" || r.Candidate == nil || !strings.Contains(*r.Candidate, "[stats.prometheus]\nenabled = true\nbind-to = \""+bind+"\"") {
			t.Fatalf("неожиданная сборка %s: %q", key, r.Error)
		}
		delete(want, key)
		in.Current[store.CurrentKey(r.Target, r.Resource)] = *r.CandidateSHA
	}
	if _, published = s.renderAll(ctx, in, time.Now()); published || fr.published != 2 {
		t.Fatal("повторно опубликован прежний конфиг")
	}
	for res, wantNo := range map[string]int{"mtg.config": 0, secondary: 6} {
		no, managed, err := s.managedByTelemux(ctx, "pr1", res)
		if err != nil || managed != (wantNo != 0) || no != wantNo {
			t.Fatalf("владелец %s: %d %t %v", res, no, managed, err)
		}
	}
}
