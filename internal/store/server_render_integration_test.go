//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// setupRender - серверы telemt 1 (pr1/ps1) и mtg 2 (pr2/ps2), у машин адреса.
func setupRender(t *testing.T) *Store {
	t.Helper()
	st := setupAll(t)
	seedTargets(t, st,
		Target{Code: "pr1", Kind: "relay", Enabled: true, Resources: []string{ResourceTelemtConfig},
			Settings: map[string]string{SettingAddress: "198.51.100.1"}},
		Target{Code: "pr2", Kind: "relay", Enabled: true, Resources: []string{}},
		Target{Code: "ps1", Kind: "exit", RelayCode: "pr1", Enabled: true, Resources: []string{ResourceTelemtConfig},
			Settings: map[string]string{SettingAddress: "192.0.2.1"}},
		Target{Code: "ps2", Kind: "exit", RelayCode: "pr2", Enabled: true, Resources: []string{ResourceMtgConfig}})
	ctx := context.Background()
	if _, err := st.CreateServer(ctx, telemtServer(1, "pr1", "ps1")); err != nil {
		t.Fatal(err)
	}
	mtg := telemtServer(2, "pr2", "ps2")
	mtg.Engine, mtg.Settings = "mtg", map[string]string{"BIND_PORT": "8767"}
	if _, err := st.CreateServer(ctx, mtg); err != nil {
		t.Fatal(err)
	}
	return st
}

func renderRow(no int, target, resource string, equal bool, errText string) RenderRow {
	sha, cand := "sha-"+target, "text "+target+"\n"
	return RenderRow{ServerNo: no, Target: target, Resource: resource, CandidateSHA: &sha, Candidate: &cand,
		Equal: equal, Error: errText, RenderedAt: time.Now().UTC()}
}

