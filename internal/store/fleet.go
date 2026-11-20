package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SilentAfter - агент без отчёта дольше этого считается молчащим (статус silent в паре).
const SilentAfter = 10 * time.Minute

// Сводный статус машины в паре.
const (
	StatusOK       = "ok"
	StatusBehind   = "behind"
	StatusFailed   = "failed"
	StatusSilent   = "silent"
	StatusNoAgent  = "no_agent"
	StatusDisabled = "disabled"
)

// Причины дрейфа.
const (
	DriftBehind = "behind"
	DriftFailed = "failed"
	DriftSilent = "silent"
)

// DesiredRevision - последняя желаемая ревизия ресурса машины.
type DesiredRevision struct {
	Resource string `json:"resource"`
	Revision int64  `json:"revision"`
}

// PairMachine - машина в паре: отчёт агента, желаемые ревизии и сводный статус.
// RelayStatus и RelayLastSeen - только у выхода в паре с релеем (R3.2): у релея и
// у машин псевдо-пары этих полей в JSON нет.
type PairMachine struct {
	Machine
	Status        string            `json:"status"`
	Desired       []DesiredRevision `json:"desired"`
	RelayStatus   string            `json:"relay_status,omitempty"`
	RelayLastSeen *time.Time        `json:"relay_last_seen"`
}

// pairMachineJSON - PairMachine без своего MarshalJSON.
type pairMachineJSON PairMachine

// MarshalJSON: у выхода пары relay_last_seen есть всегда (null - релей через мост не
// ходил), у остальных машин поля нет.
func (p PairMachine) MarshalJSON() ([]byte, error) {
	if p.RelayStatus == "" {
		return json.Marshal(struct {
			pairMachineJSON
			RelayLastSeen *time.Time `json:"relay_last_seen,omitempty"`
		}{pairMachineJSON: pairMachineJSON(p)})
	}
	return json.Marshal(pairMachineJSON(p))
}

// Pair - релей и его выходы. Relay == nil - псевдо-пара «без релея»: выходы
// без relay_code (или с несуществующим релеем) и машины вида panel.
// RelayDown, ForeignBridge, Status - по правилам R3.3-R3.4, R5.3 (у псевдо-пары
// false и пустой статус).
type Pair struct {
	Relay         *PairMachine  `json:"relay"`
	Exits         []PairMachine `json:"exits"`
	RelayDown     bool          `json:"relay_down"`
	ForeignBridge bool          `json:"foreign_bridge"`
	Status        string        `json:"status,omitempty"`
}

// DriftItem - расхождение применённого с желаемым на (машина, ресурс).
type DriftItem struct {
	Target           string     `json:"target"`
	Kind             string     `json:"kind"`
	RelayCode        string     `json:"relay_code,omitempty"`
	Reason           string     `json:"reason"`
	Resource         string     `json:"resource"`
	DesiredRevision  int64      `json:"desired_revision"`
	ObservedRevision *int64     `json:"observed_revision"`
	Since            time.Time  `json:"since"`
	SeenAt           *time.Time `json:"seen_at"`
	Error            string     `json:"error,omitempty"`
}

// ApplyLogEntry - запись журнала применений и проверок машины.
type ApplyLogEntry struct {
	At       time.Time `json:"at"`
	Resource string    `json:"resource"`
	Revision *int64    `json:"revision"`
	Status   string    `json:"status"`
	Error    string    `json:"error,omitempty"`
}

