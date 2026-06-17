//go:build integration

// Полный end-to-end: реальный PostgreSQL (store) + stateful-стаб telemt-API →
// syncpkg.SyncNode в режиме apply. Проверяет весь пайплайн desired→diff→мутации.
// Запуск: TELEMUX_TEST_DSN=postgres://... go test -tags=integration ./internal/e2e
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/telemt"
)

type stubTelemt struct {
	mu    sync.Mutex
	users map[string]map[string]any
}

func newStub() *stubTelemt { return &stubTelemt{users: map[string]map[string]any{}} }

func (s *stubTelemt) ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	out, _ := json.Marshal(map[string]any{"ok": true, "data": data, "revision": "rev"})
	w.Write(out)
}

func (s *stubTelemt) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/users", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			arr := []map[string]any{}
			for name, u := range s.users {
				arr = append(arr, map[string]any{
					"username": name, "enabled": u["enabled"],
					"max_tcp_conns": u["max_tcp_conns"], "expiration_rfc3339": u["expiration_rfc3339"],
					"current_connections": 0,
				})
			}
			s.ok(w, arr)
		case http.MethodPost:
			var b map[string]any
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &b)
			name, _ := b["username"].(string)
			s.users[name] = map[string]any{"enabled": true, "max_tcp_conns": b["max_tcp_conns"], "expiration_rfc3339": b["expiration_rfc3339"]}
			s.ok(w, map[string]any{"username": name})
		}
	})
	mux.HandleFunc("/v1/users/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		name := strings.TrimPrefix(r.URL.Path, "/v1/users/")
		switch r.Method {
		case http.MethodDelete:
			delete(s.users, name)
			s.ok(w, map[string]any{})
		case http.MethodPatch:
			var b map[string]any
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &b)
			if u := s.users[name]; u != nil {
				for k, v := range b {
					u[k] = v
				}
			}
			s.ok(w, map[string]any{})
		}
	})
	return httptest.NewServer(mux)
}

func (s *stubTelemt) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.users) }

func dsn(t *testing.T) string {
	d := os.Getenv("TELEMUX_TEST_DSN")
	if d == "" {
		t.Skip("TELEMUX_TEST_DSN не задан")
	}
	return d
}

func seedDB(t *testing.T, d string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mig, err := os.ReadFile("../../migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(mig)); err != nil {
		t.Fatalf("миграция: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE users"); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO users (username, secret, expiration_at, max_tcp_conns, enabled) VALUES
		('sub_1','eeaaa','2026-07-01T00:00:00Z',8,true),
		('sub_2','eebbb',NULL,NULL,true)`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestE2E_ApplyCreatesThenIdempotent(t *testing.T) {
	d := dsn(t)
	seedDB(t, d)

	st, err := store.Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stub := newStub()
	srv := stub.server()
	defer srv.Close()
	node := telemt.New(srv.URL, "")

	// 1-й проход: на ноде пусто → 2 create.
	res, err := syncpkg.SyncNode(context.Background(), st, node, syncpkg.Options{Mode: syncpkg.Apply})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 2 || res.Failed != 0 {
		t.Fatalf("ждали 2 применённых create, получили %+v", res)
	}
	if stub.count() != 2 {
		t.Fatalf("на ноде должно стать 2 юзера, стало %d", stub.count())
	}

	// 2-й проход: desired == remote → 0 операций (идемпотентность).
	res2, err := syncpkg.SyncNode(context.Background(), st, node, syncpkg.Options{Mode: syncpkg.Apply})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Ops) != 0 || res2.Applied != 0 {
		t.Fatalf("повторный sync должен быть пустым (идемпотентность), получили %+v", res2)
	}
}
