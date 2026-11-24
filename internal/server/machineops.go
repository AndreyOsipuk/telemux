package server

// Машины как ноды 3x-ui (ADR 0007, этап C): включение и выключение, удаление,
// проверка связи, история нагрузки и тревоги. Чтение - токен бота или вход в
// панель; запись и проверка связи - то же и только на мастере.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/agentjoin"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// MachineOpsStore - машины этапа C (реализует store.Store; nil - ручки выключены).
type MachineOpsStore interface {
	UpdateTarget(ctx context.Context, code string, p store.TargetPatch, check store.SettingsCheck) (store.Target, error)
	DeleteTarget(ctx context.Context, code string) error
	RecordMetrics(ctx context.Context, code string) error
	MachineMetrics(ctx context.Context, code string, from, to time.Time, step time.Duration) ([]store.MetricPoint, error)
	MachineEvents(ctx context.Context, afterID int64, limit int) ([]store.MachineEvent, int, error)
	PruneMachineOps(ctx context.Context, metricsBefore, eventsBefore time.Time) (int64, int64, error)
	DetectMachineEvents(ctx context.Context, silentAfter time.Duration) ([]store.MachineEvent, error)
	MachineCheckInfo(ctx context.Context, code string) (store.CheckTarget, []store.CheckEndpoint, bool, error)
}

// EndpointProber - сетевые проверки точек входа (тесты подменяют).
type EndpointProber interface {
	// DialTCP открывает и сразу закрывает TCP-соединение.
	DialTCP(ctx context.Context, addr string) error
	// TLSHandshake - TLS-рукопожатие с SNI serverName и проверкой цепочки и имени;
	// notAfter - срок сертификата сервера.
	TLSHandshake(ctx context.Context, addr, serverName string) (notAfter time.Time, err error)
}

// netProber - проверки по сети.
type netProber struct{}

func (netProber) DialTCP(ctx context.Context, addr string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

func (netProber) TLSHandshake(ctx context.Context, addr, serverName string) (time.Time, error) {
	d := tls.Dialer{Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return time.Time{}, err
	}
	defer c.Close() //nolint:errcheck // только проверка
	tc, ok := c.(*tls.Conn)
	if !ok {
		return time.Time{}, errors.New("не TLS-соединение")
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return time.Time{}, errors.New("сервер не прислал сертификат")
	}
	return certs[0].NotAfter, nil
}

const (
	// Хранение истории нагрузки и событий.
	metricsRetention = 8 * 24 * time.Hour
	eventsRetention  = 30 * 24 * time.Hour
	// alertsEvery - как часто мастер ищет переходы down/up и провалы применения.
	alertsEvery = 30 * time.Second

	// Проверка связи: параллельно не больше, длина текста ошибки в ответе.
	checkParallel = 8
	checkErrMax   = 300

	// Метрики: окно по умолчанию и предел точек.
	metricsDefaultWindow = 24 * time.Hour
	maxMetricsPoints     = 2000

	// Лента событий.
	defaultEventsLimit = 100
	maxEventsLimit     = 500

	// Тело PUT /api/v1/targets/{code}.
	maxTargetPatchBody = 64 << 10
	maxTargetSettings  = 32
	maxSettingKeyLen   = 64
	maxSettingValueLen = 4096
)

// Проверка связи: таймаут на точку и на весь запрос (переменные - для тестов).
var (
	checkPerEndpoint = 5 * time.Second
	checkBudget      = 20 * time.Second
)

// metricsSteps - допустимые шаги истории нагрузки.
var metricsSteps = map[string]time.Duration{"1m": time.Minute, "5m": 5 * time.Minute, "1h": time.Hour}

// targetSettingKeys - какие настройки задаются машине какого вида.
var targetSettingKeys = map[string][]string{
	"exit":  {store.SettingAddress, store.SettingBridgeSocks, store.SettingMaintenanceUntil},
	"relay": {store.SettingAddress, store.SettingWebDomains, store.SettingRelayPathsManaged, store.SettingMaintenanceUntil},
	"panel": {"inbound_id", "node_id", "port", "node_code"},
}

// checkTargetSettings проверяет изменённые настройки машины вида kind так же, как
// при добавлении машины. Удалить (пустое значение) можно только web_domains и
// адрес релея, а также завершить объявленное окно обслуживания.
func checkTargetSettings(kind string, patch map[string]string) error {
	allowed := targetSettingKeys[kind]
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := patch[k]
		if !slices.Contains(allowed, k) {
			if len(allowed) == 0 {
				return fmt.Errorf("машине вида %s настройки не задаются", kind)
			}
			return fmt.Errorf("%q не задаётся машине вида %s (допустимы: %s)", k, kind, strings.Join(allowed, ", "))
		}
		if v == "" {
			if k == store.SettingWebDomains || k == store.SettingMaintenanceUntil || (kind == "relay" && k == store.SettingAddress) {
				continue
			}
			return fmt.Errorf("%s нельзя удалить, только заменить", k)
		}
		switch k {
		case store.SettingAddress:
			if kind != "exit" {
				break // адрес релея (announce_ip) проверяет и приводит хранилище
			}
			if _, err := agentjoin.ParseExitAddress(v); err != nil {
				return err
			}
		case store.SettingBridgeSocks:
			if err := agentjoin.ValidBridgeSocks(v); err != nil {
				return err
			}
		case store.SettingMaintenanceUntil:
			until, err := time.Parse(time.RFC3339Nano, v)
			now := time.Now()
			if err != nil || !until.After(now) || until.Sub(now) > store.MaxMaintenanceDuration {
				return fmt.Errorf("%s: нужен будущий срок RFC3339 не дальше %s", k, store.MaxMaintenanceDuration)
			}
		case store.SettingRelayPathsManaged:
			if v != "0" && v != "1" {
				return fmt.Errorf("%s: допустимы 0 и 1", k)
			}
		case "inbound_id", "node_id", "port":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || (k == "port" && n > 65535) {
				return fmt.Errorf("%s %q: нужно целое больше 0 (port - до 65535)", k, v)
			}
		case "node_code":
			if !agentjoin.ValidCode(v) {
				return fmt.Errorf("node_code %q: нужен код машины ^[a-z][a-z0-9-]{1,31}$", v)
			}
		}
		// web_domains проверяет и приводит к каноническому виду хранилище.
	}
	return nil
}

