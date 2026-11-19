//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

// seedDesired кладёт ревизию ресурса машины, созданную age назад.
func seedDesired(t *testing.T, st *Store, target, resource string, rev int64, age time.Duration) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO desired_config (target, resource, revision, sha256, created_at)
		 VALUES ($1, $2, $3, 'x', now() - $4::int * interval '1 second')`,
		target, resource, rev, int(age.Seconds())); err != nil {
		t.Fatalf("desired %s/%s: %v", target, resource, err)
	}
}

// ageAgent сдвигает последний отчёт агента машины на age назад.
func ageAgent(t *testing.T, st *Store, target string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE agent_state SET seen_at = now() - $2::int * interval '1 second' WHERE target = $1`,
		`UPDATE observed_state SET seen_at = now() - $2::int * interval '1 second' WHERE target = $1`,
	} {
		if _, err := st.pool.Exec(ctx, q, target, int(age.Seconds())); err != nil {
			t.Fatal(err)
		}
	}
}

func report(t *testing.T, st *Store, rep ObservedReport) {
	t.Helper()
	if err := st.RecordObserved(context.Background(), rep); err != nil {
		t.Fatalf("отчёт %s: %v", rep.Target, err)
	}
}

// seedFleet - флот на все граничные случаи пар и дрейфа:
//
//	pr1 (релей, ok)      - выходы ps1 (отстаёт давно), ps2 (мост не поднят);
//	pr2 (релей, без агента) - выходы off1 (выключен), ps3 (отстаёт, но ревизия свежая);
//	без релея: pan1 (panel, failed), ps9 (релей pr9 не существует, агента нет), solo (молчит).
func seedFleet(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, `TRUNCATE agent_state`); err != nil {
		t.Fatal(err)
	}
	seedTargets(t, st,
		telemtTarget("pr1", "relay"),
		Target{Code: "pr2", Kind: "relay", Enabled: true},
		Target{Code: "ps1", Kind: "exit", RelayCode: "pr1", Resources: []string{ResourceTelemtUsers}, Enabled: true},
		Target{Code: "ps2", Kind: "exit", RelayCode: "pr1", Enabled: true},
		Target{Code: "off1", Kind: "exit", RelayCode: "pr2", Resources: []string{ResourceTelemtUsers}, Enabled: false},
		Target{Code: "ps3", Kind: "exit", RelayCode: "pr2", Resources: []string{ResourceTelemtUsers}, Enabled: true},
		Target{Code: "pan1", Kind: "panel", Resources: []string{ResourceXuiClients}, Enabled: true},
		Target{Code: "ps9", Kind: "exit", RelayCode: "pr9", Resources: []string{ResourceTelemtUsers}, Enabled: true},
		Target{Code: "solo", Kind: "exit", Enabled: true},
	)
	old := 20 * time.Minute
	seedDesired(t, st, "pr1", ResourceTelemtUsers, 3, old)
	seedDesired(t, st, "ps1", ResourceTelemtUsers, 2, time.Hour)
	seedDesired(t, st, "ps1", ResourceTelemtUsers, 3, old)
	seedDesired(t, st, "off1", ResourceTelemtUsers, 3, old)
	seedDesired(t, st, "ps3", ResourceTelemtUsers, 3, old)
	seedDesired(t, st, "ps3", ResourceTelemtUsers, 4, 0)
	seedDesired(t, st, "pan1", ResourceXuiClients, 2, old)
	seedDesired(t, st, "ps9", ResourceTelemtUsers, 3, old)

	telemt := func(rev int64, status string) []ObservedResource {
		return []ObservedResource{{Resource: ResourceTelemtUsers, Revision: rev, SHA256: "s", Status: status}}
	}
	report(t, st, ObservedReport{Target: "pr1", AgentVersion: "v1", Resources: telemt(3, "applied")})
	report(t, st, ObservedReport{Target: "ps1", AgentVersion: "v1", Resources: telemt(2, "applied")})
	report(t, st, ObservedReport{Target: "ps2", AgentVersion: "v1",
		Checks: []ObservedCheck{{Name: "bridge", OK: false, Error: "connection refused"}}})
	report(t, st, ObservedReport{Target: "ps3", AgentVersion: "v1", Resources: telemt(3, "applied")})
	report(t, st, ObservedReport{Target: "pan1", AgentVersion: "v1", Resources: []ObservedResource{
		{Resource: ResourceXuiClients, Revision: 2, Status: "failed", Error: "inbound не найден"}}})
	report(t, st, ObservedReport{Target: "solo", AgentVersion: "v1"})
	ageAgent(t, st, "solo", old)
	// Отказы начались давно: время начала - по журналу применений.
	if _, err := st.pool.Exec(ctx,
		`UPDATE apply_log SET at = now() - interval '20 minutes' WHERE target IN ('pan1', 'ps2')`); err != nil {
		t.Fatal(err)
	}
}