// MachineStatus - сводный статус машины по правилам спеки (в порядке
// приоритета): disabled, no_agent, silent (отчёт старше SilentAfter), failed
// (ресурс failed или проверка не пройдена), behind (ресурс отстаёт от желаемой
// ревизии или ещё не применён), иначе ok. В счёт идут только ресурсы, назначенные
// машине: отчёт по снятому ресурсу остаётся в observed_state, но статус не портит.
func MachineStatus(m Machine, desired []DesiredRevision, now time.Time) string {
	switch {
	case !m.Enabled:
		return StatusDisabled
	case m.SeenAt == nil:
		return StatusNoAgent
	case now.Sub(*m.SeenAt) > SilentAfter:
		return StatusSilent
	}
	assigned := make(map[string]bool, len(m.Resources))
	for _, r := range m.Resources {
		assigned[r] = true
	}
	applied := make(map[string]int64, len(m.Observed))
	for _, o := range m.Observed {
		if !assigned[o.Resource] {
			continue
		}
		if o.Status == "failed" {
			return StatusFailed
		}
		applied[o.Resource] = o.Revision
	}
	for _, c := range m.Checks {
		if !c.OK {
			return StatusFailed
		}
	}
	for _, d := range desired {
		if rev, ok := applied[d.Resource]; !ok || rev < d.Revision {
			return StatusBehind
		}
	}
	return StatusOK
}

// orphanCond - машина вне пар: panel или выход, чей relay_code не указывает на релей.
const orphanCond = `(t.kind = 'panel' OR (t.kind = 'exit' AND NOT EXISTS (
	SELECT 1 FROM targets r WHERE r.kind = 'relay' AND r.code = t.relay_code)))`

