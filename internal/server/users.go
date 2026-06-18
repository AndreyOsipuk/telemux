package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/role"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// UserAdmin — управление юзерами (реализует store.Store). Запись — только на master.
type UserAdmin interface {
	CreateUser(ctx context.Context, username, secret string, exp *time.Time, maxConns *int) (store.User, error)
	DeleteUser(ctx context.Context, username string) (bool, error)
	SetExpiration(ctx context.Context, username string, exp *time.Time) (bool, error)
	SetEnabled(ctx context.Context, username string, enabled bool) (bool, error)
	ListUsersPage(ctx context.Context, limit, offset int) ([]store.User, int, error)
}

// requireMaster: write-операции разрешены только когда локальный PG = primary.
// На реплике PG read-only → пишем понятный 409, а не глухой 500 из БД.
func (s *Server) requireMaster(w http.ResponseWriter, r *http.Request) bool {
	rl, err := role.Detect(r.Context(), s.deps.Store)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
		return false
	}
	if !rl.IsMaster() {
		writeErr(w, http.StatusConflict, fmt.Errorf("эта нода — replica; запись идёт на master"), s.deps.Log)
		return false
	}
	return true
}

func (s *Server) routesUsers() {
	// Список с пагинацией: {data, paging:{total,limit,offset}}.
	s.mux.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		limit := atoiDefault(r.URL.Query().Get("limit"), 20)
		offset := atoiDefault(r.URL.Query().Get("offset"), 0)
		users, total, err := s.deps.Users.ListUsersPage(r.Context(), limit, offset)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		writeJSON(w, map[string]any{
			"data":   users,
			"paging": map[string]int{"total": total, "limit": limit, "offset": offset},
		})
	})

	// Создать юзера (master-only). secret генерится, если не задан.
	s.mux.HandleFunc("POST /api/users", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMaster(w, r) {
			return
		}
		var body struct {
			Username     string     `json:"username"`
			Secret       string     `json:"secret"`
			ExpirationAt *time.Time `json:"expiration_at"`
			MaxTCPConns  *int       `json:"max_tcp_conns"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON с непустым username"), s.deps.Log)
			return
		}
		u, err := s.deps.Users.CreateUser(r.Context(), body.Username, body.Secret, body.ExpirationAt, body.MaxTCPConns)
		if err != nil {
			if errors.Is(err, store.ErrUserExists) {
				writeErr(w, http.StatusConflict, err, s.deps.Log)
				return
			}
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		s.markDirty() // немедленная синхра на ноды
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, u)
	})

	// Удалить юзера (master-only).
	s.mux.HandleFunc("DELETE /api/users/{username}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMaster(w, r) {
			return
		}
		ok, err := s.deps.Users.DeleteUser(r.Context(), r.PathValue("username"))
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет такого юзера"), s.deps.Log)
			return
		}
		s.markDirty()
		writeJSON(w, map[string]any{"ok": true})
	})

	// Продлить/снять срок (master-only). {expiration_at: "..."} | null.
	s.mux.HandleFunc("POST /api/users/{username}/renew", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMaster(w, r) {
			return
		}
		var body struct {
			ExpirationAt *time.Time `json:"expiration_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON {expiration_at}"), s.deps.Log)
			return
		}
		ok, err := s.deps.Users.SetExpiration(r.Context(), r.PathValue("username"), body.ExpirationAt)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет такого юзера"), s.deps.Log)
			return
		}
		s.markDirty()
		writeJSON(w, map[string]any{"ok": true})
	})

	// Вкл/выкл (master-only).
	for _, p := range []struct {
		path    string
		enabled bool
	}{{"enable", true}, {"disable", false}} {
		enabled := p.enabled
		s.mux.HandleFunc("POST /api/users/{username}/"+p.path, func(w http.ResponseWriter, r *http.Request) {
			if !s.requireMaster(w, r) {
				return
			}
			ok, err := s.deps.Users.SetEnabled(r.Context(), r.PathValue("username"), enabled)
			if err != nil {
				writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
				return
			}
			if !ok {
				writeErr(w, http.StatusNotFound, fmt.Errorf("нет такого юзера"), s.deps.Log)
				return
			}
			s.markDirty()
			writeJSON(w, map[string]any{"ok": true})
		})
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
