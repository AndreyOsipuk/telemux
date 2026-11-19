package store

// Сборка конфигов серверов (ADR 0006, этап B2): источник конфигов сервера
// (servers.config_source) и последнее сравнение кандидата telemux с текущей ревизией
// ресурса машины (server_render, migrations/0011_server_render.sql).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Источники конфигов сервера.
const (
	ConfigSourceBot     = "bot"
	ConfigSourceTelemux = "telemux"
)

var (
	// ErrRenderMismatch - переключить на telemux нельзя: последнее сравнение не совпало
	// по всем машинам сервера (или сравнения ещё не было).
	ErrRenderMismatch = errors.New("сборка telemux не совпадает с текущими конфигами машин")
	// ErrBadConfigSource - источник не bot и не telemux.
	ErrBadConfigSource = errors.New("источник конфигов - bot или telemux")
)

// RenderSummary - сводка последнего сравнения сервера для списка серверов.
type RenderSummary struct {
	Rows       int        `json:"rows"`        // ресурсов машин в сравнении
	Equal      bool       `json:"equal"`       // все собраны и совпали (и есть хоть один)
	Errors     int        `json:"errors"`      // не собрались или не назначены
	RenderedAt *time.Time `json:"rendered_at"` // когда сравнивали; null - ещё не было
}

// RenderRow - результат сборки ресурса одной машины сервера.
type RenderRow struct {
	ServerNo     int
	Target       string
	Resource     string
	CandidateSHA *string // nil - кандидат не собран
	Candidate    *string
	CurrentSHA   *string // nil - ревизий ресурса у машины ещё нет
	Equal        bool
	Error        string // "код: текст"; пусто - собран и назначен
	RenderedAt   time.Time
}

// RenderStatus - строка сравнения с текущим содержимым ресурса (для diff).
type RenderStatus struct {
	RenderRow
	Current *string // содержимое последней ревизии; nil - нет
}

// RenderMachine - машина для сборки: вид, адрес и назначенные ресурсы.
type RenderMachine struct {
	Code      string
	Kind      string
	Address   string
	Enabled   bool
	Resources []string
}

// RenderUser - включённый пользователь для [secrets] mtg.
type RenderUser struct {
	Key                     string
	Secret                  string
	ExpiresAt               *time.Time
	PreviousSecret          string
	PreviousSecretExpiresAt *time.Time
}

// RenderInput - всё, что нужно циклу сборки, одним снимком.
type RenderInput struct {
	Servers  []Server                 // все, кроме выключенных, с точками входа
	Machines map[string]RenderMachine // машины этих серверов
	Users    []RenderUser
	Current  map[string]string // target + "\x00" + resource → sha последней ревизии
}

// CurrentKey - ключ RenderInput.Current.
func CurrentKey(target, resource string) string { return target + "\x00" + resource }

