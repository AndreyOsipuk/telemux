package store

// Обновление telemt и mtg-multi кнопкой (ADR 0008): последние релизы (stack_releases),
// последнее обновление продукта на машине (stack_updates) и ресурс stack машины.
// Таблицы - migrations/0013_stack.sql.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AndreyOsipuk/telemux/internal/stack"
	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

// ResourceStack - версии бинарей telemt и mtg-multi машины (JSON stack.Content).
const ResourceStack = stack.Resource

// StackTables - таблицы миграции 0013; без них ручки обновления не монтируются.
var StackTables = []string{"stack_releases", "stack_updates"}

// CheckStackSchema - применена ли миграция 0013.
func (s *Store) CheckStackSchema(ctx context.Context) error {
	return s.checkTables(ctx, StackTables, "migrations/0013_stack.sql")
}

// StackUpdateTimeout - сколько обновление ждёт отчёта агента. Дольше - failed: иначе
// машина со старым агентом (не знает ресурс stack) навсегда осталась бы в update_in_progress.
const StackUpdateTimeout = 30 * time.Minute

// stackTimeoutError - текст ошибки обновления, не дождавшегося отчёта агента.
const stackTimeoutError = "агент не сообщил результат за 30 минут"

// stackCompareError - агент в режиме compare: бинарь не меняет, только сообщает разницу.
const stackCompareError = "агент в режиме compare: обновление не применено"

// ErrStackUpdateInProgress - обновление на машине ещё идёт (не больше одного за раз).
var ErrStackUpdateInProgress = errors.New("обновление на машине ещё идёт")

// StackRelease - последний известный релиз продукта.
type StackRelease struct {
	Product     string
	Repo        string
	Version     *string // nil - релиз ещё ни разу не узнали
	Tag         *string
	PublishedAt *time.Time
	CheckedAt   *time.Time
	Error       *string // ошибка последней проверки
}