// targetPatchBody - тело PUT /api/v1/targets/{code} (схема TargetPatch).
type targetPatchBody struct {
	Enabled                  *bool             `json:"enabled"`
	Settings                 map[string]string `json:"settings"`
	ExpectedMaintenanceUntil *string           `json:"expected_maintenance_until"`
}

func (b targetPatchBody) validate() error {
	if expected := b.ExpectedMaintenanceUntil; expected != nil {
		if _, ok := b.Settings[store.SettingMaintenanceUntil]; !ok {
			return errors.New("expected_maintenance_until требует settings.maintenance_until")
		}
		if *expected != "" {
			if _, err := time.Parse(time.RFC3339Nano, *expected); err != nil {
				return errors.New("expected_maintenance_until: нужен срок RFC3339 или пустая строка")
			}
		}
	}
	if b.Enabled == nil && b.Settings == nil {
		return errors.New("нужно enabled и (или) settings")
	}
	if len(b.Settings) > maxTargetSettings {
		return fmt.Errorf("settings: не больше %d ключей", maxTargetSettings)
	}
	for k, v := range b.Settings {
		if k == "" || len(k) > maxSettingKeyLen || len(v) > maxSettingValueLen {
			return fmt.Errorf("settings: ключ от 1 до %d символов, значение до %d", maxSettingKeyLen, maxSettingValueLen)
		}
	}
	return nil
}

// machineCode - код машины из пути; не по формату - ok=false (такой машины нет).
func machineCode(r *http.Request) (string, bool) {
	code := r.PathValue("code")
	return code, agentjoin.ValidCode(code)
}