// Миграция 0011 видна проверке схемы, config_source по умолчанию bot и не сбрасывается записью сервера.
func TestIntegration_Render_ConfigSourceDefault(t *testing.T) {
	st := setupRender(t)
	defer st.Close()
	ctx := context.Background()
	if err := st.CheckServersSchema(ctx); err != nil {
		t.Fatal(err)
	}
	srv, err := st.GetServer(ctx, 1)
	if err != nil || srv.ConfigSource != ConfigSourceBot {
		t.Fatalf("по умолчанию: %v %q", err, srv.ConfigSource)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE servers SET config_source = 'panel' WHERE no = 1`); err == nil {
		t.Fatal("CHECK config_source пропустил мусор")
	}
	if _, err := st.SetConfigSource(ctx, 1, ConfigSourceTelemux, true); err != nil {
		t.Fatal(err)
	}
	// Замена и импорт сервера источник не трогают.
	if srv, err = st.ReplaceServer(ctx, telemtServer(1, "pr1", "ps1")); err != nil || srv.ConfigSource != ConfigSourceTelemux {
		t.Fatalf("после замены: %v %q", err, srv.ConfigSource)
	}
	if _, _, err := st.ImportServers(ctx, []ServerWrite{telemtServer(1, "pr1", "ps1")}); err != nil {
		t.Fatal(err)
	}
	if srv, _ = st.GetServer(ctx, 1); srv.ConfigSource != ConfigSourceTelemux {
		t.Fatalf("после импорта: %q", srv.ConfigSource)
	}
}

// Вход цикла: не выключенные серверы с точками входа, машины с адресом и
// ресурсами, включённые пользователи, хеши текущих ревизий.
func TestIntegration_Render_Input(t *testing.T) {
	st := setupRender(t)
	defer st.Close()
	ctx := context.Background()
	off := telemtServer(3, "pr1", "ps1")
	off.State = "disabled"
	if _, err := st.CreateServer(ctx, off); err != nil {
		t.Fatal(err)
	}
	prevExp := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if _, err := st.UpsertUser(ctx, ImportRow{Username: "alice_1", Secret: sec1, PreviousSecret: sec2, PreviousSecretExpiresAt: &prevExp}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertUser(ctx, ImportRow{Username: "bob_2", Secret: sec9}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE users SET enabled = false WHERE username = 'bob_2'`); err != nil {
		t.Fatal(err)
	}
	pub, err := st.PublishResource(ctx, "ps1", ResourceTelemtConfig, []byte("include = \"x\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := st.RenderInput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Servers) != 2 || in.Servers[0].No != 1 || len(in.Servers[0].Endpoints) != 3 {
		t.Fatalf("серверы: %+v", in.Servers)
	}
	if m := in.Machines["ps1"]; m.Address != "192.0.2.1" || m.Kind != "exit" || !m.Enabled || len(m.Resources) != 1 {
		t.Fatalf("машина ps1: %+v", m)
	}
	if m := in.Machines["ps2"]; m.Address != "" {
		t.Fatalf("машина без адреса: %+v", m)
	}
	if len(in.Users) != 1 || in.Users[0].Key != "alice_1" || in.Users[0].PreviousSecret != sec2 || in.Users[0].PreviousSecretExpiresAt == nil {
		t.Fatalf("пользователи: %+v", in.Users)
	}
	if sha := in.Current[CurrentKey("ps1", ResourceTelemtConfig)]; sha == "" || pub.Revision == 0 {
		t.Fatalf("текущий хеш ps1: %q", sha)
	}
	if _, ok := in.Current[CurrentKey("pr1", ResourceTelemtConfig)]; ok {
		t.Fatal("у pr1 ревизий нет")
	}
}

// Запись сравнения заменяет прежнее целиком; сводка в списке серверов; статус с текущим
// содержимым; удаление сервера уносит его строки.
func TestIntegration_Render_SaveAndList(t *testing.T) {
	st := setupRender(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := st.PublishResource(ctx, "ps1", ResourceTelemtConfig, []byte("current ps1\n")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRender(ctx, []RenderRow{
		renderRow(1, "pr1", ResourceTelemtConfig, true, ""),
		renderRow(1, "ps1", ResourceTelemtConfig, false, ""),
		renderRow(2, "ps2", ResourceMtgConfig, false, "no_tls_domain: нет"),
		renderRow(42, "ps1", ResourceTelemtConfig, false, ""), // сервера нет - строка пропускается
	}); err != nil {
		t.Fatal(err)
	}
	list, total, err := st.ListServers(ctx, "", "", 20, 0)
	if err != nil || total != 2 {
		t.Fatal(err, total)
	}
	if r := list[0].Render; r == nil || r.Rows != 2 || r.Equal || r.Errors != 0 || r.RenderedAt == nil {
		t.Fatalf("сводка 1: %+v", list[0].Render)
	}
	if r := list[1].Render; r.Rows != 1 || r.Errors != 1 || r.Equal {
		t.Fatalf("сводка 2: %+v", list[1].Render)
	}
	rows, err := st.ListRender(ctx, 1)
	if err != nil || len(rows) != 2 {
		t.Fatal(err, rows)
	}
	if rows[0].Target != "pr1" || rows[0].Current != nil || rows[1].Current == nil || *rows[1].Current != "current ps1\n" {
		t.Fatalf("статус: %+v / %+v", rows[0], rows[1])
	}
	if _, err := st.ListRender(ctx, 50); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("нет сервера: %v", err)
	}
	// Новый проход заменяет строки целиком.
	if err := st.SaveRender(ctx, []RenderRow{renderRow(1, "pr1", ResourceTelemtConfig, true, "")}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "server_render"); n != 1 {
		t.Fatalf("после замены строк %d", n)
	}
	// Удаление сервера каскадом уносит строки.
	if _, err := st.pool.Exec(ctx, `UPDATE servers SET state = 'disabled' WHERE no = 1`); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServer(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "server_render"); n != 0 {
		t.Fatalf("после удаления сервера строк %d", n)
	}
	// Сервер без сравнения: сводка с нулями.
	srv, err := st.GetServer(ctx, 2)
	if err != nil || srv.Render == nil || srv.Render.Rows != 0 || srv.Render.RenderedAt != nil {
		t.Fatalf("сводка без строк: %v %+v", err, srv.Render)
	}
}

// Переключение на telemux - только при полном совпадении, force - всегда, обратно - всегда.
func TestIntegration_Render_SetConfigSource(t *testing.T) {
	st := setupRender(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := st.SetConfigSource(ctx, 1, ConfigSourceTelemux, false); !errors.Is(err, ErrRenderMismatch) {
		t.Fatalf("без сравнения: %v", err)
	}
	save := func(rows ...RenderRow) {
		t.Helper()
		if err := st.SaveRender(ctx, rows); err != nil {
			t.Fatal(err)
		}
	}
	save(renderRow(1, "pr1", ResourceTelemtConfig, true, ""), renderRow(1, "ps1", ResourceTelemtConfig, false, ""))
	if _, err := st.SetConfigSource(ctx, 1, ConfigSourceTelemux, false); !errors.Is(err, ErrRenderMismatch) {
		t.Fatalf("расхождение: %v", err)
	}
	save(renderRow(1, "pr1", ResourceTelemtConfig, true, ""), renderRow(1, "ps1", ResourceTelemtConfig, true, "not_assigned: x"))
	if _, err := st.SetConfigSource(ctx, 1, ConfigSourceTelemux, false); !errors.Is(err, ErrRenderMismatch) {
		t.Fatalf("ошибка сборки: %v", err)
	}
	if srv, _ := st.GetServer(ctx, 1); srv.ConfigSource != ConfigSourceBot {
		t.Fatalf("источник сменился при отказе: %q", srv.ConfigSource)
	}
	srv, err := st.SetConfigSource(ctx, 1, ConfigSourceTelemux, true)
	if err != nil || srv.ConfigSource != ConfigSourceTelemux {
		t.Fatalf("force: %v %q", err, srv.ConfigSource)
	}
	if got, _ := st.TelemuxServersOfMachine(ctx, "pr1"); len(got) != 1 || got[0].No != 1 {
		t.Fatalf("серверы машины pr1: %+v", got)
	}
	if got, _ := st.TelemuxServersOfMachine(ctx, "ps2"); len(got) != 0 {
		t.Fatalf("серверы машины ps2 (bot): %+v", got)
	}
	if srv, err = st.SetConfigSource(ctx, 1, ConfigSourceBot, false); err != nil || srv.ConfigSource != ConfigSourceBot {
		t.Fatalf("обратно: %v %q", err, srv.ConfigSource)
	}
	save(renderRow(2, "ps2", ResourceMtgConfig, true, ""))
	if srv, err = st.SetConfigSource(ctx, 2, ConfigSourceTelemux, false); err != nil || srv.ConfigSource != ConfigSourceTelemux {
		t.Fatalf("совпало: %v %q", err, srv.ConfigSource)
	}
	if _, err := st.SetConfigSource(ctx, 77, ConfigSourceBot, false); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("нет сервера: %v", err)
	}
	if _, err := st.SetConfigSource(ctx, 1, "panel", true); !errors.Is(err, ErrBadConfigSource) {
		t.Fatalf("мусор: %v", err)
	}
	// Выключенный telemux-сервер ресурсами машин не владеет.
	if _, err := st.pool.Exec(ctx, `UPDATE servers SET state = 'disabled' WHERE no = 2`); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.TelemuxServersOfMachine(ctx, "ps2"); len(got) != 0 {
		t.Fatalf("выключенный сервер: %+v", got)
	}
}

// Старое совпадение для основного слота не разрешает включить управление вторым процессом.
func TestIntegration_Render_SecondarySource(t *testing.T) {
	st := setupRender(t)
	defer st.Close()
	ctx := context.Background()
	const secondary = "mtg.secondary.config"
	seedTargets(t, st, Target{Code: "pr2", Kind: "relay", Enabled: true, Resources: []string{ResourceMtgConfig, secondary}})
	_, err := st.pool.Exec(ctx, `UPDATE servers SET settings = settings || '{"RELAY_MODE":"local-mtg","TELEMUX_RELAY_RESOURCE":"mtg.secondary.config"}'::jsonb WHERE no=2`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.PublishResource(ctx, "pr2", secondary, []byte("[secrets]\n")); err != nil {
		t.Fatal(err)
	}
	in, err := st.RenderInput(ctx)
	if err != nil || in.Current[CurrentKey("pr2", secondary)] == "" {
		t.Fatalf("SHA второго ресурса: %v", err)
	}
	exit := renderRow(2, "ps2", ResourceMtgConfig, true, "")
	if err = st.SaveRender(ctx, []RenderRow{exit, renderRow(2, "pr2", ResourceMtgConfig, true, "")}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.SetConfigSource(ctx, 2, ConfigSourceTelemux, false); !errors.Is(err, ErrRenderMismatch) {
		t.Fatalf("переключён без сверки нужного процесса: %v", err)
	}
	if err = st.SaveRender(ctx, []RenderRow{exit, renderRow(2, "pr2", secondary, true, "")}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.SetConfigSource(ctx, 2, ConfigSourceTelemux, false); err != nil {
		t.Fatal(err)
	}
}
