package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Пары, дрейф и журнал машины: ответы и ошибки совпадают со спекой.
func TestContract_Fleet(t *testing.T) {
	_, router := loadSpec(t)
	s, _ := fleetServer(t)
	checkContract(t, router, s, "GET", "/api/v1/pairs", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/pairs?limit=1&offset=2", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/pairs?offset=10", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/drift", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/drift?threshold=0&limit=1", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/targets/pr1/apply-log", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/targets/ps1/apply-log", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/targets/nobody/apply-log", "", http.StatusNotFound)
	// Мусор в параметрах: запрос вне спеки, но ответ 400 - по ней.
	checkContractResponse(t, router, s, "/api/v1/pairs", "/api/v1/pairs?limit=0", http.StatusBadRequest)
	checkContractResponse(t, router, s, "/api/v1/drift", "/api/v1/drift?threshold=100000", http.StatusBadRequest)
	checkContractResponse(t, router, s, "/api/v1/targets/pr1/apply-log", "/api/v1/targets/pr1/apply-log?offset=-1", http.StatusBadRequest)
	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest("GET", "/api/v1/drift", nil))
	validateResponse(t, router, httptest.NewRequest("GET", "/api/v1/drift", nil), unauth)
}

// checkContractResponse сверяет со спекой только ответ: запрос с мусором в
// параметрах спеке заведомо не соответствует. Маршрут ищется по specPath.
func checkContractResponse(t *testing.T, router routers.Router, s *Server, specPath, path string, wantStatus int) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+botToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("GET %s → ждали %d, получили %d: %s", path, wantStatus, rec.Code, rec.Body.String())
	}
	validateResponse(t, router, httptest.NewRequest("GET", specPath, nil), rec)
}

// validateResponse сверяет ответ rec со спекой маршрута запроса specReq.
func validateResponse(t *testing.T, router routers.Router, specReq *http.Request, rec *httptest.ResponseRecorder) {
	t.Helper()
	route, params, err := router.FindRoute(specReq)
	if err != nil {
		t.Fatalf("%s нет в спеке: %v", specReq.URL.Path, err)
	}
	res := rec.Result()
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: specReq, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}},
		Status: res.StatusCode, Header: res.Header, Body: io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
	}
	if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
		t.Fatalf("%s %d не соответствует спеке: %v\nтело: %s", specReq.URL.Path, rec.Code, err, rec.Body.String())
	}
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

