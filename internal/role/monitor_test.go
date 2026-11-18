package role

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakePatroni struct {
	leader bool
	err    error
}

func (f *fakePatroni) IsLeader(context.Context) (bool, error) { return f.leader, f.err }

type fakePeers struct {
	mu  sync.Mutex
	res []PeerStatus
}

func (f *fakePeers) set(res ...PeerStatus) {
	f.mu.Lock()
	f.res = res
	f.mu.Unlock()
}

func (f *fakePeers) Probe(context.Context) []PeerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PeerStatus(nil), f.res...)
}

type fakeStandbys struct {
	names []string
	err   error
}

func (f *fakeStandbys) StreamingStandbys(context.Context, time.Duration) ([]string, error) {
	return f.names, f.err
}

type fakeFence struct {
	calls      []bool
	terminates int
	err        error
}

func (f *fakeFence) SetWriteFence(_ context.Context, on bool) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, on)
	return nil
}

func (f *fakeFence) TerminateAppSessions(context.Context) error {
	f.terminates++
	return nil
}

// clock - ручные часы для монитора.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time                { return c.now }
func (c *clock) Advance(d time.Duration)       { c.now = c.now.Add(d) }
func newClock() *clock                         { return &clock{now: t0} }
func (m *Monitor) withClock(c *clock) *Monitor { m.now = c.Now; return m }

func TestMonitor_FullCycle(t *testing.T) {
	pg := fakeChecker{inRecovery: false}
	pat := &fakePatroni{leader: true}
	pr := &fakePeers{}
	pr.set(PeerStatus{Code: "ps2", OK: true, Role: Replica}, PeerStatus{Code: "ps3", OK: false})
	fe := &fakeFence{}
	clk := newClock()
	m := NewMonitor(cluster3(), Probes{PG: pg, Patroni: pat, Peers: pr, Fence: fe}, time.Second, nil).withClock(clk)
	ctx := context.Background()

	// Рестарт telemux на здоровом мастере: мастер сразу, без master_hold и без
	// обрыва сессий бота (fence только приводится к «выключен»).
	if st := m.Tick(ctx); !st.IsMaster() {
		t.Fatalf("старт процесса при выполненных условиях - мастер сразу: %+v", st)
	}
	if len(fe.calls) != 1 || fe.calls[0] || fe.terminates != 0 {
		t.Fatalf("на старте мастера fence только снимается: calls=%v terminates=%d", fe.calls, fe.terminates)
	}
	if !m.Current().IsMaster() {
		t.Fatal("Current должен отдавать последнее решение")
	}

	// Изоляция: соседи пропали.
	pr.set(PeerStatus{Code: "ps2"}, PeerStatus{Code: "ps3"})
	clk.Advance(11 * time.Second)
	if st := m.Tick(ctx); st.Role != Fenced {
		t.Fatalf("изоляция дольше fence_after - fenced: %+v", st)
	}
	if len(fe.calls) != 2 || !fe.calls[1] {
		t.Fatalf("fenced - fence включается: %v", fe.calls)
	}
	// Пока fenced - сессии приложений добиваются каждый раунд, ALTER SYSTEM не повторяется.
	clk.Advance(time.Second)
	m.Tick(ctx)
	if len(fe.calls) != 2 || fe.terminates != 1 {
		t.Fatalf("повторный раунд в fenced: calls=%v terminates=%d", fe.calls, fe.terminates)
	}

	// Сосед вернулся: идёт master_hold - fence не трогается, сессии не рвутся.
	pr.set(PeerStatus{Code: "ps2", OK: true}, PeerStatus{Code: "ps3"})
	clk.Advance(time.Second)
	if st := m.Tick(ctx); st.IsMaster() || !st.Holding() {
		t.Fatalf("после возврата кворума - выдержка master_hold: %+v", st)
	}
	if len(fe.calls) != 2 || fe.terminates != 1 {
		t.Fatalf("во время выдержки fence не переключается: calls=%v terminates=%d", fe.calls, fe.terminates)
	}
	clk.Advance(5 * time.Second)
	if st := m.Tick(ctx); !st.IsMaster() {
		t.Fatalf("после master_hold - мастер: %+v", st)
	}
	if len(fe.calls) != 3 || fe.calls[2] {
		t.Fatalf("стал мастером - fence снимается: %v", fe.calls)
	}
}

