package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// Лимиты runtime-отчёта агента: превышение - 400, тело больше maxRuntimeBody - 413.
const (
	maxRuntimeBody      = 2 << 20
	maxRuntimeUsers     = 5000 // в каждом продукте
	maxRuntimeActiveIPs = 20   // на пользователя
	maxRuntimeLogItems  = 5000
	maxRuntimeKeyLen    = 128
	maxRuntimeIPLen     = 64
	maxRuntimeTextLen   = 1024 // версия, ошибка, домен

	// RuntimeEveryDefault - период runtime-отчёта агента по умолчанию.
	RuntimeEveryDefault = 60 * time.Second
	// logErrorsWindow - сколько мастер хранит счётчики журнала; since раньше - 400.
	logErrorsWindow = 24 * time.Hour
	// logErrorsSkew - запас на расхождение часов бота и мастера для since.
	logErrorsSkew = time.Minute
	// runtimePruneEvery - как часто мастер чистит счётчики журнала старше суток
	// (и историю нагрузки и события - machineops.go).
	runtimePruneEvery = 10 * time.Minute

	// Период журнала в отчёте агента. Агент читает журнал не дальше чем на час
	// назад (не дошедшие отчёты), конец периода - его текущее время.
	maxLogPeriod   = time.Hour + 5*time.Minute // длина периода: окно агента с запасом
	maxLogEndAge   = time.Hour                 // конец не раньше now - час - перекос часов
	maxClockSkew   = 5 * time.Minute           // перекос часов агента и мастера
	maxLogCount    = 1_000_000_000             // счётчик одной строки журнала
	maxHostRateMbs = 1_000_000                 // net_* - мбит/с, больше 1 Тбит/с - мусор
)

// runtimeStaleDefault - порог устаревания снимка по умолчанию: три периода отчёта
// (один-два пропуска - ещё не повод считать агента молчащим).
func runtimeStaleDefault(every time.Duration) time.Duration {
	if every <= 0 {
		every = RuntimeEveryDefault
	}
	return 3 * every
}

// staleAfter - порог устаревания runtime-снимка: из настройки или по умолчанию.
func (s *Server) staleAfter() time.Duration {
	if s.deps.RuntimeStaleAfter > 0 {
		return s.deps.RuntimeStaleAfter
	}
	return runtimeStaleDefault(RuntimeEveryDefault)
}

// isStale - снимок устарел или его нет.
func (s *Server) isStale(seenAt *time.Time, now time.Time) bool {
	return seenAt == nil || now.Sub(*seenAt) > s.staleAfter()
}

// validHost проверяет нагрузку машины на разумность: проценты 0..100, остальное
// не меньше 0 и конечно (мусор от агента не должен попасть в панель и алерты).
func validHost(h *store.RuntimeHost) error {
	if h == nil {
		return nil
	}
	bad := func(v, hi float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > hi }
	switch {
	case bad(h.CPUPct, 100), bad(h.MemPct, 100), bad(h.DiskPct, 100):
		return codedErr("host_invalid", errors.New("host: cpu_pct, mem_pct, disk_pct - от 0 до 100"))
	case bad(h.Load1, math.MaxFloat64), bad(h.NetRxMbps, maxHostRateMbs), bad(h.NetTxMbps, maxHostRateMbs):
		return codedErr("host_invalid", fmt.Errorf("host: load1 не меньше 0, net_rx_mbps и net_tx_mbps - от 0 до %d", maxHostRateMbs))
	case h.UptimeS < 0:
		return codedErr("host_invalid", errors.New("host: uptime_s не меньше 0"))
	}
	return nil
}

// validLogPeriod - период журнала не в будущем, не старше окна агента и не
// длиннее его: иначе один отчёт с убежавшими часами портит суммы за сутки.
func validLogPeriod(le *store.RuntimeLogErrors, now time.Time) error {
	if le.PeriodStart.IsZero() || le.PeriodEnd.IsZero() || le.PeriodEnd.Before(le.PeriodStart) {
		return codedErr("log_period_invalid", errors.New("log_errors: нужны period_start и period_end, конец не раньше начала"))
	}
	if le.PeriodEnd.After(now.Add(maxClockSkew)) {
		return codedErr("log_period_invalid", fmt.Errorf("log_errors: period_end в будущем (часы агента спешат больше чем на %s)", maxClockSkew))
	}
	if le.PeriodEnd.Before(now.Add(-maxLogEndAge - maxClockSkew)) {
		return codedErr("log_period_invalid", fmt.Errorf("log_errors: period_end старше %s", maxLogEndAge+maxClockSkew))
	}
	if le.PeriodEnd.Sub(le.PeriodStart) > maxLogPeriod {
		return codedErr("log_period_invalid", fmt.Errorf("log_errors: период длиннее %s", maxLogPeriod))
	}
	return nil
}