// Runtime-отчёт агента и ручки бота по нему совпадают со спекой.
func TestContract_Runtime(t *testing.T) {
	_, router := loadSpec(t)
	s, _, fa := agentServer(false)
	fresh := time.Now().UTC()
	r3 := int64(3)
	fa.machines = []store.Machine{{Code: "pr2", Kind: "relay", Enabled: true, Resources: []string{store.ResourceTelemtUsers},
		SeenAt: &fresh, Checks: []store.ObservedCheck{}, TelemtVersion: "3.5.17", RuntimeSeenAt: &fresh,
		Observed: []store.MachineResource{{Resource: store.ResourceTelemtUsers, Revision: 3, SHA256: "ab", Status: "applied",
			SeenAt: fresh, DesiredRevision: &r3}}}}
	fa.snapshots = map[string]store.MachineRuntime{
		"pr2": {Code: "pr2", SeenAt: &fresh, AgentVersion: "v1", Host: &store.RuntimeHost{CPUPct: 1},
			Telemt: &store.RuntimeTelemt{Version: "3.5.17", APIOK: true}},
		"vega": {Code: "vega"},
	}
	fa.conns = []store.ConnRow{{Machine: "pr2", Product: "telemt", Key: "a_1", Connections: 2, ActiveIPs: []string{"1.2.3.4"}, SeenAt: fresh}}
	fa.summary = []store.ConnSummary{{Machine: "pr2", Product: "telemt", ConnectionsTotal: 2, UsersOnline: 1, SeenAt: fresh, APIOK: true},
		{Machine: "pr2", Product: "mtg", ConnectionsTotal: 0, UsersOnline: 0, SeenAt: fresh, APIOK: false, Error: "HTTP 502"}}
	fa.logErrors = []store.LogErrorSum{{Machine: "pr2", Key: "", Kind: "handshake", Count: 3}}
	since := url.QueryEscape(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))

	checkContract(t, router, s, "GET", "/api/v1/machines?resource=telemt.users&enabled=true&kind=relay", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/machines?resource=nope", "", http.StatusBadRequest)
	checkContract(t, router, s, "GET", "/api/v1/machines/pr2/runtime", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/machines/vega/runtime", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/machines/nobody/runtime", "", http.StatusNotFound)
	checkContract(t, router, s, "GET", "/api/v1/connections?product=telemt&min_connections=1", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/connections/summary", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/log-errors?since="+since, "", http.StatusOK)

	post := func(token, body string, want int) {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/runtime", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		if err != nil {
			t.Fatalf("POST /v1/runtime нет в спеке: %v", err)
		}
		in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
		if want == http.StatusNoContent {
			// Тело корректного отчёта должно проходить схему RuntimeReport.
			if err := openapi3filter.ValidateRequest(context.Background(), in); err != nil {
				t.Fatalf("тело отчёта не соответствует спеке: %v", err)
			}
			req.Body = io.NopCloser(strings.NewReader(body))
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("POST /v1/runtime → ждали %d, получили %d: %s", want, rec.Code, rec.Body.String())
		}
		in.Options.ExcludeRequestBody = true
		res := rec.Result()
		if err := openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: res.StatusCode, Header: res.Header,
			Body: io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
		}); err != nil {
			t.Fatalf("POST /v1/runtime %d не соответствует спеке: %v", rec.Code, err)
		}
	}
	post(pr2Token, runtimeOK, http.StatusNoContent)
	post("token-ps2", runtimeOK, http.StatusForbidden)
	post(pr2Token, `{"target":"pr2","telemt":{"users":[`+usersJSON(1, 21)+`]}}`, http.StatusBadRequest)
	post(pr2Token, `{"target":"pr2","agent_version":"`+strings.Repeat("x", 2<<20)+`"}`, http.StatusRequestEntityTooLarge)
}

// Добавление машины и join: ответы и ошибки совпадают со спекой.
func TestContract_Join(t *testing.T) {
	_, router := loadSpec(t)
	// Скрипт - текст: декодер как у файла (строка), иначе kin-openapi не знает этот тип.
	openapi3filter.RegisterBodyDecoder("text/x-shellscript", openapi3filter.FileBodyDecoder)
	s, _, _, _ := defaultJoin(t)
	bin := filepath.Join(t.TempDir(), "telemux")
	if err := os.WriteFile(bin, []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.joinBinary = func() (string, error) { return bin, nil }

	checkContract(t, router, s, "POST", "/api/v1/targets", exitBody, http.StatusCreated)
	checkContract(t, router, s, "POST", "/api/v1/targets", `{"code":"pr3","kind":"relay","resources":[]}`, http.StatusCreated)
	checkContract(t, router, s, "POST", "/api/v1/targets", exitBody, http.StatusConflict)
	checkContract(t, router, s, "POST", "/api/v1/targets",
		`{"code":"ps4","kind":"exit","relay_code":"pr1","address":"203.0.113.40","bridge_socks":"socks5://127.0.0.1:0","resources":[]}`, http.StatusBadRequest)
	checkContract(t, router, s, "POST", "/api/v1/targets/ps1/join", "", http.StatusOK)
	checkContract(t, router, s, "POST", "/api/v1/targets/nobody/join", "", http.StatusNotFound)
	checkContract(t, router, s, "POST", "/api/v1/targets/pr2/join", "", http.StatusConflict)

	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest("POST", "/api/v1/targets/ps1/join", nil))
	validateResponse(t, router, httptest.NewRequest("POST", "/api/v1/targets/ps1/join", nil), unauth)

	tok := func() string {
		rec := do(s, "POST", "/api/v1/targets/ps1/join", botToken, "")
		return tokenOf(t, decodeCmd(t, rec).Command)
	}
	live := tok()
	checkContract(t, router, s, "GET", "/v1/join/"+live, "", http.StatusOK)
	checkContract(t, router, s, "GET", "/v1/join/"+live+"/telemux", "", http.StatusOK)
	checkContract(t, router, s, "POST", "/v1/join/"+live+"/agent", "", http.StatusOK)
	checkContract(t, router, s, "POST", "/v1/join/"+live+"/agent", "", http.StatusNotFound)
	checkContract(t, router, s, "GET", "/v1/join/"+live, "", http.StatusNotFound)
	checkContract(t, router, s, "GET", "/v1/join/"+live+"/telemux", "", http.StatusNotFound)
	checkContract(t, router, s, "POST", "/v1/join/"+tok()+"/agent?format=env", "", http.StatusOK)
	s.joinBinary = func() (string, error) { return "", errors.New("не linux/amd64") }
	checkContract(t, router, s, "GET", "/v1/join/"+tok()+"/telemux", "", http.StatusServiceUnavailable)

	replica, _, _, _ := joinServer(t, joinOpts{publicURL: publicURL, inRecovery: true})
	checkContract(t, router, replica, "POST", "/api/v1/targets", exitBody, http.StatusConflict)
}

