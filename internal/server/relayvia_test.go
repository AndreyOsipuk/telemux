package server

// Тесты спецификации relay-via-exit на стороне мастера: R2.1 (сводка моста в
// runtime-отчёте), R3.1 (relay_via из заголовка моста), R5.2, контракт 5.3.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// relayViaCall - запись RecordRelayVia в фейке.
type relayViaCall struct{ target, bridge string }

func (f *fakeAgents) RecordRelayVia(_ context.Context, target, bridge string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.relayViaErr != nil {
		return f.relayViaErr
	}
	if bridge != "ps2" && bridge != "ps1" { // выходы фейкового флота
		return fmt.Errorf("%w: %q", store.ErrUnknownBridge, bridge)
	}
	f.relayVia = append(f.relayVia, relayViaCall{target, bridge})
	return nil
}

// doBridge - запрос агента через мост: с заголовком X-Telemux-Bridge (пусто - без него).
func doBridge(s *Server, method, path, token, bridge, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if bridge != "" {
		req.Header.Set("X-Telemux-Bridge", bridge)
	}
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// logServer - agentServer с журналом в буфер.
func logServer(t *testing.T) (*Server, *fakeAgents, *bytes.Buffer) {
	t.Helper()
	s, _, fa := agentServer(false)
	var buf bytes.Buffer
	s.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))
	return s, fa, &buf
}

const observedPr2 = `{"target":"pr2","agent_version":"v1","resources":[]}`

func TestRelayVia_R3_1_ObservedAndRuntimeRecordBridge(t *testing.T) {
	s, fa, _ := logServer(t)
	if rec := doBridge(s, "POST", "/v1/observed", pr2Token, "ps2", observedPr2); rec.Code != http.StatusNoContent {
		t.Fatalf("observed через мост: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doBridge(s, "POST", "/v1/runtime", pr2Token, "ps1", `{"target":"pr2","host":{"cpu_pct":1}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("runtime через мост: %d %s", rec.Code, rec.Body.String())
	}
	want := []relayViaCall{{"pr2", "ps2"}, {"pr2", "ps1"}}
	if len(fa.relayVia) != 2 || fa.relayVia[0] != want[0] || fa.relayVia[1] != want[1] {
		t.Fatalf("relay_via: %+v", fa.relayVia)
	}
}

func TestRelayVia_R3_1_NoHeaderNothingRecorded(t *testing.T) {
	s, fa, _ := logServer(t)
	if rec := doBridge(s, "POST", "/v1/observed", pr2Token, "", observedPr2); rec.Code != http.StatusNoContent {
		t.Fatalf("%d", rec.Code)
	}
	if len(fa.relayVia) != 0 {
		t.Fatalf("без заголовка relay_via не пишется: %+v", fa.relayVia)
	}
}

func TestRelayVia_R3_1_GarbageWarnedReportAccepted(t *testing.T) {
	for _, bad := range []string{"ps9", "PS2;DROP", strings.Repeat("a", 65)} {
		s, fa, logs := logServer(t)
		rec := doBridge(s, "POST", "/v1/observed", pr2Token, bad, observedPr2)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("мост %q: отчёт должен приниматься, %d", bad, rec.Code)
		}
		if len(fa.observed) != 1 || len(fa.relayVia) != 0 {
			t.Fatalf("мост %q: отчёт %d, relay_via %+v", bad, len(fa.observed), fa.relayVia)
		}
		if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "X-Telemux-Bridge") {
			t.Fatalf("мост %q: ждали WARN в логе: %s", bad, logs.String())
		}
	}
}

func TestRelayVia_R3_1_StoreFailureLoggedReportAccepted(t *testing.T) {
	s, fa, logs := logServer(t)
	fa.relayViaErr = errors.New("connection reset")
	rec := doBridge(s, "POST", "/v1/runtime", pr2Token, "ps2", `{"target":"pr2","host":{"cpu_pct":1}}`)
	if rec.Code != http.StatusNoContent || len(fa.runtime) != 1 {
		t.Fatalf("сбой записи relay_via не роняет отчёт: %d", rec.Code)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "connection reset") {
		t.Fatalf("сбой записи relay_via - в лог с причиной: %s", logs.String())
	}
}

func TestRelayVia_R2_1_RuntimeBridgeAccepted(t *testing.T) {
	s, fa, _ := logServer(t)
	seen := time.Now().UTC().Format(time.RFC3339)
	body := `{"target":"ps2","host":{"cpu_pct":1},"bridge":{"code":"ps2","relays":[{"code":"pr2","last_seen":"` + seen +
		`","requests":3,"master_ok":true,"last_error":""}],"error":""}}`
	if rec := do(s, "POST", "/v1/runtime", "token-ps2", body); rec.Code != http.StatusNoContent {
		t.Fatalf("ждали 204: %d %s", rec.Code, rec.Body.String())
	}
	b := fa.runtime[0].Bridge
	if b == nil || b.Code != "ps2" || len(b.Relays) != 1 || b.Relays[0].Code != "pr2" || b.Relays[0].Requests != 3 {
		t.Fatalf("сводка моста: %+v", b)
	}
}

