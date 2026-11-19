package server

// Обзор флота для главной страницы панели (/api/v1/overview) и подключения по
// пользователям (/api/v1/connections/by-user). Только чтение: работают и на реплике.

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

const (
	// overviewMaxMachines - сколько машин обзор отдаёт списком; флот небольшой,
	// больше - truncated: true (итоги при этом считаются по всем машинам).
	overviewMaxMachines = 500
	// certWarnDays - сертификат, которому осталось меньше, подсвечивается.
	certWarnDays = 14
)

// Runtime-статус машины по последнему runtime-отчёту агента.
const (
	RuntimeOnline   = "online"   // отчёт свежий, API продуктов отвечает
	RuntimeStale    = "stale"    // отчёт старше порога свежести, но агент молчит меньше SilentAfter
	RuntimeDegraded = "degraded" // агент жив, но API telemt или mtg не ответил
	RuntimeOffline  = "offline"  // отчёта нет или агент молчит дольше SilentAfter
)

// runtimeStatus - сводный runtime-статус машины на момент now.
func (s *Server) runtimeStatus(m store.Machine, now time.Time) string {
	if m.RuntimeSeenAt == nil || now.Sub(*m.RuntimeSeenAt) > store.SilentAfter {
		return RuntimeOffline
	}
	if now.Sub(*m.RuntimeSeenAt) > s.staleAfter() {
		return RuntimeStale
	}
	if (m.TelemtAPIOK != nil && !*m.TelemtAPIOK) || (m.MtgAPIOK != nil && !*m.MtgAPIOK) {
		return RuntimeDegraded
	}
	return RuntimeOnline
}

func (s *Server) fillRuntimeStatus(list []store.Machine, now time.Time) {
	for i := range list {
		list[i].RuntimeStatus = s.runtimeStatus(list[i], now)
	}
}

func (s *Server) fillPairsRuntimeStatus(pairs []store.Pair, now time.Time) {
	for i := range pairs {
		if r := pairs[i].Relay; r != nil {
			r.RuntimeStatus = s.runtimeStatus(r.Machine, now)
		}
		for j := range pairs[i].Exits {
			e := &pairs[i].Exits[j]
			e.RuntimeStatus = s.runtimeStatus(e.Machine, now)
		}
	}
}

// pairHealthy - все включённые машины пары в статусе ok.
func pairHealthy(p store.Pair) bool {
	ms := p.Exits
	if p.Relay != nil {
		ms = append([]store.PairMachine{*p.Relay}, p.Exits...)
	}
	for _, m := range ms {
		if m.Status != store.StatusDisabled && m.Status != store.StatusOK {
			return false
		}
	}
	return true
}

type overviewMachines struct {
	Total  int `json:"total"`
	Online int `json:"online"`
	Silent int `json:"silent"`
}

type overviewConns struct {
	Total       int64 `json:"total"`
	UsersOnline int   `json:"users_online"`
}

type overviewPairs struct {
	Total   int `json:"total"`
	OK      int `json:"ok"`
	Problem int `json:"problem"`
}

type overviewCerts struct {
	Total    int `json:"total"`
	Expiring int `json:"expiring"`
	Missing  int `json:"missing"`
}

type overviewBody struct {
	Machines           overviewMachines `json:"machines"`
	Connections        overviewConns    `json:"connections"`
	Pairs              overviewPairs    `json:"pairs"`
	Certs              *overviewCerts   `json:"certs"` // nil - сертификаты на мастере выключены
	CertWarnDays       int              `json:"cert_warn_days"`
	StaleAfterSeconds  int              `json:"stale_after_seconds"`
	SilentAfterSeconds int              `json:"silent_after_seconds"`
	Data               []store.Machine  `json:"data"`
	Truncated          bool             `json:"truncated"`
}

// parseUserKeys - ключи пользователей из повторяющегося параметра user:
// от 1 до store.MaxConnUsers, каждый непустой и не длиннее maxRuntimeKeyLen; дубли схлопываются.
func parseUserKeys(r *http.Request) ([]string, error) {
	raw := r.URL.Query()["user"]
	if len(raw) == 0 {
		return nil, errors.New("нужен хотя бы один параметр user")
	}
	if len(raw) > store.MaxConnUsers {
		return nil, fmt.Errorf("не больше %d параметров user за запрос", store.MaxConnUsers)
	}
	seen := make(map[string]bool, len(raw))
	keys := make([]string, 0, len(raw))
	for _, k := range raw {
		if k == "" || len(k) > maxRuntimeKeyLen {
			return nil, fmt.Errorf("user - непустая строка до %d символов", maxRuntimeKeyLen)
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (s *Server) routesOverview() {
	s.mux.HandleFunc("GET /api/v1/overview", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		ctx := r.Context()
		now := time.Now()
		totals, err := s.deps.Agents.FleetTotals(ctx, now.Add(-s.staleAfter()), now.Add(-store.SilentAfter))
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		list, total, err := s.deps.Agents.Machines(ctx, store.MachineFilter{}, overviewMaxMachines, 0)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.fillRuntimeStatus(list, now)
		pairs, _, err := s.deps.Agents.Pairs(ctx, overviewMaxMachines, 0)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		body := overviewBody{
			Machines:           overviewMachines{Total: totals.Machines, Online: totals.AgentsOnline, Silent: totals.AgentsSilent},
			Connections:        overviewConns{Total: totals.ConnectionsTotal, UsersOnline: totals.UsersOnline},
			CertWarnDays:       certWarnDays,
			StaleAfterSeconds:  int(s.staleAfter() / time.Second),
			SilentAfterSeconds: int(store.SilentAfter / time.Second),
			Data:               list,
			Truncated:          total > len(list),
		}
		// Псевдо-пара «без релея» в счёт пар не идёт: это не пара.
		for _, p := range pairs {
			if p.Relay == nil {
				continue
			}
			body.Pairs.Total++
			if pairHealthy(p) {
				body.Pairs.OK++
			} else {
				body.Pairs.Problem++
			}
		}
		if s.deps.Certs != nil {
			c, err := s.deps.Certs.CertCounts(ctx, now.Add(certWarnDays*24*time.Hour))
			if err != nil {
				s.writeStoreErr(w, err)
				return
			}
			body.Certs = &overviewCerts{Total: c.Total, Expiring: c.Expiring, Missing: c.Missing}
		}
		if body.Data == nil {
			body.Data = []store.Machine{}
		}
		writeJSON(w, body)
	})

	// Подключения пользователей, сложенные по всем машинам: для списка пользователей панели.
	s.mux.HandleFunc("GET /api/v1/connections/by-user", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		keys, err := parseUserKeys(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		list, err := s.deps.Agents.ConnectionsByUser(r.Context(), keys)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if list == nil {
			list = []store.UserConns{}
		}
		writeJSON(w, map[string]any{"data": list})
	})
}
