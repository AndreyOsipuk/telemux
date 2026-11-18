package server

import (
	"errors"
	"net/http"
	"strconv"
)

const (
	defaultMachinesLimit = 20
	maxMachinesLimit     = 100
)

// routesMachines - список машин флота с последним отчётом агента (панель, бот).
func (s *Server) routesMachines() {
	s.mux.HandleFunc("GET /api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		limit, offset := defaultMachinesLimit, 0
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxMachinesLimit {
				writeErr(w, http.StatusBadRequest, errors.New("limit - целое от 1 до 100"), s.deps.Log)
				return
			}
			limit = n
		}
		if v := r.URL.Query().Get("offset"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeErr(w, http.StatusBadRequest, errors.New("offset - целое не меньше 0"), s.deps.Log)
				return
			}
			offset = n
		}
		list, total, err := s.deps.Agents.Machines(r.Context(), limit, offset)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, map[string]any{
			"data":   list,
			"paging": map[string]int{"total": total, "limit": limit, "offset": offset},
		})
	})
}