// RenderInput читает серверы, их машины, пользователей и хеши текущих ревизий
// telemt.config и mtg.config - по одному запросу на вид данных, без N+1.
func (s *Store) RenderInput(ctx context.Context) (RenderInput, error) {
	in := RenderInput{Machines: map[string]RenderMachine{}, Current: map[string]string{}}
	// Серверов не больше 99 (номер 1..99): страница не нужна.
	rows, err := s.pool.Query(ctx, `SELECT `+serverCols+` FROM servers WHERE state <> 'disabled' ORDER BY no`)
	if err != nil {
		return in, fmt.Errorf("серверы для сборки: %w", err)
	}
	for rows.Next() {
		srv, err := scanServer(rows)
		if err != nil {
			rows.Close()
			return in, fmt.Errorf("серверы для сборки: %w", err)
		}
		in.Servers = append(in.Servers, srv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, fmt.Errorf("серверы для сборки: %w", err)
	}
	if err := fillEndpoints(ctx, s.pool, in.Servers); err != nil {
		return in, err
	}
	var codes []string
	for _, srv := range in.Servers {
		for _, c := range []*string{srv.RelayCode, srv.NodeCode} {
			if c != nil {
				codes = append(codes, *c)
			}
		}
	}
	if len(codes) > 0 {
		if err := s.renderMachines(ctx, codes, in); err != nil {
			return in, err
		}
	}
	urows, err := s.pool.Query(ctx,
		`SELECT username, secret, expiration_at, COALESCE(previous_secret, ''), previous_secret_expires_at
		   FROM users WHERE enabled ORDER BY username`)
	if err != nil {
		return in, fmt.Errorf("пользователи для сборки: %w", err)
	}
	defer urows.Close()
	for urows.Next() {
		var u RenderUser
		if err := urows.Scan(&u.Key, &u.Secret, &u.ExpiresAt, &u.PreviousSecret, &u.PreviousSecretExpiresAt); err != nil {
			return in, fmt.Errorf("пользователи для сборки: %w", err)
		}
		in.Users = append(in.Users, u)
	}
	if err := urows.Err(); err != nil {
		return in, fmt.Errorf("пользователи для сборки: %w", err)
	}
	return in, nil
}

// renderMachines - машины и хеши их последних ревизий конфигов движков.
func (s *Store) renderMachines(ctx context.Context, codes []string, in RenderInput) error {
	rows, err := s.pool.Query(ctx,
		`SELECT code, kind, enabled, resources, COALESCE(settings->>'address', '') FROM targets WHERE code = ANY($1)`, codes)
	if err != nil {
		return fmt.Errorf("машины для сборки: %w", err)
	}
	for rows.Next() {
		var m RenderMachine
		if err := rows.Scan(&m.Code, &m.Kind, &m.Enabled, &m.Resources, &m.Address); err != nil {
			rows.Close()
			return fmt.Errorf("машины для сборки: %w", err)
		}
		in.Machines[m.Code] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("машины для сборки: %w", err)
	}
	drows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (target, resource) target, resource, sha256 FROM desired_config
		  WHERE target = ANY($1) AND resource = ANY($2)
		  ORDER BY target, resource, revision DESC`, codes, []string{ResourceTelemtConfig, ResourceMtgConfig})
	if err != nil {
		return fmt.Errorf("текущие ревизии для сборки: %w", err)
	}
	defer drows.Close()
	for drows.Next() {
		var target, resource, sha string
		if err := drows.Scan(&target, &resource, &sha); err != nil {
			return fmt.Errorf("текущие ревизии для сборки: %w", err)
		}
		in.Current[CurrentKey(target, resource)] = sha
	}
	return drows.Err()
}

// SaveRender заменяет результаты сравнения целиком (одна транзакция): строки
// серверов и машин, которых больше нет в плане сборки, уходят.
func (s *Store) SaveRender(ctx context.Context, list []RenderRow) error {
	var (
		nos                      []int16
		targets, resources, errs []string
		candSHA, cand, curSHA    []*string
		equal                    []bool
		at                       []time.Time
	)
	for _, r := range list {
		nos = append(nos, int16(r.ServerNo))
		targets = append(targets, r.Target)
		resources = append(resources, r.Resource)
		candSHA = append(candSHA, r.CandidateSHA)
		cand = append(cand, r.Candidate)
		curSHA = append(curSHA, r.CurrentSHA)
		equal = append(equal, r.Equal)
		errs = append(errs, r.Error)
		at = append(at, r.RenderedAt)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM server_render`); err != nil {
			return fmt.Errorf("сравнение сборки: %w", err)
		}
		if len(list) == 0 {
			return nil
		}
		// Сервер или машину могли удалить между чтением и записью: такие строки
		// пропускаются (JOIN), а не валят всю запись нарушением FK.
		_, err := tx.Exec(ctx,
			`INSERT INTO server_render (server_no, target, resource, candidate_sha, candidate, current_sha, equal, error, rendered_at)
			 SELECT r.* FROM unnest($1::smallint[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[],
			                        $7::bool[], $8::text[], $9::timestamptz[])
			        AS r(server_no, target, resource, candidate_sha, candidate, current_sha, equal, error, rendered_at)
			   JOIN servers sv ON sv.no = r.server_no
			   JOIN targets t ON t.code = r.target
			 ON CONFLICT (server_no, target, resource) DO NOTHING`,
			nos, targets, resources, candSHA, cand, curSHA, equal, errs, at)
		if err != nil {
			return fmt.Errorf("сравнение сборки: %w", err)
		}
		return nil
	})
}

