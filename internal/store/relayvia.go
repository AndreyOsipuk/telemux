package store

// Релей - через свою зарубежную ноду (ADR 0009, спецификация relay-via-exit):
// через чей мост пришёл отчёт релея (relay_via), статус пары «релей - своя нода»
// и тревоги на переходах статуса пар. Таблицы - migrations/0014_relay_via.sql.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RelaySilentAfter - порог тишины релея T_relay: релей не ходил через мост своей
// ноды дольше этого - пара relay_silent (R3.2).
const RelaySilentAfter = 3 * time.Minute

// Статус своей ноды релея в паре (relay_status, R3.2).
const (
	RelayStatusOK       = "ok"
	RelayStatusSilent   = "relay_silent"
	RelayStatusExitDown = "exit_down"
)

// Статус пары (Pair.status): no_exit - у релея нет своих нод (R5.3), down - релей
// упал по правилу R3.3, иначе ok.
const (
	PairStatusOK     = "ok"
	PairStatusDown   = "down"
	PairStatusNoExit = "no_exit"
)

// Виды событий пары (machine_events.kind).
const (
	EventRelaySilent   = "relay_silent"
	EventRelayOK       = "relay_ok"
	EventForeignBridge = "foreign_bridge"
	EventRelayNoExit   = "relay_no_exit"
)

// ErrUnknownBridge - код моста из заголовка не код существующей машины вида exit.
var ErrUnknownBridge = errors.New("неизвестный мост: нет выхода с таким кодом")

// relayViaColumns - колонки миграции 0014: таблица -> колонки.
var relayViaColumns = map[string][]string{
	"agent_state":    {"relay_via", "relay_via_at"},
	"agent_runtime":  {"bridge"},
	"machine_events": {"exit"},
}