func TestRelayVia_R2_1_RuntimeBridgeValidated(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf(`{"code":"r%d"}`, i)
	}
	cases := map[string]string{
		"много релеев":        `{"code":"ps2","relays":[` + strings.Join(many, ",") + `]}`,
		"пустой код релея":    `{"code":"ps2","relays":[{"code":""}]}`,
		"релей повторяется":   `{"code":"ps2","relays":[{"code":"pr2"},{"code":"pr2"}]}`,
		"last_seen в будущем": `{"code":"ps2","relays":[{"code":"pr2","last_seen":"` + future + `"}]}`,
		"отрицательный счёт":  `{"code":"ps2","relays":[{"code":"pr2","requests":-1}]}`,
		"длинная ошибка":      `{"code":"ps2","relays":[],"error":"` + strings.Repeat("x", 1025) + `"}`,
	}
	for name, bridge := range cases {
		s, fa, _ := logServer(t)
		rec := do(s, "POST", "/v1/runtime", "token-ps2", `{"target":"ps2","host":{"cpu_pct":1},"bridge":`+bridge+`}`)
		if rec.Code != http.StatusBadRequest || len(fa.runtime) != 0 {
			t.Fatalf("%s: ждали 400, %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// R5.2: привязка выхода к несуществующему релею (или к машине не того вида) -
// 400 unknown_machine, не 5xx.
func TestRelayVia_R5_2_ExitToUnknownRelay(t *testing.T) {
	s, _, fw, _ := defaultJoin(t)
	for name, body := range map[string]string{
		"релея нет":         `{"code":"ps3","kind":"exit","relay_code":"pr9","address":"203.0.113.13","bridge_socks":"socks5://127.0.0.1:1","resources":[]}`,
		"релей - это выход": `{"code":"ps3","kind":"exit","relay_code":"ps1","address":"203.0.113.13","bridge_socks":"socks5://127.0.0.1:1","resources":[]}`,
	} {
		rec := do(s, "POST", "/api/v1/targets", botToken, body)
		var e errorBody
		if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Code != "unknown_machine" || e.Message == "" {
			t.Fatalf("%s: ждали 400 unknown_machine, %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if len(fw.calls) != 0 {
		t.Fatalf("при ошибке ввода файрвол трогать нельзя: %v", fw.calls)
	}
}

// Контракт 5.3: новые поля машин, пар и событий есть в api/openapi.yaml, ответы с
// ними проходят проверку спекой (в том числе перечисления статусов и видов событий).
func TestContract_RelayVia_5_3(t *testing.T) {
	doc, router := loadSpec(t)
	props := func(schema string) map[string]bool {
		out := map[string]bool{}
		ref := doc.Components.Schemas[schema]
		if ref == nil {
			t.Fatalf("в спеке нет схемы %s", schema)
		}
		s := ref.Value
		for name := range s.Properties {
			out[name] = true
		}
		for _, sub := range s.AllOf {
			for name := range sub.Value.Properties {
				out[name] = true
			}
		}
		return out
	}
	for schema, fields := range map[string][]string{
		"Machine":       {"relay_via", "relay_via_at", "foreign_bridge"},
		"PairMachine":   {"relay_status", "relay_last_seen"},
		"Pair":          {"relay_down", "foreign_bridge", "status"},
		"MachineEvent":  {"exit"},
		"RuntimeReport": {"bridge"},
	} {
		have := props(schema)
		for _, f := range fields {
			if !have[f] {
				t.Errorf("в схеме %s нет поля %s", schema, f)
			}
		}
	}

	fresh := time.Now().UTC()
	via := "ps9"
	s, fa := overviewServer(nil)
	relay := store.Machine{Code: "pr2", Kind: "relay", Enabled: true, Resources: []string{}, Checks: []store.ObservedCheck{},
		Observed: []store.MachineResource{}, SeenAt: &fresh, RelayVia: &via, RelayViaAt: &fresh, ForeignBridge: ptrBool(true)}
	exitOK := store.Machine{Code: "ps2", Kind: "exit", Enabled: true, Resources: []string{}, Checks: []store.ObservedCheck{},
		Observed: []store.MachineResource{}, SeenAt: &fresh}
	exitSilent, exitDown := exitOK, exitOK
	exitSilent.Code, exitDown.Code = "ps1", "ps3"
	fa.machines = []store.Machine{relay, exitOK}
	rp := store.PairMachine{Machine: relay, Status: store.StatusOK, Desired: []store.DesiredRevision{}}
	fa.pairs = []store.Pair{
		{Relay: &rp, RelayDown: true, ForeignBridge: true, Status: store.PairStatusDown, Exits: []store.PairMachine{
			{Machine: exitOK, Status: store.StatusOK, Desired: []store.DesiredRevision{}, RelayStatus: store.RelayStatusOK, RelayLastSeen: &fresh},
			{Machine: exitSilent, Status: store.StatusOK, Desired: []store.DesiredRevision{}, RelayStatus: store.RelayStatusSilent},
			{Machine: exitDown, Status: store.StatusSilent, Desired: []store.DesiredRevision{}, RelayStatus: store.RelayStatusExitDown},
		}},
		{Relay: &store.PairMachine{Machine: relay, Status: store.StatusOK, Desired: []store.DesiredRevision{}},
			Status: store.PairStatusNoExit, Exits: []store.PairMachine{}},
	}
	checkContract(t, router, s, "GET", "/api/v1/machines", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/pairs", "", http.StatusOK)

	ops, fo, _, _ := opsServer(false)
	for i, k := range []string{store.EventRelaySilent, store.EventRelayOK, store.EventForeignBridge, store.EventRelayNoExit} {
		e := store.MachineEvent{ID: int64(i + 1), At: fresh, Machine: "pr2", Kind: k, Detail: "x"}
		if k != store.EventRelayNoExit {
			e.Exit = "ps2"
		}
		fo.events = append(fo.events, e)
	}
	checkContract(t, router, ops, "GET", "/api/v1/events?after_id=0&limit=10", "", http.StatusOK)
}
