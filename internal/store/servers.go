package store

// Серверы - описание того, что видит клиент (ADR 0005, этап B1): «Сервер N»
// с движком, машинами, лимитами, настройками движка и точками входа.
// Пока справочник: ни на что не влияет (migrations/0010_servers.sql).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/AndreyOsipuk/telemux/internal/servercfg"
)

// ServersTables - таблицы миграции 0010; без них ручки серверов не монтируются.
var ServersTables = []string{"servers", "server_endpoints"}

// CheckServersSchema - применена ли миграция 0010 (серверы и точки входа).
func (s *Store) CheckServersSchema(ctx context.Context) error {
	return s.checkTables(ctx, ServersTables, "migrations/0010_servers.sql")
}

// Пределы описания сервера.
const (
	MaxServersImport   = 100 // серверов за один импорт
	MaxServerEndpoints = 50  // точек входа у сервера
	MaxExtraTLSDomains = 200 // дополнительных SNI
	maxServerNote      = 1000
	maxServerName      = 64
	maxServerEmoji     = 16
	maxEndpointLabel   = 64
)

// Состояния сервера.
var serverStates = map[string]bool{"active": true, "maintenance": true, "test": true, "disabled": true}

// Виды ссылок точек входа.
var endpointProtocols = map[string]bool{"ee": true, "dd": true, "web": true}

var countryRe = regexp.MustCompile(`^[a-z]{2}$`)

var (
	// ErrServerNotFound - сервера с таким номером нет.
	ErrServerNotFound = errors.New("сервера нет")
	// ErrServerExists - номер занят.
	ErrServerExists = errors.New("номер сервера занят")
	// ErrServerNotDisabled - удалить можно только выключенный сервер.
	ErrServerNotDisabled = errors.New("удалить можно только выключенный сервер (state=disabled)")
	// ErrEngineImmutable - движок после создания не меняется.
	ErrEngineImmutable = errors.New("движок сервера после создания не меняется")
	// ErrUnknownMachine - машины нет или она не того вида.
	ErrUnknownMachine = errors.New("неизвестная машина")
	// ErrBatchTooLarge - слишком много серверов за один импорт.
	ErrBatchTooLarge = errors.New("слишком много серверов за раз")
	// ErrBadServer - недопустимое описание сервера.
	ErrBadServer = errors.New("недопустимое описание сервера")
	// ErrBadEndpoint - недопустимая точка входа.
	ErrBadEndpoint = errors.New("недопустимая точка входа")
)

// Endpoint - точка входа: одна ссылка клиента.
type Endpoint struct {
	ID        int64  `json:"id"`
	Protocol  string `json:"protocol"`   // ee, dd, web
	Host      string `json:"host"`       // домен или IP в ссылке
	Port      int    `json:"port"`       // для web не используется
	TLSDomain string `json:"tls_domain"` // домен в ee-секрете; для ee обязателен
	ViaRelay  bool   `json:"via_relay"`
	Label     string `json:"label"`
	SortOrder int    `json:"sort_order"`
	Enabled   bool   `json:"enabled"`
	Note      string `json:"note"`
}

// LiveMachine - живые цифры машины сервера по последнему runtime-снимку,
// продукт = движок сервера. SeenAt nil - агент ещё не отчитывался.
type LiveMachine struct {
	Code        string     `json:"code"`
	SeenAt      *time.Time `json:"seen_at"`
	Stale       bool       `json:"stale"` // заполняет API по порогу устаревания
	APIOK       bool       `json:"api_ok"`
	Connections int64      `json:"connections"`
	UsersOnline int        `json:"users_online"`
}

// ServerLive - живые цифры по машинам сервера; nil - машина не задана.
type ServerLive struct {
	Relay *LiveMachine `json:"relay"`
	Node  *LiveMachine `json:"node"`
}

