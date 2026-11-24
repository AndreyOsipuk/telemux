package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Runtime-отчёт агента: нагрузка машины, версии, подключения пользователей и
// ошибки журнала. Агент шлёт его раз в минуту отдельно от отчёта observed.

// Продукты в runtime-отчёте.
const (
	ProductTelemt       = "telemt"
	ProductMtg          = "mtg"
	ProductMtgSecondary = "mtg_secondary"
)

// Виды ошибок журнала telemt (правила - как у команды /logs бота).
var LogErrorKinds = map[string]bool{"limit": true, "handshake": true, "auth": true, "timeout": true, "other": true}

// RuntimeHost - нагрузка машины.
type RuntimeHost struct {
	CPUPct    float64 `json:"cpu_pct"`
	MemPct    float64 `json:"mem_pct"`
	DiskPct   float64 `json:"disk_pct"`
	Load1     float64 `json:"load1"`
	NetRxMbps float64 `json:"net_rx_mbps"`
	NetTxMbps float64 `json:"net_tx_mbps"`
	UptimeS   int64   `json:"uptime_s"`
}

// RuntimeUser - подключения пользователя. TrafficSample подтверждает наличие
// обоих счётчиков в источнике, включая нулевые значения.
type RuntimeUser struct {
	TrafficSample bool       `json:"traffic_sample,omitempty"`
	Key           string     `json:"key"`
	Connections   int64      `json:"connections"`
	ActiveIPs     []string   `json:"active_ips"`
	BytesIn       int64      `json:"bytes_in,omitempty"`
	BytesOut      int64      `json:"bytes_out,omitempty"`
	LastSeen      *time.Time `json:"last_seen,omitempty"`
}

// RuntimeTelemt - состояние telemt на машине.
type RuntimeTelemt struct {
	TrafficGeneration string        `json:"traffic_generation,omitempty"`
	Version           string        `json:"version"`
	APIOK             bool          `json:"api_ok"`
	Error             string        `json:"error"`
	UsersConfigured   int           `json:"users_configured"`
	UsersEffective    int           `json:"users_effective"`
	ConnectionsTotal  int64         `json:"connections_total"`
	ConfigSHA256      string        `json:"config_sha256"`
	Keepalive         int           `json:"keepalive"`
	Ack               int           `json:"ack"`
	TLSDomain         string        `json:"tls_domain"`
	MaxTCPConnsMedian int           `json:"max_tcp_conns_median"`
	Users             []RuntimeUser `json:"users,omitempty"`
}

// RuntimeMtg - состояние mtg-multi на машине.
type RuntimeMtg struct {
	TrafficGeneration string        `json:"traffic_generation,omitempty"`
	Version           string        `json:"version"`
	APIOK             bool          `json:"api_ok"`
	Error             string        `json:"error"`
	ConnectionsTotal  int64         `json:"connections_total"`
	Users             []RuntimeUser `json:"users,omitempty"`
}

// LogErrorItem - сколько раз ошибка вида kind встретилась у пользователя key
// (пустой key - без пользователя: сканеры, чужие рукопожатия).
type LogErrorItem struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
}

// RuntimeLogErrors - счётчики журнала за период с прошлого отчёта.
type RuntimeLogErrors struct {
	PeriodStart time.Time      `json:"period_start"`
	PeriodEnd   time.Time      `json:"period_end"`
	Items       []LogErrorItem `json:"items"`
}

// BridgeRelay - релей в сводке моста выхода (спецификация relay-via-exit, контракт 5.1).
type BridgeRelay struct {
	Code      string     `json:"code"`
	LastSeen  *time.Time `json:"last_seen"`
	Requests  int64      `json:"requests"`
	MasterOK  bool       `json:"master_ok"`
	LastError string     `json:"last_error"`
}

// BridgeSummary - сводка моста в runtime-отчёте выхода (контракт 5.2): код моста,
// его релеи; error - мост не ответил агенту (relays тогда пустой).
type BridgeSummary struct {
	Code   string        `json:"code"`
	Relays []BridgeRelay `json:"relays"`
	Error  string        `json:"error"`
}

// RelayLastSeen - когда релей code последний раз ходил через этот мост; nil - не ходил.
func (b *BridgeSummary) RelayLastSeen(code string) *time.Time {
	if b == nil {
		return nil
	}
	for _, r := range b.Relays {
		if r.Code == code {
			return r.LastSeen
		}
	}
	return nil
}