// CheckRelayViaSchema - применена ли миграция 0014 (колонки relay_via, bridge и
// таблица pair_alert_state): без неё список машин и пары падали бы 42703.
func (s *Store) CheckRelayViaSchema(ctx context.Context) error {
	const migration = "migrations/0014_relay_via.sql"
	if err := s.checkTables(ctx, []string{"pair_alert_state"}, migration); err != nil {
		return err
	}
	tables := make([]string, 0, len(relayViaColumns))
	for t := range relayViaColumns {
		tables = append(tables, t)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT table_name, column_name FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = ANY($1)`, tables)
	if err != nil {
		return fmt.Errorf("проверка схемы: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var t, c string
		if err := rows.Scan(&t, &c); err != nil {
			return fmt.Errorf("проверка схемы: %w", err)
		}
		have[t+"."+c] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("проверка схемы: %w", err)
	}
	var missing []string
	for t, cols := range relayViaColumns {
		for _, c := range cols {
			if !have[t+"."+c] {
				missing = append(missing, t+"."+c)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("в схеме базы нет колонок %s: примените %s до запуска мастера",
			strings.Join(missing, ", "), migration)
	}
	return nil
}

// RecordRelayVia записывает, что отчёт агента target пришёл через мост выхода
// bridge (R3.1). bridge - не код существующей машины вида exit - ErrUnknownBridge,
// ничего не пишется. Пишется только машине вида relay (U18): отчёт выхода или
// панели через мост - не ошибка, но и не отметка. Строки agent_state ещё нет
// (отчёта observed не было) - не ошибка: запишется со следующим отчётом.
func (s *Store) RecordRelayVia(ctx context.Context, target, bridge string) error {
	var isExit, updated bool
	err := s.pool.QueryRow(ctx,
		`WITH b AS (SELECT EXISTS (SELECT 1 FROM targets WHERE code = $2 AND kind = 'exit') AS ok),
		      u AS (UPDATE agent_state SET relay_via = $2, relay_via_at = now()
		             WHERE target = $1 AND (SELECT ok FROM b)
		               AND EXISTS (SELECT 1 FROM targets WHERE code = $1 AND kind = 'relay') RETURNING 1)
		 SELECT (SELECT ok FROM b), EXISTS (SELECT 1 FROM u)`, target, bridge).Scan(&isExit, &updated)
	if err != nil {
		return fmt.Errorf("relay_via %s: %w", target, err)
	}
	if !isExit {
		return fmt.Errorf("%w: %q", ErrUnknownBridge, bridge)
	}
	return nil
}

// machineLastSeen - последний отчёт агента машины: observed или runtime; nil - не было.
func machineLastSeen(m Machine) *time.Time {
	switch {
	case m.SeenAt == nil:
		return m.RuntimeSeenAt
	case m.RuntimeSeenAt == nil || m.SeenAt.After(*m.RuntimeSeenAt):
		return m.SeenAt
	default:
		return m.RuntimeSeenAt
	}
}

// ExitRelayStatus - статус своей ноды exit релея relay (R3.2) и когда релей
// последний раз ходил через её мост: по сводке моста в runtime-отчёте выхода или
// по relay_via_at релея, если отчёт пришёл через этот мост (что свежее). Сводка
// моста в счёт, только если runtime-отчёт выхода не старше SilentAfter и код
// сводки - код выхода (U21): старая сводка навсегда держала бы пару в ok.
//   - exit_down - агент выхода молчит дольше SilentAfter (или его нет);
//   - ok - выход жив, релей ходил через мост не позже RelaySilentAfter назад;
//   - relay_silent - выход жив, релей через его мост молчит дольше.
func ExitRelayStatus(exit, relay Machine, now time.Time) (string, *time.Time) {
	var last *time.Time
	if bridgeFresh(exit, now) {
		last = exit.Bridge.RelayLastSeen(relay.Code)
	}
	if relay.RelayVia != nil && *relay.RelayVia == exit.Code && relay.RelayViaAt != nil &&
		(last == nil || relay.RelayViaAt.After(*last)) {
		last = relay.RelayViaAt
	}
	if last != nil {
		t := last.UTC()
		last = &t
	}
	if seen := machineLastSeen(exit); seen == nil || now.Sub(*seen) > SilentAfter {
		return RelayStatusExitDown, last
	}
	if last != nil && now.Sub(*last) <= RelaySilentAfter {
		return RelayStatusOK, last
	}
	return RelayStatusSilent, last
}

// bridgeFresh - сводке моста выхода можно верить (U21): она из runtime-отчёта не
// старше SilentAfter и с кодом этого выхода.
func bridgeFresh(exit Machine, now time.Time) bool {
	return exit.Bridge != nil && exit.Bridge.Code == exit.Code &&
		exit.RuntimeSeenAt != nil && now.Sub(*exit.RuntimeSeenAt) <= SilentAfter
}

// RelayDown - релей упал (R3.3): ни через одну свою ноду он не ходил дольше
// RelaySilentAfter (exitsLast - последние проходы через мосты своих нод) и его агент
// молчит дольше silentAfter. Релей, на котором агента ещё не ставили, не down.
func RelayDown(relay Machine, exitsLast []*time.Time, silentAfter time.Duration, now time.Time) bool {
	for _, l := range exitsLast {
		if l != nil && now.Sub(*l) <= RelaySilentAfter {
			return false
		}
	}
	seen := machineLastSeen(relay)
	return seen != nil && now.Sub(*seen) > silentAfter
}

// fillRelayStatus заполняет статусы пары с релеем: relay_status и relay_last_seen
// выходов, relay_down, foreign_bridge и статус пары. Выключенный выход показывает
// свой relay_status, но в relay_down не участвует (U20).
func fillRelayStatus(p *Pair, now time.Time) {
	if p.Relay == nil {
		return
	}
	lasts := make([]*time.Time, 0, len(p.Exits))
	for i := range p.Exits {
		st, last := ExitRelayStatus(p.Exits[i].Machine, p.Relay.Machine, now)
		p.Exits[i].RelayStatus, p.Exits[i].RelayLastSeen = st, last
		if p.Exits[i].Enabled {
			lasts = append(lasts, last)
		}
	}
	p.RelayDown = RelayDown(p.Relay.Machine, lasts, SilentAfter, now)
	p.ForeignBridge = p.Relay.ForeignBridge != nil && *p.Relay.ForeignBridge
	switch {
	case len(p.Exits) == 0:
		p.Status = PairStatusNoExit
	case p.RelayDown:
		p.Status = PairStatusDown
	default:
		p.Status = PairStatusOK
	}
}

// relayGraph - релеи и выходы флота для детектора: релей -> его свои ноды.
type relayGraph struct {
	relays []Machine
	exits  map[string][]Machine // код релея -> выходы с relay_code = релей, по коду
}

// loadRelayGraph читает релеи и выходы в транзакции детектора.
func (s *Store) loadRelayGraph(ctx context.Context, db queryer) (relayGraph, error) {
	g := relayGraph{exits: map[string][]Machine{}}
	list, err := s.queryMachines(ctx, db, `WHERE t.kind IN ('relay', 'exit') ORDER BY t.code`)
	if err != nil {
		return g, err
	}
	for _, m := range list {
		switch m.Kind {
		case "relay":
			g.relays = append(g.relays, m)
		case "exit":
			if m.RelayCode != "" {
				g.exits[m.RelayCode] = append(g.exits[m.RelayCode], m)
			}
		}
	}
	return g, nil
}

// downRelays - коды релеев, упавших по правилу R3.3; выключенные выходы не в
// счёт (U20).
func (g relayGraph) downRelays(silentAfter time.Duration, now time.Time) []string {
	out := []string{}
	for _, r := range g.relays {
		exits := g.exits[r.Code]
		lasts := make([]*time.Time, 0, len(exits))
		for _, e := range exits {
			if !e.Enabled {
				continue
			}
			_, last := ExitRelayStatus(e, r, now)
			lasts = append(lasts, last)
		}
		if RelayDown(r, lasts, silentAfter, now) {
			out = append(out, r.Code)
		}
	}
	return out
}

// pairKey - строка pair_alert_state.
type pairKey struct{ relay, exit string }

// pairEvent - событие пары до записи.
type pairEvent struct {
	relay, exit, kind, detail string
}

func fmtUTC(t *time.Time) string {
	if t == nil {
		return "ни разу"
	}
	return "с " + t.UTC().Format("2006-01-02 15:04:05") + " UTC"
}

// detectPairEvents - проход детектора по парам (R4.1-R4.4, R5.3) в транзакции tx:
// сравнивает статусы с pair_alert_state, пишет события на переходах и обновляет
// состояние. Выключенные релеи и выходы и релеи без агента тревог не порождают, их
// строки удаляются. down - релеи, упавшие по R3.3: пока релей down, пары о нём
// молчат и хранят прежнее состояние, как при exit_down (U19) - одно падение даёт
// одно событие down, а не ещё relay_silent по каждой своей ноде.
func (s *Store) detectPairEvents(ctx context.Context, tx pgx.Tx, g relayGraph, down []string, now time.Time) ([]MachineEvent, error) {
	isDown := make(map[string]bool, len(down))
	for _, c := range down {
		isDown[c] = true
	}
	prev := map[pairKey]string{}
	rows, err := tx.Query(ctx, `SELECT relay, exit, status FROM pair_alert_state`)
	if err != nil {
		return nil, fmt.Errorf("pair_alert_state: %w", err)
	}
	for rows.Next() {
		var k pairKey
		var st string
		if err := rows.Scan(&k.relay, &k.exit, &st); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan pair_alert_state: %w", err)
		}
		prev[k] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pair_alert_state: %w", err)
	}

	// U26: по каким парам последней была тревога relay_silent - только им положено
	// «снова ходит» (тихо записанное первое наблюдение тревогой не считается).
	alerted := map[pairKey]bool{}
	arows, err := tx.Query(ctx, `SELECT DISTINCT ON (machine, exit) machine, exit, kind
	                               FROM machine_events
	                              WHERE exit IS NOT NULL AND kind IN ('relay_silent', 'relay_ok')
	                              ORDER BY machine, exit, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("machine_events: %w", err)
	}
	for arows.Next() {
		var k pairKey
		var kind string
		if err := arows.Scan(&k.relay, &k.exit, &kind); err != nil {
			arows.Close()
			return nil, fmt.Errorf("scan machine_events: %w", err)
		}
		alerted[k] = kind == EventRelaySilent
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, fmt.Errorf("machine_events: %w", err)
	}

	want := map[pairKey]string{} // строки, которые должны быть после прохода
	var events []pairEvent
	set := func(k pairKey, status string, onNew func(old string, had bool)) {
		want[k] = status
		old, had := prev[k]
		if !had || old != status {
			onNew(old, had)
		}
	}
	for _, r := range g.relays {
		// Выключенный релей и релей, где агента ещё не ставили, тревог не порождают.
		if !r.Enabled || machineLastSeen(r) == nil {
			continue
		}
		exits := g.exits[r.Code]
		if len(exits) == 0 {
			set(pairKey{r.Code, ""}, PairStatusNoExit, func(string, bool) {
				events = append(events, pairEvent{r.Code, "", EventRelayNoExit,
					"у релея " + r.Code + " нет своих нод: ни у одного выхода relay_code = " + r.Code})
			})
		}
		for _, e := range exits {
			if !e.Enabled {
				continue
			}
			k := pairKey{r.Code, e.Code}
			st, last := ExitRelayStatus(e, r, now)
			if st == RelayStatusExitDown || isDown[r.Code] {
				// О релее пара молчит: прежнее состояние сохраняется (R4.3, U19).
				if old, had := prev[k]; had {
					want[k] = old
				}
				continue
			}
			set(k, st, func(old string, had bool) {
				switch {
				case !had:
					// U26: первое наблюдение пары - молча (сводки мостов могли ещё не прийти).
				case st == RelayStatusSilent:
					events = append(events, pairEvent{r.Code, e.Code, EventRelaySilent,
						"релей " + r.Code + " не ходит через мост " + e.Code + " " + fmtUTC(last)})
				case st == RelayStatusOK && old == RelayStatusSilent && alerted[k]:
					events = append(events, pairEvent{r.Code, e.Code, EventRelayOK,
						"релей " + r.Code + " снова ходит через мост " + e.Code})
				}
			})
		}
		if r.ForeignBridge != nil && *r.ForeignBridge && r.RelayVia != nil {
			via := *r.RelayVia
			set(pairKey{r.Code, via}, EventForeignBridge, func(string, bool) {
				events = append(events, pairEvent{r.Code, via, EventForeignBridge,
					"релей " + r.Code + " ходит через мост чужого выхода " + via})
			})
		}
	}

	var delRelays, delExits []string
	for k := range prev {
		if _, ok := want[k]; !ok {
			delRelays, delExits = append(delRelays, k.relay), append(delExits, k.exit)
		}
	}
	if len(delRelays) > 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM pair_alert_state p USING unnest($1::text[], $2::text[]) AS d(relay, exit)
			  WHERE p.relay = d.relay AND p.exit = d.exit`, delRelays, delExits); err != nil {
			return nil, fmt.Errorf("pair_alert_state: %w", err)
		}
	}
	var upRelays, upExits, upStatus []string
	for k, st := range want {
		if old, had := prev[k]; !had || old != st {
			upRelays, upExits, upStatus = append(upRelays, k.relay), append(upExits, k.exit), append(upStatus, st)
		}
	}
	if len(upRelays) > 0 {
		// Граф прочитан в начале прохода: релей могли удалить параллельно. Строку
		// пишем, только если релей есть, и держим его строку FOR KEY SHARE до
		// конца транзакции - иначе FK pair_alert_state.relay (23503) отравил бы
		// транзакцию детектора и откатил бы и тревоги down/up.
		if _, err := tx.Exec(ctx,
			`INSERT INTO pair_alert_state (relay, exit, status, changed_at)
			 SELECT u.relay, u.exit, u.status, now()
			   FROM unnest($1::text[], $2::text[], $3::text[]) AS u(relay, exit, status)
			   JOIN targets t ON t.code = u.relay
			    FOR KEY SHARE OF t
			 ON CONFLICT (relay, exit) DO UPDATE SET status = EXCLUDED.status, changed_at = EXCLUDED.changed_at`,
			upRelays, upExits, upStatus); err != nil {
			return nil, fmt.Errorf("pair_alert_state: %w", err)
		}
	}
	if len(events) == 0 {
		return nil, nil
	}
	machines := make([]string, len(events))
	exits := make([]string, len(events))
	kinds := make([]string, len(events))
	details := make([]string, len(events))
	for i, e := range events {
		machines[i], exits[i], kinds[i], details[i] = e.relay, e.exit, e.kind, e.detail
	}
	rows, err = tx.Query(ctx,
		`INSERT INTO machine_events (machine, kind, detail, exit)
		 SELECT machine, kind, detail, NULLIF(exit, '')
		   FROM unnest($1::text[], $2::text[], $3::text[], $4::text[]) WITH ORDINALITY AS e(machine, kind, detail, exit, n)
		  ORDER BY n
		 RETURNING `+eventCols, machines, kinds, details, exits)
	if err != nil {
		return nil, fmt.Errorf("события пар: %w", err)
	}
	return scanEvents(rows)
}
