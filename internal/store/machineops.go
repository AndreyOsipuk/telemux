package store

// Машины как ноды 3x-ui (ADR 0007, этап C): включение и выключение машины,
// удаление, история нагрузки (machine_metrics), тревоги (machine_events) и данные
// для проверки связи. Таблицы - migrations/0012_machine_ops.sql.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MachineOpsTables - таблицы миграции 0012; без них ручки этапа C не монтируются.
var MachineOpsTables = []string{"machine_metrics", "machine_events", "machine_alert_state", "machine_apply_alerts"}

// CheckMachineOpsSchema - применены ли миграции 0012 и 0010 (удаление машины и
// проверка связи читают серверы и точки входа).
func (s *Store) CheckMachineOpsSchema(ctx context.Context) error {
	if err := s.checkTables(ctx, ServersTables, "migrations/0010_servers.sql"); err != nil {
		return err
	}
	return s.checkTables(ctx, MachineOpsTables, "migrations/0012_machine_ops.sql")
}

// Ключ advisory-lock детектора тревог: два прохода (например, на двух узлах в
// момент смены мастера) не пишут одно событие дважды.
const alertsLockKey int64 = 0x74656c656d757802 // "telemux" + 2

// maxCheckEndpoints - сколько точек входа машины проверяется за один запрос.
const maxCheckEndpoints = 200

var (
	// ErrMachineEnabled - удалить можно только выключенную машину.
	ErrMachineEnabled = errors.New("машина включена: сначала выключите её (enabled=false)")
	// ErrMachineInUse - на машину ссылаются серверы, выходы или входы панели.
	ErrMachineInUse = errors.New("машина используется")
	// ErrMachineDisabled - машина выключена: новые ревизии ей не публикуются.
	ErrMachineDisabled = errors.New("машина выключена: новые ревизии ей не публикуются")
	// ErrMaintenanceConflict - окно поменялось после проверки состояния.
	ErrMaintenanceConflict = errors.New("окно обслуживания уже занято или изменилось")
)

// MachineInUseError - кто ссылается на машину (удаление запрещено).
type MachineInUseError struct {
	Code    string
	Servers []int    // номера серверов (relay_code или node_code)
	Exits   []string // выходы релея
	Panels  []string // входы панели с node_code этой машины
}

func (e *MachineInUseError) Error() string {
	var parts []string
	if len(e.Servers) > 0 {
		nums := make([]string, len(e.Servers))
		for i, n := range e.Servers {
			nums[i] = strconv.Itoa(n)
		}
		parts = append(parts, "серверы "+strings.Join(nums, ", "))
	}
	if len(e.Exits) > 0 {
		parts = append(parts, "выходы "+strings.Join(e.Exits, ", "))
	}
	if len(e.Panels) > 0 {
		parts = append(parts, "входы панели "+strings.Join(e.Panels, ", "))
	}
	return fmt.Sprintf("на машину %q ссылаются %s", e.Code, strings.Join(parts, "; "))
}

// Is - errors.Is(err, ErrMachineInUse).
func (e *MachineInUseError) Is(target error) bool { return target == ErrMachineInUse }

// TargetPatch - частичное обновление машины: nil - поле не меняется.
type TargetPatch struct {
	Enabled *bool
	// Settings сливается с текущими настройками: ключ с пустым значением удаляется.
	Settings map[string]string
	// ExpectedMaintenanceUntil - ожидаемый срок; пустая строка означает отсутствие активного окна.
	ExpectedMaintenanceUntil *string
}

// SettingsCheck проверяет настройки машины вида kind после слияния (ошибка - 400).
type SettingsCheck func(kind string, settings map[string]string) error

