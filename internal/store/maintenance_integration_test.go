//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestIntegration_MaintenancePausesOnlySelectedPaths(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	pairsAlive(t, st)
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("первый проход по живым парам: %v", got)
	}

	maintenance := func(code string, until time.Time) {
		t.Helper()
		_, err := st.pool.Exec(context.Background(), `UPDATE targets SET settings=settings || jsonb_build_object('maintenance_until', $2::text) WHERE code=$1`, code, until.Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	until := now.Add(5 * time.Minute)
	maintenance("ps1", until)
	// Мосты обесточены, но запланированы работы только на ps1.
	setExitBridge(t, st, "ps1", 0, "pr1", 5*time.Minute)
	setExitBridge(t, st, "ps2", 0, "pr2", 5*time.Minute)
	if got := detect(t, st); !slices.Equal(got, []string{"pr2:relay_silent:ps2"}) {
		t.Fatalf("обслуживание ps1 не скрыло только её путь: %v", got)
	}
	ps1 := exitIn(t, pairsByRelay(t, st)["pr1"], "ps1")
	raw, err := json.Marshal(ps1)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Until *time.Time `json:"maintenance_until"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Until == nil || !payload.Until.Equal(until) {
		t.Fatalf("в API нет нужного срока: %v, %v", payload.Until, err)
	}

	// Пара уже в аварии. Плановый перезапуск pr2 не должен породить
	// восстановление, даже если свежий мост появился до закрытия окна.
	maintenance("pr2", until)
	setExitBridge(t, st, "ps2", 0, "pr2", 0)
	if got := detect(t, st); len(got) != 0 {
		t.Fatalf("ложное восстановление внутри окна: %v", got)
	}

	maintenance("ps1", now.Add(-time.Minute))
	maintenance("pr2", now.Add(-time.Minute))
	if got := detect(t, st); !slices.Equal(got, []string{"pr1:relay_silent:ps1", "pr2:relay_ok:ps2"}) {
		t.Fatalf("первый проход после истечения двух окон: %v", got)
	}
}

func TestIntegration_MaintenanceWindowOwnership(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	ctx := context.Background()
	const key = SettingMaintenanceUntil
	now := time.Now().UTC()
	first := now.Add(2 * time.Minute).Format(time.RFC3339Nano)
	second := now.Add(3 * time.Minute).Format(time.RFC3339Nano)
	noWindow := ""
	save := func(until string, expected *string) error {
		t.Helper()
		_, err := st.UpdateTarget(ctx, "ps1", TargetPatch{
			Settings:                 map[string]string{key: until},
			ExpectedMaintenanceUntil: expected,
		}, nil)
		return err
	}
	if err := save(first, &noWindow); err != nil {
		t.Fatal(err)
	}
	if err := save(first, nil); err != nil {
		t.Fatalf("повтор открытия того же окна: %v", err)
	}
	if err := save(second, nil); !errors.Is(err, ErrMaintenanceConflict) {
		t.Fatalf("второй оператор перезаписал действующее окно: %v", err)
	}
	if err := save("", &second); !errors.Is(err, ErrMaintenanceConflict) {
		t.Fatalf("чужое окно удалилось: %v", err)
	}
	if err := save("", &first); err != nil {
		t.Fatal(err)
	}
	if err := save(second, &noWindow); err != nil {
		t.Fatal(err)
	}
	if err := save("", &first); !errors.Is(err, ErrMaintenanceConflict) {
		t.Fatalf("просроченная очистка удалила следующее окно: %v", err)
	}
	target, err := st.TargetByCode(ctx, "ps1")
	if err != nil || target.Settings[key] != second {
		t.Fatalf("новое окно не сохранено: %+v, %v", target.Settings, err)
	}
}

func TestIntegration_MaintenanceRetainsLivenessAndDoesNotMuteApplyErrors(t *testing.T) {
	st := setupOps(t)
	defer st.Close()
	pairsAlive(t, st)
	detect(t, st)

	ctx := context.Background()
	if _, err := st.UpdateTarget(ctx, "ps1", TargetPatch{
		Settings: map[string]string{SettingMaintenanceUntil: time.Now().Add(time.Minute).Format(time.RFC3339Nano)},
	}, nil); err != nil {
		t.Fatal(err)
	}
	setSeen(t, st, "ps1", SilentAfter+time.Minute)
	setExitBridge(t, st, "ps1", SilentAfter+time.Minute, "pr1", 0)
	// Уже применявшая ресурс машина может отчитаться о настоящей ошибке.
	setFailed(t, st, "ps1", ResourceTelemtUsers, 101)
	if got := detect(t, st); !slices.Equal(got, []string{"ps1:apply_failed"}) {
		t.Fatalf("плановое окно могло скрыть реальную ошибку или дать ложную тревогу: %v", got)
	}
	if _, err := st.UpdateTarget(ctx, "ps1", TargetPatch{
		Settings: map[string]string{SettingMaintenanceUntil: ""},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if got := detect(t, st); !slices.Equal(got, []string{"ps1:down"}) {
		t.Fatalf("по завершении окна не обнаружилось реальное отсутствие агента: %v", got)
	}
}
