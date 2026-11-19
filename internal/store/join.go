package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
)

// Добавление машины одной командой: одноразовый токен установки агента
// (agent_join_tokens). Скрипт установки меняет его на токен агента.

// Ключи targets.settings машины-выхода.
const (
	SettingAddress     = "address"      // публичный IPv4 выхода
	SettingBridgeSocks = "bridge_socks" // socks-вход на релее к этому выходу
)

var (
	// ErrTargetExists - машина с таким кодом уже заведена.
	ErrTargetExists = errors.New("машина с таким кодом уже есть")
	// ErrBadRelay - у выхода указан несуществующий релей или машина другого вида.
	ErrBadRelay = errors.New("релей выхода")
	// ErrBadJoinToken - токен установки неизвестен, просрочен или уже использован.
	ErrBadJoinToken = errors.New("токен установки не найден")
	// ErrRelayNoExits - у релея нет выходов с адресом и socks-входом: ему не через что идти к мастеру.
	ErrRelayNoExits = errors.New("у релея нет выходов с адресом и socks-входом (address, bridge_socks)")
	// ErrJoinNotSupported - машине этого вида агент не ставится (panel).
	ErrJoinNotSupported = errors.New("для машин этого вида агент не ставится")
)

// JoinToken - одноразовый токен установки (выдаётся один раз, в базе - хеш).
type JoinToken struct {
	Token     string
	ExpiresAt time.Time
}

// JoinIssue - выпущенный токен и всё, что нужно для команды установки.
type JoinIssue struct {
	Target Target
	Join   *JoinToken // nil - токен не выпущен (новый релей без выходов)
	Exits  []Target   // для релея: выходы с адресом и socks-входом, по коду
}

// JoinInfo - машина живого токена установки (для скрипта и бинаря).
type JoinInfo struct {
	Target Target
	Exits  []Target
}

// JoinExchange - итог обмена токена установки на токен агента.
type JoinExchange struct {
	Target     Target
	Exits      []Target
	AgentToken string
}

