package server

// Серверы - описание того, что видит клиент (ADR 0005, этап B1). Пока справочник:
// хранится и правится в панели, но ни на что не влияет. Чтение - токен бота или
// вход в панель, запись - то же и только на мастере.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/servercfg"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// ServerStore - справочник серверов (реализует store.Store).
type ServerStore interface {
	ListServers(ctx context.Context, engine, state string, limit, offset int) ([]store.Server, int, error)
	GetServer(ctx context.Context, no int) (store.Server, error)
	CreateServer(ctx context.Context, w store.ServerWrite) (store.Server, error)
	ReplaceServer(ctx context.Context, w store.ServerWrite) (store.Server, error)
	DeleteServer(ctx context.Context, no int) error
	ImportServers(ctx context.Context, items []store.ServerWrite) (inserted, updated int, err error)
}

// Пределы тела: один сервер (до 200 доменов и 50 точек входа) и импорт до 100 серверов.
const (
	maxServerBody       = 256 << 10
	maxServerImportBody = 8 << 20
)

var serverStateFilter = map[string]bool{"active": true, "maintenance": true, "test": true, "disabled": true}

// serverNo - номер сервера из пути; не число 1..99 - ok=false (такого сервера нет).
func serverNo(r *http.Request) (int, bool) {
	no, err := strconv.Atoi(r.PathValue("no"))
	return no, err == nil && no >= 1 && no <= 99
}

// decodeBody читает JSON-тело с пределом размера: большое - 413, мусор - 400.
func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, limit int64, v any, what string) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("тело больше %d байт", limit), s.deps.Log)
		return false
	}
	writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON %s: %w", what, err), s.deps.Log)
	return false
}

// fillServerLive отмечает устаревшие runtime-снимки машин серверов.
func (s *Server) fillServerLive(list []store.Server, now time.Time) {
	for i := range list {
		if l := list[i].Live; l != nil {
			for _, m := range []*store.LiveMachine{l.Relay, l.Node} {
				if m != nil {
					m.Stale = s.isStale(m.SeenAt, now)
				}
			}
		}
	}
}

func (s *Server) routesServers() {
	// Страница серверов с точками входа и живыми цифрами по машинам.
	s.mux.HandleFunc("GET /api/v1/servers", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, ok := s.readPage(w, r)
		if !ok {
			return
		}
		engine, state := r.URL.Query().Get("engine"), r.URL.Query().Get("state")
		if engine != "" && !servercfg.ValidEngine(engine) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("engine %q: допустимы telemt, mtg", engine), s.deps.Log)
			return
		}
		if state != "" && !serverStateFilter[state] {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("state %q: допустимы active, maintenance, test, disabled", state), s.deps.Log)
			return
		}
		list, total, err := s.deps.Servers.ListServers(r.Context(), engine, state, limit, offset)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.fillServerLive(list, time.Now())
		writeJSON(w, pageBody(list, total, limit, offset))
	})

	// Каталог полей настроек движка.
	s.mux.HandleFunc("GET /api/v1/servers/fields", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		engine := r.URL.Query().Get("engine")
		if engine != "" && !servercfg.ValidEngine(engine) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("engine %q: допустимы telemt, mtg", engine), s.deps.Log)
			return
		}
		writeJSON(w, map[string]any{"data": servercfg.Fields(engine)})
	})

	s.mux.HandleFunc("GET /api/v1/servers/{no}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		no, ok := serverNo(r)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет сервера %q", r.PathValue("no")), s.deps.Log)
			return
		}
		srv, err := s.deps.Servers.GetServer(r.Context(), no)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		list := []store.Server{srv}
		s.fillServerLive(list, time.Now())
		writeJSON(w, list[0])
	})

	// Создать сервер: 201; номер занят - 409.
	s.mux.HandleFunc("POST /api/v1/servers", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		var body store.ServerWrite
		if !s.decodeBody(w, r, maxServerBody, &body, "сервера (ServerWrite)") {
			return
		}
		if _, err := body.Server(); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		srv, err := s.deps.Servers.CreateServer(r.Context(), body)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		list := []store.Server{srv}
		s.fillServerLive(list, time.Now())
		writeJSONStatus(w, http.StatusCreated, list[0])
	})

	// Заменить сервер целиком вместе с точками входа.
	s.mux.HandleFunc("PUT /api/v1/servers/{no}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		no, ok := serverNo(r)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет сервера %q", r.PathValue("no")), s.deps.Log)
			return
		}
		var body store.ServerWrite
		if !s.decodeBody(w, r, maxServerBody, &body, "сервера (ServerWrite)") {
			return
		}
		if body.No == 0 {
			body.No = no
		}
		if body.No != no {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("номер в теле (%d) не совпадает с номером в пути (%d)", body.No, no), s.deps.Log)
			return
		}
		if _, err := body.Server(); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		srv, err := s.deps.Servers.ReplaceServer(r.Context(), body)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		list := []store.Server{srv}
		s.fillServerLive(list, time.Now())
		writeJSON(w, list[0])
	})

	// Удалить выключенный сервер; иначе 409.
	s.mux.HandleFunc("DELETE /api/v1/servers/{no}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		no, ok := serverNo(r)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет сервера %q", r.PathValue("no")), s.deps.Log)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		if err := s.deps.Servers.DeleteServer(r.Context(), no); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Импорт: массив ServerWrite до 100 штук, upsert по номеру, одна транзакция.
	s.mux.HandleFunc("PUT /api/v1/servers", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		var body []store.ServerWrite
		if !s.decodeBody(w, r, maxServerImportBody, &body, "массива серверов (ServerWrite[])") {
			return
		}
		if len(body) > store.MaxServersImport {
			writeErr(w, http.StatusBadRequest, codedErr("batch_too_large",
				fmt.Errorf("серверов %d, не больше %d за раз", len(body), store.MaxServersImport)), s.deps.Log)
			return
		}
		seen := map[int]bool{}
		for i, item := range body {
			if _, err := item.Server(); err != nil {
				s.writeStoreErr(w, fmt.Errorf("[%d] сервер %d: %w", i, item.No, err))
				return
			}
			if seen[item.No] {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("[%d] номер %d повторяется", i, item.No), s.deps.Log)
				return
			}
			seen[item.No] = true
		}
		if !s.requireMaster(w, r) {
			return
		}
		ins, upd, err := s.deps.Servers.ImportServers(r.Context(), body)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, map[string]int{"inserted": ins, "updated": upd})
	})
}
