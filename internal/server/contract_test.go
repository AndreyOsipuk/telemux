package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/AndreyOsipuk/telemux/api"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// Тест контракта: ответы API совпадают со спекой api/openapi.yaml.

func loadSpec(t *testing.T) (*openapi3.T, routers.Router) {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(api.Spec)
	if err != nil {
		t.Fatalf("спека не читается: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("спека невалидна: %v", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return doc, router
}

// checkContract выполняет запрос и сверяет запрос и ответ со спекой.
func checkContract(t *testing.T, router routers.Router, s *Server, method, path, body string, wantStatus int) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+botToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	route, params, err := router.FindRoute(req)
	if err != nil {
		t.Fatalf("%s %s нет в спеке: %v", method, path, err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s → ждали %d, получили %d: %s", method, path, wantStatus, rec.Code, rec.Body.String())
	}
	in := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, ExcludeRequestBody: true},
	}
	res := rec.Result()
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: res.StatusCode, Header: res.Header,
		Body: io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
	}
	if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
		t.Fatalf("%s %s %d не соответствует спеке: %v\nтело: %s", method, path, rec.Code, err, rec.Body.String())
	}
}

func TestContract_V1(t *testing.T) {
	_, router := loadSpec(t)
	s, fr := v1Server(false, 0)
	r1 := int64(1)
	fr.status[1] = []store.TargetStatus{{Code: "ps2", DesiredRevision: 1, ObservedRevision: &r1, Status: "applied", Applied: true}}

	checkContract(t, router, s, "PUT", "/api/v1/users", usersBody("alice_1", "bob_2"), http.StatusOK)
	checkContract(t, router, s, "PUT", "/api/v1/users", `{"users":[{"key":"bad key","secret":"x"}]}`, http.StatusBadRequest)
	checkContract(t, router, s, "PUT", "/api/v1/users/carol_3", `{"secret":"`+s1+`"}`, http.StatusCreated)
	checkContract(t, router, s, "PUT", "/api/v1/users/carol_3", `{"secret":"`+s2+`"}`, http.StatusOK)
	checkContract(t, router, s, "DELETE", "/api/v1/users/carol_3", "", http.StatusOK)
	checkContract(t, router, s, "DELETE", "/api/v1/users/carol_3", "", http.StatusNotFound)
	checkContract(t, router, s, "GET", "/api/v1/revisions/1/status", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/revisions/999/status", "", http.StatusNotFound)

	// Ресурс машины - текстом, а не JSON.
	putText := func(path, body string, want int) {
		t.Helper()
		req := httptest.NewRequest("PUT", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+botToken)
		req.Header.Set("Content-Type", "text/plain")
		route, params, err := router.FindRoute(req)
		if err != nil {
			t.Fatalf("PUT %s нет в спеке: %v", path, err)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("PUT %s → ждали %d, получили %d: %s", path, want, rec.Code, rec.Body.String())
		}
		in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, ExcludeRequestBody: true}}
		res := rec.Result()
		if err := openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: res.StatusCode, Header: res.Header,
			Body: io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
		}); err != nil {
			t.Fatalf("PUT %s %d не соответствует спеке: %v", path, rec.Code, err)
		}
	}
	putText("/api/v1/targets/pr2/resources/mtg.config", "[secrets]\n", http.StatusOK)
	putText("/api/v1/targets/nobody/resources/mtg.config", "[secrets]\n", http.StatusNotFound)

	guarded, _ := v1Server(false, 20)
	checkContract(t, router, guarded, "PUT", "/api/v1/users", usersBody("alice_1"), http.StatusConflict)
	replica, _ := v1Server(true, 0)
	checkContract(t, router, replica, "DELETE", "/api/v1/users/alice_1", "", http.StatusConflict)
}

func TestContract_Agents(t *testing.T) {
	_, router := loadSpec(t)
	s, fr, _ := agentServer(false)
	fr.rev = 2
	check := func(method, path, token, body string, want int) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		route, params, err := router.FindRoute(req)
		if err != nil {
			t.Fatalf("%s %s нет в спеке: %v", method, path, err)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s → ждали %d, получили %d: %s", method, path, want, rec.Code, rec.Body.String())
		}
		in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, ExcludeRequestBody: true}}
		res := rec.Result()
		if err := openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: res.StatusCode, Header: res.Header,
			Body: io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
		}); err != nil {
			t.Fatalf("%s %s %d не соответствует спеке: %v", method, path, rec.Code, err)
		}
	}
	check("GET", "/v1/desired?target=pr2&rev=1", pr2Token, "", http.StatusOK)
	check("GET", "/v1/desired?target=pr2&rev=2&wait=10ms", pr2Token, "", http.StatusNotModified)
	check("GET", "/v1/desired?target=ps2&rev=1", pr2Token, "", http.StatusForbidden)
	check("GET", "/v1/desired?target=pr2&rev=1", "nope", "", http.StatusUnauthorized)
	check("POST", "/v1/observed", pr2Token, `{"target":"pr2","resources":[{"resource":"telemt.users","revision":2,"status":"compared"}]}`, http.StatusNoContent)
	check("POST", "/v1/observed", pr2Token, `{"target":"pr2","resources":[{"resource":"telemt.users","revision":2,"status":"bad"}]}`, http.StatusBadRequest)
}

// Спека отдаётся панели в JSON без авторизации (для страницы /api-docs).
func TestContract_SpecServed(t *testing.T) {
	doc, _ := loadSpec(t)
	s, _ := v1Server(false, 0)
	rec := do(s, "GET", "/api/openapi.json", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d", rec.Code)
	}
	var got openapi3.T
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("не JSON: %v", err)
	}
	if got.Info == nil || got.Info.Title != doc.Info.Title || len(got.Paths.Map()) != len(doc.Paths.Map()) {
		t.Fatal("отдана не та спека")
	}
}
