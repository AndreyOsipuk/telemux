//go:build integration

package store

import (
	"context"
	"testing"
)

// Машина без ресурсов (выход без telemt и mtg) отчитывается о себе: мастер
// хранит, когда агент был жив, версию, мост и проверки. Смена результата
// проверки попадает в журнал, повторы - нет.
func TestIntegration_RecordObserved_MachineOnly(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, `TRUNCATE agent_state`); err != nil {
		t.Fatal(err)
	}
	seedTargets(t, st, Target{Code: "vega", Kind: "exit", RelayCode: "pr1", Enabled: true})

	rep := ObservedReport{Target: "vega", AgentVersion: "v1", Checks: []ObservedCheck{{Name: "bridge", OK: true}}}
	for i := 0; i < 2; i++ {
		if err := st.RecordObserved(ctx, rep); err != nil {
			t.Fatal(err)
		}
	}
	rep.Checks[0] = ObservedCheck{Name: "bridge", OK: false, Error: "connection refused"}
	if err := st.RecordObserved(ctx, rep); err != nil {
		t.Fatal(err)
	}

	var version string
	var checks []ObservedCheck
	if err := st.pool.QueryRow(ctx, `SELECT agent_version, checks FROM agent_state WHERE target = 'vega'`).
		Scan(&version, &checks); err != nil {
		t.Fatal(err)
	}
	if version != "v1" || len(checks) != 1 || checks[0].OK {
		t.Fatalf("agent_state не обновлён: %s %+v", version, checks)
	}
	var rows []string
	r, err := st.pool.Query(ctx, `SELECT resource || ':' || status FROM apply_log WHERE target = 'vega' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var s string
		_ = r.Scan(&s)
		rows = append(rows, s)
	}
	r.Close()
	if len(rows) != 2 || rows[0] != "check:bridge:ok" || rows[1] != "check:bridge:failed" {
		t.Fatalf("в журнале ждали смену проверки ok → failed, получили %v", rows)
	}
}

// Список машин: все машины, включая те, где агента ещё нет, с ресурсами и
// последним отчётом; постранично.
func TestIntegration_Machines(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, `TRUNCATE agent_state`); err != nil {
		t.Fatal(err)
	}
	seedTargets(t, st,
		telemtTarget("pr1", "relay"),
		Target{Code: "vega", Kind: "exit", RelayCode: "pr1", Enabled: true},
		Target{Code: "ps3", Kind: "exit", RelayCode: "pr3", Enabled: true},
	)
	if err := st.RecordObserved(ctx, ObservedReport{Target: "pr1", AgentVersion: "v1", Resources: []ObservedResource{
		{Resource: ResourceTelemtUsers, Revision: 3, SHA256: "s", Status: "applied"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObserved(ctx, ObservedReport{Target: "vega", AgentVersion: "v1",
		Checks: []ObservedCheck{{Name: "bridge", OK: true}}}); err != nil {
		t.Fatal(err)
	}

	list, total, err := st.Machines(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(list) != 3 {
		t.Fatalf("ждали 3 машины, получили total=%d len=%d", total, len(list))
	}
	by := map[string]Machine{}
	for _, m := range list {
		by[m.Code] = m
	}
	if m := by["pr1"]; m.SeenAt == nil || len(m.Observed) != 1 || m.Observed[0].Status != "applied" || m.Observed[0].Revision != 3 {
		t.Fatalf("pr1: %+v", m)
	}
	if m := by["vega"]; m.SeenAt == nil || len(m.Resources) != 0 || len(m.Checks) != 1 || !m.Checks[0].OK || m.RelayCode != "pr1" {
		t.Fatalf("vega: %+v", m)
	}
	if m := by["ps3"]; m.SeenAt != nil || m.AgentVersion != "" {
		t.Fatalf("ps3 без агента, а отчёт есть: %+v", m)
	}

	page, total, err := st.Machines(ctx, 2, 2)
	if err != nil || total != 3 || len(page) != 1 {
		t.Fatalf("вторая страница: total=%d len=%d err=%v", total, len(page), err)
	}
}
