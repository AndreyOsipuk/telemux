//go:build integration

// Сборка конфигов серверов (ADR 0006) на реальной базе: цикл пишет server_render,
// публикует только при config_source=telemux и только при изменении, бот получает
// 409 managed_by_telemux, переключение обратно возвращает публикацию боту.
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndreyOsipuk/telemux/internal/server"
	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
)

const renderBotToken = "render-bot"

// renderDB - все миграции и чистые таблицы флота.
func renderDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("../../migrations/*.sql")
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("миграция %s: %v", f, err)
		}
	}
	if _, err := pool.Exec(ctx, `TRUNCATE users, targets, desired_config, observed_state, servers, server_endpoints, server_render CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

type renderEnv struct {
	t    *testing.T
	st   *store.Store
	pool *pgxpool.Pool
	srv  *server.Server
}

func (e *renderEnv) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+renderBotToken)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// revisions - число ревизий ресурса машины.
func (e *renderEnv) revisions(target, resource string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM desired_config WHERE target = $1 AND resource = $2`, target, resource).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *renderEnv) status(no string) []map[string]any {
	e.t.Helper()
	rec := e.do("GET", "/api/v1/servers/"+no+"/render", "")
	if rec.Code != http.StatusOK {
		e.t.Fatalf("статус: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		e.t.Fatal(err)
	}
	return resp.Data
}

func TestE2E_RenderLoop(t *testing.T) {
	ctx := context.Background()
	pool := renderDB(t)
	defer pool.Close()
	st, err := store.Open(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, tg := range []store.Target{
		{Code: "pr1", Kind: "relay", Enabled: true, Resources: []string{store.ResourceTelemtConfig},
			Settings: map[string]string{store.SettingAddress: "198.51.100.1"}},
		{Code: "ps1", Kind: "exit", RelayCode: "pr1", Enabled: true, Resources: []string{}, // ресурс не назначен
			Settings: map[string]string{store.SettingAddress: "192.0.2.1"}},
	} {
		if err := st.UpsertTarget(ctx, tg); err != nil {
			t.Fatal(err)
		}
	}
	relay, node := "pr1", "ps1"
	w := store.ServerWrite{No: 1, Name: "Финляндия", Engine: "telemt", RelayCode: &relay, NodeCode: &node,
		TLSDomain: "www.example.com", Settings: map[string]string{}}
	if _, err := st.CreateServer(ctx, w); err != nil {
		t.Fatal(err)
	}
	e := &renderEnv{t: t, st: st, pool: pool, srv: server.New(server.Deps{
		Store: st, Node: nil, Version: "test", SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		BotToken: renderBotToken, Users: st, Revisions: st, Servers: st, Render: st,
	})}

	// bot: сравнение пишется, ревизий нет.
	e.srv.RenderNow(ctx)
	rows := e.status("1")
	if len(rows) != 2 {
		t.Fatalf("строк сравнения %d: %+v", len(rows), rows)
	}
	byTarget := map[string]map[string]any{}
	for _, r := range rows {
		byTarget[r["target"].(string)] = r
	}
	if r := byTarget["pr1"]; r["equal"] != false || r["error"] != nil || r["candidate_sha"] == nil || r["current_sha"] != nil ||
		!strings.Contains(r["diff"].(string), `+announce_ip = "198.51.100.1"`) {
		t.Fatalf("pr1 до переключения: %+v", r)
	}
	if r := byTarget["ps1"]; r["error"] == nil || !strings.HasPrefix(r["error"].(string), "not_assigned") {
		t.Fatalf("ps1: %+v", r)
	}
	if n := e.revisions("pr1", store.ResourceTelemtConfig); n != 0 {
		t.Fatalf("на bot telemux не публикует, ревизий %d", n)
	}
	// Без совпадения - 409, с force - переключение.
	if rec := e.do("PUT", "/api/v1/servers/1/config-source", `{"source":"telemux"}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "render_mismatch") {
		t.Fatalf("переключение без совпадения: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("PUT", "/api/v1/servers/1/config-source", `{"source":"telemux","force":true}`); rec.Code != http.StatusOK {
		t.Fatalf("force: %d %s", rec.Code, rec.Body.String())
	}

	// telemux: первая ревизия, повтор - без новой.
	e.srv.RenderNow(ctx)
	if n := e.revisions("pr1", store.ResourceTelemtConfig); n != 1 {
		t.Fatalf("после переключения ревизий %d, ждали 1", n)
	}
	if n := e.revisions("ps1", store.ResourceTelemtConfig); n != 0 {
		t.Fatalf("не назначенному ресурсу опубликовано %d", n)
	}
	e.srv.RenderNow(ctx)
	if n := e.revisions("pr1", store.ResourceTelemtConfig); n != 1 {
		t.Fatalf("без изменений новая ревизия: %d", n)
	}
	d, err := st.LatestDesired(ctx, "pr1", store.ResourceTelemtConfig)
	if err != nil || !strings.Contains(string(d.Content), `announce_ip = "198.51.100.1"`) {
		t.Fatalf("опубликовано: %v", err)
	}
	if r := e.status("1"); r[0]["target"] != "pr1" || r[0]["equal"] != true || r[0]["diff"] != "" {
		t.Fatalf("pr1 после публикации: %+v", r[0])
	}
	// Бот этот ресурс больше не публикует.
	if rec := e.do("PUT", "/api/v1/targets/pr1/resources/telemt.config", "include = \"/etc/telemt/users.toml\"\n"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "managed_by_telemux") {
		t.Fatalf("бот на telemux-машину: %d %s", rec.Code, rec.Body.String())
	}

	// Правка сервера - новая ревизия с новым портом.
	w.Settings = map[string]string{"port": "8443"}
	if _, err := st.ReplaceServer(ctx, w); err != nil {
		t.Fatal(err)
	}
	e.srv.RenderNow(ctx)
	if n := e.revisions("pr1", store.ResourceTelemtConfig); n != 2 {
		t.Fatalf("после правки ревизий %d, ждали 2", n)
	}
	if d, _ := st.LatestDesired(ctx, "pr1", store.ResourceTelemtConfig); !strings.Contains(string(d.Content), "\nport = 8443\n") {
		t.Fatal("в ревизии нет нового порта")
	}

	// Обратно на bot: telemux не публикует, бот - снова может.
	if rec := e.do("PUT", "/api/v1/servers/1/config-source", `{"source":"bot"}`); rec.Code != http.StatusOK {
		t.Fatalf("обратно: %d %s", rec.Code, rec.Body.String())
	}
	w.Settings = map[string]string{"port": "9443"}
	if _, err := st.ReplaceServer(ctx, w); err != nil {
		t.Fatal(err)
	}
	e.srv.RenderNow(ctx)
	if n := e.revisions("pr1", store.ResourceTelemtConfig); n != 2 {
		t.Fatalf("на bot telemux опубликовал: ревизий %d", n)
	}
	if rec := e.do("PUT", "/api/v1/targets/pr1/resources/telemt.config", "include = \"/etc/telemt/users.toml\"\n"); rec.Code != http.StatusOK {
		t.Fatalf("бот после возврата: %d %s", rec.Code, rec.Body.String())
	}
	// Список серверов несёт источник и сводку.
	rec := e.do("GET", "/api/v1/servers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"config_source":"bot"`) ||
		!strings.Contains(rec.Body.String(), `"render":{"rows":2`) {
		t.Fatalf("список: %d %s", rec.Code, rec.Body.String())
	}
}
