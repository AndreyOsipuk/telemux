package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
)

// Этап 0 плана: исправления найденных ошибок HTTP-API.

// errUsers - UserAdmin, у которого все операции падают с заданной ошибкой.
type errUsers struct{ err error }

func (e errUsers) CreateUser(context.Context, string, string, *time.Time, *int) (store.User, error) {
	return store.User{}, e.err
}
func (e errUsers) DeleteUser(context.Context, string) (bool, error) { return false, e.err }
func (e errUsers) SetExpiration(context.Context, string, *time.Time) (bool, error) {
	return false, e.err
}
func (e errUsers) SetEnabled(context.Context, string, bool) (bool, error) { return false, e.err }
func (e errUsers) ListUsersPage(context.Context, int, int) ([]store.User, int, error) {
	return nil, 0, e.err
}
func (e errUsers) ReconcileUsers(context.Context, []store.ImportRow, bool) (store.ReconcileResult, error) {
	return store.ReconcileResult{}, e.err
}

func errUsersServer(err error) *Server {
	return New(Deps{
		Store: fakeStore{}, Users: errUsers{err: err}, Node: &fakeNode{},
		Version: "v1", SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
	})
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) apiError {
	t.Helper()
	if ct := rec.Result().Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("ошибка должна быть JSON, Content-Type=%q", ct)
	}
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("тело ошибки не JSON: %v: %s", err, rec.Body.String())
	}
	if e.Code == "" || e.Message == "" {
		t.Fatalf("в ошибке нужны code и message, получили %s", rec.Body.String())
	}
	return e
}

// Секрет кластера принимается только в виде "Bearer <секрет>": голый секрет
// без схемы раньше проходил, потому что TrimPrefix ничего не отрезал.
func TestClusterAuth_RequiresBearerScheme(t *testing.T) {
	s, _ := clusterServer("sek")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/cluster/heartbeat", strings.NewReader(`{"code":"ps1"}`))
	req.Header.Set("Authorization", "sek")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("секрет без Bearer → ждали 401, получили %d", rec.Code)
	}
}

// В paging.limit возвращается фактически применённый лимит, а не запрошенный.
func TestUsers_PagingReportsAppliedLimit(t *testing.T) {
	s := New(Deps{Store: fakeStore{}, Users: newFakeUsers(150), Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}})
	rec := do(s, "GET", "/api/users?limit=500", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("limit=500 → ждали 200 (зажать до 100), получили %d", rec.Code)
	}
	var out struct {
		Data   []store.User   `json:"data"`
		Paging map[string]int `json:"paging"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Paging["limit"] != 100 || len(out.Data) != 100 || out.Paging["total"] != 150 {
		t.Fatalf("ждали limit=100, 100 строк, total=150; получили limit=%d, строк=%d, total=%d",
			out.Paging["limit"], len(out.Data), out.Paging["total"])
	}
}

// Мусор в параметрах пагинации - ошибка клиента (400), а не тихая подмена.
func TestUsers_PagingRejectsBadInput(t *testing.T) {
	s := New(Deps{Store: fakeStore{}, Users: newFakeUsers(3), Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}})
	for _, q := range []string{"limit=abc", "limit=0", "limit=-5", "offset=-1", "offset=x"} {
		rec := do(s, "GET", "/api/users?"+q, "", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → ждали 400, получили %d", q, rec.Code)
		}
		if e := decodeErr(t, rec); e.Code != "bad_request" {
			t.Fatalf("%s → ждали code=bad_request, получили %q", q, e.Code)
		}
	}
}

// 201 приходит с Content-Type JSON (раньше WriteHeader вызывался до заголовка).
func TestUsers_CreateHasJSONContentType(t *testing.T) {
	s, _ := usersServer(false)
	rec := do(s, "POST", "/api/users", "", `{"username":"sub_ct"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create → %d", rec.Code)
	}
	// Result() - снимок заголовков на момент WriteHeader, как их увидит клиент.
	if ct := rec.Result().Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("201 без JSON Content-Type: %q", ct)
	}
}

// Ошибки клиента - в формате {code, message}.
func TestErrors_ClientErrorsHaveCode(t *testing.T) {
	s, _ := usersServer(false)
	cases := []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"POST", "/api/users", `{}`, http.StatusBadRequest, "bad_request"},
		{"DELETE", "/api/users/nobody", "", http.StatusNotFound, "not_found"},
		{"POST", "/api/users/nobody/renew", `{"expiration_at":null}`, http.StatusNotFound, "not_found"},
	}
	for _, c := range cases {
		rec := do(s, c.method, c.path, "", c.body)
		if rec.Code != c.status {
			t.Fatalf("%s %s → ждали %d, получили %d", c.method, c.path, c.status, rec.Code)
		}
		if e := decodeErr(t, rec); e.Code != c.code {
			t.Fatalf("%s %s → ждали code=%s, получили %q", c.method, c.path, c.code, e.Code)
		}
	}
	s.deps.Users.CreateUser(context.Background(), "dup", "", nil, nil)
	rec := do(s, "POST", "/api/users", "", `{"username":"dup"}`)
	if rec.Code != http.StatusConflict || decodeErr(t, rec).Code != "conflict" {
		t.Fatalf("дубль → ждали 409 conflict, получили %d %s", rec.Code, rec.Body.String())
	}
}

// Ошибка хранилища, не связанная с доступностью базы, - это баг (500), а не 503.
// Текст ошибки базы клиенту не уходит.
func TestErrors_StoreFailureIs500WithoutDetails(t *testing.T) {
	leak := errors.New(`list users: ERROR: column "secret_x" does not exist (SQLSTATE 42703)`)
	s := errUsersServer(leak)
	rec := do(s, "GET", "/api/users", "", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("ждали 500, получили %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret_x") || strings.Contains(rec.Body.String(), "SQLSTATE") {
		t.Fatalf("текст ошибки базы утёк клиенту: %s", rec.Body.String())
	}
	if e := decodeErr(t, rec); e.Code != "internal" {
		t.Fatalf("ждали code=internal, получили %q", e.Code)
	}
}

// Недоступность базы (сеть, таймаут) - 503.
func TestErrors_StoreUnavailableIs503(t *testing.T) {
	for _, err := range []error{
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
		context.DeadlineExceeded,
	} {
		s := errUsersServer(err)
		rec := do(s, "GET", "/api/users", "", "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%v → ждали 503, получили %d", err, rec.Code)
		}
		if e := decodeErr(t, rec); e.Code != "unavailable" {
			t.Fatalf("%v → ждали code=unavailable, получили %q", err, e.Code)
		}
	}
}
