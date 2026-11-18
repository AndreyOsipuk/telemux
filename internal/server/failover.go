package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/AndreyOsipuk/telemux/internal/peers"
	"github.com/AndreyOsipuk/telemux/internal/role"
)

// RoleSource - текущее решение автомата роли (role.Monitor).
type RoleSource interface {
	Current() role.State
}

// currentRole - роль узла. С монитором - его решение (кворум, Patroni, гистерезис);
// без него - прежнее поведение: роль = роль локального PG.
func (s *Server) currentRole(ctx context.Context) (role.State, error) {
	if s.deps.Role != nil {
		return s.deps.Role.Current(), nil
	}
	rl, err := role.Detect(ctx, s.deps.Store)
	if err != nil {
		s.deps.Log.Error("роль: локальный PG", "err", err) // текст ошибки - в лог, не клиенту
		return role.State{Role: role.Unknown, Reason: "локальный PG недоступен"}, err
	}
	st := role.State{Role: rl, Quorum: true}
	if rl.IsMaster() {
		st.Master = s.deps.SelfCode
	}
	return st, nil
}

// masterURL - адрес мастера по его коду (из списка соседей) или запасной MasterURL.
func (s *Server) masterURL(code string) string {
	if u, ok := s.deps.Peers[code]; ok && code != "" {
		return u
	}
	return s.deps.MasterURL
}

// writeNotMaster - отказ в записи на не-мастере с подсказкой, куда идти.
// 409 (а не 5xx): это не сбой, а адресация не на тот узел; клиент идёт по подсказке
// или перебирает список машин, спрашивая GET /api/leader.
func (s *Server) writeNotMaster(w http.ResponseWriter, st role.State) {
	url := s.masterURL(st.Master)
	if url != "" {
		w.Header().Set("X-Telemux-Master", url)
	}
	s.deps.Log.Warn("запись на не-мастере отклонена", "role", st.Role, "reason", st.Reason, "master", st.Master)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":      "not_master",
		"code":       "not_master",
		"message":    fmt.Sprintf("узел в роли %s; запись принимает только мастер", st.Role),
		"role":       st.Role,
		"reason":     st.Reason,
		"master":     st.Master,
		"master_url": url,
	})
}

func (s *Server) routesFailover() {
	// Роль узла с причиной - для панели и диагностики.
	s.mux.HandleFunc("GET /api/role", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.currentRole(r.Context())
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		writeJSON(w, map[string]any{
			"role": st.Role, "is_master": st.IsMaster(), "reason": st.Reason,
			"quorum": st.Quorum, "seen": st.Seen, "since": st.Since,
			"master": st.Master, "master_url": s.masterURL(st.Master),
		})
	})

	// Лидерство для supervisor бота, reverse-proxy вебхуков и агентов:
	// 200 - этот узел мастер; 503 - нет (с подсказкой мастера). Без сессии.
	s.mux.HandleFunc("GET /api/leader", func(w http.ResponseWriter, r *http.Request) {
		st, _ := s.currentRole(r.Context()) // ошибка → Unknown → 503
		url := s.masterURL(st.Master)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if st.IsMaster() {
			_ = json.NewEncoder(w).Encode(map[string]any{"master": true, "code": s.deps.SelfCode})
			return
		}
		if url != "" {
			w.Header().Set("X-Telemux-Master", url)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"master": false, "error": "not_master", "code": "not_master", "role": st.Role, "reason": st.Reason,
			"master_code": st.Master, "master_url": url,
		})
	})

	// Пинг соседа (канал кворума). Под cluster-secret: чужой не может «изобразить» соседа.
	s.mux.HandleFunc("GET "+peers.PingPath, func(w http.ResponseWriter, r *http.Request) {
		if !s.clusterAuthed(r) {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("неверный cluster-secret"), s.deps.Log)
			return
		}
		st, _ := s.currentRole(r.Context())
		writeJSON(w, peers.Ping{Code: s.deps.SelfCode, Role: st.Role})
	})
}
