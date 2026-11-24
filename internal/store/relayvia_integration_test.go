//go:build integration

package store

// Интеграционные тесты спецификации relay-via-exit: разделы R2-R5 и хранилище 5.4.
// Пары в setupOps: pr1 <- ps1, pr2 <- ps2.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// setExitBridge - runtime-снимок выхода ago назад со сводкой моста: релей relay
// ходил через мост relayAgo назад (relay пустой - сводка без релеев).
func setExitBridge(t *testing.T, st *Store, exit string, ago time.Duration, relay string, relayAgo time.Duration) {
	t.Helper()
	sum := BridgeSummary{Code: exit, Relays: []BridgeRelay{}}
	if relay != "" {
		seen := time.Now().Add(-relayAgo).UTC()
		sum.Relays = append(sum.Relays, BridgeRelay{Code: relay, LastSeen: &seen, Requests: 10, MasterOK: true})
	}
	raw, _ := json.Marshal(sum)
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO agent_runtime (target, seen_at, bridge) VALUES ($1, now() - make_interval(secs => $2), $3::jsonb)
		 ON CONFLICT (target) DO UPDATE SET seen_at = EXCLUDED.seen_at, bridge = EXCLUDED.bridge`,
		exit, ago.Seconds(), string(raw)); err != nil {
		t.Fatal(err)
	}
}

// setRelayVia - последний отчёт релея пришёл через мост exit ago назад.
func setRelayVia(t *testing.T, st *Store, relay, exit string, ago time.Duration) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`UPDATE agent_state SET relay_via = $2, relay_via_at = now() - make_interval(secs => $3) WHERE target = $1`,
		relay, exit, ago.Seconds()); err != nil {
		t.Fatal(err)
	}
}

func pairsByRelay(t *testing.T, st *Store) map[string]Pair {
	t.Helper()
	list, _, err := st.Pairs(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Pair{}
	for _, p := range list {
		if p.Relay != nil {
			out[p.Relay.Code] = p
		}
	}
	return out
}

func exitIn(t *testing.T, p Pair, code string) PairMachine {
	t.Helper()
	for _, e := range p.Exits {
		if e.Code == code {
			return e
		}
	}
	t.Fatalf("в паре нет выхода %s", code)
	return PairMachine{}
}

func machineByCode(t *testing.T, st *Store, code string) Machine {
	t.Helper()
	list, _, err := st.Machines(context.Background(), MachineFilter{}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Code == code {
			return m
		}
	}
	t.Fatalf("нет машины %s", code)
	return Machine{}
}

func detect(t *testing.T, st *Store) []string {
	t.Helper()
	evs, err := st.DetectMachineEvents(context.Background(), SilentAfter)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range evs {
		s := e.Machine + ":" + e.Kind
		if e.Exit != "" {
			s += ":" + e.Exit
		}
		out = append(out, s)
	}
	return out
}

func TestIntegration_RelayVia_5_4_MigrationIdempotentAndChecked(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	setupAll(t).Close() // повторный накат всех миграций
	if err := st.CheckRelayViaSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_RelayVia_R3_1_RecordValidExit(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	setSeen(t, st, "pr2", 0)
	if err := st.RecordRelayVia(ctx, "pr2", "ps2"); err != nil {
		t.Fatal(err)
	}
	m := machineByCode(t, st, "pr2")
	if m.RelayVia == nil || *m.RelayVia != "ps2" || m.RelayViaAt == nil || time.Since(*m.RelayViaAt) > time.Minute {
		t.Fatalf("relay_via: %v %v", m.RelayVia, m.RelayViaAt)
	}
}

func TestIntegration_RelayVia_R3_1_GarbageIgnored(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	setSeen(t, st, "pr2", 0)
	for _, bad := range []string{"nope", "pr1" /* релей, а не выход */, ""} {
		if err := st.RecordRelayVia(ctx, "pr2", bad); !errors.Is(err, ErrUnknownBridge) {
			t.Fatalf("мост %q: ждали ErrUnknownBridge, %v", bad, err)
		}
	}
	m := machineByCode(t, st, "pr2")
	if m.RelayVia != nil || m.RelayViaAt != nil {
		t.Fatalf("мусорный код не должен записаться: %v", m.RelayVia)
	}
	// Отчёта агента ещё не было (строки agent_state нет) - не ошибка.
	if err := st.RecordRelayVia(ctx, "pr1", "ps1"); err != nil {
		t.Fatalf("без строки agent_state: %v", err)
	}
}

func TestIntegration_RelayVia_R2_1_BridgeStored(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	seen := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	rep := RuntimeReport{Target: "ps2", AgentVersion: "v1", Host: &RuntimeHost{},
		Bridge: &BridgeSummary{Code: "ps2", Relays: []BridgeRelay{{Code: "pr2", LastSeen: &seen, Requests: 5, MasterOK: true}}}}
	if err := st.RecordRuntime(ctx, rep); err != nil {
		t.Fatal(err)
	}
	m := machineByCode(t, st, "ps2")
	if m.Bridge == nil || m.Bridge.Code != "ps2" || len(m.Bridge.Relays) != 1 || !m.Bridge.Relays[0].LastSeen.Equal(seen) {
		t.Fatalf("сводка моста не сохранилась: %+v", m.Bridge)
	}
	// Отчёт без моста (старый агент) - сводка снимается.
	rep.Bridge = nil
	if err := st.RecordRuntime(ctx, rep); err != nil {
		t.Fatal(err)
	}
	if m := machineByCode(t, st, "ps2"); m.Bridge != nil {
		t.Fatalf("отчёт без bridge должен снять сводку: %+v", m.Bridge)
	}
}

func TestIntegration_RelayVia_R3_2_PairStatuses(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	setSeen(t, st, "pr1", 0)
	setSeen(t, st, "pr2", 0)

	t.Run("ok по сводке моста", func(t *testing.T) {
		setSeen(t, st, "ps1", 0)
		setExitBridge(t, st, "ps1", 0, "pr1", 30*time.Second)
		e := exitIn(t, pairsByRelay(t, st)["pr1"], "ps1")
		if e.RelayStatus != RelayStatusOK || e.RelayLastSeen == nil {
			t.Fatalf("ждали ok: %s %v", e.RelayStatus, e.RelayLastSeen)
		}
	})
	t.Run("ok по relay_via_at", func(t *testing.T) {
		setSeen(t, st, "ps2", 0)
		setExitBridge(t, st, "ps2", 0, "", 0)
		setRelayVia(t, st, "pr2", "ps2", 20*time.Second)
		e := exitIn(t, pairsByRelay(t, st)["pr2"], "ps2")
		if e.RelayStatus != RelayStatusOK {
			t.Fatalf("ждали ok по relay_via_at: %s", e.RelayStatus)
		}
	})
	t.Run("relay_silent", func(t *testing.T) {
		setExitBridge(t, st, "ps1", 0, "pr1", 5*time.Minute)
		e := exitIn(t, pairsByRelay(t, st)["pr1"], "ps1")
		if e.RelayStatus != RelayStatusSilent || e.RelayLastSeen == nil {
			t.Fatalf("ждали relay_silent: %s %v", e.RelayStatus, e.RelayLastSeen)
		}
	})
	t.Run("exit_down", func(t *testing.T) {
		setSeen(t, st, "ps1", 20*time.Minute)
		setExitBridge(t, st, "ps1", 20*time.Minute, "pr1", 0)
		e := exitIn(t, pairsByRelay(t, st)["pr1"], "ps1")
		if e.RelayStatus != RelayStatusExitDown {
			t.Fatalf("ждали exit_down: %s", e.RelayStatus)
		}
	})
	t.Run("relay_status только у выходов пары", func(t *testing.T) {
		p := pairsByRelay(t, st)["pr1"]
		if p.Relay.RelayStatus != "" {
			t.Fatalf("у релея relay_status быть не должно: %q", p.Relay.RelayStatus)
		}
	})
}

func TestIntegration_RelayVia_R3_3_RelayDownOnlyWhenSilentEverywhere(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	// Агент релея молчит, но выход видит его через мост - не down.
	setSeen(t, st, "pr1", 30*time.Minute)
	setSeen(t, st, "ps1", 0)
	setExitBridge(t, st, "ps1", 0, "pr1", 10*time.Second)
	if p := pairsByRelay(t, st)["pr1"]; p.RelayDown {
		t.Fatal("своя нода видит релей - он не down")
	}
	// Нода перестала видеть релей - down.
	setExitBridge(t, st, "ps1", 0, "pr1", 10*time.Minute)
	if p := pairsByRelay(t, st)["pr1"]; !p.RelayDown || p.Status != PairStatusDown {
		t.Fatalf("не видно ни через одну ноду и агент молчит - down: %+v", p.Status)
	}
	// Агент релея жив - не down, даже если мост его не видит.
	setSeen(t, st, "pr1", 0)
	if p := pairsByRelay(t, st)["pr1"]; p.RelayDown || p.Status != PairStatusOK {
		t.Fatalf("агент жив - не down: %v %s", p.RelayDown, p.Status)
	}
}

func TestIntegration_RelayVia_R3_4_ForeignBridge(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	setSeen(t, st, "pr1", 0)
	setSeen(t, st, "ps1", 0)
	setSeen(t, st, "ps2", 0)

	setRelayVia(t, st, "pr1", "ps1", 0) // свой мост
	if m := machineByCode(t, st, "pr1"); m.ForeignBridge == nil || *m.ForeignBridge {
		t.Fatalf("свой мост: foreign_bridge=false, %v", m.ForeignBridge)
	}
	setRelayVia(t, st, "pr1", "ps2", 0) // мост выхода pr2
	if m := machineByCode(t, st, "pr1"); m.ForeignBridge == nil || !*m.ForeignBridge {
		t.Fatalf("чужой мост: foreign_bridge=true, %v", m.ForeignBridge)
	}
	if p := pairsByRelay(t, st)["pr1"]; !p.ForeignBridge {
		t.Fatal("в паре foreign_bridge=true")
	}
	if m := machineByCode(t, st, "ps1"); m.ForeignBridge != nil {
		t.Fatalf("foreign_bridge - только у релея: %v", *m.ForeignBridge)
	}
}

func TestIntegration_RelayVia_R5_1_PairsOnlyFromTunnels(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	before := pairsByRelay(t, st)
	// Сервер на релее pr1 с нодой ps2 (выход pr2): пары не меняются.
	if _, err := st.CreateServer(ctx, telemtServer(1, "pr1", "ps2")); err != nil {
		t.Fatal(err)
	}
	after := pairsByRelay(t, st)
	for _, r := range []string{"pr1", "pr2"} {
		var b, a []string
		for _, e := range before[r].Exits {
			b = append(b, e.Code)
		}
		for _, e := range after[r].Exits {
			a = append(a, e.Code)
		}
		if !slices.Equal(a, b) {
			t.Fatalf("карта серверов изменила пару %s: %v -> %v", r, b, a)
		}
	}
}

func TestIntegration_RelayVia_R5_3_NoExit(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	seedTargets(t, st, telemtTarget("pr3", "relay"))
	setSeen(t, st, "pr3", 0)
	p := pairsByRelay(t, st)["pr3"]
	if p.Status != PairStatusNoExit || len(p.Exits) != 0 {
		t.Fatalf("релей без своих нод - no_exit: %q", p.Status)
	}
	if got := detect(t, st); !slices.Contains(got, "pr3:relay_no_exit") {
		t.Fatalf("ждали relay_no_exit: %v", got)
	}
	if got := detect(t, st); slices.Contains(got, "pr3:relay_no_exit") {
		t.Fatalf("relay_no_exit - один раз на переходе: %v", got)
	}
}

// pairsAlive - обе пары живы и релеи ходят через свои мосты.
func pairsAlive(t *testing.T, st *Store) {
	t.Helper()
	for _, c := range []string{"pr1", "pr2", "ps1", "ps2"} {
		setSeen(t, st, c, 0)
	}
	setExitBridge(t, st, "ps1", 0, "pr1", 0)
	setExitBridge(t, st, "ps2", 0, "pr2", 0)
}

func TestIntegration_RelayVia_R4_1_SilentAndOkOnTransition(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	pairsAlive(t, st)
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("первый проход по живым парам - тихо: %v", got)
	}
	setExitBridge(t, st, "ps2", 0, "pr2", 5*time.Minute)
	if got := detect(t, st); !slices.Equal(got, []string{"pr2:relay_silent:ps2"}) {
		t.Fatalf("ждали relay_silent по паре pr2/ps2: %v", got)
	}
	// R4.2: повтор без изменений - без событий.
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("повтор не должен давать событий: %v", got)
	}
	setExitBridge(t, st, "ps2", 0, "pr2", 0)
	if got := detect(t, st); !slices.Equal(got, []string{"pr2:relay_ok:ps2"}) {
		t.Fatalf("ждали relay_ok: %v", got)
	}
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("R4.2: повтор после relay_ok: %v", got)
	}
}

// U26: первое наблюдение пары (детектор её ещё не видел - пустое состояние после
// установки или новая пара) записывается без события: при старте мастера сводки
// мостов ещё не пришли. «Снова ходит» уходит только после тревоги.
func TestIntegration_RelayVia_U26_FirstObservationSilent(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	pairsAlive(t, st)
	setExitBridge(t, st, "ps2", 0, "pr2", 5*time.Minute)
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("первое наблюдение - без событий, даже если релей молчит: %v", got)
	}
	setExitBridge(t, st, "ps2", 0, "pr2", 0)
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("relay_ok без предшествующей тревоги не нужен: %v", got)
	}
	setExitBridge(t, st, "ps2", 0, "pr2", 5*time.Minute)
	if got := detect(t, st); !slices.Equal(got, []string{"pr2:relay_silent:ps2"}) {
		t.Fatalf("переход ok -> relay_silent - тревога: %v", got)
	}
	setExitBridge(t, st, "ps2", 0, "pr2", 0)
	if got := detect(t, st); !slices.Equal(got, []string{"pr2:relay_ok:ps2"}) {
		t.Fatalf("после тревоги - relay_ok: %v", got)
	}
}

func TestIntegration_RelayVia_R4_3_ExitDownNoRelaySilent(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	pairsAlive(t, st)
	detect(t, st)
	// Выход лёг: о релее пара молчит, relay_silent нет.
	setSeen(t, st, "ps2", 20*time.Minute)
	setExitBridge(t, st, "ps2", 20*time.Minute, "pr2", 20*time.Minute)
	if got := detect(t, st); slices.ContainsFunc(got, func(s string) bool { return s == "pr2:relay_silent:ps2" }) {
		t.Fatalf("exit_down не создаёт relay_silent: %v", got)
	}

	// relay_silent -> exit_down -> ok: relay_ok приходит, тревога не висит.
	setSeen(t, st, "ps2", 0)
	setExitBridge(t, st, "ps2", 0, "pr2", 5*time.Minute)
	if got := detect(t, st); !slices.Contains(got, "pr2:relay_silent:ps2") {
		t.Fatalf("ждали relay_silent: %v", got)
	}
	setSeen(t, st, "ps2", 20*time.Minute)
	setExitBridge(t, st, "ps2", 20*time.Minute, "pr2", 20*time.Minute)
	detect(t, st)
	setSeen(t, st, "ps2", 0)
	setExitBridge(t, st, "ps2", 0, "pr2", 0)
	if got := detect(t, st); !slices.Contains(got, "pr2:relay_ok:ps2") {
		t.Fatalf("после exit_down пара вернулась в ok - relay_ok: %v", got)
	}
}

func TestIntegration_RelayVia_R4_1_ForeignBridgeEvent(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	pairsAlive(t, st)
	detect(t, st)
	setRelayVia(t, st, "pr1", "ps2", 0)
	if got := detect(t, st); !slices.Contains(got, "pr1:foreign_bridge:ps2") {
		t.Fatalf("ждали foreign_bridge: %v", got)
	}
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("R4.2: повтор foreign_bridge: %v", got)
	}
}

func TestIntegration_RelayVia_R4_1_DownUpByRule(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	pairsAlive(t, st)
	detect(t, st)
	// Агент pr1 молчит, но ps1 его видит - не down.
	setSeen(t, st, "pr1", 30*time.Minute)
	if got := detect(t, st); slices.Contains(got, "pr1:down") {
		t.Fatalf("своя нода видит релей - down нет: %v", got)
	}
	// Ни одна нода не видит - down.
	setExitBridge(t, st, "ps1", 0, "pr1", 10*time.Minute)
	if got := detect(t, st); !slices.Contains(got, "pr1:down") {
		t.Fatalf("ждали down по R3.3: %v", got)
	}
	// Нода снова видит - up.
	setExitBridge(t, st, "ps1", 0, "pr1", 0)
	if got := detect(t, st); !slices.Contains(got, "pr1:up") {
		t.Fatalf("ждали up: %v", got)
	}
}

// Релей без агента (его ещё не ставили) тревог пар не порождает - как down в ADR 0007.
func TestIntegration_RelayVia_R4_1_RelayWithoutAgentNoPairAlerts(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	setSeen(t, st, "ps1", 0)
	setExitBridge(t, st, "ps1", 0, "", 0)
	seedTargets(t, st, telemtTarget("pr3", "relay"))
	if e := exitIn(t, pairsByRelay(t, st)["pr1"], "ps1"); e.RelayStatus != RelayStatusSilent {
		t.Fatalf("статус пары по-прежнему relay_silent: %s", e.RelayStatus)
	}
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("релей без агента - без тревог пар: %v", got)
	}
}

func TestIntegration_RelayVia_R4_4_DisabledNoAlerts(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	pairsAlive(t, st)
	detect(t, st)
	off := false
	if _, err := st.UpdateTarget(ctx, "ps2", TargetPatch{Enabled: &off}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTarget(ctx, "pr1", TargetPatch{Enabled: &off}, nil); err != nil {
		t.Fatal(err)
	}
	setExitBridge(t, st, "ps2", 0, "pr2", 5*time.Minute) // выключенный выход
	setRelayVia(t, st, "pr1", "ps2", 0)                  // выключенный релей
	setExitBridge(t, st, "ps1", 0, "pr1", 10*time.Minute)
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("выключенные машины тревог не порождают: %v", got)
	}
}

// U18: relay_via пишется только релею; отчёт выхода через мост - без отметки.
func TestIntegration_RelayVia_U18_OnlyRelayRecorded(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	setSeen(t, st, "ps1", 0)
	if err := st.RecordRelayVia(ctx, "ps1", "ps2"); err != nil {
		t.Fatalf("отчёт выхода через мост - не ошибка: %v", err)
	}
	if m := machineByCode(t, st, "ps1"); m.RelayVia != nil || m.RelayViaAt != nil {
		t.Fatalf("relay_via у выхода записываться не должен: %v %v", *m.RelayVia, m.RelayViaAt)
	}
	// Мусорный мост у выхода - по-прежнему ErrUnknownBridge (WARN на сервере).
	if err := st.RecordRelayVia(ctx, "ps1", "nope"); !errors.Is(err, ErrUnknownBridge) {
		t.Fatalf("ждали ErrUnknownBridge: %v", err)
	}
}

// U19: пока релей down (R3.3), пары по нему молчат: одно падение - одно событие down,
// а подъём - одно up, без relay_silent/relay_ok по своим нодам.
func TestIntegration_RelayVia_U19_DownRelayNoPairEvents(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	pairsAlive(t, st)
	detect(t, st)
	setSeen(t, st, "pr1", 30*time.Minute)
	setExitBridge(t, st, "ps1", 0, "pr1", 10*time.Minute)
	if got := detect(t, st); !slices.Equal(got, []string{"pr1:down"}) {
		t.Fatalf("падение релея - одно событие down: %v", got)
	}
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("повтор при лежащем релее - тихо: %v", got)
	}
	setSeen(t, st, "pr1", 0)
	setExitBridge(t, st, "ps1", 0, "pr1", 0)
	if got := detect(t, st); !slices.Equal(got, []string{"pr1:up"}) {
		t.Fatalf("подъём релея - одно событие up: %v", got)
	}
	// Релей поднялся, но через свою ноду не ходит: relay_silent приходит после up.
	setSeen(t, st, "pr1", 30*time.Minute)
	setExitBridge(t, st, "ps1", 0, "pr1", 10*time.Minute)
	detect(t, st) // down
	setSeen(t, st, "pr1", 0)
	if got := detect(t, st); !slices.Equal(got, []string{"pr1:up", "pr1:relay_silent:ps1"}) {
		t.Fatalf("релей жив, но мост его не видит - up и relay_silent: %v", got)
	}
}

// U20: выключенный выход не держит релей живым - ни в API пар, ни в детекторе.
func TestIntegration_RelayVia_U20_DisabledExitNotInDown(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	pairsAlive(t, st)
	detect(t, st)
	// Второй выход pr1 - выключен, его мост ещё видит релей.
	seedTargets(t, st, Target{Code: "ps3", Kind: "exit", RelayCode: "pr1", Enabled: true, Resources: []string{ResourceTelemtUsers}})
	setSeen(t, st, "ps3", 0)
	setExitBridge(t, st, "ps3", 0, "pr1", 0)
	off := false
	if _, err := st.UpdateTarget(ctx, "ps3", TargetPatch{Enabled: &off}, nil); err != nil {
		t.Fatal(err)
	}
	setSeen(t, st, "pr1", 30*time.Minute)
	setExitBridge(t, st, "ps1", 0, "pr1", 10*time.Minute)
	if p := pairsByRelay(t, st)["pr1"]; !p.RelayDown || p.Status != PairStatusDown {
		t.Fatalf("видит только выключенный выход - релей down: %v %q", p.RelayDown, p.Status)
	}
	if got := detect(t, st); !slices.Contains(got, "pr1:down") {
		t.Fatalf("детектор: ждали pr1:down, %v", got)
	}
}

// U22: статусы пар в API считаются от now() базы, как в детекторе. Часы базы
// «спешат» на час (public.now() перекрывает pg_catalog.now() в search_path
// отдельного пула): с часами мастера пары были бы ok, с часами базы - выходы молчат.
func TestIntegration_RelayVia_U22_PairsUseDBClock(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	pairsAlive(t, st)
	if _, err := st.pool.Exec(ctx,
		`CREATE OR REPLACE FUNCTION public.now() RETURNS timestamptz LANGUAGE sql STABLE
		   AS $$ SELECT pg_catalog.now() + interval '1 hour' $$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS public.now()`) })
	dsn := mustDSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	skew, err := Open(ctx, dsn+sep+"search_path=public,pg_catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer skew.Close()
	var dbNow time.Time
	if err := skew.pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil || time.Until(dbNow) < 50*time.Minute {
		t.Fatalf("часы базы не сдвинуты: %v %v", dbNow, err)
	}
	p := pairsByRelay(t, skew)["pr1"]
	if e := exitIn(t, p, "ps1"); e.RelayStatus != RelayStatusExitDown {
		t.Fatalf("по часам базы выход молчит час - exit_down, а %q", e.RelayStatus)
	}
	if !p.RelayDown {
		t.Fatal("по часам базы релей молчит час - relay_down")
	}
}

// Детектор читает граф в начале прохода; релей удалили параллельно - вставка в
// pair_alert_state не падает FK 23503 и не откатывает весь проход.
func TestIntegration_RelayVia_PairStateRelayDeletedConcurrently(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	pairsAlive(t, st)
	g, err := st.loadRelayGraph(ctx, st.pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Удаление релея pr2 «между» чтением графа и записью состояния пар.
	if _, err := st.pool.Exec(ctx, `UPDATE targets SET relay_code = NULL WHERE code = 'ps2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM targets WHERE code = 'pr2'`); err != nil {
		t.Fatal(err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.detectPairEvents(ctx, tx, g, nil, now); err != nil {
		t.Fatalf("релей удалён параллельно - проход не должен падать: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var relays []string
	rows, err := st.pool.Query(ctx, `SELECT relay FROM pair_alert_state ORDER BY relay`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		relays = append(relays, r)
	}
	rows.Close()
	if !slices.Equal(relays, []string{"pr1"}) {
		t.Fatalf("строка удалённого релея не пишется, живого - пишется: %v", relays)
	}
}