// probeResult - итог одной сетевой проверки. ms - сколько она заняла.
type probeResult struct {
	OK    bool   `json:"ok"`
	Ms    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

// tlsProbeResult - TLS-рукопожатие и сертификат.
type tlsProbeResult struct {
	probeResult
	NotAfter *time.Time `json:"not_after"`
	DaysLeft *int       `json:"days_left"`
}

// endpointCheck - проверка одной точки входа.
type endpointCheck struct {
	ServerNo   int             `json:"server_no"`
	ServerName string          `json:"server_name"`
	EndpointID int64           `json:"endpoint_id"`
	Protocol   string          `json:"protocol"`
	Host       string          `json:"host"`
	Port       int             `json:"port"`
	ViaRelay   bool            `json:"via_relay"`
	TCP        probeResult     `json:"tcp"`
	TLS        *tlsProbeResult `json:"tls"` // только у web
}

// agentFreshness - свежесть агента машины.
type agentFreshness struct {
	SeenAt             *time.Time `json:"seen_at"`
	AgeSeconds         *int64     `json:"age_seconds"`
	Fresh              bool       `json:"fresh"`  // отчёт не старше порога свежести
	Silent             bool       `json:"silent"` // молчит дольше silent_after (или не отчитывался)
	StaleAfterSeconds  int        `json:"stale_after_seconds"`
	SilentAfterSeconds int        `json:"silent_after_seconds"`
}

// machineCheck - ответ POST /api/v1/machines/{code}/check.
type machineCheck struct {
	Code      string          `json:"code"`
	Kind      string          `json:"kind"`
	Enabled   bool            `json:"enabled"`
	CheckedAt time.Time       `json:"checked_at"`
	OK        bool            `json:"ok"` // агент свежий и все точки входа ответили
	Agent     agentFreshness  `json:"agent"`
	Endpoints []endpointCheck `json:"endpoints"`
	Truncated bool            `json:"truncated"`
}

func (s *Server) prober() EndpointProber {
	if s.deps.Prober != nil {
		return s.deps.Prober
	}
	return netProber{}
}

func shortErr(err error) string {
	msg := err.Error()
	if len(msg) > checkErrMax {
		msg = msg[:checkErrMax] + "…"
	}
	return msg
}

// probeEndpoint проверяет точку входа за checkPerEndpoint: TCP к host:port, у web -
// ещё TLS-рукопожатие с SNI=host (порт web - 443, поле port для web не используется).
func (s *Server) probeEndpoint(ctx context.Context, ep store.CheckEndpoint, now time.Time) endpointCheck {
	res := endpointCheck{ServerNo: ep.ServerNo, ServerName: ep.ServerName, EndpointID: ep.EndpointID,
		Protocol: ep.Protocol, Host: ep.Host, Port: ep.Port, ViaRelay: ep.ViaRelay}
	if ep.Protocol == "web" {
		res.Port = 443
		res.TLS = &tlsProbeResult{}
	}
	ctx, cancel := context.WithTimeout(ctx, checkPerEndpoint)
	defer cancel()
	addr := net.JoinHostPort(ep.Host, strconv.Itoa(res.Port))
	p := s.prober()
	start := time.Now()
	err := p.DialTCP(ctx, addr)
	res.TCP.Ms = time.Since(start).Milliseconds()
	if err != nil {
		res.TCP.Error = shortErr(err)
		if res.TLS != nil {
			res.TLS.Error = "не проверялось: нет TCP-подключения"
		}
		return res
	}
	res.TCP.OK = true
	if res.TLS == nil {
		return res
	}
	start = time.Now()
	notAfter, err := p.TLSHandshake(ctx, addr, ep.Host)
	res.TLS.Ms = time.Since(start).Milliseconds()
	if err != nil {
		res.TLS.Error = shortErr(err)
		return res
	}
	res.TLS.OK = true
	na := notAfter.UTC()
	days := int(math.Floor(na.Sub(now).Hours() / 24))
	res.TLS.NotAfter, res.TLS.DaysLeft = &na, &days
	return res
}

// checkMachine проверяет точки входа параллельно (не больше checkParallel) в
// пределах общего бюджета checkBudget; не успевшие - с ошибкой, а не 5xx.
func (s *Server) checkMachine(ctx context.Context, eps []store.CheckEndpoint, now time.Time) []endpointCheck {
	ctx, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()
	out := make([]endpointCheck, len(eps))
	sem := make(chan struct{}, checkParallel)
	var wg sync.WaitGroup
	for i, ep := range eps {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			out[i] = s.skippedEndpoint(ep)
			continue
		}
		wg.Add(1)
		go func(i int, ep store.CheckEndpoint) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = s.probeEndpoint(ctx, ep, now)
		}(i, ep)
	}
	wg.Wait()
	return out
}

// skippedEndpoint - точка, до которой не дошла очередь за общий бюджет.
func (s *Server) skippedEndpoint(ep store.CheckEndpoint) endpointCheck {
	res := endpointCheck{ServerNo: ep.ServerNo, ServerName: ep.ServerName, EndpointID: ep.EndpointID,
		Protocol: ep.Protocol, Host: ep.Host, Port: ep.Port, ViaRelay: ep.ViaRelay}
	msg := fmt.Sprintf("не проверялось: истёк общий бюджет %s", checkBudget)
	res.TCP.Error = msg
	if ep.Protocol == "web" {
		res.Port = 443
		res.TLS = &tlsProbeResult{probeResult: probeResult{Error: msg}}
	}
	return res
}