// RuntimeReport - тело POST /v1/runtime. telemt, mtg, log_errors - nil, если
// на машине нет этого продукта. bridge - только у выхода с мостом (агенту задан
// адрес моста); у остальных поля в JSON нет.
type RuntimeReport struct {
	ObservedAt   *time.Time        `json:"observed_at,omitempty"`
	Target       string            `json:"target"`
	AgentVersion string            `json:"agent_version"`
	Host         *RuntimeHost      `json:"host"`
	Telemt       *RuntimeTelemt    `json:"telemt"`
	Mtg          *RuntimeMtg       `json:"mtg"`
	MtgSecondary *RuntimeMtg       `json:"mtg_secondary,omitempty"`
	LogErrors    *RuntimeLogErrors `json:"log_errors"`
	Bridge       *BridgeSummary    `json:"bridge,omitempty"`
}

// RuntimeTables - таблицы миграции 0007, без которых мастер не стартует.
var RuntimeTables = []string{"agent_runtime", "runtime_user_conns", "runtime_log_errors"}

// CheckRuntimeSchema проверяет миграции снимков и учёта трафика в текущей схеме.
func (s *Store) CheckRuntimeSchema(ctx context.Context) error {
	if err := s.checkTables(ctx, RuntimeTables, "migrations/0007_agent_runtime.sql"); err != nil {
		return err
	}
	return s.checkTables(ctx, []string{"runtime_traffic_counters", "runtime_traffic_deltas"}, "migrations/0016_runtime_traffic.sql")
}

