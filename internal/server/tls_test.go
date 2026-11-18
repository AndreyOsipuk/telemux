package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Наружу по TLS мастер отдаёт только ручки агентов: панель, API бота и
// документация остаются на локальном адресе.
func TestAgentHandler_OnlyAgentRoutes(t *testing.T) {
	s, _, _ := agentServer(false)
	h := s.AgentHandler()
	for path, want := range map[string]int{
		"/healthz":                     http.StatusOK,
		"/v1/desired?target=pr2&rev=0": http.StatusUnauthorized, // ручка есть, нужен токен
		"/api/users":                   http.StatusNotFound,
		"/api/v1/revisions/1/status":   http.StatusNotFound,
		"/api/openapi.json":            http.StatusNotFound,
		"/":                            http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Fatalf("%s → ждали %d, получили %d", path, want, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/observed", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /v1/observed без токена → ждали 401, получили %d", rec.Code)
	}
}