// Промоут реплики: из replica в мастер - только через master_hold.
func TestMonitor_PromotedReplicaWaitsHold(t *testing.T) {
	pg := &switchChecker{inRecovery: true}
	pr := &fakePeers{}
	pr.set(PeerStatus{Code: "ps2", OK: true})
	clk := newClock()
	m := NewMonitor(cluster3(), Probes{PG: pg, Patroni: &fakePatroni{leader: true}, Peers: pr}, time.Second, nil).withClock(clk)
	if st := m.Tick(context.Background()); st.Role != Replica {
		t.Fatalf("ждали replica: %+v", st)
	}
	pg.inRecovery = false // Patroni промоутил
	clk.Advance(time.Second)
	if st := m.Tick(context.Background()); st.IsMaster() {
		t.Fatal("сразу после промоута - выдержка master_hold, не мастер")
	}
	clk.Advance(5 * time.Second)
	if st := m.Tick(context.Background()); !st.IsMaster() {
		t.Fatalf("после master_hold - мастер: %+v", st)
	}
}

type switchChecker struct{ inRecovery bool }

func (c *switchChecker) IsInRecovery(context.Context) (bool, error) { return c.inRecovery, nil }

// Канал репликации засчитывается в кворум, даже если HTTP до соседей закрыт.
func TestMonitor_StandbyChannelCountsForQuorum(t *testing.T) {
	pr := &fakePeers{}
	pr.set(PeerStatus{Code: "ps2"}, PeerStatus{Code: "ps3"})
	clk := newClock()
	m := NewMonitor(cluster3(), Probes{
		PG: fakeChecker{}, Patroni: &fakePatroni{leader: true}, Peers: pr,
		Standbys: &fakeStandbys{names: []string{"ps3"}},
	}, time.Second, nil).withClock(clk)
	m.Tick(context.Background())
	clk.Advance(5 * time.Second)
	st := m.Tick(context.Background())
	if !st.IsMaster() || len(st.Seen) != 1 || st.Seen[0] != "ps3" {
		t.Fatalf("реплика ps3 стримит - кворум есть: %+v", st)
	}
}

// Ошибка pg_stat_replication не роняет решение: канал просто не засчитывается.
func TestMonitor_StandbyErrorIgnored(t *testing.T) {
	pr := &fakePeers{}
	pr.set(PeerStatus{Code: "ps2", OK: true})
	m := NewMonitor(cluster3(), Probes{
		PG: fakeChecker{}, Patroni: &fakePatroni{leader: true}, Peers: pr,
		Standbys: &fakeStandbys{err: errors.New("permission denied")},
	}, time.Second, nil)
	if st := m.Tick(context.Background()); !st.Quorum {
		t.Fatalf("HTTP-канал жив - кворум есть: %+v", st)
	}
}

// На реплике fence не трогается, подсказка мастера приходит от соседей.
func TestMonitor_ReplicaNoFenceAndHint(t *testing.T) {
	pr := &fakePeers{}
	pr.set(PeerStatus{Code: "ps2", OK: true, Role: Master}, PeerStatus{Code: "ps3", OK: true, Role: Replica})
	fe := &fakeFence{}
	m := NewMonitor(cluster3(), Probes{PG: fakeChecker{inRecovery: true}, Patroni: &fakePatroni{}, Peers: pr, Fence: fe}, time.Second, nil)
	st := m.Tick(context.Background())
	if st.Role != Replica || st.Master != "ps2" {
		t.Fatalf("реплика с подсказкой мастера ps2, получили %+v", st)
	}
	if len(fe.calls) != 0 {
		t.Fatalf("на реплике fence не трогается: %v", fe.calls)
	}
}

// Ошибка применения fence повторяется в следующем раунде.
func TestMonitor_FenceErrorRetried(t *testing.T) {
	fe := &fakeFence{err: errors.New("permission denied")}
	m := NewMonitor(cluster3(), Probes{PG: fakeChecker{}, Patroni: &fakePatroni{}, Fence: fe}, time.Second, nil)
	m.Tick(context.Background())
	fe.err = nil
	m.Tick(context.Background())
	if len(fe.calls) != 1 || !fe.calls[0] {
		t.Fatalf("после ошибки fence должен примениться в следующем раунде: %v", fe.calls)
	}
}

func TestMonitor_InitialStateNotMaster(t *testing.T) {
	m := NewMonitor(cluster3(), Probes{PG: fakeChecker{}}, 0, nil)
	if m.Current().IsMaster() || m.Current().Role != Unknown {
		t.Fatalf("до первого раунда - unknown, получили %+v", m.Current())
	}
}
