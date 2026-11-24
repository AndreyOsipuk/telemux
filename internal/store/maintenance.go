package store

import "time"

const SettingMaintenanceUntil = "maintenance_until"
const MaxMaintenanceDuration = 15 * time.Minute

// maintenanceTime читает настройку без SQL-приведения: неверный срок в
// старых данных не должен выключать проверку всего флота.
func maintenanceTime(raw string) *time.Time {
	until, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	until = until.UTC()
	return &until
}

func (m Machine) inMaintenance(now time.Time) bool {
	return m.MaintenanceUntil != nil && m.MaintenanceUntil.After(now)
}

// checkMaintenanceTransition вызывается под блокировкой строки машины.
func checkMaintenanceTransition(current string, patch TargetPatch, now time.Time) error {
	next, changed := patch.Settings[SettingMaintenanceUntil]
	if !changed {
		return nil
	}
	currentUntil := maintenanceTime(current)
	active := currentUntil != nil && currentUntil.After(now)
	if expected := patch.ExpectedMaintenanceUntil; expected != nil {
		if *expected == "" {
			if active {
				return ErrMaintenanceConflict
			}
		} else if until := maintenanceTime(*expected); until == nil || currentUntil == nil || !currentUntil.Equal(*until) {
			return ErrMaintenanceConflict
		}
	} else if active && next != "" {
		if until := maintenanceTime(next); until == nil || !currentUntil.Equal(*until) {
			return ErrMaintenanceConflict
		}
	}
	return nil
}