func pairCodes(p Pair) (relay string, exits []string) {
	if p.Relay != nil {
		relay = p.Relay.Code
	}
	for _, e := range p.Exits {
		exits = append(exits, e.Code)
	}
	return relay, exits
}

// Пары: релей с двумя выходами, релей без агента, псевдо-пара «без релея» в
// конце (выход без relay_code, выход с несуществующим релеем, panel);
// статусы машин считаются по отчётам и желаемым ревизиям.
func TestIntegration_Pairs(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedFleet(t, st)

	pairs, total, err := st.Pairs(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(pairs) != 3 {
		t.Fatalf("ждали 2 релея и псевдо-пару, получили total=%d len=%d", total, len(pairs))
	}
	want := []struct {
		relay string
		exits []string
	}{
		{"pr1", []string{"ps1", "ps2"}},
		{"pr2", []string{"off1", "ps3"}},
		{"", []string{"pan1", "ps9", "solo"}},
	}
	status := map[string]string{}
	for i, w := range want {
		relay, exits := pairCodes(pairs[i])
		if relay != w.relay || len(exits) != len(w.exits) {
			t.Fatalf("пара %d: ждали %s %v, получили %s %v", i, w.relay, w.exits, relay, exits)
		}
		for j := range exits {
			if exits[j] != w.exits[j] {
				t.Fatalf("пара %d: ждали выходы %v, получили %v", i, w.exits, exits)
			}
		}
		if pairs[i].Relay != nil {
			status[pairs[i].Relay.Code] = pairs[i].Relay.Status
		}
		for _, e := range pairs[i].Exits {
			status[e.Code] = e.Status
		}
	}
	wantStatus := map[string]string{
		"pr1": StatusOK, "pr2": StatusNoAgent, "ps1": StatusBehind, "ps2": StatusFailed,
		"off1": StatusDisabled, "ps3": StatusBehind, "pan1": StatusFailed, "ps9": StatusNoAgent, "solo": StatusSilent,
	}
	for code, w := range wantStatus {
		if status[code] != w {
			t.Errorf("%s: ждали статус %s, получили %s", code, w, status[code])
		}
	}
	ps1 := pairs[0].Exits[0]
	if len(ps1.Desired) != 1 || ps1.Desired[0].Revision != 3 || len(ps1.Observed) != 1 || ps1.Observed[0].Revision != 2 {
		t.Fatalf("ps1: желаемая - последняя ревизия, применённая - из отчёта: %+v", ps1)
	}
	if ps2 := pairs[0].Exits[1]; ps2.Desired == nil || len(ps2.Desired) != 0 || len(ps2.Checks) != 1 {
		t.Fatalf("ps2 без ресурсов: desired - пустой массив, проверки на месте: %+v", ps2)
	}
	if pairs[1].Relay.SeenAt != nil {
		t.Fatal("pr2 без агента, а seen_at есть")
	}

	// Пагинация идёт по релеям, псевдо-пара - последняя.
	page, total, err := st.Pairs(ctx, 1, 1)
	if relay, exits := pairCodes(page[0]); err != nil || total != 3 || len(page) != 1 || relay != "pr2" || len(exits) != 2 {
		t.Fatalf("вторая страница: total=%d %+v %v", total, page, err)
	}
	page, _, err = st.Pairs(ctx, 1, 2)
	if err != nil || len(page) != 1 || page[0].Relay != nil || len(page[0].Exits) != 3 {
		t.Fatalf("третья страница - псевдо-пара: %+v %v", page, err)
	}
	page, _, err = st.Pairs(ctx, 2, 1)
	if err != nil || len(page) != 2 || page[1].Relay != nil {
		t.Fatalf("страница с последним релеем и псевдо-парой: %+v %v", page, err)
	}
	page, total, err = st.Pairs(ctx, 5, 3)
	if err != nil || total != 3 || page == nil || len(page) != 0 {
		t.Fatalf("за концом списка - пустой массив: total=%d %+v %v", total, page, err)
	}
}

// Без машин вне пар псевдо-пары нет.
func TestIntegration_Pairs_NoOrphans(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	if _, err := st.pool.Exec(context.Background(), `TRUNCATE agent_state`); err != nil {
		t.Fatal(err)
	}
	seedTargets(t, st, telemtTarget("pr1", "relay"),
		Target{Code: "ps1", Kind: "exit", RelayCode: "pr1", Enabled: true})
	pairs, total, err := st.Pairs(context.Background(), 20, 0)
	if err != nil || total != 1 || len(pairs) != 1 || pairs[0].Relay == nil || len(pairs[0].Exits) != 1 {
		t.Fatalf("ждали одну пару без псевдо-пары: total=%d %+v %v", total, pairs, err)
	}
	if pairs[0].Relay.Status != StatusNoAgent {
		t.Fatalf("релей без агента: %s", pairs[0].Relay.Status)
	}
}

// Пары и список машин идут одним путём выборки: у машины в паре те же поля, что
// в /api/v1/machines - sha256 и desired_revision ресурса, версии из runtime-отчёта.
func TestIntegration_Pairs_MachineFields(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"),
		Target{Code: "ps1", Kind: "exit", RelayCode: "pr1", Enabled: true})
	seedDesired(t, st, "pr1", ResourceTelemtUsers, 3, time.Minute)
	report(t, st, ObservedReport{Target: "pr1", AgentVersion: "v1", Resources: []ObservedResource{
		{Resource: ResourceTelemtUsers, Revision: 3, SHA256: "ab", Status: "applied"}}})
	if err := st.RecordRuntime(ctx, telemtRep("pr1")); err != nil {
		t.Fatal(err)
	}

	pairs, _, err := st.Pairs(ctx, 20, 0)
	if err != nil || len(pairs) != 1 || pairs[0].Relay == nil {
		t.Fatalf("пары: %+v %v", pairs, err)
	}
	relay := pairs[0].Relay
	if relay.TelemtVersion != "3.5.17" || relay.RuntimeSeenAt == nil || len(relay.Observed) != 1 ||
		relay.Observed[0].SHA256 != "ab" || relay.Observed[0].DesiredRevision == nil || *relay.Observed[0].DesiredRevision != 3 {
		t.Fatalf("у релея в паре нет полей Machine: %+v", relay.Machine)
	}
	if relay.Status != StatusOK {
		t.Fatalf("статус релея: %s", relay.Status)
	}
	machines, _, err := st.Machines(ctx, MachineFilter{Kind: "relay"}, 20, 0)
	if err != nil || len(machines) != 1 || machines[0].TelemtVersion != relay.TelemtVersion ||
		*machines[0].Observed[0].DesiredRevision != *relay.Observed[0].DesiredRevision {
		t.Fatalf("список машин расходится с парой: %+v %v", machines, err)
	}
	if ex := pairs[0].Exits; len(ex) != 1 || ex[0].RuntimeSeenAt != nil || ex[0].TelemtVersion != "" {
		t.Fatalf("у выхода без runtime-отчёта поля должны быть пустыми: %+v", ex)
	}
}

