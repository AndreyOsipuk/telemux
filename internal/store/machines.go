package store

import (
	"context"
	"fmt"
	"time"
)

// MachineResource - что машина последний раз сообщила по одному ресурсу.
type MachineResource struct {
	Resource string    `json:"resource"`
	Revision int64     `json:"revision"`
	Status   string    `json:"status"`
	Error    string    `json:"error,omitempty"`
	SeenAt   time.Time `json:"seen_at"`
}

// Machine - машина флота и последний отчёт её агента. SeenAt == nil - агент
// ещё ни разу не отчитывался.
type Machine struct {
	Code         string            `json:"code"`
	Kind         string            `json:"kind"`
	RelayCode    string            `json:"relay_code,omitempty"`
	Enabled      bool              `json:"enabled"`
	Resources    []string          `json:"resources"`
	AgentVersion string            `json:"agent_version,omitempty"`
	Via          string            `json:"via,omitempty"`
	SeenAt       *time.Time        `json:"seen_at"`
	Checks       []ObservedCheck   `json:"checks"`
	Observed     []MachineResource `json:"observed"`
}

// Machines - машины по коду, постранично, с последним отчётом агента и
// состоянием ресурсов. total - всего машин.
func (s *Store) Machines(ctx context.Context, limit, offset int) ([]Machine, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM targets`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count targets: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT t.code, t.kind, COALESCE(t.relay_code, ''), t.enabled, t.resources,
		        COALESCE(a.agent_version, ''), COALESCE(a.via, ''), a.seen_at, COALESCE(a.checks, '[]')
		   FROM targets t
		   LEFT JOIN agent_state a ON a.target = t.code
		  ORDER BY t.code
		  LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("machines: %w", err)
	}
	var list []Machine
	idx := map[string]int{}
	for rows.Next() {
		var m Machine
		if err := rows.Scan(&m.Code, &m.Kind, &m.RelayCode, &m.Enabled, &m.Resources,
			&m.AgentVersion, &m.Via, &m.SeenAt, &m.Checks); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("scan machine: %w", err)
		}
		if m.Resources == nil {
			m.Resources = []string{}
		}
		m.Observed = []MachineResource{}
		idx[m.Code] = len(list)
		list = append(list, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("machines: %w", err)
	}
	if len(list) == 0 {
		return []Machine{}, total, nil
	}
	codes := make([]string, 0, len(list))
	for _, m := range list {
		codes = append(codes, m.Code)
	}
	obs, err := s.pool.Query(ctx,
		`SELECT target, resource, COALESCE(revision, 0), status, COALESCE(error, ''), seen_at
		   FROM observed_state WHERE target = ANY($1) ORDER BY target, resource`, codes)
	if err != nil {
		return nil, 0, fmt.Errorf("machines observed: %w", err)
	}
	defer obs.Close()
	for obs.Next() {
		var code string
		var r MachineResource
		if err := obs.Scan(&code, &r.Resource, &r.Revision, &r.Status, &r.Error, &r.SeenAt); err != nil {
			return nil, 0, fmt.Errorf("scan observed: %w", err)
		}
		list[idx[code]].Observed = append(list[idx[code]].Observed, r)
	}
	return list, total, obs.Err()
}