// queryer - пул или открытая транзакция.
type queryer interface {
	execer
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const targetCols = `code, kind, COALESCE(relay_code, ''), resources, enabled, settings`

func scanTarget(row pgx.Row) (Target, error) {
	var t Target
	err := row.Scan(&t.Code, &t.Kind, &t.RelayCode, &t.Resources, &t.Enabled, &t.Settings)
	return t, err
}

// TargetByCode - машина по коду; нет такой - ErrUnknownTarget.
func (s *Store) TargetByCode(ctx context.Context, code string) (Target, error) {
	return loadTarget(ctx, s.pool, code, "")
}

// loadTarget читает машину; lock - "FOR UPDATE"/"FOR SHARE" внутри транзакции.
func loadTarget(ctx context.Context, db queryer, code, lock string) (Target, error) {
	t, err := scanTarget(db.QueryRow(ctx, `SELECT `+targetCols+` FROM targets WHERE code = $1 `+lock, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrUnknownTarget
	}
	if err != nil {
		return t, fmt.Errorf("машина %q: %w", code, err)
	}
	return t, nil
}

// relayExits - включённые выходы релея, у которых есть адрес и socks-вход
// (через их мосты агент релея ходит к мастеру), по коду. Одним запросом.
func relayExits(ctx context.Context, db queryer, relay string) ([]Target, error) {
	rows, err := db.Query(ctx,
		`SELECT `+targetCols+` FROM targets
		  WHERE kind = 'exit' AND relay_code = $1 AND enabled
		    AND COALESCE(settings->>'address', '') <> '' AND COALESCE(settings->>'bridge_socks', '') <> ''
		  ORDER BY code`, relay)
	if err != nil {
		return nil, fmt.Errorf("выходы релея %q: %w", relay, err)
	}
	defer rows.Close()
	out := []Target{}
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, fmt.Errorf("scan выхода: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// issueJoin гасит прежние неиспользованные токены машины и выпускает новый.
func issueJoin(ctx context.Context, db queryer, target string, ttl time.Duration) (*JoinToken, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, `DELETE FROM agent_join_tokens WHERE target = $1 AND used_at IS NULL`, target); err != nil {
		return nil, fmt.Errorf("гашение токенов %q: %w", target, err)
	}
	var exp time.Time
	if err := db.QueryRow(ctx,
		`INSERT INTO agent_join_tokens (token_sha256, target, expires_at)
		 VALUES ($1, $2, now() + make_interval(secs => $3)) RETURNING expires_at`,
		tokenHash(token), target, ttl.Seconds()).Scan(&exp); err != nil {
		return nil, fmt.Errorf("выпуск токена установки %q: %w", target, err)
	}
	return &JoinToken{Token: token, ExpiresAt: exp}, nil
}

// CreateTarget заводит новую машину и в той же транзакции выпускает токен
// установки. Занятый код - ErrTargetExists, у выхода плохой релей - ErrBadRelay.
// Новому релею без выходов токен не выпускается (Join = nil): команду ему не собрать.
func (s *Store) CreateTarget(ctx context.Context, t Target, ttl time.Duration) (JoinIssue, error) {
	res := JoinIssue{Exits: []Target{}}
	if t.Resources == nil {
		t.Resources = []string{}
	}
	if t.Settings == nil {
		t.Settings = map[string]string{}
	}
	if err := normalizeSettings(t.Kind, t.Settings); err != nil {
		return res, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin create target: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var relay *string
	if t.Kind == "exit" {
		r, err := loadTarget(ctx, tx, t.RelayCode, "FOR SHARE")
		if errors.Is(err, ErrUnknownTarget) {
			return res, fmt.Errorf("%w: машины %q нет", ErrBadRelay, t.RelayCode)
		}
		if err != nil {
			return res, err
		}
		if r.Kind != "relay" {
			return res, fmt.Errorf("%w: %q - не релей, а %s", ErrBadRelay, t.RelayCode, r.Kind)
		}
		relay = &t.RelayCode
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO targets (code, kind, relay_code, resources, enabled, settings)
		 VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (code) DO NOTHING`,
		t.Code, t.Kind, relay, t.Resources, t.Enabled, t.Settings)
	if err != nil {
		return res, fmt.Errorf("создание машины %q: %w", t.Code, err)
	}
	if tag.RowsAffected() == 0 {
		return res, ErrTargetExists
	}
	if t.Kind == "relay" {
		if res.Exits, err = relayExits(ctx, tx, t.Code); err != nil {
			return res, err
		}
	}
	if t.Kind == "exit" || len(res.Exits) > 0 {
		if res.Join, err = issueJoin(ctx, tx, t.Code, ttl); err != nil {
			return res, err
		}
	}
	if res.Target, err = loadTarget(ctx, tx, t.Code, ""); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit create target: %w", err)
	}
	return res, nil
}

// IssueJoinToken выпускает новый токен установки; прежний неиспользованный
// сгорает. Нет машины - ErrUnknownTarget, panel - ErrJoinNotSupported, релей
// без выходов - ErrRelayNoExits (прежний токен тогда остаётся).
func (s *Store) IssueJoinToken(ctx context.Context, code string, ttl time.Duration) (JoinIssue, error) {
	res := JoinIssue{Exits: []Target{}}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin join: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// FOR UPDATE сериализует выпуски одной машины.
	if res.Target, err = loadTarget(ctx, tx, code, "FOR UPDATE"); err != nil {
		return res, err
	}
	if res.Exits, err = joinExits(ctx, tx, res.Target); err != nil {
		return res, err
	}
	if res.Join, err = issueJoin(ctx, tx, code, ttl); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit join: %w", err)
	}
	return res, nil
}

// joinExits проверяет, что машине можно поставить агента, и для релея
// возвращает его выходы.
func joinExits(ctx context.Context, db queryer, t Target) ([]Target, error) {
	switch t.Kind {
	case "exit":
		return []Target{}, nil
	case "relay":
		exits, err := relayExits(ctx, db, t.Code)
		if err != nil {
			return nil, err
		}
		if len(exits) == 0 {
			return nil, ErrRelayNoExits
		}
		return exits, nil
	default:
		return nil, ErrJoinNotSupported
	}
}

// JoinTarget - машина живого (не просроченного и не использованного) токена
// установки. Токен не тратится. Иначе - ErrBadJoinToken.
func (s *Store) JoinTarget(ctx context.Context, token string) (JoinInfo, error) {
	var info JoinInfo
	t, err := scanTarget(s.pool.QueryRow(ctx,
		`SELECT `+targetCols+` FROM targets
		  WHERE code = (SELECT target FROM agent_join_tokens
		                 WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > now())`,
		tokenHash(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return info, ErrBadJoinToken
	}
	if err != nil {
		return info, fmt.Errorf("токен установки: %w", err)
	}
	info.Target = t
	if info.Exits, err = joinExits(ctx, s.pool, t); err != nil {
		return info, err
	}
	return info, nil
}

// ExchangeJoin атомарно тратит токен установки и выпускает токен агента
// (прежний токен агента машины перестаёт действовать). Повтор, просроченный
// или чужой токен - ErrBadJoinToken. Релей без выходов - ErrRelayNoExits, токен
// тогда не тратится.
func (s *Store) ExchangeJoin(ctx context.Context, token string) (JoinExchange, error) {
	var ex JoinExchange
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ex, fmt.Errorf("begin exchange: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var code string
	err = tx.QueryRow(ctx,
		`UPDATE agent_join_tokens SET used_at = now()
		  WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > now()
		  RETURNING target`, tokenHash(token)).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return ex, ErrBadJoinToken
	}
	if err != nil {
		return ex, fmt.Errorf("обмен токена установки: %w", err)
	}
	if ex.Target, err = loadTarget(ctx, tx, code, "FOR UPDATE"); err != nil {
		return ex, err
	}
	if ex.Exits, err = joinExits(ctx, tx, ex.Target); err != nil {
		return ex, err
	}
	if ex.AgentToken, err = issueAgentToken(ctx, tx, code); err != nil {
		return ex, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ex, fmt.Errorf("commit exchange: %w", err)
	}
	return ex, nil
}

// ExitAddresses - публичные IPv4 включённых машин-выходов из settings.address
// (для восстановления разрешений на файрволе мастера). Значение, которое не
// разбирается как IPv4, пропускается.
func (s *Store) ExitAddresses(ctx context.Context) ([]netip.Addr, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT settings->>'address' FROM targets
		  WHERE kind = 'exit' AND enabled AND COALESCE(settings->>'address', '') <> ''
		  ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("exit addresses: %w", err)
	}
	defer rows.Close()
	var out []netip.Addr
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan exit address: %w", err)
		}
		if a, err := netip.ParseAddr(raw); err == nil && a.Is4() && !a.IsUnspecified() {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}
