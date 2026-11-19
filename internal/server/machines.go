package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// Порог дрейфа в секундах. Лимиты страницы - defaultPageLimit и maxPageLimit
// (users.go), но здесь limit больше 100 - 400 по контракту, а не зажим.
const (
	defaultDriftThreshold = 600
	maxDriftThreshold     = 86400
)

// parseStrictPaging - limit (1..100, по умолчанию 20) и offset (>= 0) списков API;
// limit больше 100 - ошибка по контракту, а не зажим.
func parseStrictPaging(r *http.Request) (limit, offset int, err error) {
	limit, err = intParam(r, "limit", defaultPageLimit, 1, maxPageLimit)
	if err != nil {
		return 0, 0, err
	}
	offset, err = intParam(r, "offset", 0, 0, -1)
	if err != nil {
		return 0, 0, err
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
	store.ResourceTelemtConfig: true, store.ResourceTLSWeb: true,
}

// machineKinds - виды машин.
var machineKinds = map[string]bool{"exit": true, "relay": true, "panel": true}

// parseMachineFilter - фильтры resource, enabled, kind; неизвестное значение - ошибка.
func parseMachineFilter(r *http.Request) (store.MachineFilter, error) {
	q := r.URL.Query()
	f := store.MachineFilter{Resource: q.Get("resource"), Kind: q.Get("kind")}
	if f.Resource != "" && !machineResources[f.Resource] {
		return f, fmt.Errorf("resource %q: допустимы telemt.users, telemt.config, mtg.config, xui.clients, tls.web", f.Resource)
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

// routesMachines - машины флота (с фильтрами и runtime-полями), пары, дрейф и
// журнал машины (панель, бот). Только чтение: работают и на реплике.
func (s *Server) routesMachines() {
	s.mux.HandleFunc("GET /api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, ok := s.readPage(w, r)
		if !ok {
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
		s.fillRuntimeStatus(list, time.Now())
		writeJSON(w, pageBody(list, total, limit, offset))
	})

	// Пары: релей и его выходы; машины вне пар - в псевдо-паре relay: null в конце.
	s.mux.HandleFunc("GET /api/v1/pairs", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, ok := s.readPage(w, r)
		if !ok {
			return
		}
		pairs, total, err := s.deps.Agents.Pairs(r.Context(), limit, offset)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.fillPairsRuntimeStatus(pairs, time.Now())
		writeJSON(w, pageBody(pairs, total, limit, offset))
	})

	// Дрейф: машины, где применённое расходится с желаемым дольше порога.
	s.mux.HandleFunc("GET /api/v1/drift", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		threshold, err := intParam(r, "threshold", defaultDriftThreshold, 0, maxDriftThreshold)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		limit, offset, ok := s.readPage(w, r)
		if !ok {
			return
		}
		items, total, err := s.deps.Agents.Drift(r.Context(), time.Duration(threshold)*time.Second, limit, offset)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		body := pageBody(items, total, limit, offset)
		body["threshold_seconds"] = threshold
		writeJSON(w, body)
	})

	// Журнал применений и проверок машины, новые первыми.
	s.mux.HandleFunc("GET /api/v1/targets/{code}/apply-log", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, ok := s.readPage(w, r)
		if !ok {
			return
		}
		code := r.PathValue("code")
		list, total, err := s.deps.Agents.ApplyLog(r.Context(), code, limit, offset)
		if errors.Is(err, store.ErrUnknownTarget) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, pageBody(list, total, limit, offset))
	})
	s.routesRuntime()
	s.routesOverview()
}

// readPage проверяет доступ (токен бота или сессия) и разбирает limit (1..100,
// по умолчанию 20) и offset (>= 0); на мусор пишет 400 и возвращает ok=false.
func (s *Server) readPage(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	if !s.requireBotOrSession(w, r) {
		return 0, 0, false
	}
	limit, offset, err := parseStrictPaging(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err, s.deps.Log)
		return 0, 0, false
	}
	return limit, offset, true
}

// intParam - целый query-параметр в [lo, hi] (hi < 0 - без верхней границы);
// пустой или отсутствующий - def.
func intParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || (hi >= 0 && n > hi) {
		if hi < 0 {
			return 0, fmt.Errorf("%s - целое не меньше %d", name, lo)
		}
		return 0, fmt.Errorf("%s - целое от %d до %d", name, lo, hi)
	}
	return n, nil
}

// pageBody - ответ списка: {data, paging:{total, limit, offset}}.
func pageBody[T any](data []T, total, limit, offset int) map[string]any {
	if data == nil {
		data = []T{}
	}
	return map[string]any{
		"data":   data,
		"paging": map[string]int{"total": total, "limit": limit, "offset": offset},
	}
}