// Server - сервер со списком точек входа.
type Server struct {
	No               int               `json:"no"`
	Name             string            `json:"name"`
	Emoji            string            `json:"emoji"`
	Country          string            `json:"country"`
	Engine           string            `json:"engine"`
	RelayCode        *string           `json:"relay_code"`
	NodeCode         *string           `json:"node_code"`
	State            string            `json:"state"`
	RelayOnly        bool              `json:"relay_only"`
	NodeRetired      bool              `json:"node_retired"`
	TLSDomain        string            `json:"tls_domain"`
	ExtraTLSDomains  []string          `json:"extra_tls_domains"`
	UserMaxTCPConns  *int              `json:"user_max_tcp_conns"`
	UserMaxUniqueIPs *int              `json:"user_max_unique_ips"`
	Settings         map[string]string `json:"settings"`
	Note             string            `json:"note"`
	Endpoints        []Endpoint        `json:"endpoints"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	Live             *ServerLive       `json:"live,omitempty"`
}

// EndpointWrite - точка входа в теле записи: пропущенные поля - по умолчанию
// (port 443, via_relay true, sort_order 100, enabled true).
type EndpointWrite struct {
	Protocol  string `json:"protocol"`
	Host      string `json:"host"`
	Port      *int   `json:"port"`
	TLSDomain string `json:"tls_domain"`
	ViaRelay  *bool  `json:"via_relay"`
	Label     string `json:"label"`
	SortOrder *int   `json:"sort_order"`
	Enabled   *bool  `json:"enabled"`
	Note      string `json:"note"`
}

// ServerWrite - тело записи сервера (POST, PUT, импорт). state пусто - active.
type ServerWrite struct {
	No               int               `json:"no"`
	Name             string            `json:"name"`
	Emoji            string            `json:"emoji"`
	Country          string            `json:"country"`
	Engine           string            `json:"engine"`
	RelayCode        *string           `json:"relay_code"`
	NodeCode         *string           `json:"node_code"`
	State            string            `json:"state"`
	RelayOnly        bool              `json:"relay_only"`
	NodeRetired      bool              `json:"node_retired"`
	TLSDomain        string            `json:"tls_domain"`
	ExtraTLSDomains  []string          `json:"extra_tls_domains"`
	UserMaxTCPConns  *int              `json:"user_max_tcp_conns"`
	UserMaxUniqueIPs *int              `json:"user_max_unique_ips"`
	Settings         map[string]string `json:"settings"`
	Note             string            `json:"note"`
	Endpoints        []EndpointWrite   `json:"endpoints"`
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// codePtr - код машины без пробелов; пусто - nil.
func codePtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return nil
	}
	return &v
}

// Server проверяет тело и приводит его к каноническому виду (значения по
// умолчанию, регистр доменов, настройки по каталогу). Ошибки - ErrBadServer,
// ErrBadEndpoint, servercfg.ErrUnknownSetting/ErrBadSetting с именем поля.
func (w ServerWrite) Server() (Server, error) {
	srv := Server{
		No: w.No, Name: strings.TrimSpace(w.Name), Emoji: strings.TrimSpace(w.Emoji),
		Country: strings.ToLower(strings.TrimSpace(w.Country)), Engine: w.Engine,
		RelayCode: codePtr(w.RelayCode), NodeCode: codePtr(w.NodeCode), State: w.State,
		RelayOnly: w.RelayOnly, NodeRetired: w.NodeRetired,
		TLSDomain:       strings.ToLower(strings.TrimSpace(w.TLSDomain)),
		UserMaxTCPConns: w.UserMaxTCPConns, UserMaxUniqueIPs: w.UserMaxUniqueIPs,
		Note: w.Note, ExtraTLSDomains: []string{}, Endpoints: []Endpoint{},
	}
	if srv.State == "" {
		srv.State = "active"
	}
	bad := func(format string, a ...any) (Server, error) {
		return Server{}, fmt.Errorf("%w: "+format, append([]any{ErrBadServer}, a...)...)
	}
	switch {
	case srv.No < 1 || srv.No > 99:
		return bad("no - номер от 1 до 99, получено %d", srv.No)
	case srv.Name == "":
		return bad("name - пустое имя")
	case utf8.RuneCountInString(srv.Name) > maxServerName:
		return bad("name длиннее %d символов", maxServerName)
	case utf8.RuneCountInString(srv.Emoji) > maxServerEmoji:
		return bad("emoji длиннее %d символов", maxServerEmoji)
	case srv.Country != "" && !countryRe.MatchString(srv.Country):
		return bad("country %q - две латинские буквы или пусто", w.Country)
	case !servercfg.ValidEngine(srv.Engine):
		return bad("engine %q - допустимы telemt, mtg", srv.Engine)
	case !serverStates[srv.State]:
		return bad("state %q - допустимы active, maintenance, test, disabled", srv.State)
	case srv.TLSDomain != "" && !servercfg.ValidDomain(srv.TLSDomain):
		return bad("tls_domain %q - нужно имя хоста", w.TLSDomain)
	case srv.UserMaxTCPConns != nil && *srv.UserMaxTCPConns < 1:
		return bad("user_max_tcp_conns - больше 0 или null")
	case srv.UserMaxUniqueIPs != nil && *srv.UserMaxUniqueIPs < 1:
		return bad("user_max_unique_ips - больше 0 или null")
	case utf8.RuneCountInString(srv.Note) > maxServerNote:
		return bad("note длиннее %d символов", maxServerNote)
	case len(w.ExtraTLSDomains) > MaxExtraTLSDomains:
		return bad("extra_tls_domains: %d доменов, не больше %d", len(w.ExtraTLSDomains), MaxExtraTLSDomains)
	case len(w.Endpoints) > MaxServerEndpoints:
		return bad("endpoints: %d точек входа, не больше %d", len(w.Endpoints), MaxServerEndpoints)
	}
	for _, raw := range w.ExtraTLSDomains {
		d := strings.ToLower(strings.TrimSpace(raw))
		if !servercfg.ValidDomain(d) {
			return bad("extra_tls_domains: %q - нужно имя хоста", raw)
		}
		if !slices.Contains(srv.ExtraTLSDomains, d) {
			srv.ExtraTLSDomains = append(srv.ExtraTLSDomains, d)
		}
	}
	settings, err := servercfg.Normalize(srv.Engine, w.Settings)
	if err != nil {
		return Server{}, err
	}
	srv.Settings = settings
	seen := map[string]bool{}
	for i, ew := range w.Endpoints {
		ep, err := ew.endpoint()
		if err != nil {
			return Server{}, fmt.Errorf("%w: endpoints[%d]: %v", ErrBadEndpoint, i, err)
		}
		key := fmt.Sprintf("%s|%s|%d", ep.Protocol, ep.Host, ep.Port)
		if seen[key] {
			return Server{}, fmt.Errorf("%w: endpoints[%d]: %s %s:%d повторяется", ErrBadEndpoint, i, ep.Protocol, ep.Host, ep.Port)
		}
		seen[key] = true
		srv.Endpoints = append(srv.Endpoints, ep)
	}
	return srv, nil
}

// endpoint проверяет точку входа и подставляет значения по умолчанию.
func (w EndpointWrite) endpoint() (Endpoint, error) {
	ep := Endpoint{
		Protocol: w.Protocol, Host: strings.ToLower(strings.TrimSpace(w.Host)), Port: intOr(w.Port, 443),
		TLSDomain: strings.ToLower(strings.TrimSpace(w.TLSDomain)), ViaRelay: boolOr(w.ViaRelay, true),
		Label: strings.TrimSpace(w.Label), SortOrder: intOr(w.SortOrder, 100), Enabled: boolOr(w.Enabled, true), Note: w.Note,
	}
	switch {
	case !endpointProtocols[ep.Protocol]:
		return ep, fmt.Errorf("protocol %q - допустимы ee, dd, web", w.Protocol)
	case !servercfg.ValidHost(ep.Host):
		return ep, fmt.Errorf("host %q - нужен домен или IP", w.Host)
	case ep.Port < 1 || ep.Port > 65535:
		return ep, fmt.Errorf("port %d - от 1 до 65535", ep.Port)
	case ep.Protocol == "ee" && ep.TLSDomain == "":
		return ep, errors.New("tls_domain обязателен для ee")
	case ep.TLSDomain != "" && !servercfg.ValidDomain(ep.TLSDomain):
		return ep, fmt.Errorf("tls_domain %q - нужно имя хоста", w.TLSDomain)
	case utf8.RuneCountInString(ep.Label) > maxEndpointLabel:
		return ep, fmt.Errorf("label длиннее %d символов", maxEndpointLabel)
	case utf8.RuneCountInString(ep.Note) > maxServerNote:
		return ep, fmt.Errorf("note длиннее %d символов", maxServerNote)
	case ep.SortOrder < 0 || ep.SortOrder > 1_000_000:
		return ep, fmt.Errorf("sort_order %d - от 0 до 1000000", ep.SortOrder)
	}
	return ep, nil
}

// ServerFilter - фильтры списка; пустые поля не фильтруют.
type ServerFilter struct {
	Engine string
	State  string
}

const serverCols = `no, name, emoji, country, engine, relay_code, node_code, state, relay_only, node_retired,
	tls_domain, extra_tls_domains, user_max_tcp_conns, user_max_unique_ips, settings, note, created_at, updated_at`

func scanServer(row pgx.Row) (Server, error) {
	var srv Server
	var no int16
	err := row.Scan(&no, &srv.Name, &srv.Emoji, &srv.Country, &srv.Engine, &srv.RelayCode, &srv.NodeCode, &srv.State,
		&srv.RelayOnly, &srv.NodeRetired, &srv.TLSDomain, &srv.ExtraTLSDomains, &srv.UserMaxTCPConns, &srv.UserMaxUniqueIPs,
		&srv.Settings, &srv.Note, &srv.CreatedAt, &srv.UpdatedAt)
	srv.No = int(no)
	if srv.ExtraTLSDomains == nil {
		srv.ExtraTLSDomains = []string{}
	}
	if srv.Settings == nil {
		srv.Settings = map[string]string{}
	}
	srv.Endpoints = []Endpoint{}
	return srv, err
}

// ListServers - страница серверов по номеру с точками входа и живыми цифрами.
// Три запроса на страницу (серверы, точки входа, живые цифры), без N+1.
func (s *Store) ListServers(ctx context.Context, engine, state string, limit, offset int) ([]Server, int, error) {
	const where = `WHERE ($1 = '' OR engine = $1) AND ($2 = '' OR state = $2)`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM servers `+where, engine, state).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count servers: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+serverCols+` FROM servers `+where+` ORDER BY no LIMIT $3 OFFSET $4`,
		engine, state, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("servers: %w", err)
	}
	out := []Server{}
	for rows.Next() {
		srv, err := scanServer(rows)
		if err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("scan servers: %w", err)
		}
		out = append(out, srv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("servers: %w", err)
	}
	if err := s.fillServers(ctx, s.pool, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// GetServer - сервер по номеру; ErrServerNotFound - нет.
func (s *Store) GetServer(ctx context.Context, no int) (Server, error) {
	srv, err := scanServer(s.pool.QueryRow(ctx, `SELECT `+serverCols+` FROM servers WHERE no = $1`, no))
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, fmt.Errorf("%w: %d", ErrServerNotFound, no)
	}
	if err != nil {
		return Server{}, fmt.Errorf("server %d: %w", no, err)
	}
	list := []Server{srv}
	if err := s.fillServers(ctx, s.pool, list); err != nil {
		return Server{}, err
	}
	return list[0], nil
}

