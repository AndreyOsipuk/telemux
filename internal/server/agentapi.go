package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// AgentStore - машины, их бандлы и отчёты (реализует store.Store).
type AgentStore interface {
	// AgentTargetByToken - машина токена и включена ли она: агент выключенной
	// машины отчитывается, но новых ревизий не получает.
	AgentTargetByToken(ctx context.Context, token string) (code string, enabled bool, err error)
	MaxRevision(ctx context.Context, target string) (int64, error)
	DesiredBundle(ctx context.Context, target string) (store.Bundle, error)
	RecordObserved(ctx context.Context, rep store.ObservedReport) error
	// RecordRelayVia - отчёт агента пришёл через мост выхода bridge (R3.1);
	// не выход - store.ErrUnknownBridge.
	RecordRelayVia(ctx context.Context, target, bridge string) error
	Machines(ctx context.Context, f store.MachineFilter, limit, offset int) ([]store.Machine, int, error)

	// Runtime-отчёт агента и ручки бота по нему (runtime.go).
	RecordRuntime(ctx context.Context, rep store.RuntimeReport) error
	MachineRuntime(ctx context.Context, code string) (store.MachineRuntime, error)
	Connections(ctx context.Context, f store.ConnFilter, limit, offset int) ([]store.ConnRow, int, error)
	ConnectionsSummary(ctx context.Context, limit, offset int) ([]store.ConnSummary, int, error)
	LogErrors(ctx context.Context, since time.Time, machine string, limit, offset int) ([]store.LogErrorSum, int, error)
	// Подключения по пользователям (сумма по машинам) и итоги флота для обзора (overview.go).
	ConnectionsByUser(ctx context.Context, keys []string) ([]store.UserConns, error)
	FleetTotals(ctx context.Context, freshSince, silentBefore time.Time) (store.FleetTotals, error)
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
	code, _ := s.agentTargetEnabled(w, r)
	return code
}

// agentTargetEnabled - машина по Bearer-токену агента и включена ли она; пишет
// 401 и возвращает "", если токен чужой.
func (s *Server) agentTargetEnabled(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		writeErr(w, http.StatusUnauthorized, errors.New("нужен токен агента"), s.deps.Log)
		return "", false
	}
	code, enabled, err := s.deps.Agents.AgentTargetByToken(r.Context(), token)
	if errors.Is(err, store.ErrBadAgentToken) {
		writeErr(w, http.StatusUnauthorized, errors.New("неизвестный токен агента"), s.deps.Log)
		return "", false
	}
	if err != nil {
		s.writeStoreErr(w, err)
		return "", false
	}
	return code, enabled
}

func (s *Server) routesAgents() {
	// Желаемое состояние машины, long-poll: ответ сразу, если есть ревизия новее rev,
	// иначе ожидание до wait (максимум 60 с) и 304. Выключенная машина новых
	// ревизий не получает: всегда ожидание и 304 (агент при этом отчитывается).
	s.mux.HandleFunc("GET /v1/desired", func(w http.ResponseWriter, r *http.Request) {
		code, enabled := s.agentTargetEnabled(w, r)
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
		if !enabled {
			// Пауза до wait, а не мгновенный 304: иначе агент крутил бы опрос без задержки.
			select {
			case <-deadline.C:
				w.WriteHeader(http.StatusNotModified)
			case <-r.Context().Done():
			}
			return
		}
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
		s.recordRelayVia(r, code)
		w.WriteHeader(http.StatusNoContent)
	})
}

// bridgeCodeRe - код выхода в заголовке моста (как код машины).
var bridgeCodeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// recordRelayVia записывает, через мост какого выхода пришёл принятый отчёт
// агента target (заголовок X-Telemux-Bridge, R3.1). Заголовка нет - ничего. Не код
// существующего выхода - WARN, отчёт всё равно принят. Сбой базы - ERROR в лог:
// отчёт уже сохранён, отметка моста - вспомогательная и придёт со следующим.
func (s *Server) recordRelayVia(r *http.Request, target string) {
	vals, ok := r.Header[bridgeHeader]
	if !ok {
		return
	}
	bridge := strings.Join(vals, ",")
	if !bridgeCodeRe.MatchString(bridge) {
		s.deps.Log.Warn("relay_via: мусор в X-Telemux-Bridge, отметка моста пропущена", "target", target, "bridge", clipLog(bridge))
		return
	}
	err := s.deps.Agents.RecordRelayVia(r.Context(), target, bridge)
	switch {
	case errors.Is(err, store.ErrUnknownBridge):
		s.deps.Log.Warn("relay_via: X-Telemux-Bridge - не код существующего выхода, отметка моста пропущена",
			"target", target, "bridge", bridge)
	case err != nil:
		s.deps.Log.Error("relay_via: отметка моста не записана", "target", target, "bridge", bridge, "err", err)
	}
}

// bridgeHeader - X-Telemux-Bridge в каноническом виде (ключ http.Header).
const bridgeHeader = "X-Telemux-Bridge"

// clipLog - не больше 80 символов чужой строки в лог.
func clipLog(v string) string {
	if len(v) > 80 {
		return v[:80] + "..."
	}
	return v
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
