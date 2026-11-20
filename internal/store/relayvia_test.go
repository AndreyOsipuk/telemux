package store

// Юнит-тесты правил статуса пар (спецификация relay-via-exit, R3.2-R3.3, контракт 5.3).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func ago(now time.Time, d time.Duration) *time.Time {
	t := now.Add(-d)
	return &t
}

func bridgeWith(relay string, seen *time.Time) *BridgeSummary {
	return &BridgeSummary{Code: "ps2", Relays: []BridgeRelay{{Code: relay, LastSeen: seen}}}
}

func TestExitRelayStatus_R3_2_U21(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	via := "ps2"
	other := "ps9"
	relay := Machine{Code: "pr2", Kind: "relay"}
	cases := []struct {
		name     string
		exit     Machine
		relay    Machine
		want     string
		wantLast *time.Time
	}{
		{"ok по сводке моста",
			Machine{Code: "ps2", SeenAt: ago(now, time.Minute), RuntimeSeenAt: ago(now, time.Minute),
				Bridge: bridgeWith("pr2", ago(now, time.Minute))},
			relay, RelayStatusOK, ago(now, time.Minute)},
		{"ok по relay_via_at",
			Machine{Code: "ps2", RuntimeSeenAt: ago(now, 10*time.Second)},
			Machine{Code: "pr2", RelayVia: &via, RelayViaAt: ago(now, 2*time.Minute)}, RelayStatusOK, ago(now, 2*time.Minute)},
		{"берётся свежее из сводки и relay_via_at",
			Machine{Code: "ps2", SeenAt: ago(now, 0), RuntimeSeenAt: ago(now, 0), Bridge: bridgeWith("pr2", ago(now, 10*time.Minute))},
			Machine{Code: "pr2", RelayVia: &via, RelayViaAt: ago(now, time.Minute)}, RelayStatusOK, ago(now, time.Minute)},
		{"relay_via через чужой мост не в счёт",
			Machine{Code: "ps2", SeenAt: ago(now, 0)},
			Machine{Code: "pr2", RelayVia: &other, RelayViaAt: ago(now, 0)}, RelayStatusSilent, nil},
		{"relay_silent - дольше T_relay",
			Machine{Code: "ps2", SeenAt: ago(now, 0), RuntimeSeenAt: ago(now, 0), Bridge: bridgeWith("pr2", ago(now, RelaySilentAfter+time.Second))},
			relay, RelayStatusSilent, ago(now, RelaySilentAfter+time.Second)},
		{"relay_silent - в сводке другой релей",
			Machine{Code: "ps2", SeenAt: ago(now, 0), RuntimeSeenAt: ago(now, 0), Bridge: bridgeWith("pr1", ago(now, 0))},
			relay, RelayStatusSilent, nil},
		{"exit_down - выход молчит",
			Machine{Code: "ps2", SeenAt: ago(now, SilentAfter+time.Second), RuntimeSeenAt: ago(now, SilentAfter+time.Second),
				Bridge: bridgeWith("pr2", ago(now, 0))},
			relay, RelayStatusExitDown, nil},
		{"exit_down - агента выхода нет", Machine{Code: "ps2"}, relay, RelayStatusExitDown, nil},
		// U21: сводка моста - только из свежего runtime-отчёта и со своим кодом.
		{"U21 runtime-отчёт выхода старый - сводка не в счёт",
			Machine{Code: "ps2", SeenAt: ago(now, 0), RuntimeSeenAt: ago(now, SilentAfter+time.Second),
				Bridge: bridgeWith("pr2", ago(now, 0))},
			relay, RelayStatusSilent, nil},
		{"U21 runtime-отчёта нет - сводка не в счёт",
			Machine{Code: "ps2", SeenAt: ago(now, 0), Bridge: bridgeWith("pr2", ago(now, 0))},
			relay, RelayStatusSilent, nil},
		{"U21 код сводки не код выхода - сводка не в счёт",
			Machine{Code: "ps1", SeenAt: ago(now, 0), RuntimeSeenAt: ago(now, 0), Bridge: bridgeWith("pr2", ago(now, 0))},
			relay, RelayStatusSilent, nil},
		{"U21 runtime-отчёт на пороге - сводка в счёт",
			Machine{Code: "ps2", RuntimeSeenAt: ago(now, SilentAfter), Bridge: bridgeWith("pr2", ago(now, time.Minute))},
			relay, RelayStatusOK, ago(now, time.Minute)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, last := ExitRelayStatus(c.exit, c.relay, now)
			if got != c.want {
				t.Fatalf("статус %q, ждали %q", got, c.want)
			}
			if (last == nil) != (c.wantLast == nil) || (last != nil && !last.Equal(*c.wantLast)) {
				t.Fatalf("relay_last_seen %v, ждали %v", last, c.wantLast)
			}
		})
	}
}

