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
	SHA256   string    `json:"sha256"`
	Status   string    `json:"status"`
	Error    string    `json:"error,omitempty"`
	SeenAt   time.Time `json:"seen_at"`
	// DesiredRevision - последняя желаемая ревизия этого ресурса для машины;
	// nil - мастер ресурс машине не публиковал.
	DesiredRevision *int64 `json:"desired_revision"`
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
	// Из последнего runtime-отчёта агента (пусто - отчёта нет или продукта нет).
	TelemtVersion string     `json:"telemt_version,omitempty"`
	MtgVersion    string     `json:"mtg_version,omitempty"`
	RuntimeSeenAt *time.Time `json:"runtime_seen_at"`
}

// MachineFilter - фильтры списка машин; пустые поля не фильтруют.
type MachineFilter struct {
	Resource string // машине назначен ресурс
	Enabled  *bool
	Kind     string
}

// Machines - машины по коду, постранично, с последним отчётом агента,
// состоянием ресурсов и версиями из runtime-отчёта. total - всего машин с фильтром.
func (s *Store) Machines(ctx context.Context, f MachineFilter, limit, offset int) ([]Machine, int, error) {
	const where = `WHERE ($1 = '' OR $1 = ANY(t.resources)) AND ($2::boolean IS NULL OR t.enabled = $2)
	                 AND ($3 = '' OR t.kind = $3)`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM targets t `+where,
		f.Resource, f.Enabled, f.Kind).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count targets: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT t.code, t.kind, COALESCE(t.relay_code, ''), t.enabled, t.resources,
		        COALESCE(a.agent_version, ''), COALESCE(a.via, ''), a.seen_at, COALESCE(a.checks, '[]'),
		        COALESCE(r.telemt->>'version', ''), COALESCE(r.mtg->>'version', ''), r.seen_at
		   FROM targets t
		   LEFT JOIN agent_state a ON a.target = t.code
		   LEFT JOIN agent_runtime r ON r.target = t.code
		  `+where+`
		  ORDER BY t.code
		  LIMIT $4 OFFSET $5`, f.Resource, f.Enabled, f.Kind, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("machines: %w", err)
	}
	var list []Machine
	idx := map[string]int{}
	for rows.Next() {
		var m Machine
		if err := rows.Scan(&m.Code, &m.Kind, &m.RelayCode, &m.Enabled, &m.Resources,
			&m.AgentVersion, &m.Via, &m.SeenAt, &m.Checks,
			&m.TelemtVersion, &m.MtgVersion, &m.RuntimeSeenAt); err != nil {
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
		`SELECT o.target, o.resource, COALESCE(o.revision, 0), COALESCE(o.sha256, ''), o.status,
		        COALESCE(o.error, ''), o.seen_at,
		        (SELECT max(d.revision) FROM desired_config d WHERE d.target = o.target AND d.resource = o.resource)
		   FROM observed_state o WHERE o.target = ANY($1) ORDER BY o.target, o.resource`, codes)
	if err != nil {
		return nil, 0, fmt.Errorf("machines observed: %w", err)
	}
	defer obs.Close()
	for obs.Next() {
		var code string
		var r MachineResource
		if err := obs.Scan(&code, &r.Resource, &r.Revision, &r.SHA256, &r.Status, &r.Error, &r.SeenAt, &r.DesiredRevision); err != nil {
			return nil, 0, fmt.Errorf("scan observed: %w", err)
		}
		list[idx[code]].Observed = append(list[idx[code]].Observed, r)
	}
	return list, total, obs.Err()
}
