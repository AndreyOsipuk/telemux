package role

import (
	"slices"
	"time"
)

// Дополнительные роли автомата фейловера (см. docs/adr/0002-master-failover.md).
const (
	// Fenced - локальный PG primary, но право записи не подтверждено: нет кворума,
	// Patroni не лидер или ещё не выдержан master_hold. Запись приложений запрещена.
	Fenced Role = "fenced"
	// Unknown - не удалось спросить локальный PG. Fail-closed: не мастер.
	Unknown Role = "unknown"
)

// Config - параметры автомата роли.
type Config struct {
	// Self - код этой машины (совпадает с именем участника Patroni).
	Self string
	// Peers - коды соседних голосующих машин (без Self). Пусто - одиночный режим,
	// кворум из одного голоса выполнен всегда.
	Peers []string
	// RequirePatroni - мастер только если локальный Patroni считает себя лидером.
	RequirePatroni bool
	// FenceAfter - сколько primary может быть без кворума, прежде чем стать fenced.
	// Прощает кратковременные сбои сети. Должно быть меньше ttl Patroni.
	FenceAfter time.Duration
	// MasterHold - сколько условия мастера должны держаться подряд, прежде чем
	// узел станет мастером (антифлаппинг: мастер теряется сразу, приобретается медленно).
	MasterHold time.Duration
}

// Quorum - сколько голосов (включая свой) нужно для большинства.
func (c Config) Quorum() int { return (len(c.Peers)+1)/2 + 1 }

// Sample - одно наблюдение за состоянием узла.
type Sample struct {
	// At - момент наблюдения. Брать из time.Now(): сравнение идёт по монотонным часам.
	At time.Time

	// PGErr - ошибка опроса локального PG; InRecovery - pg_is_in_recovery().
	PGErr      error
	InRecovery bool

	// PatroniErr - ошибка опроса Patroni; PatroniLeader - Patroni считает себя лидером.
	PatroniErr    error
	PatroniLeader bool

	// Seen - соседи, видимые хотя бы по одному каналу (HTTP-пинг telemux или
	// streaming-реплика со свежим reply_time). Коды вне Config.Peers игнорируются.
	Seen []string
	// MasterHint - код мастера по словам соседей (для подсказки клиентам).
	MasterHint string
}

// State - текущее решение автомата.
type State struct {
	Role   Role      `json:"role"`
	Reason string    `json:"reason,omitempty"`
	Quorum bool      `json:"quorum"`
	Seen   []string  `json:"seen"`
	Since  time.Time `json:"since"`
	// Master - код мастера: свой, если мы мастер, иначе подсказка соседей.
	Master string `json:"master,omitempty"`

	initial        bool      // ни одного наблюдения ещё не было (старт процесса)
	holding        bool      // условия мастера выполнены, идёт выдержка master_hold
	lastQuorum     time.Time // последнее наблюдение с кворумом
	everQuorum     bool      // кворум был хотя бы раз (до этого - fail-closed)
	candidateSince time.Time // с какого момента держатся условия мастера
}

// Initial - состояние до первого наблюдения (старт процесса).
//
// Со старта master_hold не выдерживается: истории флаппинга у нового процесса нет,
// а выдержка при каждом рестарте telemux на здоровом мастере останавливала бы бота.
// Все остальные условия (PG primary, Patroni, кворум) проверяются как обычно.
func Initial() State {
	return State{Role: Unknown, Reason: "ещё не было наблюдений", initial: true}
}

// Holding - условия мастера выполнены, но ещё выдерживается master_hold.
// В это время PG-fence не переключается: узел либо только что был fenced
// (fence уже включён), либо стал primary после промоута.
func (s State) Holding() bool { return s.holding }

// IsMaster - узел вправе принимать запись приложений.
func (s State) IsMaster() bool { return s.Role == Master }

// PGPrimary - локальный PG сейчас primary (для решения о PG-fence).
func (s Sample) PGPrimary() bool { return s.PGErr == nil && !s.InRecovery }

// Next - чистая функция перехода автомата: предыдущее состояние + наблюдение →
// новое состояние. Все ошибки трактуются в сторону «не мастер».
// Reason уходит клиентам (/api/leader, 409), поэтому без текста ошибок PG/Patroni:
// сами ошибки пишет в лог Monitor.
//
// Мастер = PG primary И (Patroni лидер, если требуется) И кворум И master_hold выдержан.
func Next(cfg Config, prev State, s Sample) State {
	st := State{
		Seen:       seenPeers(cfg, s.Seen),
		lastQuorum: prev.lastQuorum,
		everQuorum: prev.everQuorum,
	}
	if 1+len(st.Seen) >= cfg.Quorum() {
		st.lastQuorum = s.At
		st.everQuorum = true
	}
	st.Quorum = st.everQuorum && s.At.Sub(st.lastQuorum) <= cfg.FenceAfter

	switch {
	case s.PGErr != nil:
		st.Role, st.Reason = Unknown, "локальный PG недоступен"
	case s.InRecovery:
		st.Role = Replica
	case cfg.RequirePatroni && s.PatroniErr != nil:
		st.Role, st.Reason = Fenced, "Patroni недоступен"
	case cfg.RequirePatroni && !s.PatroniLeader:
		st.Role, st.Reason = Fenced, "Patroni не считает узел лидером"
	case !st.Quorum:
		st.Role, st.Reason = Fenced, "нет кворума голосующих машин"
	case prev.Role == Master:
		st.Role = Master
	default:
		cand := prev.candidateSince
		if cand.IsZero() {
			cand = s.At
		}
		st.candidateSince = cand
		if prev.initial || s.At.Sub(cand) >= cfg.MasterHold {
			st.Role = Master
		} else {
			st.Role, st.Reason = Fenced, "условия мастера выполнены, выдерживается master_hold"
			st.holding = true
		}
	}
	if st.Role == Master {
		st.candidateSince = time.Time{}
		st.Master = cfg.Self
	} else {
		st.Master = s.MasterHint
	}

	st.Since = prev.Since
	if st.Role != prev.Role || st.Since.IsZero() {
		st.Since = s.At
	}
	return st
}

// seenPeers - уникальные отсортированные коды из Seen, входящие в Peers.
func seenPeers(cfg Config, seen []string) []string {
	out := make([]string, 0, len(seen))
	for _, c := range seen {
		if c != cfg.Self && slices.Contains(cfg.Peers, c) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out
}
