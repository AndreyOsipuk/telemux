package role

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// PatroniChecker - опрос локального Patroni.
type PatroniChecker interface {
	IsLeader(ctx context.Context) (bool, error)
}

// PeerStatus - ответ соседа на пинг.
type PeerStatus struct {
	Code string
	OK   bool
	Role Role
}

// PeerProber опрашивает соседние telemux (HTTP-канал кворума).
type PeerProber interface {
	Probe(ctx context.Context) []PeerStatus
}

// StandbyLister - живые streaming-реплики локального primary (второй канал кворума).
type StandbyLister interface {
	StreamingStandbys(ctx context.Context, maxAge time.Duration) ([]string, error)
}

// Fencer включает/выключает PG-fence (read-only + обрыв сессий приложений).
type Fencer interface {
	// SetWriteFence: on - default_transaction_read_only=on и обрыв сессий приложений;
	// off - сброс read-only.
	SetWriteFence(ctx context.Context, on bool) error
	// TerminateAppSessions рвёт сессии ролей приложений (пока узел fenced).
	TerminateAppSessions(ctx context.Context) error
}

// Probes - источники наблюдений. Обязателен только PG, остальное - по конфигурации.
type Probes struct {
	PG       RecoveryChecker
	Patroni  PatroniChecker // nil → условие Patroni не проверяется
	Peers    PeerProber     // nil → HTTP-канал кворума выключен
	Standbys StandbyLister  // nil → канал репликации выключен
	Fence    Fencer         // nil → PG-fence выключен
}

// Monitor периодически наблюдает узел, ведёт автомат роли и применяет PG-fence.
type Monitor struct {
	cfg      Config
	probes   Probes
	interval time.Duration
	timeout  time.Duration // таймаут одного раунда опросов
	log      *slog.Logger
	now      func() time.Time

	mu         sync.RWMutex
	state      State
	fenceKnown bool // состояние PG-fence приведено к желаемому хотя бы раз
	fenceOn    bool
}

// NewMonitor собирает монитор. interval - период наблюдения (0 → 2 с).
func NewMonitor(cfg Config, p Probes, interval time.Duration, log *slog.Logger) *Monitor {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Monitor{
		cfg: cfg, probes: p, interval: interval, timeout: interval, log: log, now: time.Now,
		state: Initial(),
	}
}

// Current - последнее решение (потокобезопасно).
func (m *Monitor) Current() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Run наблюдает до отмены ctx.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		m.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick - один раунд: опросы → переход автомата → PG-fence. Возвращает новое состояние.
func (m *Monitor) Tick(ctx context.Context) State {
	rctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	s := m.observe(rctx)

	m.mu.Lock()
	prev := m.state
	next := Next(m.cfg, prev, s)
	m.state = next
	m.mu.Unlock()

	if next.Role != prev.Role {
		m.log.Warn("роль узла изменилась", "from", prev.Role, "to", next.Role,
			"reason", next.Reason, "seen", next.Seen, "quorum", next.Quorum,
			"pg_err", errText(s.PGErr), "patroni_err", errText(s.PatroniErr))
	} else if s.PGErr != nil || s.PatroniErr != nil {
		m.log.Debug("ошибка опроса", "pg_err", errText(s.PGErr), "patroni_err", errText(s.PatroniErr))
	}
	m.applyFence(ctx, s, next)
	return next
}

// observe собирает наблюдение. Опросы идут параллельно, каждая ошибка - в Sample.
func (m *Monitor) observe(ctx context.Context) Sample {
	var (
		wg sync.WaitGroup
		s  = Sample{}
		mu sync.Mutex
	)
	addSeen := func(codes ...string) {
		mu.Lock()
		s.Seen = append(s.Seen, codes...)
		mu.Unlock()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		rec, err := m.probes.PG.IsInRecovery(ctx)
		mu.Lock()
		s.InRecovery, s.PGErr = rec, err
		mu.Unlock()
	}()
	if m.probes.Patroni != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lead, err := m.probes.Patroni.IsLeader(ctx)
			mu.Lock()
			s.PatroniLeader, s.PatroniErr = lead, err
			mu.Unlock()
		}()
	}
	if m.probes.Peers != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, p := range m.probes.Peers.Probe(ctx) {
				if !p.OK {
					continue
				}
				addSeen(p.Code)
				if p.Role == Master {
					mu.Lock()
					s.MasterHint = p.Code
					mu.Unlock()
				}
			}
		}()
	}
	if m.probes.Standbys != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Реплика шлёт отчёт раз в wal_receiver_status_interval (10 с по умолчанию);
			// старше FenceAfter - не считаем живой.
			names, err := m.probes.Standbys.StreamingStandbys(ctx, m.standbyMaxAge())
			if err != nil {
				// На реплике/при нехватке прав - не канал кворума, но не ошибка роли.
				m.log.Debug("pg_stat_replication", "err", err)
				return
			}
			addSeen(names...)
		}()
	}
	wg.Wait()
	s.At = m.now()
	return s
}

func (m *Monitor) standbyMaxAge() time.Duration {
	if m.cfg.FenceAfter > 15*time.Second {
		return m.cfg.FenceAfter
	}
	return 15 * time.Second
}

// applyFence приводит PG-fence к желаемому: на локальном primary fence включён,
// пока узел не мастер. На реплике, при недоступном PG и во время выдержки
// master_hold fence не трогаем.
// Пока узел fenced, сессии приложений рвутся на каждом раунде (идемпотентно).
func (m *Monitor) applyFence(ctx context.Context, s Sample, st State) {
	if m.probes.Fence == nil || !s.PGPrimary() || st.Holding() {
		return
	}
	want := !st.IsMaster()
	m.mu.RLock()
	same := m.fenceKnown && m.fenceOn == want
	m.mu.RUnlock()
	if same {
		if want {
			// Добиваем сессии, открытые в обход read-only (например, до reload).
			if err := m.probes.Fence.TerminateAppSessions(ctx); err != nil {
				m.log.Error("PG-fence: обрыв сессий приложений", "err", err)
			}
		}
		return
	}
	if err := m.probes.Fence.SetWriteFence(ctx, want); err != nil {
		m.log.Error("PG-fence: не удалось применить", "on", want, "err", err)
		m.mu.Lock()
		m.fenceKnown = false
		m.mu.Unlock()
		return
	}
	m.log.Warn("PG-fence", "on", want, "role", st.Role, "reason", st.Reason)
	m.mu.Lock()
	m.fenceKnown, m.fenceOn = true, want
	m.mu.Unlock()
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