// checkTables - есть ли в текущей схеме все таблицы tables; нет - ошибка с
// подсказкой, какую миграцию применить.
func (s *Store) checkTables(ctx context.Context, tables []string, migration string) error {
	rows, err := s.pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables
		  WHERE table_schema = current_schema() AND table_name = ANY($1)`, tables)
	if err != nil {
		return fmt.Errorf("проверка схемы: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("проверка схемы: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("проверка схемы: %w", err)
	}
	var missing []string
	for _, t := range tables {
		if !have[t] {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("в схеме базы нет таблиц %s: примените %s до запуска мастера",
			strings.Join(missing, ", "), migration)
	}
	return nil
}

// PruneLogErrors удаляет счётчики журнала с концом периода раньше before.
// Вызывается периодической задачей мастера, не в каждом отчёте.
func (s *Store) PruneLogErrors(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM runtime_log_errors WHERE period_end < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("чистка runtime_log_errors: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RecordRuntime сохраняет снимок машины одной транзакцией: строка agent_runtime
// перезаписывается; подключения заменяются по продукту - только у продукта с
// api_ok=true (у продукта без API строки остаются и устаревают по seen_at, у
// продукта, которого нет в отчёте, - удаляются); счётчики журнала дописываются
// без дублей (повтор того же периода не удваивает сумму).
func (s *Store) RecordRuntime(ctx context.Context, rep RuntimeReport) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin runtime: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return fmt.Errorf("runtime now: %w", err)
	}
	// Списки пользователей в снимок машины не кладутся - они в runtime_user_conns.
	var telemtDoc, mtgDoc, hostDoc any
	if rep.Telemt != nil {
		t := *rep.Telemt
		t.Users = nil
		telemtDoc = t
	}
	if rep.Mtg != nil {
		m := *rep.Mtg
		m.Users = nil
		mtgDoc = m
	}
	if rep.Host != nil {
		hostDoc = *rep.Host
	}
	// Сводка моста выхода (R2.1); отчёт без неё (не выход или старый агент) - NULL.
	var bridgeDoc any
	if rep.Bridge != nil {
		bridgeDoc = *rep.Bridge
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_runtime (target, agent_version, host, telemt, mtg, bridge, seen_at, mtg_secondary_api_ok)
		 VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (target) DO UPDATE
		   SET agent_version = EXCLUDED.agent_version, host = EXCLUDED.host,
		       telemt = EXCLUDED.telemt, mtg = EXCLUDED.mtg, bridge = EXCLUDED.bridge,
		       seen_at = EXCLUDED.seen_at, mtg_secondary_api_ok = EXCLUDED.mtg_secondary_api_ok`,
		rep.Target, rep.AgentVersion, hostDoc, telemtDoc, mtgDoc, bridgeDoc, now,
		rep.MtgSecondary != nil && rep.MtgSecondary.APIOK); err != nil {
		return fmt.Errorf("agent_runtime %s: %w", rep.Target, err)
	}
	if rep.ObservedAt != nil {
		if t := rep.Telemt; t != nil && t.APIOK {
			if err := recordTraffic(ctx, tx, rep.Target, ProductTelemt, t.TrafficGeneration, *rep.ObservedAt, t.Users); err != nil {
				return err
			}
		}
		for _, source := range []struct {
			product string
			stats   *RuntimeMtg
		}{{ProductMtg, rep.Mtg}, {ProductMtgSecondary, rep.MtgSecondary}} {
			if m := source.stats; m != nil && m.APIOK {
				if err := recordTraffic(ctx, tx, rep.Target, source.product, m.TrafficGeneration, *rep.ObservedAt, m.Users); err != nil {
					return err
				}
			}
		}
	}
	// Какие продукты заменяются: есть в отчёте и API ответил - новыми строками,
	// нет в отчёте - удаляются; API не ответил - строки не трогаем.
	var replace []string
	var rows [][]any
	add := func(product string, present, apiOK bool, users []RuntimeUser) {
		if present && !apiOK {
			return
		}
		replace = append(replace, product)
		for _, u := range users {
			ips := u.ActiveIPs
			if ips == nil {
				ips = []string{}
			}
			rows = append(rows, []any{rep.Target, product, u.Key, u.Connections, ips, u.BytesIn, u.BytesOut, u.LastSeen, now})
		}
	}
	if rep.Telemt != nil {
		add(ProductTelemt, true, rep.Telemt.APIOK, rep.Telemt.Users)
	} else {
		add(ProductTelemt, false, false, nil)
	}
	if rep.Mtg != nil {
		add(ProductMtg, true, rep.Mtg.APIOK, rep.Mtg.Users)
	} else {
		add(ProductMtg, false, false, nil)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runtime_user_conns WHERE target = $1 AND product = ANY($2)`,
		rep.Target, replace); err != nil {
		return fmt.Errorf("runtime_user_conns %s: %w", rep.Target, err)
	}
	if len(rows) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"runtime_user_conns"},
			[]string{"target", "product", "user_key", "connections", "active_ips", "bytes_in", "bytes_out", "last_seen", "seen_at"},
			pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("runtime_user_conns %s: %w", rep.Target, err)
		}
	}
	if le := rep.LogErrors; le != nil && len(le.Items) > 0 {
		keys := make([]string, len(le.Items))
		kinds := make([]string, len(le.Items))
		counts := make([]int64, len(le.Items))
		for i, it := range le.Items {
			keys[i], kinds[i], counts[i] = it.Key, it.Kind, it.Count
		}
		// Повтор периода (агент не дождался ответа, мост переслал дважды) - DO NOTHING.
		if _, err := tx.Exec(ctx,
			`INSERT INTO runtime_log_errors (target, user_key, kind, count, period_start, period_end)
			 SELECT $1, u.user_key, u.kind, u.count, $5, $6
			   FROM unnest($2::text[], $3::text[], $4::bigint[]) AS u(user_key, kind, count)
			 ON CONFLICT (target, period_start, user_key, kind) DO NOTHING`,
			rep.Target, keys, kinds, counts, le.PeriodStart, le.PeriodEnd); err != nil {
			return fmt.Errorf("runtime_log_errors %s: %w", rep.Target, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit runtime: %w", err)
	}
	return nil
}

// MachineRuntime - последний снимок машины без списков пользователей.
// SeenAt == nil - агент ещё не присылал runtime-отчёт.
type MachineRuntime struct {
	Code         string         `json:"code"`
	SeenAt       *time.Time     `json:"seen_at"`
	Stale        bool           `json:"stale"`
	AgentVersion string         `json:"agent_version"`
	Host         *RuntimeHost   `json:"host"`
	Telemt       *RuntimeTelemt `json:"telemt"`
	Mtg          *RuntimeMtg    `json:"mtg"`
}

// MachineRuntime возвращает снимок машины; ErrUnknownTarget - машины нет.
func (s *Store) MachineRuntime(ctx context.Context, code string) (MachineRuntime, error) {
	mr := MachineRuntime{Code: code}
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT TRUE, COALESCE(r.agent_version, ''), r.host, r.telemt, r.mtg, r.seen_at
		   FROM targets t LEFT JOIN agent_runtime r ON r.target = t.code
		  WHERE t.code = $1`, code).
		Scan(&exists, &mr.AgentVersion, &mr.Host, &mr.Telemt, &mr.Mtg, &mr.SeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return mr, ErrUnknownTarget
	}
	if err != nil {
		return mr, fmt.Errorf("runtime %q: %w", code, err)
	}
	return mr, nil
}

// ConnFilter - фильтры списка подключений; пустые поля не фильтруют.
type ConnFilter struct {
	Machine        string
	Product        string
	MinConnections int64
	User           string // префикс ключа пользователя (starts_with, без шаблонов LIKE)
}