// Pairs - пары флота постранично: страница идёт по релеям по коду, псевдо-пара
// «без релея» - последней (если такие машины есть). total - релеи плюс псевдо-пара.
// Запросов - фиксированное число на страницу, без цикла по машинам. Статусы
// считаются от now() базы, как в детекторе (U22): часы мастера и базы могут
// расходиться, и тогда API и тревоги говорили бы разное.
func (s *Store) Pairs(ctx context.Context, limit, offset int) ([]Pair, int, error) {
	var relays int
	var hasOrphans bool
	var now time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM targets WHERE kind = 'relay'),
		        EXISTS (SELECT 1 FROM targets t WHERE `+orphanCond+`), now()`).Scan(&relays, &hasOrphans, &now); err != nil {
		return nil, 0, fmt.Errorf("count pairs: %w", err)
	}
	total := relays
	if hasOrphans {
		total++
	}
	pairs := []Pair{}
	if offset >= total {
		return pairs, total, nil
	}
	relayCodes := []string{}
	if offset < relays {
		rows, err := s.pool.Query(ctx,
			`SELECT code FROM targets WHERE kind = 'relay' ORDER BY code LIMIT $1 OFFSET $2`, limit, offset)
		if err != nil {
			return nil, 0, fmt.Errorf("relays: %w", err)
		}
		relayCodes, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, 0, fmt.Errorf("relays: %w", err)
		}
	}
	withOrphans := hasOrphans && offset+limit > relays

	list, err := s.queryMachines(ctx, s.pool,
		`WHERE t.code = ANY($1)
		    OR (t.kind = 'exit' AND t.relay_code = ANY($1))
		    OR ($2 AND `+orphanCond+`)
		  ORDER BY t.code`, relayCodes, withOrphans)
	if err != nil {
		return nil, 0, err
	}
	desired, err := s.latestDesired(ctx, machineCodes(list))
	if err != nil {
		return nil, 0, err
	}
	pm := func(m Machine) PairMachine {
		d := desired[m.Code]
		if d == nil {
			d = []DesiredRevision{}
		}
		return PairMachine{Machine: m, Status: MachineStatus(m, d, now), Desired: d}
	}

	byCode := make(map[string]Machine, len(list))
	for _, m := range list {
		byCode[m.Code] = m
	}
	pairIdx := make(map[string]int, len(relayCodes))
	for _, code := range relayCodes {
		m, ok := byCode[code]
		if !ok || m.Kind != "relay" {
			continue // релей удалили между запросами
		}
		relay := pm(m)
		pairIdx[code] = len(pairs)
		pairs = append(pairs, Pair{Relay: &relay, Exits: []PairMachine{}})
	}
	orphans := Pair{Exits: []PairMachine{}}
	for _, m := range list { // list уже по коду - выходы в паре тоже по коду
		if m.Kind == "relay" {
			continue
		}
		if i, ok := pairIdx[m.RelayCode]; ok && m.Kind == "exit" {
			pairs[i].Exits = append(pairs[i].Exits, pm(m))
			continue
		}
		if withOrphans {
			orphans.Exits = append(orphans.Exits, pm(m))
		}
	}
	for i := range pairs {
		fillRelayStatus(&pairs[i], now)
	}
	if withOrphans && len(orphans.Exits) > 0 {
		pairs = append(pairs, orphans)
	}
	return pairs, total, nil
}

// latestDesired - последняя желаемая ревизия каждого назначенного ресурса машин
// codes одним запросом (по индексу idx_desired_latest).
func (s *Store) latestDesired(ctx context.Context, codes []string) (map[string][]DesiredRevision, error) {
	out := map[string][]DesiredRevision{}
	if len(codes) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT t.code, d.resource, d.revision
		   FROM targets t
		   CROSS JOIN LATERAL unnest(t.resources) AS r(resource)
		   CROSS JOIN LATERAL (
		     SELECT dc.resource, dc.revision FROM desired_config dc
		      WHERE dc.target = t.code AND dc.resource = r.resource
		      ORDER BY dc.revision DESC LIMIT 1) d
		  WHERE t.code = ANY($1)
		  ORDER BY t.code, d.resource`, codes)
	if err != nil {
		return nil, fmt.Errorf("latest desired: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var d DesiredRevision
		if err := rows.Scan(&code, &d.Resource, &d.Revision); err != nil {
			return nil, fmt.Errorf("scan desired: %w", err)
		}
		out[code] = append(out[code], d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("latest desired: %w", err)
	}
	return out, nil
}

// driftSQL - все расхождения включённых машин, держащиеся дольше порога ($1 секунд):
//   - behind: последняя желаемая ревизия назначенного ресурса не применена, since - её created_at;
//   - failed: ресурс в статусе failed или проверка не пройдена (ресурс check:<имя>),
//     since - начало отказа по журналу применений (последняя запись по ресурсу;
//     журнал пишет только смены состояния), если записи нет - seen_at отчёта;
//   - silent: агент не отчитывался дольше порога, since - seen_at.
//
// Машина без агента не silent (её агента ещё не ставили), но её ресурсы - behind.
const driftSQL = `
WITH latest AS (
  SELECT t.code, d.resource, d.revision, d.created_at
    FROM targets t
    CROSS JOIN LATERAL unnest(t.resources) AS r(resource)
    CROSS JOIN LATERAL (
      SELECT dc.resource, dc.revision, dc.created_at FROM desired_config dc
       WHERE dc.target = t.code AND dc.resource = r.resource
       ORDER BY dc.revision DESC LIMIT 1) d
   WHERE t.enabled
), drift AS (
  SELECT t.code AS target, t.kind, COALESCE(t.relay_code, '') AS relay_code, 'behind' AS reason,
         l.resource, l.revision AS desired_revision, o.revision AS observed_revision,
         l.created_at AS since, COALESCE(a.seen_at, o.seen_at) AS seen_at, COALESCE(o.error, '') AS error
    FROM latest l
    JOIN targets t ON t.code = l.code
    LEFT JOIN observed_state o ON o.target = l.code AND o.resource = l.resource
    LEFT JOIN agent_state a ON a.target = l.code
   WHERE (o.revision IS NULL OR o.revision < l.revision)
     AND l.created_at <= now() - $1::int * interval '1 second'
  UNION ALL
  SELECT t.code, t.kind, COALESCE(t.relay_code, ''), 'failed',
         o.resource, COALESCE(l.revision, 0), o.revision,
         COALESCE(j.at, o.seen_at), COALESCE(a.seen_at, o.seen_at), COALESCE(o.error, '')
    FROM targets t
    JOIN observed_state o ON o.target = t.code AND o.resource = ANY(t.resources)
    LEFT JOIN latest l ON l.code = t.code AND l.resource = o.resource
    LEFT JOIN agent_state a ON a.target = t.code
    LEFT JOIN LATERAL (
      SELECT al.at FROM apply_log al WHERE al.target = t.code AND al.resource = o.resource
       ORDER BY al.at DESC, al.id DESC LIMIT 1) j ON true
   WHERE t.enabled AND o.status = 'failed'
     AND COALESCE(j.at, o.seen_at) <= now() - $1::int * interval '1 second'
  UNION ALL
  SELECT t.code, t.kind, COALESCE(t.relay_code, ''), 'failed',
         'check:' || (c->>'name'), 0, NULL::bigint,
         COALESCE(j.at, a.seen_at), a.seen_at, COALESCE(c->>'error', '')
    FROM targets t
    JOIN agent_state a ON a.target = t.code
    CROSS JOIN LATERAL jsonb_array_elements(a.checks) AS c
    LEFT JOIN LATERAL (
      SELECT al.at FROM apply_log al WHERE al.target = t.code AND al.resource = 'check:' || (c->>'name')
       ORDER BY al.at DESC, al.id DESC LIMIT 1) j ON true
   WHERE t.enabled AND NOT COALESCE((c->>'ok')::boolean, false)
     AND COALESCE(j.at, a.seen_at) <= now() - $1::int * interval '1 second'
  UNION ALL
  SELECT t.code, t.kind, COALESCE(t.relay_code, ''), 'silent', '', 0, NULL::bigint, a.seen_at, a.seen_at, ''
    FROM targets t
    JOIN agent_state a ON a.target = t.code
   WHERE t.enabled AND a.seen_at <= now() - $1::int * interval '1 second'
)`

// Drift - расхождения применённого с желаемым дольше threshold, постранично;
// порядок - машина, причина, ресурс. Выключенные машины не попадают.
func (s *Store) Drift(ctx context.Context, threshold time.Duration, limit, offset int) ([]DriftItem, int, error) {
	secs := int(threshold / time.Second)
	var total int
	if err := s.pool.QueryRow(ctx, driftSQL+` SELECT count(*) FROM drift`, secs).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count drift: %w", err)
	}
	rows, err := s.pool.Query(ctx, driftSQL+`
		SELECT target, kind, relay_code, reason, resource, desired_revision, observed_revision, since, seen_at, error
		  FROM drift ORDER BY target, reason, resource LIMIT $2 OFFSET $3`, secs, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("drift: %w", err)
	}
	defer rows.Close()
	items := []DriftItem{}
	for rows.Next() {
		var it DriftItem
		if err := rows.Scan(&it.Target, &it.Kind, &it.RelayCode, &it.Reason, &it.Resource, &it.DesiredRevision,
			&it.ObservedRevision, &it.Since, &it.SeenAt, &it.Error); err != nil {
			return nil, 0, fmt.Errorf("scan drift: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("drift: %w", err)
	}
	return items, total, nil
}

// ApplyLog - журнал применений и проверок машины, новые первыми, постранично.
// Неизвестная машина - ErrUnknownTarget.
func (s *Store) ApplyLog(ctx context.Context, target string, limit, offset int) ([]ApplyLogEntry, int, error) {
	var total int
	err := s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM apply_log WHERE target = t.code) FROM targets t WHERE t.code = $1`,
		target).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrUnknownTarget
	}
	if err != nil {
		return nil, 0, fmt.Errorf("count apply_log %s: %w", target, err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT at, resource, revision, status, COALESCE(error, '')
		   FROM apply_log WHERE target = $1
		  ORDER BY at DESC, id DESC LIMIT $2 OFFSET $3`, target, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("apply_log %s: %w", target, err)
	}
	defer rows.Close()
	list := []ApplyLogEntry{}
	for rows.Next() {
		var e ApplyLogEntry
		if err := rows.Scan(&e.At, &e.Resource, &e.Revision, &e.Status, &e.Error); err != nil {
			return nil, 0, fmt.Errorf("scan apply_log: %w", err)
		}
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("apply_log %s: %w", target, err)
	}
	return list, total, nil
}
