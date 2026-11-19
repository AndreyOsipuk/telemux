package main

import (
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

func TestFormatMachine(t *testing.T) {
	now := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	seen := now.Add(-90 * time.Second)
	vega := formatMachine(store.Machine{Code: "vega", Kind: "exit", Enabled: true, Resources: []string{}, SeenAt: &seen,
		Checks: []store.ObservedCheck{{Name: "bridge", OK: false, Error: "refused"}}}, now)
	for _, want := range []string{"vega", "агент 1m30s назад", "bridge НЕ ok (refused)", "без ресурсов"} {
		if !strings.Contains(vega, want) {
			t.Fatalf("в %q нет %q", vega, want)
		}
	}
	ps3 := formatMachine(store.Machine{Code: "ps3", Kind: "exit", Enabled: true, Resources: []string{}}, now)
	if !strings.Contains(ps3, "агента нет") {
		t.Fatalf("машина без агента: %q", ps3)
	}
	pr1 := formatMachine(store.Machine{Code: "pr1", Kind: "relay", Enabled: true, Resources: []string{"telemt.users"}, SeenAt: &seen,
		Observed: []store.MachineResource{{Resource: "telemt.users", Revision: 12, Status: "applied"}}}, now)
	if !strings.Contains(pr1, "telemt.users r12 applied") || strings.Contains(pr1, "без ресурсов") {
		t.Fatalf("релей: %q", pr1)
	}
}

// Порог устаревания runtime-снимка: три периода отчёта или явная настройка.
func TestRuntimeStaleAfterEnv(t *testing.T) {
	t.Setenv("TELEMUX_RUNTIME_EVERY", "")
	t.Setenv("TELEMUX_RUNTIME_STALE_AFTER", "")
	if d, err := runtimeStaleAfter(); err != nil || d != 3*time.Minute {
		t.Fatalf("по умолчанию 3 × 60s: %v %v", d, err)
	}
	t.Setenv("TELEMUX_RUNTIME_EVERY", "30s")
	if d, _ := runtimeStaleAfter(); d != 90*time.Second {
		t.Fatalf("три периода по 30s: %v", d)
	}
	t.Setenv("TELEMUX_RUNTIME_STALE_AFTER", "10m")
	if d, _ := runtimeStaleAfter(); d != 10*time.Minute {
		t.Fatalf("явная настройка: %v", d)
	}
	t.Setenv("TELEMUX_RUNTIME_STALE_AFTER", "ноль")
	if _, err := runtimeStaleAfter(); err == nil {
		t.Fatal("мусор в настройке - ошибка")
	}
}
