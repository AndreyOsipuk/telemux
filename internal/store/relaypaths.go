package store

import (
	"context"
	"fmt"
)

const (
	SettingRelayPathsManaged = "relay_paths_managed"
	MaxRelayMasterPaths      = 16
)

// RelayMasterTargets возвращает снимок включённых выходов релея. managed=true
// без выходов отличается от отсутствующего или отключённого управления списком.
func (s *Store) RelayMasterTargets(ctx context.Context, relay string) (managed bool, exits []Target, err error) {
	rows, err := s.pool.Query(ctx, `SELECT e.code, COALESCE(e.settings->>'address', ''),
		       COALESCE(e.settings->>'bridge_socks', '') FROM targets r
		LEFT JOIN LATERAL (
			SELECT code, settings FROM targets
			WHERE kind = 'exit' AND enabled AND relay_code = r.code
			ORDER BY code LIMIT $2
		) e ON true
		WHERE r.code = $1 AND r.kind = 'relay' AND r.enabled
		  AND r.settings->>'relay_paths_managed' = '1'
		ORDER BY e.code`, relay, MaxRelayMasterPaths+1)
	if err != nil {
		return false, nil, fmt.Errorf("пути релея %q: %w", relay, err)
	}
	defer rows.Close()
	for rows.Next() {
		managed = true
		var code *string
		var address, socks string
		if err := rows.Scan(&code, &address, &socks); err != nil {
			return false, nil, fmt.Errorf("чтение пути релея %q: %w", relay, err)
		}
		if code != nil {
			exits = append(exits, Target{Code: *code, Settings: map[string]string{SettingAddress: address, SettingBridgeSocks: socks}})
		}
	}
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("чтение путей релея %q: %w", relay, err)
	}
	if len(exits) > MaxRelayMasterPaths {
		return false, nil, fmt.Errorf("у релея %q больше %d путей", relay, MaxRelayMasterPaths)
	}
	return managed, exits, nil
}
