package store

import (
	"context"
	"fmt"

	"github.com/AndreyOsipuk/telemux/internal/backend"
)

// ListMtgNodes возвращает активные ноды с backend='mtg-multi' в виде backend.Node
// (готовых для backend.NodeBackend.Sync). Ноды telemt сюда не попадают — у них
// свой путь синхронизации. Дефолты: ssh_user=root, ssh_port=22, config_path и
// reload_cmd — стандартные пути mtg-multi, если в БД пусто.
func (s *Store) ListMtgNodes(ctx context.Context) ([]backend.Node, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT code, address,
		       COALESCE(ssh_user, 'root'),
		       COALESCE(ssh_port, 22),
		       COALESCE(mtg_bind_to, ''),
		       COALESCE(mtg_api_bind_to, ''),
		       COALESCE(mtg_fronting_domain, ''),
		       COALESCE(mtg_prefer_ip, 'only-ipv4'),
		       COALESCE(mtg_max_conns, 0),
		       COALESCE(mtg_config_path, '/etc/mtg-multi/config.toml'),
		       COALESCE(mtg_reload_cmd, 'systemctl restart mtg-multi')
		FROM nodes
		WHERE backend = 'mtg-multi' AND enabled = TRUE
		ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("ListMtgNodes: %w", err)
	}
	defer rows.Close()

	var out []backend.Node
	for rows.Next() {
		var n backend.Node
		var mtg backend.MtgNodeCfg
		if err := rows.Scan(
			&n.Code, &n.Address, &n.SSHUser, &n.SSHPort,
			&mtg.BindTo, &mtg.APIBindTo, &mtg.FrontingDomain, &mtg.PreferIP,
			&mtg.MaxConns, &mtg.ConfigPath, &mtg.ReloadCmd,
		); err != nil {
			return nil, fmt.Errorf("ListMtgNodes scan: %w", err)
		}
		n.Backend = "mtg-multi"
		n.Mtg = &mtg
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListMtgNodes rows: %w", err)
	}
	return out, nil
}
