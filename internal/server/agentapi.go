package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// AgentStore - машины, их бандлы и отчёты (реализует store.Store).
type AgentStore interface {
	TargetByAgentToken(ctx context.Context, token string) (string, error)
	MaxRevision(ctx context.Context, target string) (int64, error)
	DesiredBundle(ctx context.Context, target string) (store.Bundle, error)
	RecordObserved(ctx context.Context, rep store.ObservedReport) error
	Machines(ctx context.Context, f store.MachineFilter, limit, offset int) ([]store.Machine, int, error)

	// Runtime-отчёт агента и ручки бота по нему (runtime.go).
	RecordRuntime(ctx context.Context, rep store.RuntimeReport) error
	MachineRuntime(ctx context.Context, code string) (store.MachineRuntime, error)
	Connections(ctx context.Context, f store.ConnFilter, limit, offset int) ([]store.ConnRow, int, error)
	ConnectionsSummary(ctx context.Context, limit, offset int) ([]store.ConnSummary, int, error)
	LogErrors(ctx context.Context, since time.Time, machine string, limit, offset int) ([]store.LogErrorSum, int, error)
	// PruneLogErrors удаляет счётчики журнала с концом периода раньше before.
	PruneLogErrors(ctx context.Context, before time.Time) (int64, error)

	// Ручки панели (fleet.go).
	Pairs(ctx context.Context, limit, offset int) ([]store.Pair, int, error)
	Drift(ctx context.Context, threshold time.Duration, limit, offset int) ([]store.DriftItem, int, error)
	ApplyLog(ctx context.Context, target string, limit, offset int) ([]store.ApplyLogEntry, int, error)
}

const (
	defaultDesiredWait = 55 * time.Second // меньше типичных таймаутов прокси (60 с)
	maxDesiredWait     = 60 * time.Second
	maxObservedItems   = 50
	maxObservedChecks  = 20
)

// revisionWatch будит ожидающие long-poll запросы при публикации на этом узле.
// Публикации с других узлов ловит периодическая перепроверка базы.
type revisionWatch struct {
	mu sync.Mutex
	ch chan struct{}
}

// wait возвращает канал, который закроется при следующей публикации.
func (w *revisionWatch) wait() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch == nil {
		w.ch = make(chan struct{})
	}
	return w.ch
}

func (w *revisionWatch) bump() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch != nil {
		close(w.ch)
	}
	w.ch = make(chan struct{})
}

// agentTarget - машина по Bearer-токену агента; пишет 401 и возвращает "", если токен чужой.
func (s *Server) agentTarget(w http.ResponseWriter, r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		writeErr(w, http.StatusUnauthorized, errors.New("нужен токен агента"), s.deps.Log)
		return ""
	}
	code, err := s.deps.Agents.TargetByAgentToken(r.Context(), token)
	if errors.Is(err, store.ErrBadAgentToken) {
		writeErr(w, http.StatusUnauthorized, errors.New("неизвестный токен агента"), s.deps.Log)
		return ""
	}
	if err != nil {
		s.writeStoreErr(w, err)
		return ""
	}
	return code
}

func (s *Server) routesAgents() {
	// Желаемое состояние машины, long-poll: ответ сразу, если есть ревизия новее rev,
	// иначе ожидание до wait (максимум 60 с) и 304.
	s.mux.HandleFunc("GET /v1/desired", func(w http.ResponseWriter, r *http.Request) {
		code := s.agentTarget(w, r)
		if code == "" {
			return
		}
		q := r.URL.Query()
		if q.Get("target") != code {
			writeErr(w, http.StatusForbidden, fmt.Errorf("токен выдан машине %q", code), s.deps.Log)
			return
		}
		known, err := strconv.ParseInt(q.Get("rev"), 10, 64)
		if err != nil || known < 0 {
			writeErr(w, http.StatusBadRequest, errors.New("rev - целое не меньше 0"), s.deps.Log)
			return
		}
		wait := defaultDesiredWait
		if v := q.Get("wait"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				writeErr(w, http.StatusBadRequest, errors.New("wait - длительность, например 30s"), s.deps.Log)
				return
			}
			wait = min(d, maxDesiredWait)
		}

		deadline := time.NewTimer(wait)
		defer deadline.Stop()
		poll := time.NewTicker(s.deps.DesiredPollInterval)
		defer poll.Stop()
		for {
			woken := s.revs.wait() // подписка до проверки - публикация между ними не потеряется
			rev, err := s.deps.Agents.MaxRevision(r.Context(), code)
			if err != nil {
				s.writeStoreErr(w, err)
				return
			}
			if rev > known {
				b, err := s.deps.Agents.DesiredBundle(r.Context(), code)
				if err != nil {
					s.writeStoreErr(w, err)
					return
				}
				writeJSON(w, b)
				return
			}
			select {
			case <-woken:
			case <-poll.C:
			case <-deadline.C:
				w.WriteHeader(http.StatusNotModified)
				return
			case <-r.Context().Done():
				return
			}
		}
	})

	// Отчёт агента о применённом (или сравнённом) состоянии.
	s.mux.HandleFunc("POST /v1/observed", func(w http.ResponseWriter, r *http.Request) {
		code := s.agentTarget(w, r)
		if code == "" {
			return
		}
		var rep store.ObservedReport
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&rep); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON отчёта: %w", err), s.deps.Log)
			return
		}
		if rep.Target != code {
			writeErr(w, http.StatusForbidden, fmt.Errorf("токен выдан машине %q", code), s.deps.Log)
			return
		}
		// Пустой список ресурсов - отчёт машины без ресурсов (агент жив, проверки).
		if len(rep.Resources) > maxObservedItems {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("в отчёте не больше %d ресурсов", maxObservedItems), s.deps.Log)
			return
		}
		if len(rep.Checks) > maxObservedChecks {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("в отчёте не больше %d проверок", maxObservedChecks), s.deps.Log)
			return
		}
		for _, c := range rep.Checks {
			if c.Name == "" || len(c.Name) > 64 || len(c.Error) > 1024 {
				writeErr(w, http.StatusBadRequest, errors.New("у проверки нужно имя до 64 символов, причина - до 1024"), s.deps.Log)
				return
			}
		}
		for _, res := range rep.Resources {
			if res.Resource == "" || res.Revision < 0 {
				writeErr(w, http.StatusBadRequest, errors.New("у ресурса нужны имя и ревизия не меньше 0"), s.deps.Log)
				return
			}
			switch res.Status {
			case "applied", "failed", "pending", "compared":
			default:
				writeErr(w, http.StatusBadRequest, fmt.Errorf("статус %q: допустимы applied, failed, pending, compared", res.Status), s.deps.Log)
				return
			}
		}
		if !s.requireMaster(w, r) {
			return
		}
		if err := s.deps.Agents.RecordObserved(r.Context(), rep); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// AgentHandler - обработчик для внешнего TLS-слушателя: только ручки агентов и
// /healthz. Панель, API бота и документация наружу не отдаются.
func (s *Server) AgentHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/v1/") {
			s.mux.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusNotFound, errors.New("на этом адресе только API агентов"), s.deps.Log)
	})
}