func TestRelayDown_R3_3(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	silent := Machine{Code: "pr2", SeenAt: ago(now, SilentAfter+time.Minute)}
	alive := Machine{Code: "pr2", SeenAt: ago(now, time.Minute)}
	cases := []struct {
		name  string
		relay Machine
		lasts []*time.Time
		want  bool
	}{
		{"агент молчит, ни одна нода не видит", silent, []*time.Time{nil, ago(now, 10*time.Minute)}, true},
		{"агент молчит, одна нода видит", silent, []*time.Time{ago(now, 10*time.Minute), ago(now, time.Minute)}, false},
		{"агент жив, ноды не видят", alive, []*time.Time{nil}, false},
		{"агент молчит, своих нод нет", silent, nil, true},
		{"агента не ставили", Machine{Code: "pr2"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RelayDown(c.relay, c.lasts, SilentAfter, now); got != c.want {
				t.Fatalf("down=%v, ждали %v", got, c.want)
			}
		})
	}
}

func TestPairJSON_5_3(t *testing.T) {
	now := time.Now()
	relay := PairMachine{Machine: Machine{Code: "pr2", Kind: "relay", SeenAt: &now}}
	p := Pair{Relay: &relay, Exits: []PairMachine{{Machine: Machine{Code: "ps2", Kind: "exit", SeenAt: &now}}}}
	fillRelayStatus(&p, now)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	// У выхода - relay_status и relay_last_seen (null), у релея этих полей нет.
	for _, want := range []string{`"relay_status":"relay_silent"`, `"relay_last_seen":null`,
		`"relay_down":false`, `"foreign_bridge":false`, `"status":"ok"`, `"relay_via":null`} {
		if !strings.Contains(s, want) {
			t.Fatalf("нет %s: %s", want, s)
		}
	}
	if strings.Count(s, `"relay_last_seen"`) != 1 || strings.Count(s, `"relay_status"`) != 1 {
		t.Fatalf("relay_status и relay_last_seen - только у выхода: %s", s)
	}
	if strings.Contains(s, `"bridge"`) {
		t.Fatalf("сводка моста в API машин не отдаётся: %s", s)
	}
}

// U20: выключенный выход не участвует в статусе пары и в расчёте down релея:
// его мост, видящий релей, не держит релей живым.
func TestFillRelayStatus_U20_DisabledExitIgnored(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	relay := PairMachine{Machine: Machine{Code: "pr2", Kind: "relay", Enabled: true, SeenAt: ago(now, SilentAfter+time.Minute)}}
	on := PairMachine{Machine: Machine{Code: "ps1", Kind: "exit", Enabled: true, RelayCode: "pr2",
		RuntimeSeenAt: ago(now, 0), Bridge: &BridgeSummary{Code: "ps1", Relays: []BridgeRelay{}}}}
	off := PairMachine{Machine: Machine{Code: "ps2", Kind: "exit", Enabled: false, RelayCode: "pr2",
		RuntimeSeenAt: ago(now, 0), Bridge: bridgeWith("pr2", ago(now, 0))}}
	p := Pair{Relay: &relay, Exits: []PairMachine{on, off}}
	fillRelayStatus(&p, now)
	if !p.RelayDown || p.Status != PairStatusDown {
		t.Fatalf("выключенный выход не держит релей живым: down=%v status=%q", p.RelayDown, p.Status)
	}

	g := relayGraph{relays: []Machine{relay.Machine}, exits: map[string][]Machine{"pr2": {on.Machine, off.Machine}}}
	if got := g.downRelays(SilentAfter, now); len(got) != 1 || got[0] != "pr2" {
		t.Fatalf("детектор: выключенный выход не в расчёте down, %v", got)
	}

	// Только выключенные выходы: релей судится по своему агенту (как без своих нод).
	p = Pair{Relay: &relay, Exits: []PairMachine{off}}
	fillRelayStatus(&p, now)
	if !p.RelayDown {
		t.Fatal("только выключенный выход видит релей - релей down по своему агенту")
	}
}