// parseMetricsRange - окно и шаг истории нагрузки; ошибка - текст для 400.
func parseMetricsRange(r *http.Request, now time.Time) (from, to time.Time, step time.Duration, stepName string, err error) {
	q := r.URL.Query()
	stepName = q.Get("step")
	if stepName == "" {
		stepName = "5m"
	}
	step, ok := metricsSteps[stepName]
	if !ok {
		return from, to, 0, "", fmt.Errorf("step %q: допустимы 1m, 5m, 1h", stepName)
	}
	parse := func(name string) (time.Time, bool, error) {
		v := q.Get(name)
		if v == "" {
			return time.Time{}, false, nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return t, false, fmt.Errorf("%s - время RFC3339, например 2026-11-19T10:00:00Z", name)
		}
		return t, true, nil
	}
	to, hasTo, err := parse("to")
	if err != nil {
		return from, to, 0, "", err
	}
	if !hasTo {
		to = now
	}
	from, hasFrom, err := parse("from")
	if err != nil {
		return from, to, 0, "", err
	}
	if !hasFrom {
		from = to.Add(-metricsDefaultWindow)
	}
	if !from.Before(to) {
		return from, to, 0, "", errors.New("from должно быть раньше to")
	}
	if points := math.Ceil(float64(to.Sub(from)) / float64(step)); points > maxMetricsPoints {
		return from, to, 0, "", codedErr("too_many_points",
			fmt.Errorf("точек %.0f, не больше %d: сузьте окно или увеличьте step", points, maxMetricsPoints))
	}
	return from.UTC(), to.UTC(), step, stepName, nil
}

// republishAfterEnable - включённая машина догоняет пользователей, изменённых,
// пока она была выключена (новая ревизия telemt.users только тем, у кого она устарела).
func (s *Server) republishAfterEnable(ctx context.Context, code string) {
	if s.deps.Revisions == nil {
		return
	}
	res, err := s.deps.Revisions.PublishTelemtUsers(ctx, s.deps.DefaultMaxConns)
	if err != nil {
		s.deps.Log.Error("машина включена, но публикация telemt.users не удалась - будет при следующем изменении пользователей",
			"target", code, "err", err)
		return
	}
	if len(res.Changed) > 0 {
		s.deps.Log.Info("машина включена: опубликована ревизия telemt.users", "target", code, "revision", res.Revision, "changed", res.Changed)
	}
}

