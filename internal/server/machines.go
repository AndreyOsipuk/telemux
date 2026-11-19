package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

const (
	defaultMachinesLimit = 20
	maxMachinesLimit     = 100
)

// parseStrictPaging - limit (1..100, по умолчанию 20) и offset (>= 0) списков API.
func parseStrictPaging(r *http.Request) (limit, offset int, err error) {
	limit = defaultMachinesLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxMachinesLimit {
			return 0, 0, errors.New("limit - целое от 1 до 100")
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, 0, errors.New("offset - целое не меньше 0")
		}
		offset = n
	}
	return limit, offset, nil
}

// writePage пишет страницу списка {data, paging{total, limit, offset}}.
func writePage(w http.ResponseWriter, data any, total, limit, offset int) {
	writeJSON(w, map[string]any{
		"data":   data,
		"paging": map[string]int{"total": total, "limit": limit, "offset": offset},
	})
}

// machineResources - ресурсы, по которым фильтруется список машин.
var machineResources = map[string]bool{
	store.ResourceTelemtUsers: true, store.ResourceMtgConfig: true, store.ResourceXuiClients: true,
}

// machineKinds - виды машин.
var machineKinds = map[string]bool{"exit": true, "relay": true, "panel": true}

// parseMachineFilter - фильтры resource, enabled, kind; неизвестное значение - ошибка.
func parseMachineFilter(r *http.Request) (store.MachineFilter, error) {
	q := r.URL.Query()
	f := store.MachineFilter{Resource: q.Get("resource"), Kind: q.Get("kind")}
	if f.Resource != "" && !machineResources[f.Resource] {
		return f, fmt.Errorf("resource %q: допустимы telemt.users, mtg.config, xui.clients", f.Resource)
	}
	if f.Kind != "" && !machineKinds[f.Kind] {
		return f, fmt.Errorf("kind %q: допустимы exit, relay, panel", f.Kind)
	}
	switch v := q.Get("enabled"); v {
	case "":
	case "true", "false":
		b := v == "true"
		f.Enabled = &b
	default:
		return f, fmt.Errorf("enabled %q: допустимы true, false", v)
	}
	return f, nil
}

// routesMachines - список машин флота с последним отчётом агента (панель, бот).
func (s *Server) routesMachines() {
	s.mux.HandleFunc("GET /api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		limit, offset, err := parseStrictPaging(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		f, err := parseMachineFilter(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		list, total, err := s.deps.Agents.Machines(r.Context(), f, limit, offset)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writePage(w, list, total, limit, offset)
	})
	s.routesRuntime()
}
