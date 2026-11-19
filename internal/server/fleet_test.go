package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

func (f *fakeAgents) Pairs(_ context.Context, limit, offset int) ([]store.Pair, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastLimit, f.lastOffset = limit, offset
	if f.fail != nil {
		return nil, 0, f.fail
	}
	return page(f.pairs, limit, offset), len(f.pairs), nil
}

func (f *fakeAgents) Drift(_ context.Context, threshold time.Duration, limit, offset int) ([]store.DriftItem, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastLimit, f.lastOffset, f.lastThreshold = limit, offset, threshold
	return page(f.drift, limit, offset), len(f.drift), nil
}

func (f *fakeAgents) ApplyLog(_ context.Context, target string, limit, offset int) ([]store.ApplyLogEntry, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastLimit, f.lastOffset = limit, offset
	log, ok := f.applyLog[target]
	if !ok {
		return nil, 0, store.ErrUnknownTarget
	}
	return page(log, limit, offset), len(log), nil
}

// page - срез страницы из runtime_test.go.

func pairMachine(code, kind, status string) store.PairMachine {
	return store.PairMachine{
		Machine: store.Machine{Code: code, Kind: kind, Enabled: true, Resources: []string{},
			Checks: []store.ObservedCheck{}, Observed: []store.MachineResource{}},
		Status: status, Desired: []store.DesiredRevision{},
	}
}

// fleetServer - сервер с флотом: две пары и псевдо-пара, дрейф, журнал машины pr1.
func fleetServer(t *testing.T) (*Server, *fakeAgents) {
	t.Helper()
	s, _, fa := agentServer(false)
	relay := pairMachine("pr1", "relay", store.StatusOK)
	relay2 := pairMachine("pr2", "relay", store.StatusNoAgent)
	fa.pairs = []store.Pair{
		{Relay: &relay, Exits: []store.PairMachine{pairMachine("ps1", "exit", store.StatusBehind)}},
		{Relay: &relay2, Exits: []store.PairMachine{}},
		{Relay: nil, Exits: []store.PairMachine{pairMachine("pan1", "panel", store.StatusFailed)}},
	}
	now := time.Now().UTC()
	r2 := int64(2)
	fa.drift = []store.DriftItem{
		{Target: "ps1", Kind: "exit", RelayCode: "pr1", Reason: "behind", Resource: store.ResourceTelemtUsers,
			DesiredRevision: 3, ObservedRevision: &r2, Since: now, SeenAt: &now},
		{Target: "solo", Kind: "exit", Reason: "silent", Since: now, SeenAt: &now},
	}
	fa.applyLog = map[string][]store.ApplyLogEntry{
		"pr1": {
			{At: now, Resource: "check:bridge", Status: "failed", Error: "refused"},
			{At: now.Add(-time.Minute), Resource: store.ResourceTelemtUsers, Revision: &r2, Status: "applied"},
		},
		"ps1": {},
	}
	return s, fa
}

type pagedResp[T any] struct {
	Data      []T                                `json:"data"`
	Paging    struct{ Total, Limit, Offset int } `json:"paging"`
	Threshold *int                               `json:"threshold_seconds"`
}