// ConnRow - подключения пользователя на машине по последнему снимку.
type ConnRow struct {
	Machine     string     `json:"machine"`
	Product     string     `json:"product"`
	Key         string     `json:"key"`
	Connections int64      `json:"connections"`
	ActiveIPs   []string   `json:"active_ips"`
	BytesIn     int64      `json:"bytes_in"`
	BytesOut    int64      `json:"bytes_out"`
	LastSeen    *time.Time `json:"last_seen"`
	SeenAt      time.Time  `json:"seen_at"`
	Stale       bool       `json:"stale"`
}

// Connections - подключения по фильтру, порядок machine, product, key; total - с фильтром.
func (s *Store) Connections(ctx context.Context, f ConnFilter, limit, offset int) ([]ConnRow, int, error) {
	const where = `WHERE ($1 = '' OR target = $1) AND ($2 = '' OR product = $2) AND connections >= $3
	                 AND starts_with(user_key, $4)`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runtime_user_conns `+where,
		f.Machine, f.Product, f.MinConnections, f.User).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count connections: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT target, product, user_key, connections, active_ips, bytes_in, bytes_out, last_seen, seen_at
		   FROM runtime_user_conns `+where+`
		  ORDER BY target, product, user_key
		  LIMIT $5 OFFSET $6`, f.Machine, f.Product, f.MinConnections, f.User, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("connections: %w", err)
	}
	defer rows.Close()
	out := []ConnRow{}
	for rows.Next() {
		var c ConnRow
		if err := rows.Scan(&c.Machine, &c.Product, &c.Key, &c.Connections, &c.ActiveIPs,
			&c.BytesIn, &c.BytesOut, &c.LastSeen, &c.SeenAt); err != nil {
			return nil, 0, fmt.Errorf("scan connections: %w", err)
		}
		if c.ActiveIPs == nil {
			c.ActiveIPs = []string{}
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// ConnSummary - сводка подключений машины по продукту. Строка есть и тогда,
// когда API продукта не ответил (api_ok=false, error - причина): connections_total
// и users_online - по строкам последнего удачного снимка (или 0). По api_ok=false
// бот идёт за подключениями по SSH.
type ConnSummary struct {
	Machine          string    `json:"machine"`
	Product          string    `json:"product"`
	ConnectionsTotal int64     `json:"connections_total"`
	UsersOnline      int       `json:"users_online"`
	SeenAt           time.Time `json:"seen_at"`
	Stale            bool      `json:"stale"`
	APIOK            bool      `json:"api_ok"`
	Error            string    `json:"error,omitempty"`
}

// summarySQL - строка на машину и продукт, у которых есть снимок.
const summarySQL = `
  FROM agent_runtime r
  CROSS JOIN LATERAL (VALUES ('telemt', r.telemt), ('mtg', r.mtg)) AS v(product, doc)
  CROSS JOIN LATERAL (SELECT v.product, v.doc,
                             COALESCE(jsonb_typeof(v.doc->'api_ok') = 'boolean' AND (v.doc->>'api_ok')::boolean, false) AS api_ok) AS p
 WHERE p.doc IS NOT NULL AND jsonb_typeof(p.doc) = 'object'`

// ConnectionsSummary - сводка по машинам, порядок machine, product.
func (s *Store) ConnectionsSummary(ctx context.Context, limit, offset int) ([]ConnSummary, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) `+summarySQL).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count summary: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT r.target, p.product,
		        CASE WHEN p.api_ok THEN COALESCE((p.doc->>'connections_total')::bigint, 0)
		             ELSE (SELECT COALESCE(sum(c.connections), 0)::bigint FROM runtime_user_conns c
		                    WHERE c.target = r.target AND c.product = p.product) END,
		        (SELECT count(*) FROM runtime_user_conns c
		          WHERE c.target = r.target AND c.product = p.product AND c.connections > 0),
		        r.seen_at, p.api_ok, COALESCE(p.doc->>'error', '')`+summarySQL+`
		  ORDER BY r.target, p.product
		  LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("summary: %w", err)
	}
	defer rows.Close()
	out := []ConnSummary{}
	for rows.Next() {
		var c ConnSummary
		if err := rows.Scan(&c.Machine, &c.Product, &c.ConnectionsTotal, &c.UsersOnline, &c.SeenAt, &c.APIOK, &c.Error); err != nil {
			return nil, 0, fmt.Errorf("scan summary: %w", err)
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// LogErrorSum - сумма ошибок журнала за окно.
type LogErrorSum struct {
	Machine string `json:"machine"`
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Count   int64  `json:"count"`
}

// LogErrors - суммы ошибок по периодам, закончившимся после since; machine
// пустой - все машины. Порядок count desc, machine, key, kind.
func (s *Store) LogErrors(ctx context.Context, since time.Time, machine string, limit, offset int) ([]LogErrorSum, int, error) {
	const grouped = `SELECT target, user_key, kind, sum(count)::bigint AS cnt
	                   FROM runtime_log_errors
	                  WHERE period_end > $1 AND ($2 = '' OR target = $2)
	                  GROUP BY target, user_key, kind`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM (`+grouped+`) g`, since, machine).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count log errors: %w", err)
	}
	rows, err := s.pool.Query(ctx, grouped+` ORDER BY cnt DESC, target, user_key, kind LIMIT $3 OFFSET $4`,
		since, machine, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("log errors: %w", err)
	}
	defer rows.Close()
	out := []LogErrorSum{}
	for rows.Next() {
		var e LogErrorSum
		if err := rows.Scan(&e.Machine, &e.Key, &e.Kind, &e.Count); err != nil {
			return nil, 0, fmt.Errorf("scan log errors: %w", err)
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// MaxConnUsers - сколько пользователей можно спросить в ConnectionsByUser за раз.
const MaxConnUsers = 100

// UserConns - подключения пользователя по всем машинам (последние снимки).
type UserConns struct {
	Key         string     `json:"key"`
	Connections int64      `json:"connections"`
	Machines    int        `json:"machines"` // машин, где у пользователя есть подключения
	Online      bool       `json:"online"`   // connections > 0
	SeenAt      *time.Time `json:"seen_at"`  // самый свежий снимок с этим пользователем; nil - строк нет
}

// ConnectionsByUser - сумма подключений по машинам для каждого из keys одним
// запросом; порядок и состав - как в keys (пользователь без строк - нули).
func (s *Store) ConnectionsByUser(ctx context.Context, keys []string) ([]UserConns, error) {
	out := make([]UserConns, len(keys))
	idx := make(map[string]int, len(keys))
	for i, k := range keys {
		out[i] = UserConns{Key: k}
		idx[k] = i
	}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT user_key, COALESCE(sum(connections), 0)::bigint,
		        count(DISTINCT target) FILTER (WHERE connections > 0)::int, max(seen_at)
		   FROM runtime_user_conns WHERE user_key = ANY($1)
		  GROUP BY user_key`, keys)
	if err != nil {
		return nil, fmt.Errorf("connections by user: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u UserConns
		if err := rows.Scan(&u.Key, &u.Connections, &u.Machines, &u.SeenAt); err != nil {
			return nil, fmt.Errorf("scan connections by user: %w", err)
		}
		u.Online = u.Connections > 0
		if i, ok := idx[u.Key]; ok {
			out[i] = u
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("connections by user: %w", err)
	}
	return out, nil
}

// FleetTotals - итоги флота для обзора панели.
type FleetTotals struct {
	Machines         int
	AgentsOnline     int // runtime-отчёт свежее порога
	AgentsSilent     int // включённые машины без runtime-отчёта дольше порога или вовсе без него
	ConnectionsTotal int64
	UsersOnline      int // уникальные пользователи с подключениями
}

// FleetTotals считает итоги одним запросом: машины, свежие и молчащие агенты
// (freshSince и silentBefore - границы по seen_at runtime-отчёта), подключения
// и уникальные пользователи с подключениями по всем машинам.
func (s *Store) FleetTotals(ctx context.Context, freshSince, silentBefore time.Time) (FleetTotals, error) {
	var t FleetTotals
	err := s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM targets)::int,
		        (SELECT count(*) FROM agent_runtime WHERE seen_at > $1)::int,
		        (SELECT count(*) FROM targets t LEFT JOIN agent_runtime r ON r.target = t.code
		          WHERE t.enabled AND (r.seen_at IS NULL OR r.seen_at < $2))::int,
		        (SELECT COALESCE(sum(connections), 0) FROM runtime_user_conns)::bigint,
		        (SELECT count(DISTINCT user_key) FROM runtime_user_conns WHERE connections > 0)::int`,
		freshSince, silentBefore).
		Scan(&t.Machines, &t.AgentsOnline, &t.AgentsSilent, &t.ConnectionsTotal, &t.UsersOnline)
	if err != nil {
		return t, fmt.Errorf("итоги флота: %w", err)
	}
	return t, nil
}