// StackReleases - сохранённые релизы по продуктам (строк может не быть).
func (s *Store) StackReleases(ctx context.Context) ([]StackRelease, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT product, repo, version, tag, published_at, checked_at, error FROM stack_releases ORDER BY product`)
	if err != nil {
		return nil, fmt.Errorf("stack_releases: %w", err)
	}
	defer rows.Close()
	out := []StackRelease{}
	for rows.Next() {
		var r StackRelease
		if err := rows.Scan(&r.Product, &r.Repo, &r.Version, &r.Tag, &r.PublishedAt, &r.CheckedAt, &r.Error); err != nil {
			return nil, fmt.Errorf("scan stack_releases: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stack_releases: %w", err)
	}
	return out, nil
}

// StackReleaseCheck - итог проверки релиза продукта: Err пуст - релиз узнан.
type StackReleaseCheck struct {
	Product     string
	Repo        string
	Version     string
	Tag         string
	PublishedAt time.Time
	Err         string
}

// SaveStackRelease сохраняет итог проверки. Ошибка не стирает прошлый релиз: меняются
// только error, checked_at и repo.
func (s *Store) SaveStackRelease(ctx context.Context, c StackReleaseCheck) error {
	if !stack.ValidProduct(c.Product) {
		return fmt.Errorf("stack_releases: неизвестный продукт %q", c.Product)
	}
	var err error
	if c.Err != "" {
		_, err = s.pool.Exec(ctx,
			`INSERT INTO stack_releases (product, repo, checked_at, error) VALUES ($1, $2, now(), $3)
			 ON CONFLICT (product) DO UPDATE
			   SET repo = EXCLUDED.repo, checked_at = EXCLUDED.checked_at, error = EXCLUDED.error`,
			c.Product, c.Repo, truncate(c.Err, 1000))
	} else {
		_, err = s.pool.Exec(ctx,
			`INSERT INTO stack_releases (product, repo, version, tag, published_at, checked_at, error)
			 VALUES ($1, $2, $3, $4, $5, now(), NULL)
			 ON CONFLICT (product) DO UPDATE
			   SET repo = EXCLUDED.repo, version = EXCLUDED.version, tag = EXCLUDED.tag,
			       published_at = EXCLUDED.published_at, checked_at = EXCLUDED.checked_at, error = NULL`,
			c.Product, c.Repo, c.Version, c.Tag, c.PublishedAt)
	}
	if err != nil {
		return fmt.Errorf("stack_releases %s: %w", c.Product, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// StackUpdate - последнее обновление продукта на машине (схема StackUpdateState).
type StackUpdate struct {
	Code        string     `json:"code"`
	Product     string     `json:"product"`
	From        *string    `json:"from"`
	To          string     `json:"to"`
	Revision    int64      `json:"revision"`
	Status      string     `json:"status"` // pending | applied | failed
	Error       *string    `json:"error"`
	RequestedAt time.Time  `json:"requested_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

// StackProductInfo - продукт на машине: версия из runtime-отчёта и последнее обновление.
type StackProductInfo struct {
	Current *string      // nil - агент версию не прислал
	Update  *StackUpdate // nil - обновлений через telemux не было
}

// StackMachine - машина для страницы версий и проверок обновления.
type StackMachine struct {
	Code    string
	Kind    string
	Enabled bool
	Arch    string            // из runtime host.arch; пусто - агент не прислал
	Telemt  *StackProductInfo // nil - агент продукт не присылает
	Mtg     *StackProductInfo
}

// Действующий статус обновления u (алиас stack_updates) по отчёту агента o (алиас
// observed_state ресурса stack): записанный итог; иначе итог агента, если его отчёт
// дошёл до ревизии обновления (compared - failed: агент в режиме compare бинарь не
// меняет); иначе failed по таймауту; иначе pending.
var (
	stackTimeoutSQL = `make_interval(secs => ` + strconv.Itoa(int(StackUpdateTimeout/time.Second)) + `)`

	stackStatusSQL = func(u string) string {
		return `CASE WHEN ` + u + `.status <> 'pending' THEN ` + u + `.status
		             WHEN o.revision >= ` + u + `.revision AND o.status IN ('applied', 'failed') THEN o.status
		             WHEN o.revision >= ` + u + `.revision AND o.status = 'compared' THEN 'failed'
		             WHEN ` + u + `.requested_at < now() - ` + stackTimeoutSQL + ` THEN 'failed'
		             ELSE 'pending' END`
	}
	stackErrorSQL = func(u string) string {
		return `CASE WHEN ` + u + `.status <> 'pending' THEN ` + u + `.error
		             WHEN o.revision >= ` + u + `.revision AND o.status = 'failed' THEN COALESCE(NULLIF(o.error, ''), 'агент сообщил failed')
		             WHEN o.revision >= ` + u + `.revision AND o.status = 'applied' THEN NULL
		             WHEN o.revision >= ` + u + `.revision AND o.status = 'compared' THEN '` + stackCompareError + `'
		             WHEN ` + u + `.requested_at < now() - ` + stackTimeoutSQL + ` THEN '` + stackTimeoutError + `'
		             END`
	}
	stackFinishedSQL = func(u string) string {
		return `CASE WHEN ` + u + `.status <> 'pending' THEN ` + u + `.finished_at
		             WHEN o.revision >= ` + u + `.revision AND o.status IN ('applied', 'failed', 'compared') THEN o.seen_at
		             WHEN ` + u + `.requested_at < now() - ` + stackTimeoutSQL + ` THEN ` + u + `.requested_at + ` + stackTimeoutSQL + `
		             END`
	}
	// stackUpdateCols - колонки обновления u для scanUpdate (8 штук).
	stackUpdateCols = func(u string) string {
		return u + `.product, ` + u + `.from_version, ` + u + `.to_version, ` + u + `.revision, ` +
			stackStatusSQL(u) + `, ` + stackErrorSQL(u) + `, ` + u + `.requested_at, ` + stackFinishedSQL(u)
	}
)

// updateScan - приёмники колонок stackUpdateCols (продукт NULL - обновления нет).
type updateScan struct {
	product, from, to, status, err *string
	rev                            *int64
	requested, finished            *time.Time
}

func (u *updateScan) dest() []any {
	return []any{&u.product, &u.from, &u.to, &u.rev, &u.status, &u.err, &u.requested, &u.finished}
}

func (u *updateScan) update(code string) *StackUpdate {
	if u.product == nil || u.to == nil || u.rev == nil || u.status == nil || u.requested == nil {
		return nil
	}
	return &StackUpdate{Code: code, Product: *u.product, From: u.from, To: *u.to, Revision: *u.rev,
		Status: *u.status, Error: u.err, RequestedAt: u.requested.UTC(), FinishedAt: utcPtr(u.finished)}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

// stackMachineSQL - машины (релеи и выходы) с версиями из runtime-отчёта и последними
// обновлениями обоих продуктов одним запросом. $1 - условие на код (NULL - все).
const stackMachineFrom = `
   FROM targets t
   LEFT JOIN agent_runtime r ON r.target = t.code
   LEFT JOIN observed_state o ON o.target = t.code AND o.resource = 'stack'
   LEFT JOIN stack_updates ut ON ut.target = t.code AND ut.product = 'telemt'
   LEFT JOIN stack_updates um ON um.target = t.code AND um.product = 'mtg'`

var stackMachineSelect = `SELECT t.code, t.kind, t.enabled, COALESCE(r.host->>'arch', ''),
        COALESCE(jsonb_typeof(r.telemt) = 'object', false), NULLIF(r.telemt->>'version', ''),
        COALESCE(jsonb_typeof(r.mtg) = 'object', false), NULLIF(r.mtg->>'version', ''),
        ` + stackUpdateCols("ut") + `,
        ` + stackUpdateCols("um") + stackMachineFrom

func scanStackMachine(row pgx.Row) (StackMachine, error) {
	var m StackMachine
	var hasT, hasM bool
	var curT, curM *string
	var ut, um updateScan
	dest := []any{&m.Code, &m.Kind, &m.Enabled, &m.Arch, &hasT, &curT, &hasM, &curM}
	dest = append(dest, ut.dest()...)
	dest = append(dest, um.dest()...)
	if err := row.Scan(dest...); err != nil {
		return m, err
	}
	if hasT {
		m.Telemt = &StackProductInfo{Current: curT, Update: ut.update(m.Code)}
	}
	if hasM {
		m.Mtg = &StackProductInfo{Current: curM, Update: um.update(m.Code)}
	}
	return m, nil
}

// StackMachines - страница релеев и выходов по коду с версиями продуктов и последними
// обновлениями (одним запросом); total - сколько всего таких машин.
func (s *Store) StackMachines(ctx context.Context, limit, offset int) ([]StackMachine, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM targets WHERE kind IN ('relay', 'exit')`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count stack machines: %w", err)
	}
	rows, err := s.pool.Query(ctx, stackMachineSelect+`
		  WHERE t.kind IN ('relay', 'exit')
		  ORDER BY t.code LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("stack machines: %w", err)
	}
	defer rows.Close()
	out := []StackMachine{}
	for rows.Next() {
		m, err := scanStackMachine(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan stack machine: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("stack machines: %w", err)
	}
	return out, total, nil
}

// StackMachine - одна машина любого вида; нет - ErrUnknownTarget.
func (s *Store) StackMachine(ctx context.Context, code string) (StackMachine, error) {
	m, err := scanStackMachine(s.pool.QueryRow(ctx, stackMachineSelect+` WHERE t.code = $1`, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrUnknownTarget
	}
	if err != nil {
		return m, fmt.Errorf("stack machine %q: %w", code, err)
	}
	return m, nil
}

// latestStack - содержимое последней ревизии ресурса stack машины; ревизии нет - пустое.
func latestStack(ctx context.Context, db queryer, target string) (stack.Content, error) {
	var content []byte
	err := db.QueryRow(ctx,
		`SELECT content FROM desired_config WHERE target = $1 AND resource = $2
		  ORDER BY revision DESC LIMIT 1`, target, ResourceStack).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return stack.Content{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ресурс stack машины %q: %w", target, err)
	}
	c, err := stack.Parse(content)
	if err != nil {
		return nil, fmt.Errorf("ресурс stack машины %q: %w", target, err)
	}
	if c == nil {
		c = stack.Content{}
	}
	return c, nil
}

// StackBlobAllowed - опубликован ли машине бинарь sha256 в последней ревизии её ресурса stack.
func (s *Store) StackBlobAllowed(ctx context.Context, target, sha string) (bool, error) {
	c, err := latestStack(ctx, s.pool, target)
	if err != nil {
		return false, err
	}
	for _, b := range c {
		if b.SHA256 == sha {
			return true, nil
		}
	}
	return false, nil
}

// PublishStackUpdate публикует машине новую ревизию ресурса stack только с бинарём этого
// продукта и записывает обновление в stack_updates - одной транзакцией под блокировкой
// публикаций. Ресурс stack - разовая команда: после итога продукт убирается из ресурса
// (finishLocked), поэтому сливать с прошлой ревизией нечего. На машине не больше одного
// незавершённого обновления (по любому продукту): так итог ревизии относится к одному
// продукту. Перед проверкой записывается итог, если агент уже отчитался. Ресурс stack
// назначается машине (targets.resources), если ещё не назначен. Нет машины -
// ErrUnknownTarget, выключена - ErrMachineDisabled, идёт другое обновление -
// ErrStackUpdateInProgress.
func (s *Store) PublishStackUpdate(ctx context.Context, code, product string, bin stack.Binary, from *string) (StackUpdate, error) {
	u := StackUpdate{Code: code, Product: product, From: from, To: bin.Version, Status: "pending"}
	if !stack.ValidProduct(product) {
		return u, fmt.Errorf("stack: неизвестный продукт %q", product)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return u, fmt.Errorf("begin publish stack: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, publishLockKey); err != nil {
		return u, fmt.Errorf("publish lock: %w", err)
	}
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT enabled FROM targets WHERE code = $1 FOR UPDATE`, code).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrUnknownTarget
	}
	if err != nil {
		return u, fmt.Errorf("машина %q: %w", code, err)
	}
	if !enabled {
		return u, ErrMachineDisabled
	}
	if _, err := finishLocked(ctx, tx, &code); err != nil {
		return u, err
	}
	var pending bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM stack_updates WHERE target = $1 AND status = 'pending')`, code).Scan(&pending); err != nil {
		return u, fmt.Errorf("обновления машины %q: %w", code, err)
	}
	if pending {
		return u, ErrStackUpdateInProgress
	}
	body, err := stack.Content{product: bin}.Marshal()
	if err != nil {
		return u, fmt.Errorf("stack: %w", err)
	}
	if _, err := stack.Parse(body); err != nil {
		return u, err
	}
	if u.Revision, err = insertStackRevision(ctx, tx, code, body); err != nil {
		return u, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE targets SET resources = array_append(resources, $2), updated_at = now()
		  WHERE code = $1 AND NOT ($2 = ANY(resources))`, code, ResourceStack); err != nil {
		return u, fmt.Errorf("назначение ресурса stack машине %q: %w", code, err)
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO stack_updates (target, product, from_version, to_version, revision, status, error, requested_at, finished_at)
		 VALUES ($1, $2, $3, $4, $5, 'pending', NULL, now(), NULL)
		 ON CONFLICT (target, product) DO UPDATE
		   SET from_version = EXCLUDED.from_version, to_version = EXCLUDED.to_version, revision = EXCLUDED.revision,
		       status = 'pending', error = NULL, requested_at = EXCLUDED.requested_at, finished_at = NULL
		 RETURNING requested_at`, code, product, from, bin.Version, u.Revision).Scan(&u.RequestedAt); err != nil {
		return u, fmt.Errorf("stack_updates %s %s: %w", code, product, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return u, fmt.Errorf("commit publish stack: %w", err)
	}
	u.RequestedAt = u.RequestedAt.UTC()
	return u, nil
}

// insertStackRevision пишет новую ревизию ресурса stack машины (всегда новую: повтор того
// же бинаря после провала - новая попытка агента).
func insertStackRevision(ctx context.Context, tx pgx.Tx, code string, body []byte) (int64, error) {
	var rev int64
	if err := tx.QueryRow(ctx, `SELECT nextval('config_revision_seq')`).Scan(&rev); err != nil {
		return 0, fmt.Errorf("next revision: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO desired_config (target, resource, revision, sha256, content) VALUES ($1, $2, $3, $4, $5)`,
		code, ResourceStack, rev, telemtcfg.SHA256(body), body); err != nil {
		return 0, fmt.Errorf("insert desired stack: %w", err)
	}
	return rev, nil
}

// FinishStackUpdates записывает итог незавершённых обновлений (applied или failed по
// отчёту агента, failed по таймауту) и той же транзакцией убирает завершённые продукты из
// ресурса stack их машин. Возвращает завершённые. Только на мастере.
func (s *Store) FinishStackUpdates(ctx context.Context) ([]StackUpdate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin finish stack: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, publishLockKey); err != nil {
		return nil, fmt.Errorf("publish lock: %w", err)
	}
	done, err := finishLocked(ctx, tx, nil)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit finish stack: %w", err)
	}
	return done, nil
}

// finishLocked - итог незавершённых обновлений (машины target или всех, target = nil) и
// новая ревизия ресурса stack без завершённых продуктов, если содержимое изменилось
// (продуктов не осталось - {}). Ресурс stack - разовая команда: иначе агент при
// перепроверке снова поставил бы бинарь поверх собранного вручную, а машина, вернувшаяся
// после таймаута, применила бы устаревшее обновление. Вызывается в транзакции под
// publishLockKey.
func finishLocked(ctx context.Context, tx pgx.Tx, target *string) ([]StackUpdate, error) {
	rows, err := tx.Query(ctx,
		`WITH x AS (
		   SELECT u.target, u.product, `+stackStatusSQL("u")+` AS st, `+stackErrorSQL("u")+` AS err,
		          `+stackFinishedSQL("u")+` AS fin
		     FROM stack_updates u
		     LEFT JOIN observed_state o ON o.target = u.target AND o.resource = 'stack'
		    WHERE u.status = 'pending' AND ($1::text IS NULL OR u.target = $1)
		 )
		 UPDATE stack_updates u SET status = x.st, error = x.err, finished_at = COALESCE(x.fin, now())
		   FROM x
		  WHERE u.target = x.target AND u.product = x.product AND x.st <> 'pending'
		 RETURNING u.target, u.product, u.from_version, u.to_version, u.revision, u.status, u.error,
		           u.requested_at, u.finished_at`, target)
	if err != nil {
		return nil, fmt.Errorf("завершение обновлений stack: %w", err)
	}
	out := []StackUpdate{}
	for rows.Next() {
		var u StackUpdate
		if err := rows.Scan(&u.Code, &u.Product, &u.From, &u.To, &u.Revision, &u.Status, &u.Error,
			&u.RequestedAt, &u.FinishedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan stack_updates: %w", err)
		}
		out = append(out, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("завершение обновлений stack: %w", err)
	}
	byTarget := map[string][]string{}
	order := []string{}
	for _, u := range out {
		if _, ok := byTarget[u.Code]; !ok {
			order = append(order, u.Code)
		}
		byTarget[u.Code] = append(byTarget[u.Code], u.Product)
	}
	for _, code := range order {
		content, err := latestStack(ctx, tx, code)
		if err != nil {
			content = stack.Content{} // испорченная ревизия: заменяется пустой
		}
		changed := err != nil
		for _, p := range byTarget[code] {
			if _, ok := content[p]; ok {
				delete(content, p)
				changed = true
			}
		}
		if !changed {
			continue
		}
		body, err := content.Marshal()
		if err != nil {
			return nil, fmt.Errorf("stack: %w", err)
		}
		if _, err := insertStackRevision(ctx, tx, code, body); err != nil {
			return nil, err
		}
	}
	return out, nil
}