func decodePaged[T any](t *testing.T, rec *httptest.ResponseRecorder) pagedResp[T] {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	var resp pagedResp[T]
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// Без токена бота и без сессии новые ручки закрыты; с токеном бота - открыты
// (путь пропущен сессионным middleware, проверку делает обработчик).
func TestFleet_Auth(t *testing.T) {
	s, _ := fleetServer(t)
	for _, p := range []string{"/api/v1/pairs", "/api/v1/drift", "/api/v1/targets/pr1/apply-log"} {
		if rec := do(s, "GET", p, "", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s без токена → ждали 401, получили %d", p, rec.Code)
		}
		if rec := do(s, "GET", p, "nope", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s с чужим токеном → ждали 401, получили %d", p, rec.Code)
		}
		if rec := do(s, "GET", p, pr2Token, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s с токеном агента → ждали 401, получили %d", p, rec.Code)
		}
		if rec := do(s, "GET", p, botToken, ""); rec.Code != http.StatusOK {
			t.Fatalf("%s с токеном бота → ждали 200, получили %d: %s", p, rec.Code, rec.Body.String())
		}
	}
}

// Вход в панель (сессия) тоже открывает ручки.
func TestFleet_Session(t *testing.T) {
	s, _ := fleetServer(t)
	login := httptest.NewRecorder()
	s.Handler().ServeHTTP(login, httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"admin","password":"pw"}`)))
	if login.Code != http.StatusOK {
		t.Fatalf("вход: %d", login.Code)
	}
	cookies := login.Result().Cookies()
	for _, p := range []string{"/api/v1/pairs", "/api/v1/drift", "/api/v1/targets/pr1/apply-log"} {
		req := httptest.NewRequest("GET", p, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s по сессии → ждали 200, получили %d", p, rec.Code)
		}
	}
}

// Внешний TLS-адрес агентов новых ручек не отдаёт.
func TestFleet_NotOnAgentHandler(t *testing.T) {
	s, _ := fleetServer(t)
	for _, p := range []string{"/api/v1/pairs", "/api/v1/drift", "/api/v1/targets/pr1/apply-log"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Authorization", "Bearer "+botToken)
		s.AgentHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s на адресе агентов → ждали 404, получили %d", p, rec.Code)
		}
	}
}

func TestFleet_BadPaging(t *testing.T) {
	s, _ := fleetServer(t)
	for _, p := range []string{"/api/v1/pairs", "/api/v1/drift", "/api/v1/targets/pr1/apply-log"} {
		for _, q := range []string{"limit=0", "limit=101", "limit=abc", "limit=", "offset=-1", "offset=abc", "limit=1.5"} {
			rec := do(s, "GET", p+"?"+q, botToken, "")
			if q == "limit=" {
				// Пустое значение - как отсутствующее: умолчание.
				if rec.Code != http.StatusOK {
					t.Fatalf("%s?%s → ждали 200, получили %d", p, q, rec.Code)
				}
				continue
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s?%s → ждали 400, получили %d", p, q, rec.Code)
			}
			var e errorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != "bad_request" {
				t.Fatalf("%s?%s: тело ошибки %s", p, q, rec.Body.String())
			}
		}
	}
}

func TestFleet_Pairs(t *testing.T) {
	s, fa := fleetServer(t)
	resp := decodePaged[store.Pair](t, do(s, "GET", "/api/v1/pairs", botToken, ""))
	if fa.lastLimit != 20 || fa.lastOffset != 0 {
		t.Fatalf("по умолчанию ждали limit 20 offset 0, получили %d %d", fa.lastLimit, fa.lastOffset)
	}
	if len(resp.Data) != 3 || resp.Paging.Total != 3 || resp.Paging.Limit != 20 || resp.Data[2].Relay != nil ||
		resp.Data[0].Relay.Code != "pr1" || resp.Data[0].Exits[0].Status != store.StatusBehind {
		t.Fatalf("не тот ответ: %+v", resp)
	}
	resp = decodePaged[store.Pair](t, do(s, "GET", "/api/v1/pairs?limit=1&offset=2", botToken, ""))
	if len(resp.Data) != 1 || resp.Data[0].Relay != nil || resp.Paging.Offset != 2 || resp.Paging.Limit != 1 {
		t.Fatalf("страница с псевдо-парой: %+v", resp)
	}
	rec := do(s, "GET", "/api/v1/pairs?offset=10", botToken, "")
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("пустая страница - [], а не null: %s", rec.Body.String())
	}
	// Псевдо-пара сериализуется с relay: null, а не без поля.
	rec = do(s, "GET", "/api/v1/pairs?offset=2", botToken, "")
	if !strings.Contains(rec.Body.String(), `"relay":null`) {
		t.Fatalf("псевдо-пара без relay:null: %s", rec.Body.String())
	}
}

// Ошибка базы - 503 с общим текстом, без деталей.
func TestFleet_StoreError(t *testing.T) {
	s, fa := fleetServer(t)
	fa.fail = context.DeadlineExceeded
	rec := do(s, "GET", "/api/v1/pairs", botToken, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ждали 503, получили %d", rec.Code)
	}
	fa.fail = errors.New("pg: секретная деталь")
	rec = do(s, "GET", "/api/v1/pairs", botToken, "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "секретная") {
		t.Fatalf("ждали 500 без деталей, получили %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFleet_Drift(t *testing.T) {
	s, fa := fleetServer(t)
	resp := decodePaged[store.DriftItem](t, do(s, "GET", "/api/v1/drift", botToken, ""))
	if fa.lastThreshold != 600*time.Second || resp.Threshold == nil || *resp.Threshold != 600 {
		t.Fatalf("порог по умолчанию 600 с: в стор %s, в ответе %v", fa.lastThreshold, resp.Threshold)
	}
	if len(resp.Data) != 2 || resp.Paging.Total != 2 || resp.Data[1].Reason != "silent" {
		t.Fatalf("не тот ответ: %+v", resp)
	}
	resp = decodePaged[store.DriftItem](t, do(s, "GET", "/api/v1/drift?threshold=0&limit=1", botToken, ""))
	if fa.lastThreshold != 0 || *resp.Threshold != 0 || len(resp.Data) != 1 {
		t.Fatalf("порог 0: %s %+v", fa.lastThreshold, resp)
	}
	decodePaged[store.DriftItem](t, do(s, "GET", "/api/v1/drift?threshold=86400", botToken, ""))
	if fa.lastThreshold != 24*time.Hour {
		t.Fatalf("порог 86400: %s", fa.lastThreshold)
	}
	for _, q := range []string{"threshold=-1", "threshold=100000", "threshold=86401", "threshold=abc", "threshold=1.5"} {
		if rec := do(s, "GET", "/api/v1/drift?"+q, botToken, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → ждали 400, получили %d", q, rec.Code)
		}
	}
}

func TestFleet_ApplyLog(t *testing.T) {
	s, fa := fleetServer(t)
	resp := decodePaged[store.ApplyLogEntry](t, do(s, "GET", "/api/v1/targets/pr1/apply-log?limit=1&offset=1", botToken, ""))
	if fa.lastLimit != 1 || fa.lastOffset != 1 || len(resp.Data) != 1 || resp.Paging.Total != 2 ||
		resp.Data[0].Revision == nil || *resp.Data[0].Revision != 2 {
		t.Fatalf("не тот ответ: %+v", resp)
	}
	rec := do(s, "GET", "/api/v1/targets/ps1/apply-log", botToken, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("пустой журнал - []: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(s, "GET", "/api/v1/targets/nobody/apply-log", botToken, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("неизвестная машина → ждали 404, получили %d", rec.Code)
	}
	var e errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != "not_found" {
		t.Fatalf("тело 404: %s", rec.Body.String())
	}
}