func (s *Server) routesMachineOps() {
	// Включить, выключить машину и поправить её настройки (частичное обновление).
	s.mux.HandleFunc("PUT /api/v1/targets/{code}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		code, ok := machineCode(r)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		var body targetPatchBody
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTargetPatchBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("тело больше %d байт", maxTargetPatchBody), s.deps.Log)
				return
			}
			writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON {enabled?, settings?}: %w", err), s.deps.Log)
			return
		}
		if err := body.validate(); err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		var check store.SettingsCheck
		if body.Settings != nil {
			check = func(kind string, _ map[string]string) error { return checkTargetSettings(kind, body.Settings) }
		}
		t, err := s.deps.Ops.UpdateTarget(r.Context(), code, store.TargetPatch{
			Enabled: body.Enabled, Settings: body.Settings, ExpectedMaintenanceUntil: body.ExpectedMaintenanceUntil,
		}, check)
		if errors.Is(err, store.ErrUnknownTarget) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.deps.Log.Info("машина изменена", "target", code, "enabled", t.Enabled, "settings_changed", body.Settings != nil)
		if body.Enabled != nil && *body.Enabled {
			s.republishAfterEnable(r.Context(), code)
		}
		s.revs.bump()
		if t.Resources == nil {
			t.Resources = []string{}
		}
		if t.Settings == nil {
			t.Settings = map[string]string{}
		}
		writeJSON(w, t)
	})

	// Удалить выключенную машину, на которую никто не ссылается.
	s.mux.HandleFunc("DELETE /api/v1/targets/{code}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		code, ok := machineCode(r)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		err := s.deps.Ops.DeleteTarget(r.Context(), code)
		if errors.Is(err, store.ErrUnknownTarget) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.deps.Log.Info("машина удалена", "target", code)
		w.WriteHeader(http.StatusNoContent)
	})

	// Проверка связи: свежесть агента и точки входа серверов машины.
	s.mux.HandleFunc("POST /api/v1/machines/{code}/check", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		code, ok := machineCode(r)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		t, eps, truncated, err := s.deps.Ops.MachineCheckInfo(r.Context(), code)
		if errors.Is(err, store.ErrUnknownTarget) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		now := time.Now()
		res := machineCheck{Code: t.Code, Kind: t.Kind, Enabled: t.Enabled, CheckedAt: now.UTC(), Truncated: truncated,
			Agent: agentFreshness{SeenAt: t.SeenAt, StaleAfterSeconds: int(s.staleAfter() / time.Second),
				SilentAfterSeconds: int(store.SilentAfter / time.Second), Silent: true}}
		if t.SeenAt != nil {
			age := now.Sub(*t.SeenAt)
			sec := int64(age / time.Second)
			res.Agent.AgeSeconds = &sec
			res.Agent.Fresh = age <= s.staleAfter()
			res.Agent.Silent = age > store.SilentAfter
		}
		res.Endpoints = s.checkMachine(r.Context(), eps, now)
		res.OK = res.Agent.Fresh
		for _, e := range res.Endpoints {
			if !e.TCP.OK || (e.TLS != nil && !e.TLS.OK) {
				res.OK = false
			}
		}
		writeJSON(w, res)
	})

	// История нагрузки машины, усреднённая по шагу.
	s.mux.HandleFunc("GET /api/v1/machines/{code}/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		from, to, step, stepName, err := parseMetricsRange(r, time.Now())
		if err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		code := r.PathValue("code")
		points, err := s.deps.Ops.MachineMetrics(r.Context(), code, from, to, step)
		if errors.Is(err, store.ErrUnknownTarget) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"code": code, "from": from, "to": to, "step": stepName, "data": points})
	})

	// Лента тревог для бота: события после after_id по возрастанию id.
	s.mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		var afterID int64
		if v := r.URL.Query().Get("after_id"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				writeErr(w, http.StatusBadRequest, errors.New("after_id - целое не меньше 0"), s.deps.Log)
				return
			}
			afterID = n
		}
		limit, err := intParam(r, "limit", defaultEventsLimit, 1, maxEventsLimit)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		list, total, err := s.deps.Ops.MachineEvents(r.Context(), afterID, limit)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, pageBody(list, total, limit, 0))
	})
}

// recordMetrics пишет строку истории нагрузки после принятого runtime-отчёта.
// Отдельно от транзакции отчёта: сбой истории не теряет снимок (ошибка - в лог).
func (s *Server) recordMetrics(ctx context.Context, code string) {
	if s.deps.Ops == nil {
		return
	}
	if err := s.deps.Ops.RecordMetrics(ctx, code); err != nil {
		s.deps.Log.Error("история нагрузки: строка не записана", "target", code, "err", err)
	}
}

// pruneMachineOps чистит историю нагрузки старше 8 дней и события старше 30 дней.
// Вызывается из цикла чистки runtime (только на мастере).
func (s *Server) pruneMachineOps(ctx context.Context) {
	if s.deps.Ops == nil {
		return
	}
	now := time.Now()
	m, e, err := s.deps.Ops.PruneMachineOps(ctx, now.Add(-metricsRetention), now.Add(-eventsRetention))
	if err != nil {
		s.deps.Log.Warn("чистка истории нагрузки и событий не удалась, повтор при следующем проходе", "err", err)
		return
	}
	if m > 0 || e > 0 {
		s.deps.Log.Info("чистка истории нагрузки и событий", "metrics", m, "events", e)
	}
}

// alertsOnce - один проход детектора тревог (только мастер).
func (s *Server) alertsOnce(ctx context.Context) {
	st, err := s.currentRole(ctx)
	if err != nil || !st.IsMaster() {
		return
	}
	events, err := s.deps.Ops.DetectMachineEvents(ctx, store.SilentAfter)
	if err != nil {
		s.deps.Log.Error("тревоги: проход не удался, повтор через 30 с", "err", err)
		return
	}
	for _, e := range events {
		s.deps.Log.Warn("тревога", "id", e.ID, "machine", e.Machine, "kind", e.Kind, "detail", e.Detail)
	}
}

// alertsLoop - детектор тревог раз в alertsEvery.
func (s *Server) alertsLoop(ctx context.Context) {
	t := time.NewTicker(alertsEvery)
	defer t.Stop()
	for {
		s.alertsOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