// ListRender - сравнение сервера по машинам с текущим содержимым ресурсов.
// У сервера не больше двух машин и двух ресурсов - страница не нужна.
// ErrServerNotFound - сервера нет.
func (s *Store) ListRender(ctx context.Context, no int) ([]RenderStatus, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM servers WHERE no = $1)`, no).Scan(&exists); err != nil {
		return nil, fmt.Errorf("сервер %d: %w", no, err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %d", ErrServerNotFound, no)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT r.server_no, r.target, r.resource, r.candidate_sha, r.candidate, r.current_sha, r.equal, r.error,
		        r.rendered_at, d.content
		   FROM server_render r
		   LEFT JOIN LATERAL (SELECT content FROM desired_config
		                       WHERE target = r.target AND resource = r.resource
		                       ORDER BY revision DESC LIMIT 1) d ON true
		  WHERE r.server_no = $1
		  ORDER BY r.target, r.resource`, no)
	if err != nil {
		return nil, fmt.Errorf("сравнение сервера %d: %w", no, err)
	}
	defer rows.Close()
	out := []RenderStatus{}
	for rows.Next() {
		var st RenderStatus
		var sno int16
		var current []byte
		if err := rows.Scan(&sno, &st.Target, &st.Resource, &st.CandidateSHA, &st.Candidate, &st.CurrentSHA,
			&st.Equal, &st.Error, &st.RenderedAt, &current); err != nil {
			return nil, fmt.Errorf("сравнение сервера %d: %w", no, err)
		}
		st.ServerNo = int(sno)
		if current != nil {
			c := string(current)
			st.Current = &c
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// SetConfigSource меняет источник конфигов сервера. На telemux без force - только
// если последнее сравнение есть и совпало по всем машинам (иначе ErrRenderMismatch);
// обратно на bot - всегда. ErrServerNotFound - сервера нет.
func (s *Store) SetConfigSource(ctx context.Context, no int, source string, force bool) (Server, error) {
	if source != ConfigSourceBot && source != ConfigSourceTelemux {
		return Server{}, fmt.Errorf("%w, получено %q", ErrBadConfigSource, source)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var cur string
		err := tx.QueryRow(ctx, `SELECT config_source FROM servers WHERE no = $1 FOR UPDATE`, no).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrServerNotFound, no)
		}
		if err != nil {
			return fmt.Errorf("сервер %d: %w", no, err)
		}
		if source == ConfigSourceTelemux && !force {
			var total, ok int
			if err := tx.QueryRow(ctx,
				`SELECT count(*), count(*) FILTER (WHERE equal AND error = '') FROM server_render WHERE server_no = $1`,
				no).Scan(&total, &ok); err != nil {
				return fmt.Errorf("сравнение сервера %d: %w", no, err)
			}
			if total == 0 {
				return fmt.Errorf("%w: у сервера %d сравнения ещё не было (нет машин для сборки или цикл не прошёл)", ErrRenderMismatch, no)
			}
			if ok < total {
				return fmt.Errorf("%w: сервер %d - совпало %d из %d (GET /api/v1/servers/%d/render)", ErrRenderMismatch, no, ok, total, no)
			}
		}
		if cur == source {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE servers SET config_source = $2, updated_at = now() WHERE no = $1`, no, source); err != nil {
			return fmt.Errorf("сервер %d: %w", no, err)
		}
		return nil
	})
	if err != nil {
		return Server{}, err
	}
	return s.GetServer(ctx, no)
}

// TelemuxServersOfMachine - не выключенные серверы с config_source=telemux, у которых
// машина - релей или машина за рубежом (какой ресурс чей, решает сборщик).
func (s *Store) TelemuxServersOfMachine(ctx context.Context, target string) ([]Server, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+serverCols+` FROM servers
		  WHERE config_source = 'telemux' AND state <> 'disabled' AND (relay_code = $1 OR node_code = $1)
		  ORDER BY no`, target)
	if err != nil {
		return nil, fmt.Errorf("серверы машины %q: %w", target, err)
	}
	defer rows.Close()
	out := []Server{}
	for rows.Next() {
		srv, err := scanServer(rows)
		if err != nil {
			return nil, fmt.Errorf("серверы машины %q: %w", target, err)
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// fillRenderSummary - сводка сравнения для страницы серверов одним запросом.
func fillRenderSummary(ctx context.Context, q querier, list []Server) error {
	nos := make([]int16, len(list))
	idx := make(map[int]int, len(list))
	for i := range list {
		nos[i] = int16(list[i].No)
		idx[list[i].No] = i
		list[i].Render = &RenderSummary{}
	}
	rows, err := q.Query(ctx,
		`SELECT server_no, count(*), count(*) FILTER (WHERE equal AND error = ''), count(*) FILTER (WHERE error <> ''),
		        max(rendered_at)
		   FROM server_render WHERE server_no = ANY($1) GROUP BY server_no`, nos)
	if err != nil {
		return fmt.Errorf("сводка сборки: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var no int16
		var total, ok, errs int
		var at *time.Time
		if err := rows.Scan(&no, &total, &ok, &errs, &at); err != nil {
			return fmt.Errorf("сводка сборки: %w", err)
		}
		if i, found := idx[int(no)]; found {
			list[i].Render = &RenderSummary{Rows: total, Equal: total > 0 && ok == total, Errors: errs, RenderedAt: at}
		}
	}
	return rows.Err()
}
