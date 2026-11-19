package store

import (
	"testing"
	"time"
)

// Сводный статус машины в паре: правила и их приоритет из спеки.
func TestMachineStatus(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Minute)
	stale := now.Add(-SilentAfter - time.Second)
	desired := []DesiredRevision{{Resource: ResourceTelemtUsers, Revision: 5}}
	base := func() Machine {
		return Machine{Code: "ps1", Kind: "exit", Enabled: true, Resources: []string{ResourceTelemtUsers}, SeenAt: &fresh,
			Observed: []MachineResource{{Resource: ResourceTelemtUsers, Revision: 5, Status: "applied"}}}
	}
	cases := []struct {
		name    string
		mut     func(m *Machine)
		desired []DesiredRevision
		want    string
	}{
		{"всё применено", func(*Machine) {}, desired, StatusOK},
		{"выключена важнее всего", func(m *Machine) { m.Enabled = false; m.SeenAt = nil }, desired, StatusDisabled},
		{"агента нет", func(m *Machine) { m.SeenAt = nil; m.Observed = nil }, desired, StatusNoAgent},
		{"молчит дольше 10 минут", func(m *Machine) { m.SeenAt = &stale }, desired, StatusSilent},
		{"ресурс failed", func(m *Machine) { m.Observed[0].Status = "failed" }, desired, StatusFailed},
		{"проверка не пройдена", func(m *Machine) { m.Checks = []ObservedCheck{{Name: "bridge", OK: false}} }, desired, StatusFailed},
		{"failed важнее behind", func(m *Machine) { m.Observed[0].Status = "failed"; m.Observed[0].Revision = 1 }, desired, StatusFailed},
		{"отстаёт", func(m *Machine) { m.Observed[0].Revision = 4 }, desired, StatusBehind},
		{"ресурс ни разу не применён", func(m *Machine) { m.Observed = nil }, desired, StatusBehind},
		{"машина без ресурсов", func(m *Machine) { m.Resources = []string{}; m.Observed = nil }, nil, StatusOK},
		{"failed по снятому ресурсу не в счёт", func(m *Machine) {
			m.Observed = append(m.Observed, MachineResource{Resource: ResourceMtgConfig, Status: "failed"})
		}, desired, StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := base()
			c.mut(&m)
			if got := MachineStatus(m, c.desired, now); got != c.want {
				t.Fatalf("ждали %s, получили %s", c.want, got)
			}
		})
	}
}