// UpdateTarget меняет enabled и настройки машины одной транзакцией под блокировкой
// строки. check вызывается на итоговых настройках (после слияния), ошибка
// оборачивается в ErrBadSettings. При выключении сбрасывается состояние детектора
// тревог: после включения машина начинает с чистого листа. Нет машины - ErrUnknownTarget.
func (s *Store) UpdateTarget(ctx context.Context, code string, p TargetPatch, check SettingsCheck) (Target, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Target{}, fmt.Errorf("begin update target: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	t, err := loadTarget(ctx, tx, code, "FOR UPDATE")
	if err != nil {
		return t, err
	}
	if t.Settings == nil {
		t.Settings = map[string]string{}
	}
	if p.Settings != nil {
		merged := maps.Clone(t.Settings)
		for k, v := range p.Settings {
			if v == "" {
				delete(merged, k)
			} else {
				merged[k] = v
			}
		}
		if check != nil {
			if err := check(t.Kind, merged); err != nil {
				return t, fmt.Errorf("%w: %v", ErrBadSettings, err)
			}
		}
		if err := checkMaintenanceTransition(t.Settings[SettingMaintenanceUntil], p, time.Now()); err != nil {
			return t, err
		}
		if err := normalizeSettings(t.Kind, merged); err != nil {
			return t, err
		}
		t.Settings = merged
	}
	if p.Enabled != nil {
		t.Enabled = *p.Enabled
	}
	if _, err := tx.Exec(ctx,
		`UPDATE targets SET enabled = $2, settings = $3, updated_at = now() WHERE code = $1`,
		code, t.Enabled, t.Settings); err != nil {
		return t, fmt.Errorf("update target %q: %w", code, err)
	}
	if !t.Enabled {
		if _, err := tx.Exec(ctx, `DELETE FROM machine_alert_state WHERE code = $1`, code); err != nil {
			return t, fmt.Errorf("сброс тревог %q: %w", code, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return t, fmt.Errorf("commit update target: %w", err)
	}
	return t, nil
}

// DeleteTarget удаляет выключенную машину со всем, что к ней относится: токены,
// желаемое и применённое состояние, журнал, runtime, метрики. Включённая -
// ErrMachineEnabled; на неё ссылаются серверы, выходы или входы панели -
// *MachineInUseError (errors.Is(err, ErrMachineInUse)). Нет машины - ErrUnknownTarget.
// События machine_events остаются: их ещё может не забрать бот.
func (s *Store) DeleteTarget(ctx context.Context, code string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete target: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	t, err := loadTarget(ctx, tx, code, "FOR UPDATE")
	if err != nil {
		return err
	}
	if t.Enabled {
		return ErrMachineEnabled
	}
	inUse := &MachineInUseError{Code: code, Servers: []int{}, Exits: []string{}, Panels: []string{}}
	if err := collectInts(ctx, tx, &inUse.Servers,
		`SELECT no FROM servers WHERE relay_code = $1 OR node_code = $1 ORDER BY no`, code); err != nil {
		return fmt.Errorf("серверы машины %q: %w", code, err)
	}
	if err := collectStrings(ctx, tx, &inUse.Exits,
		`SELECT code FROM targets WHERE kind = 'exit' AND relay_code = $1 ORDER BY code`, code); err != nil {
		return fmt.Errorf("выходы машины %q: %w", code, err)
	}
	if err := collectStrings(ctx, tx, &inUse.Panels,
		`SELECT code FROM targets WHERE kind = 'panel' AND settings->>'node_code' = $1 ORDER BY code`, code); err != nil {
		return fmt.Errorf("входы панели машины %q: %w", code, err)
	}
	if len(inUse.Servers)+len(inUse.Exits)+len(inUse.Panels) > 0 {
		return inUse
	}
	// Таблицы без внешнего ключа на targets чистятся явно, остальное - каскадом.
	for _, q := range []string{
		`DELETE FROM observed_state WHERE target = $1`,
		`DELETE FROM apply_log WHERE target = $1`,
		`DELETE FROM agent_state WHERE target = $1`,
		`DELETE FROM targets WHERE code = $1`,
	} {
		if _, err := tx.Exec(ctx, q, code); err != nil {
			return fmt.Errorf("удаление машины %q: %w", code, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete target: %w", err)
	}
	return nil
}

func collectInts(ctx context.Context, db queryer, out *[]int, sql string, args ...any) error {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return err
		}
		*out = append(*out, v)
	}
	return rows.Err()
}

func collectStrings(ctx context.Context, db queryer, out *[]string, sql string, args ...any) error {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
		*out = append(*out, v)
	}
	return rows.Err()
}

// hostNum - число из JSON-снимка нагрузки; не число или нет снимка - NULL.
func hostNum(key string) string {
	return `CASE WHEN jsonb_typeof(r.host->'` + key + `') = 'number' THEN (r.host->>'` + key + `')::float8 END`
}

// RecordMetrics пишет строку истории нагрузки машины из её последнего
// runtime-снимка (agent_runtime) и подключений (runtime_user_conns - как в
// /api/v1/machines). Время округляется до минуты; повтор в ту же минуту
// перезаписывает строку. Нет снимка - ничего не пишется.
func (s *Store) RecordMetrics(ctx context.Context, code string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO machine_metrics (code, at, cpu, mem, disk, load1, rx_bps, tx_bps, connections, users_online)
		 SELECT r.target, date_trunc('minute', r.seen_at),
		        `+hostNum("cpu_pct")+`, `+hostNum("mem_pct")+`, `+hostNum("disk_pct")+`, `+hostNum("load1")+`,
		        `+hostNum("net_rx_mbps")+` * 1e6, `+hostNum("net_tx_mbps")+` * 1e6,
		        COALESCE(u.conns, 0), COALESCE(u.online, 0)
		   FROM agent_runtime r
		   LEFT JOIN LATERAL (
		     SELECT sum(c.connections)::bigint AS conns,
		            count(DISTINCT c.user_key) FILTER (WHERE c.connections > 0)::int AS online
		       FROM runtime_user_conns c WHERE c.target = r.target) u ON TRUE
		  WHERE r.target = $1
		 ON CONFLICT (code, at) DO UPDATE
		   SET cpu = EXCLUDED.cpu, mem = EXCLUDED.mem, disk = EXCLUDED.disk, load1 = EXCLUDED.load1,
		       rx_bps = EXCLUDED.rx_bps, tx_bps = EXCLUDED.tx_bps,
		       connections = EXCLUDED.connections, users_online = EXCLUDED.users_online`, code)
	if err != nil {
		return fmt.Errorf("machine_metrics %s: %w", code, err)
	}
	return nil
}

// MetricPoint - средние значения нагрузки машины за шаг. Нагрузка - nil, если
// за шаг агент ни разу не прислал host.
type MetricPoint struct {
	At          time.Time `json:"at"`
	CPU         *float64  `json:"cpu"`
	Mem         *float64  `json:"mem"`
	Disk        *float64  `json:"disk"`
	Load1       *float64  `json:"load1"`
	RxBps       *float64  `json:"rx_bps"`
	TxBps       *float64  `json:"tx_bps"`
	Connections float64   `json:"connections"`
	UsersOnline float64   `json:"users_online"`
}

// metricsOrigin - начало отсчёта шагов: границы шагов не зависят от from.
var metricsOrigin = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// MachineMetrics - история нагрузки машины в [from, to), усреднённая по шагу step
// (date_bin в SQL). Шаги без строк не возвращаются. Нет машины - ErrUnknownTarget.
func (s *Store) MachineMetrics(ctx context.Context, code string, from, to time.Time, step time.Duration) ([]MetricPoint, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM targets WHERE code = $1)`, code).Scan(&exists); err != nil {
		return nil, fmt.Errorf("машина %q: %w", code, err)
	}
	if !exists {
		return nil, ErrUnknownTarget
	}
	rows, err := s.pool.Query(ctx,
		`SELECT date_bin(make_interval(secs => $4), at, $5) AS bucket,
		        avg(cpu)::float8, avg(mem)::float8, avg(disk)::float8, avg(load1)::float8,
		        avg(rx_bps)::float8, avg(tx_bps)::float8,
		        avg(connections)::float8, avg(users_online)::float8
		   FROM machine_metrics
		  WHERE code = $1 AND at >= $2 AND at < $3
		  GROUP BY bucket ORDER BY bucket`,
		code, from, to, step.Seconds(), metricsOrigin)
	if err != nil {
		return nil, fmt.Errorf("machine_metrics %s: %w", code, err)
	}
	defer rows.Close()
	out := []MetricPoint{}
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.At, &p.CPU, &p.Mem, &p.Disk, &p.Load1, &p.RxBps, &p.TxBps, &p.Connections, &p.UsersOnline); err != nil {
			return nil, fmt.Errorf("scan machine_metrics: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MachineEvent - тревога по машине. У события пары (relay_silent, relay_ok,
// foreign_bridge) machine - релей, exit - выход; у остальных exit пустой.
type MachineEvent struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Machine string    `json:"machine"`
	Kind    string    `json:"kind"` // down | up | apply_failed | relay_silent | relay_ok | foreign_bridge | relay_no_exit
	Detail  string    `json:"detail"`
	Exit    string    `json:"exit,omitempty"`
}

// eventCols - колонки machine_events в порядке scanEvents.
const eventCols = `id, at, machine, kind, detail, COALESCE(exit, '')`

// MachineEvents - события с id больше afterID по возрастанию id, не больше limit;
// total - сколько всего событий после afterID.
func (s *Store) MachineEvents(ctx context.Context, afterID int64, limit int) ([]MachineEvent, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM machine_events WHERE id > $1`, afterID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count machine_events: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+eventCols+` FROM machine_events WHERE id > $1 ORDER BY id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("machine_events: %w", err)
	}
	list, err := scanEvents(rows)
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func scanEvents(rows pgx.Rows) ([]MachineEvent, error) {
	defer rows.Close()
	out := []MachineEvent{}
	for rows.Next() {
		var e MachineEvent
		if err := rows.Scan(&e.ID, &e.At, &e.Machine, &e.Kind, &e.Detail, &e.Exit); err != nil {
			return nil, fmt.Errorf("scan machine_events: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("machine_events: %w", err)
	}
	return out, nil
}

// PruneMachineOps удаляет строки истории нагрузки раньше metricsBefore и события
// раньше eventsBefore. Вызывается периодической задачей мастера.
func (s *Store) PruneMachineOps(ctx context.Context, metricsBefore, eventsBefore time.Time) (metrics, events int64, err error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM machine_metrics WHERE at < $1`, metricsBefore)
	if err != nil {
		return 0, 0, fmt.Errorf("чистка machine_metrics: %w", err)
	}
	metrics = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `DELETE FROM machine_events WHERE at < $1`, eventsBefore)
	if err != nil {
		return metrics, 0, fmt.Errorf("чистка machine_events: %w", err)
	}
	return metrics, tag.RowsAffected(), nil
}

// DetectMachineEvents - один проход детектора тревог по включённым машинам с агентом
// (машина, где агента ещё не ставили, не молчит):
//   - down - агент молчит дольше silentAfter (последний отчёт observed или runtime),
//     up - снова на связи; событие пишется только на переход. Машину, впервые
//     увиденной живой, детектор запоминает молча, впервые увиденной молчащей - down.
//     Релей down только по правилу R3.3: агент молчит и ни одна своя нода не видела
//     его через мост дольше RelaySilentAfter;
//   - события пар relay_silent, relay_ok, foreign_bridge, relay_no_exit - на
//     переходах статуса пары (detectPairEvents);
//   - apply_failed - агент сообщил провал применения ресурса; одно событие на
//     ревизию ресурса (повторы отчёта и проходы детектора его не дублируют).
//
// Плановое окно приостанавливает down/up и события затронутых пар, сохраняя
// прежнее состояние. Ошибки применения ресурсов идут своим потоком.
// Одна транзакция под advisory-lock. Возвращает новые события.
func (s *Store) DetectMachineEvents(ctx context.Context, silentAfter time.Duration) ([]MachineEvent, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin alerts: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, alertsLockKey); err != nil {
		return nil, fmt.Errorf("alerts lock: %w", err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return nil, fmt.Errorf("alerts now: %w", err)
	}
	graph, err := s.loadRelayGraph(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	downRelays := graph.downRelays(silentAfter, now)
	maintenance := graph.maintenanceTargets()
	rows, err := tx.Query(ctx,
		`WITH cur AS (
		   SELECT t.code, t.kind, GREATEST(a.seen_at, r.seen_at) AS seen
		     FROM targets t
		     LEFT JOIN agent_state a ON a.target = t.code
		     LEFT JOIN agent_runtime r ON r.target = t.code
		    WHERE t.enabled AND NOT (t.code = ANY($3::text[]))
		      AND (a.seen_at IS NOT NULL OR r.seen_at IS NOT NULL)
		 ), st AS (
		   INSERT INTO machine_alert_state AS s (code, down, changed_at)
		   SELECT code,
		          CASE WHEN kind = 'relay' THEN code = ANY($2::text[])
		               ELSE seen < now() - make_interval(secs => $1) END,
		          now() FROM cur
		   ON CONFLICT (code) DO UPDATE SET down = EXCLUDED.down, changed_at = EXCLUDED.changed_at
		    WHERE s.down <> EXCLUDED.down
		   RETURNING s.code, s.down, (s.xmax = 0) AS inserted
		 )
		 INSERT INTO machine_events (machine, kind, detail)
		 SELECT st.code,
		        CASE WHEN st.down THEN 'down' ELSE 'up' END,
		        CASE WHEN st.down
		             THEN 'агент молчит с ' || to_char(cur.seen AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') || ' UTC'
		                  || CASE WHEN cur.kind = 'relay' THEN ', и ни одна своя нода не видит релей через мост' ELSE '' END
		             WHEN cur.kind = 'relay' THEN 'релей снова на связи'
		             ELSE 'агент снова на связи' END
		   FROM st JOIN cur ON cur.code = st.code
		  WHERE NOT (st.inserted AND NOT st.down)
		  ORDER BY st.code
		 RETURNING `+eventCols, silentAfter.Seconds(), downRelays, maintenance)
	if err != nil {
		return nil, fmt.Errorf("тревоги down/up: %w", err)
	}
	events, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx,
		`WITH f AS (
		   SELECT o.target, o.resource, o.revision, COALESCE(o.error, '') AS error
		     FROM observed_state o
		     JOIN targets t ON t.code = o.target AND t.enabled
		     LEFT JOIN machine_apply_alerts m ON m.code = o.target AND m.resource = o.resource
		    WHERE o.status = 'failed' AND o.revision IS NOT NULL
		      AND (m.revision IS NULL OR o.revision > m.revision)
		 ), mark AS (
		   INSERT INTO machine_apply_alerts (code, resource, revision, at)
		   SELECT target, resource, revision, now() FROM f
		   ON CONFLICT (code, resource) DO UPDATE SET revision = EXCLUDED.revision, at = EXCLUDED.at
		 )
		 INSERT INTO machine_events (machine, kind, detail)
		 SELECT target, 'apply_failed',
		        resource || ', ревизия ' || revision || CASE WHEN error <> '' THEN ': ' || left(error, 500) ELSE '' END
		   FROM f ORDER BY target, resource
		 RETURNING `+eventCols)
	if err != nil {
		return nil, fmt.Errorf("тревоги apply_failed: %w", err)
	}
	failed, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	pairs, err := s.detectPairEvents(ctx, tx, graph, downRelays, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit alerts: %w", err)
	}
	return append(append(events, failed...), pairs...), nil
}

// CheckTarget - машина для проверки связи и последний отчёт её агента.
type CheckTarget struct {
	Code    string
	Kind    string
	Enabled bool
	SeenAt  *time.Time // последний отчёт observed или runtime; nil - агент не отчитывался
}

// CheckEndpoint - точка входа сервера, которую обслуживает машина.
type CheckEndpoint struct {
	ServerNo   int
	ServerName string
	EndpointID int64
	Protocol   string
	Host       string
	Port       int
	ViaRelay   bool
}

// MachineCheckInfo - машина и включённые точки входа её серверов (кроме серверов в
// state=disabled): через релей - на релее (relay_code), напрямую - на машине за
// рубежом (node_code). Не больше maxCheckEndpoints; truncated - были ещё.
// Нет машины - ErrUnknownTarget.
func (s *Store) MachineCheckInfo(ctx context.Context, code string) (CheckTarget, []CheckEndpoint, bool, error) {
	t := CheckTarget{Code: code}
	err := s.pool.QueryRow(ctx,
		`SELECT t.kind, t.enabled, GREATEST(a.seen_at, r.seen_at)
		   FROM targets t
		   LEFT JOIN agent_state a ON a.target = t.code
		   LEFT JOIN agent_runtime r ON r.target = t.code
		  WHERE t.code = $1`, code).Scan(&t.Kind, &t.Enabled, &t.SeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, nil, false, ErrUnknownTarget
	}
	if err != nil {
		return t, nil, false, fmt.Errorf("машина %q: %w", code, err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT s.no, s.name, e.id, e.protocol, e.host, e.port, e.via_relay
		   FROM server_endpoints e JOIN servers s ON s.no = e.server_no
		  WHERE e.enabled AND s.state <> 'disabled'
		    AND ((e.via_relay AND s.relay_code = $1) OR (NOT e.via_relay AND s.node_code = $1))
		  ORDER BY s.no, e.sort_order, e.id
		  LIMIT $2`, code, maxCheckEndpoints+1)
	if err != nil {
		return t, nil, false, fmt.Errorf("точки входа машины %q: %w", code, err)
	}
	defer rows.Close()
	eps := []CheckEndpoint{}
	for rows.Next() {
		var e CheckEndpoint
		if err := rows.Scan(&e.ServerNo, &e.ServerName, &e.EndpointID, &e.Protocol, &e.Host, &e.Port, &e.ViaRelay); err != nil {
			return t, nil, false, fmt.Errorf("scan точки входа: %w", err)
		}
		eps = append(eps, e)
	}
	if err := rows.Err(); err != nil {
		return t, nil, false, fmt.Errorf("точки входа машины %q: %w", code, err)
	}
	truncated := len(eps) > maxCheckEndpoints
	if truncated {
		eps = eps[:maxCheckEndpoints]
	}
	return t, eps, truncated, nil
}