// querier - пул или транзакция.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// fillServers дополняет серверы точками входа и живыми цифрами: по одному
// запросу на всю страницу.
func (s *Store) fillServers(ctx context.Context, q querier, list []Server) error {
	if len(list) == 0 {
		return nil
	}
	nos := make([]int16, len(list))
	idx := make(map[int]int, len(list))
	for i, srv := range list {
		nos[i] = int16(srv.No)
		idx[srv.No] = i
	}
	rows, err := q.Query(ctx,
		`SELECT server_no, id, protocol, host, port, tls_domain, via_relay, label, sort_order, enabled, note
		   FROM server_endpoints WHERE server_no = ANY($1)
		  ORDER BY server_no, sort_order, id`, nos)
	if err != nil {
		return fmt.Errorf("server endpoints: %w", err)
	}
	for rows.Next() {
		var no int16
		var ep Endpoint
		if err := rows.Scan(&no, &ep.ID, &ep.Protocol, &ep.Host, &ep.Port, &ep.TLSDomain, &ep.ViaRelay,
			&ep.Label, &ep.SortOrder, &ep.Enabled, &ep.Note); err != nil {
			rows.Close()
			return fmt.Errorf("scan server endpoints: %w", err)
		}
		if i, ok := idx[int(no)]; ok {
			list[i].Endpoints = append(list[i].Endpoints, ep)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("server endpoints: %w", err)
	}
	return s.fillLive(ctx, q, list)
}

// fillLive - живые цифры машин сервера (продукт = движок) одним запросом:
// подключения - из снимка продукта, если его API ответил, иначе сумма строк
// пользователей; онлайн - пользователи с подключениями.
func (s *Store) fillLive(ctx context.Context, q querier, list []Server) error {
	var codes, products []string
	for i := range list {
		list[i].Live = &ServerLive{}
		for _, c := range []*string{list[i].RelayCode, list[i].NodeCode} {
			if c != nil {
				codes = append(codes, *c)
				products = append(products, list[i].Engine)
			}
		}
	}
	if len(codes) == 0 {
		return nil
	}
	rows, err := q.Query(ctx,
		`SELECT DISTINCT q.code, q.product, r.seen_at, p.api_ok,
		        CASE WHEN p.api_ok THEN COALESCE((p.doc->>'connections_total')::bigint, 0) ELSE c.total END,
		        c.online
		   FROM unnest($1::text[], $2::text[]) AS q(code, product)
		   LEFT JOIN agent_runtime r ON r.target = q.code
		  CROSS JOIN LATERAL (SELECT d.doc, COALESCE(d.doc->'api_ok' = 'true'::jsonb, false) AS api_ok
		                        FROM (SELECT CASE q.product WHEN 'telemt' THEN r.telemt ELSE r.mtg END AS doc) d) p
		  CROSS JOIN LATERAL (SELECT COALESCE(sum(u.connections), 0)::bigint AS total,
		                             (count(*) FILTER (WHERE u.connections > 0))::int AS online
		                        FROM runtime_user_conns u WHERE u.target = q.code AND u.product = q.product) c`,
		codes, products)
	if err != nil {
		return fmt.Errorf("server live: %w", err)
	}
	defer rows.Close()
	live := map[string]LiveMachine{}
	for rows.Next() {
		var lm LiveMachine
		var product string
		if err := rows.Scan(&lm.Code, &product, &lm.SeenAt, &lm.APIOK, &lm.Connections, &lm.UsersOnline); err != nil {
			return fmt.Errorf("scan server live: %w", err)
		}
		live[lm.Code+"|"+product] = lm
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("server live: %w", err)
	}
	for i := range list {
		if c := list[i].RelayCode; c != nil {
			lm := live[*c+"|"+list[i].Engine]
			lm.Code = *c
			list[i].Live.Relay = &lm
		}
		if c := list[i].NodeCode; c != nil {
			lm := live[*c+"|"+list[i].Engine]
			lm.Code = *c
			list[i].Live.Node = &lm
		}
	}
	return nil
}

// checkMachines - relay_code - машина вида relay, node_code - вида exit; один
// запрос на все серверы, до записи.
func checkMachines(ctx context.Context, tx pgx.Tx, list []Server) error {
	var codes []string
	for _, srv := range list {
		for _, c := range []*string{srv.RelayCode, srv.NodeCode} {
			if c != nil && !slices.Contains(codes, *c) {
				codes = append(codes, *c)
			}
		}
	}
	if len(codes) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT code, kind FROM targets WHERE code = ANY($1)`, codes)
	if err != nil {
		return fmt.Errorf("машины серверов: %w", err)
	}
	kinds := map[string]string{}
	for rows.Next() {
		var code, kind string
		if err := rows.Scan(&code, &kind); err != nil {
			rows.Close()
			return fmt.Errorf("машины серверов: %w", err)
		}
		kinds[code] = kind
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("машины серверов: %w", err)
	}
	for _, srv := range list {
		if c := srv.RelayCode; c != nil && kinds[*c] != "relay" {
			return fmt.Errorf("%w: сервер %d: relay_code %q - нужен существующий релей (вид relay)", ErrUnknownMachine, srv.No, *c)
		}
		if c := srv.NodeCode; c != nil && kinds[*c] != "exit" {
			return fmt.Errorf("%w: сервер %d: node_code %q - нужна существующая машина за рубежом (вид exit)", ErrUnknownMachine, srv.No, *c)
		}
	}
	return nil
}

// replaceEndpoints заменяет точки входа серверов: удаление и одна вставка на всех.
func replaceEndpoints(ctx context.Context, tx pgx.Tx, list []Server) error {
	nos := make([]int16, len(list))
	var (
		srvNo                                 []int16
		protos, hosts, tlsDomains, labels, ns []string
		ports, sorts                          []int32
		via, enabled                          []bool
	)
	for i, srv := range list {
		nos[i] = int16(srv.No)
		for _, ep := range srv.Endpoints {
			srvNo = append(srvNo, int16(srv.No))
			protos = append(protos, ep.Protocol)
			hosts = append(hosts, ep.Host)
			ports = append(ports, int32(ep.Port))
			tlsDomains = append(tlsDomains, ep.TLSDomain)
			via = append(via, ep.ViaRelay)
			labels = append(labels, ep.Label)
			sorts = append(sorts, int32(ep.SortOrder))
			enabled = append(enabled, ep.Enabled)
			ns = append(ns, ep.Note)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM server_endpoints WHERE server_no = ANY($1)`, nos); err != nil {
		return fmt.Errorf("точки входа: %w", err)
	}
	if len(srvNo) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO server_endpoints (server_no, protocol, host, port, tls_domain, via_relay, label, sort_order, enabled, note)
		 SELECT * FROM unnest($1::smallint[], $2::text[], $3::text[], $4::int[], $5::text[], $6::bool[],
		                      $7::text[], $8::int[], $9::bool[], $10::text[])`,
		srvNo, protos, hosts, ports, tlsDomains, via, labels, sorts, enabled, ns); err != nil {
		return fmt.Errorf("точки входа: %w", err)
	}
	return nil
}

// serverArgs - значения колонок сервера для INSERT/UPDATE ($1..$16).
func serverArgs(srv Server) []any {
	return []any{srv.No, srv.Name, srv.Emoji, srv.Country, srv.Engine, srv.RelayCode, srv.NodeCode, srv.State,
		srv.RelayOnly, srv.NodeRetired, srv.TLSDomain, srv.ExtraTLSDomains, srv.UserMaxTCPConns, srv.UserMaxUniqueIPs,
		srv.Settings, srv.Note}
}

const insertServerSQL = `INSERT INTO servers (no, name, emoji, country, engine, relay_code, node_code, state, relay_only,
	node_retired, tls_domain, extra_tls_domains, user_max_tcp_conns, user_max_unique_ips, settings, note)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`

const updateServerSQL = `UPDATE servers SET name = $2, emoji = $3, country = $4, relay_code = $6, node_code = $7,
	state = $8, relay_only = $9, node_retired = $10, tls_domain = $11, extra_tls_domains = $12,
	user_max_tcp_conns = $13, user_max_unique_ips = $14, settings = $15, note = $16, updated_at = now()
	WHERE no = $1 AND engine = $5`

// CreateServer создаёт сервер с точками входа в одной транзакции.
// ErrServerExists - номер занят; ErrUnknownMachine - машины нет или не того вида.
func (s *Store) CreateServer(ctx context.Context, w ServerWrite) (Server, error) {
	srv, err := w.Server()
	if err != nil {
		return Server{}, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkMachines(ctx, tx, []Server{srv}); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, insertServerSQL+` ON CONFLICT (no) DO NOTHING`, serverArgs(srv)...)
		if err != nil {
			return fmt.Errorf("сервер %d: %w", srv.No, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: %d", ErrServerExists, srv.No)
		}
		return replaceEndpoints(ctx, tx, []Server{srv})
	})
	if err != nil {
		return Server{}, err
	}
	return s.GetServer(ctx, srv.No)
}

// ReplaceServer заменяет сервер целиком вместе с точками входа (одна транзакция).
// ErrServerNotFound - нет; ErrEngineImmutable - попытка сменить движок.
func (s *Store) ReplaceServer(ctx context.Context, w ServerWrite) (Server, error) {
	srv, err := w.Server()
	if err != nil {
		return Server{}, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var engine string
		err := tx.QueryRow(ctx, `SELECT engine FROM servers WHERE no = $1 FOR UPDATE`, srv.No).Scan(&engine)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrServerNotFound, srv.No)
		}
		if err != nil {
			return fmt.Errorf("сервер %d: %w", srv.No, err)
		}
		if engine != srv.Engine {
			return fmt.Errorf("%w: сервер %d - %s, а не %s", ErrEngineImmutable, srv.No, engine, srv.Engine)
		}
		if err := checkMachines(ctx, tx, []Server{srv}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, updateServerSQL, serverArgs(srv)...); err != nil {
			return fmt.Errorf("сервер %d: %w", srv.No, err)
		}
		return replaceEndpoints(ctx, tx, []Server{srv})
	})
	if err != nil {
		return Server{}, err
	}
	return s.GetServer(ctx, srv.No)
}

// DeleteServer удаляет выключенный сервер (точки входа - каскадом).
// ErrServerNotFound - нет; ErrServerNotDisabled - сервер не disabled.
func (s *Store) DeleteServer(ctx context.Context, no int) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM servers WHERE no = $1 FOR UPDATE`, no).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrServerNotFound, no)
		}
		if err != nil {
			return fmt.Errorf("сервер %d: %w", no, err)
		}
		if state != "disabled" {
			return fmt.Errorf("%w: сервер %d - %s", ErrServerNotDisabled, no, state)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM servers WHERE no = $1`, no); err != nil {
			return fmt.Errorf("сервер %d: %w", no, err)
		}
		return nil
	})
}

// ImportServers - upsert по номеру до MaxServersImport серверов в одной
// транзакции: любая ошибка (описание, машина, смена движка) - 0 изменений.
func (s *Store) ImportServers(ctx context.Context, items []ServerWrite) (inserted, updated int, err error) {
	if len(items) > MaxServersImport {
		return 0, 0, fmt.Errorf("%w: %d > %d", ErrBatchTooLarge, len(items), MaxServersImport)
	}
	list := make([]Server, 0, len(items))
	nos := make([]int16, 0, len(items))
	seen := map[int]bool{}
	for i, w := range items {
		srv, err := w.Server()
		if err != nil {
			return 0, 0, fmt.Errorf("[%d] сервер %d: %w", i, w.No, err)
		}
		if seen[srv.No] {
			return 0, 0, fmt.Errorf("%w: [%d] номер %d повторяется", ErrBadServer, i, srv.No)
		}
		seen[srv.No] = true
		list = append(list, srv)
		nos = append(nos, int16(srv.No))
	}
	if len(list) == 0 {
		return 0, 0, nil
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkMachines(ctx, tx, list); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT no, engine FROM servers WHERE no = ANY($1) FOR UPDATE`, nos)
		if err != nil {
			return fmt.Errorf("импорт серверов: %w", err)
		}
		engines := map[int]string{}
		for rows.Next() {
			var no int16
			var engine string
			if err := rows.Scan(&no, &engine); err != nil {
				rows.Close()
				return fmt.Errorf("импорт серверов: %w", err)
			}
			engines[int(no)] = engine
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("импорт серверов: %w", err)
		}
		batch := &pgx.Batch{}
		for _, srv := range list {
			if engine, ok := engines[srv.No]; ok {
				if engine != srv.Engine {
					return fmt.Errorf("%w: сервер %d - %s, а не %s", ErrEngineImmutable, srv.No, engine, srv.Engine)
				}
				batch.Queue(updateServerSQL, serverArgs(srv)...)
				updated++
			} else {
				batch.Queue(insertServerSQL, serverArgs(srv)...)
				inserted++
			}
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("импорт серверов: %w", err)
		}
		return replaceEndpoints(ctx, tx, list)
	})
	if err != nil {
		return 0, 0, err
	}
	return inserted, updated, nil
}

// inTx выполняет fn в транзакции: ошибка - откат, иначе коммит.
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
