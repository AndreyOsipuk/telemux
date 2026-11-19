package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 4xx из-за базы: клиенту - обезличенный текст без SQL и имён таблиц,
// реальная ошибка - только в лог.
func TestStoreErr_4xxHidesDatabaseText(t *testing.T) {
	cases := []struct {
		pg     *pgconn.PgError
		status int
	}{
		{&pgconn.PgError{Code: "23503", Message: `insert or update on table "runtime_user_conns" violates foreign key constraint "runtime_user_conns_target_fkey"`,
			Detail: `Key (target)=(pr9) is not present in table "targets".`}, http.StatusBadRequest},
		{&pgconn.PgError{Code: "23505", Message: `duplicate key value violates unique constraint "users_pkey"`}, http.StatusConflict},
		{&pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "x"`}, http.StatusBadRequest},
		{&pgconn.PgError{Code: "40P01", Message: `deadlock detected`}, http.StatusConflict},
		{&pgconn.PgError{Code: "25006", Message: `cannot execute INSERT in a read-only transaction`}, http.StatusConflict},
	}
	for _, c := range cases {
		s, _, fa := agentServer(false)
		fa.runtimeErr = fmt.Errorf("agent_runtime pr2: %w", c.pg)
		rec := do(s, "POST", "/v1/runtime", pr2Token, runtimeOK)
		if rec.Code != c.status {
			t.Fatalf("%s → ждали %d, получили %d", c.pg.Code, c.status, rec.Code)
		}
		e := decodeErr(t, rec)
		body := rec.Body.String()
		for _, leak := range []string{"runtime_user_conns", "users_pkey", "targets", "violates", "deadlock", "INSERT", "uuid", "agent_runtime"} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s: в ответе клиенту текст базы %q: %s", c.pg.Code, leak, body)
			}
		}
		if e.Message == "" {
			t.Fatalf("%s: нужен понятный текст: %s", c.pg.Code, body)
		}
	}
}

// Все маршруты /api/v1/* без токена - 401. Маршруты берутся из исходников
// (HandleFunc), чтобы новая ручка не проскочила мимо проверки.
func TestAuth_EveryV1RouteRequiresToken(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`HandleFunc\("([A-Z]+) (/api/v1/[^"]*)"`)
	var routes [][2]string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			routes = append(routes, [2]string{m[1], m[2]})
		}
	}
	if len(routes) < 10 {
		t.Fatalf("нашлось подозрительно мало маршрутов /api/v1: %v", routes)
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	// Сервер со всеми группами маршрутов: агенты, машины и добавление машины (Joins).
	s, _, _, _ := defaultJoin(t)
	for _, rt := range routes {
		path := param.ReplaceAllString(rt[1], "x1")
		for _, tok := range []string{"", "чужой-токен"} {
			rec := do(s, rt[0], path, tok, `{}`)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s с токеном %q → ждали 401, получили %d: %s", rt[0], path, tok, rec.Code, rec.Body.String())
			}
		}
	}
}