func TestContract_Certs(t *testing.T) {
	_, router := loadSpec(t)
	fc := newFakeCerts()
	na := time.Now().Add(50 * 24 * time.Hour).UTC()
	tried := time.Now().UTC()
	fc.list = []store.CertInfo{
		{Machine: "pr1", Domain: "web1.example.com", Assigned: true, NotAfter: &na, IssuedAt: &tried, LastTryAt: &tried},
		{Machine: "pr2", Domain: "web2.example.com", LastError: "нет зоны", LastTryAt: &tried},
		{Machine: "pr2", Domain: "web6.example.com"},
	}
	s, _, _ := certsServer(false, fc)
	checkContract(t, router, s, "GET", "/api/v1/certs?limit=2&offset=0", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/certs?offset=2", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/certs?limit=0", "", http.StatusBadRequest)
}

// Обзор, подключения по пользователям, поиск по пользователю и сертификаты машины
// совпадают со спекой.
func TestContract_Overview(t *testing.T) {
	_, router := loadSpec(t)
	fresh := time.Now().UTC()
	fc := newFakeCerts()
	fc.counts = store.CertCounts{Total: 2, Expiring: 1}
	fc.list = []store.CertInfo{{Machine: "pr2", Domain: "web.example.com", Assigned: true}}
	s, fa := overviewServer(fc)
	fa.totals = store.FleetTotals{Machines: 2, AgentsOnline: 1, AgentsSilent: 1, ConnectionsTotal: 5, UsersOnline: 2}
	fa.machines = []store.Machine{
		{Code: "pr2", Kind: "relay", Enabled: true, Resources: []string{}, Checks: []store.ObservedCheck{}, Observed: []store.MachineResource{},
			SeenAt: &fresh, RuntimeSeenAt: &fresh, TelemtVersion: "3.5.17", TelemtAPIOK: ptrBool(true), MtgAPIOK: ptrBool(false),
			Host: &store.RuntimeHost{CPUPct: 10, MemPct: 20, DiskPct: 30, Load1: 0.5, UptimeS: 60}, Connections: 5, UsersOnline: 2},
		{Code: "ps2", Kind: "exit", Enabled: true, Resources: []string{}, Checks: []store.ObservedCheck{}, Observed: []store.MachineResource{}},
	}
	relay := store.PairMachine{Machine: fa.machines[0], Status: store.StatusOK, Desired: []store.DesiredRevision{}}
	fa.pairs = []store.Pair{{Relay: &relay, Exits: []store.PairMachine{}}}
	fa.conns = []store.ConnRow{{Machine: "pr2", Product: "telemt", Key: "a_1", Connections: 5, ActiveIPs: []string{}, SeenAt: fresh}}

	checkContract(t, router, s, "GET", "/api/v1/overview", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/machines", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/pairs", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/connections/by-user?user=a_1&user=b_2", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/connections/by-user?user=", "", http.StatusBadRequest)
	checkContract(t, router, s, "GET", "/api/v1/connections?user=a_&min_connections=1", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/certs?machine=pr2", "", http.StatusOK)

	// Без сертификатов на мастере - certs: null.
	plain, fa2 := overviewServer(nil)
	fa2.machines = []store.Machine{}
	checkContract(t, router, plain, "GET", "/api/v1/overview", "", http.StatusOK)
}