// Дрейф: отставание старше порога, отказ ресурса и проверки, молчание агента;
// выключенные машины и свежие ревизии не попадают.
func TestIntegration_Drift(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedFleet(t, st)

	items, total, err := st.Drift(ctx, 10*time.Minute, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ target, reason, resource string }
	want := []row{
		{"pan1", "failed", ResourceXuiClients},
		{"ps1", "behind", ResourceTelemtUsers},
		{"ps2", "failed", "check:bridge"},
		{"ps9", "behind", ResourceTelemtUsers},
		{"solo", "silent", ""},
	}
	if total != len(want) || len(items) != len(want) {
		t.Fatalf("ждали %d строк, получили total=%d %+v", len(want), total, items)
	}
	for i, w := range want {
		got := row{items[i].Target, items[i].Reason, items[i].Resource}
		if got != w {
			t.Fatalf("строка %d: ждали %+v, получили %+v", i, w, got)
		}
	}
	by := map[string]DriftItem{}
	for _, it := range items {
		by[it.Target] = it
	}
	if it := by["ps1"]; it.DesiredRevision != 3 || it.ObservedRevision == nil || *it.ObservedRevision != 2 ||
		it.Kind != "exit" || it.RelayCode != "pr1" || it.SeenAt == nil || time.Since(it.Since) < 19*time.Minute {
		t.Fatalf("ps1 behind: %+v", it)
	}
	if it := by["ps9"]; it.ObservedRevision != nil || it.SeenAt != nil {
		t.Fatalf("ps9 без агента: применённой ревизии и seen_at нет: %+v", it)
	}
	if it := by["pan1"]; it.Error != "inbound не найден" || it.DesiredRevision != 2 || time.Since(it.Since) < 19*time.Minute {
		t.Fatalf("pan1 failed: since - начало отказа по журналу: %+v", it)
	}
	if it := by["ps2"]; it.Error != "connection refused" || it.DesiredRevision != 0 {
		t.Fatalf("ps2 failed check: %+v", it)
	}
	if it := by["solo"]; it.DesiredRevision != 0 || it.SeenAt == nil || !it.Since.Equal(*it.SeenAt) {
		t.Fatalf("solo silent: since - последний отчёт: %+v", it)
	}

	// Свежая ревизия ps3 - в дрейфе только при нулевом пороге; off1 выключена - никогда.
	all, _, err := st.Drift(ctx, 0, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ps3 bool
	for _, it := range all {
		if it.Target == "off1" {
			t.Fatalf("выключенная машина в дрейфе: %+v", it)
		}
		if it.Target == "ps3" && it.Reason == "behind" && it.DesiredRevision == 4 {
			ps3 = true
		}
	}
	if !ps3 {
		t.Fatalf("при пороге 0 ждали отставание ps3: %+v", all)
	}

	page, total, err := st.Drift(ctx, 10*time.Minute, 2, 2)
	if err != nil || total != 5 || len(page) != 2 || page[0].Target != "ps2" || page[1].Target != "ps9" {
		t.Fatalf("страница дрейфа: total=%d %+v %v", total, page, err)
	}
	page, _, err = st.Drift(ctx, 10*time.Minute, 2, 10)
	if err != nil || page == nil || len(page) != 0 {
		t.Fatalf("за концом списка - пустой массив: %+v %v", page, err)
	}
}

// Журнал машины: новые первыми, постранично; неизвестная машина - ErrUnknownTarget.
func TestIntegration_ApplyLog(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"), telemtTarget("ps1", "exit"))
	for i := 1; i <= 5; i++ {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO apply_log (target, resource, revision, status, error, at)
			 VALUES ('pr1', $1, $2, 'applied', NULL, now() - $3::int * interval '1 minute')`,
			ResourceTelemtUsers, i, 10-i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO apply_log (target, resource, status, error) VALUES ('pr1', 'check:bridge', 'failed', 'refused'),
		                                                               ('ps1', $1, 'applied', NULL)`, ResourceTelemtUsers); err != nil {
		t.Fatal(err)
	}

	page, total, err := st.ApplyLog(ctx, "pr1", 2, 0)
	if err != nil || total != 6 || len(page) != 2 {
		t.Fatalf("первая страница: total=%d %+v %v", total, page, err)
	}
	if page[0].Resource != "check:bridge" || page[0].Revision != nil || page[0].Error != "refused" ||
		page[1].Revision == nil || *page[1].Revision != 5 {
		t.Fatalf("новые первыми: %+v", page)
	}
	page, _, err = st.ApplyLog(ctx, "pr1", 2, 4)
	if err != nil || len(page) != 2 || *page[1].Revision != 1 {
		t.Fatalf("последняя страница: %+v %v", page, err)
	}
	page, total, err = st.ApplyLog(ctx, "pr1", 2, 6)
	if err != nil || total != 6 || page == nil || len(page) != 0 {
		t.Fatalf("за концом - пустой массив: %+v %v", page, err)
	}
	if _, _, err := st.ApplyLog(ctx, "nobody", 20, 0); err != ErrUnknownTarget {
		t.Fatalf("неизвестная машина: ждали ErrUnknownTarget, получили %v", err)
	}
}