func validRuntimeUsers(product string, users []store.RuntimeUser) error {
	if len(users) > maxRuntimeUsers {
		return fmt.Errorf("%s: не больше %d пользователей", product, maxRuntimeUsers)
	}
	seen := make(map[string]bool, len(users))
	for _, u := range users {
		if u.Key == "" || len(u.Key) > maxRuntimeKeyLen {
			return fmt.Errorf("%s: у пользователя нужен ключ до %d символов", product, maxRuntimeKeyLen)
		}
		if seen[u.Key] {
			return fmt.Errorf("%s: пользователь %q повторяется", product, u.Key)
		}
		seen[u.Key] = true
		if u.Connections < 0 || u.BytesIn < 0 || u.BytesOut < 0 {
			return fmt.Errorf("%s: у %q отрицательные счётчики", product, u.Key)
		}
		if len(u.ActiveIPs) > maxRuntimeActiveIPs {
			return fmt.Errorf("%s: у %q больше %d адресов", product, u.Key, maxRuntimeActiveIPs)
		}
		for _, ip := range u.ActiveIPs {
			if ip == "" || len(ip) > maxRuntimeIPLen {
				return fmt.Errorf("%s: у %q пустой или слишком длинный адрес", product, u.Key)
			}
		}
	}
	return nil
}

func validText(name string, vals ...string) error {
	for _, v := range vals {
		if len(v) > maxRuntimeTextLen {
			return fmt.Errorf("%s: строка длиннее %d символов", name, maxRuntimeTextLen)
		}
	}
	return nil
}

// validateRuntime проверяет лимиты и значения отчёта (target сверяется отдельно);
// now - время мастера для проверки периода журнала.
func validateRuntime(rep store.RuntimeReport, now time.Time) error {
	if err := validText("agent_version", rep.AgentVersion); err != nil {
		return err
	}
	if err := validHost(rep.Host); err != nil {
		return err
	}
	if t := rep.Telemt; t != nil {
		if err := validText("telemt", t.Version, t.Error, t.TLSDomain, t.ConfigSHA256); err != nil {
			return err
		}
		if t.UsersConfigured < 0 || t.UsersEffective < 0 || t.ConnectionsTotal < 0 {
			return errors.New("telemt: отрицательные счётчики")
		}
		if err := validRuntimeUsers(store.ProductTelemt, t.Users); err != nil {
			return err
		}
	}
	if m := rep.Mtg; m != nil {
		if err := validText("mtg", m.Version, m.Error); err != nil {
			return err
		}
		if m.ConnectionsTotal < 0 {
			return errors.New("mtg: отрицательные счётчики")
		}
		if err := validRuntimeUsers(store.ProductMtg, m.Users); err != nil {
			return err
		}
	}
	if le := rep.LogErrors; le != nil {
		if len(le.Items) > maxRuntimeLogItems {
			return fmt.Errorf("log_errors: не больше %d строк", maxRuntimeLogItems)
		}
		if err := validLogPeriod(le, now); err != nil {
			return err
		}
		seen := make(map[[2]string]bool, len(le.Items))
		for _, it := range le.Items {
			if !store.LogErrorKinds[it.Kind] {
				return fmt.Errorf("log_errors: вид %q, допустимы limit, handshake, auth, timeout, other", it.Kind)
			}
			if len(it.Key) > maxRuntimeKeyLen || it.Count < 0 {
				return errors.New("log_errors: ключ до 128 символов, счётчик не меньше 0")
			}
			if it.Count > maxLogCount {
				return codedErr("log_count_invalid", fmt.Errorf("log_errors: счётчик больше %d", maxLogCount))
			}
			k := [2]string{it.Key, it.Kind}
			if seen[k] {
				return fmt.Errorf("log_errors: строка %q/%s повторяется", it.Key, it.Kind)
			}
			seen[k] = true
		}
	}
	return nil
}

// pruneRuntime удаляет счётчики журнала старше суток, историю нагрузки старше
// 8 дней и события старше 30 дней. Только на мастере: на реплике база только для чтения.
func (s *Server) pruneRuntime(ctx context.Context) {
	st, err := s.currentRole(ctx)
	if err != nil || !st.IsMaster() {
		return
	}
	defer s.pruneMachineOps(ctx)
	n, err := s.deps.Agents.PruneLogErrors(ctx, time.Now().Add(-logErrorsWindow))
	if err != nil {
		s.deps.Log.Warn("чистка runtime_log_errors не удалась, повтор при следующем проходе", "err", err)
		return
	}
	if n > 0 {
		s.deps.Log.Info("чистка runtime_log_errors", "deleted", n)
	}
}

