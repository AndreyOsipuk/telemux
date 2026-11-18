package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// UserAdmin — управление юзерами (реализует store.Store). Запись — только на master.
type UserAdmin interface {
	CreateUser(ctx context.Context, username, secret string, exp *time.Time, maxConns *int) (store.User, error)
	DeleteUser(ctx context.Context, username string) (bool, error)
	SetExpiration(ctx context.Context, username string, exp *time.Time) (bool, error)
	SetEnabled(ctx context.Context, username string, enabled bool) (bool, error)
	ListUsersPage(ctx context.Context, limit, offset int) ([]store.User, int, error)
	ReconcileUsers(ctx context.Context, rows []store.ImportRow, force bool) (store.ReconcileResult, error)
}

// requireMaster: write-операции разрешены только мастеру (PG primary + кворум +
// Patroni-лидер, см. ADR 0002). На не-мастере - 409 not_master с подсказкой
// мастера, а не глухой 500 из read-only БД.
func (s *Server) requireMaster(w http.ResponseWriter, r *http.Request) bool {
	st, err := s.currentRole(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
		return false
	}
	if !st.IsMaster() {
		s.writeNotMaster(w, st)
		return false
	}
	return true
}

func (s *Server) routesUsers() {
	// Список с пагинацией: {data, paging:{total,limit,offset}}.
	s.mux.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, err := parsePaging(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		users, total, err := s.deps.Users.ListUsersPage(r.Context(), limit, offset)
		if err != nil {
			s.writeStoreErr(w, err)
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
			s.writeStoreErr(w, err)
			return
		}
		s.markDirty() // немедленная синхра на ноды
		writeJSONStatus(w, http.StatusCreated, u)
	})

	// Удалить юзера (master-only).
	s.mux.HandleFunc("DELETE /api/users/{username}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireMaster(w, r) {
			return
		}
		ok, err := s.deps.Users.DeleteUser(r.Context(), r.PathValue("username"))
		if err != nil {
			s.writeStoreErr(w, err)
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
			s.writeStoreErr(w, err)
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
				s.writeStoreErr(w, err)
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

// Пагинация списков: limit 1..100 (по умолчанию 20, больше 100 - зажимается
// до 100), offset >= 0. Мусор и отрицательные значения - ошибка клиента.
const (
	defaultPageLimit = 20
	maxPageLimit     = 100
)

func parsePaging(r *http.Request) (limit, offset int, err error) {
	limit, offset = defaultPageLimit, 0
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 1 {
			return 0, 0, fmt.Errorf("limit - целое от 1 до %d", maxPageLimit)
		}
		limit = min(n, maxPageLimit)
	}
	if v := q.Get("offset"); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 0 {
			return 0, 0, fmt.Errorf("offset - целое не меньше 0")
		}
		offset = n
	}
	return limit, offset, nil
}