// runtimePruneLoop - периодическая чистка счётчиков журнала (раз в runtimePruneEvery).
// Отдельно от отчётов агентов: удаление в каждом отчёте каждой машины давало
// лишние блокировки и дедлоки между параллельными отчётами.
func (s *Server) runtimePruneLoop(ctx context.Context) {
	t := time.NewTicker(runtimePruneEvery)
	defer t.Stop()
	for {
		s.pruneRuntime(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) routesRuntime() {
	// Runtime-отчёт агента: нагрузка, версии, подключения, ошибки журнала.
	s.mux.HandleFunc("POST /v1/runtime", func(w http.ResponseWriter, r *http.Request) {
		code := s.agentTarget(w, r)
		if code == "" {
			return
		}
		var rep store.RuntimeReport
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRuntimeBody)).Decode(&rep); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("отчёт больше %d байт", maxRuntimeBody), s.deps.Log)
				return
			}
			writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON отчёта: %w", err), s.deps.Log)
			return
		}
		if rep.Target != code {
			writeErr(w, http.StatusForbidden, fmt.Errorf("токен выдан машине %q", code), s.deps.Log)
			return
		}
		if err := validateRuntime(rep, time.Now()); err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		if err := s.deps.Agents.RecordRuntime(r.Context(), rep); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.recordMetrics(r.Context(), code)
		w.WriteHeader(http.StatusNoContent)
	})

	// Последний снимок машины без списков пользователей.
	s.mux.HandleFunc("GET /api/v1/machines/{code}/runtime", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		mr, err := s.deps.Agents.MachineRuntime(r.Context(), r.PathValue("code"))
		if errors.Is(err, store.ErrUnknownTarget) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", r.PathValue("code")), s.deps.Log)
			return
		}
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if mr.Telemt != nil {
			mr.Telemt.Users = nil
		}
		if mr.Mtg != nil {
			mr.Mtg.Users = nil
		}
		mr.Stale = s.isStale(mr.SeenAt, time.Now())
		writeJSON(w, mr)
	})

	// Подключения пользователей по последним снимкам машин.
	s.mux.HandleFunc("GET /api/v1/connections", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		limit, offset, err := parseStrictPaging(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		q := r.URL.Query()
		f := store.ConnFilter{Machine: q.Get("machine"), Product: q.Get("product"), User: q.Get("user")}
		if len(f.User) > maxRuntimeKeyLen {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("user - префикс ключа до %d символов", maxRuntimeKeyLen), s.deps.Log)
			return
		}
		if f.Product != "" && f.Product != store.ProductTelemt && f.Product != store.ProductMtg {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("product %q: допустимы telemt, mtg", f.Product), s.deps.Log)
			return
		}
		if v := q.Get("min_connections"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				writeErr(w, http.StatusBadRequest, errors.New("min_connections - целое не меньше 0"), s.deps.Log)
				return
			}
			f.MinConnections = n
		}
		rows, total, err := s.deps.Agents.Connections(r.Context(), f, limit, offset)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		now := time.Now()
		for i := range rows {
			rows[i].Stale = s.isStale(&rows[i].SeenAt, now)
		}
		if rows == nil {
			rows = []store.ConnRow{}
		}
		writePage(w, rows, total, limit, offset)
	})

	// Сводка подключений: строка на машину и продукт.
	s.mux.HandleFunc("GET /api/v1/connections/summary", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		limit, offset, err := parseStrictPaging(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		rows, total, err := s.deps.Agents.ConnectionsSummary(r.Context(), limit, offset)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		now := time.Now()
		for i := range rows {
			rows[i].Stale = s.isStale(&rows[i].SeenAt, now)
		}
		if rows == nil {
			rows = []store.ConnSummary{}
		}
		writePage(w, rows, total, limit, offset)
	})

	// Ошибки журнала telemt за окно (не больше 24 ч).
	s.mux.HandleFunc("GET /api/v1/log-errors", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		limit, offset, err := parseStrictPaging(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		q := r.URL.Query()
		since, err := time.Parse(time.RFC3339, q.Get("since"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("since - обязательное время RFC3339, например 2026-11-19T10:00:00Z"), s.deps.Log)
			return
		}
		if since.Before(time.Now().Add(-logErrorsWindow - logErrorsSkew)) {
			writeErr(w, http.StatusBadRequest, errors.New("since не раньше 24 часов назад: старше счётчики не хранятся"), s.deps.Log)
			return
		}
		rows, total, err := s.deps.Agents.LogErrors(r.Context(), since, q.Get("machine"), limit, offset)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if rows == nil {
			rows = []store.LogErrorSum{}
		}
		writePage(w, rows, total, limit, offset)
	})
}
