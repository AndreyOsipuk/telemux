package role

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }

func cluster3() Config {
	return Config{
		Self: "ps1", Peers: []string{"ps2", "ps3"}, RequirePatroni: true,
		FenceAfter: 10 * time.Second, MasterHold: 5 * time.Second,
	}
}

// primary - наблюдение «PG primary, Patroni лидер, видим соседей seen».
func primary(sec float64, seen ...string) Sample {
	return Sample{At: at(sec), PatroniLeader: true, Seen: seen}
}

// run прогоняет последовательность наблюдений и возвращает все состояния.
func run(cfg Config, ss ...Sample) []State {
	var st State
	out := make([]State, 0, len(ss))
	for _, s := range ss {
		st = Next(cfg, st, s)
		out = append(out, st)
	}
	return out
}

func TestQuorumSize(t *testing.T) {
	for _, c := range []struct{ peers, want int }{{0, 1}, {1, 2}, {2, 2}, {3, 3}, {4, 3}} {
		cfg := Config{Peers: make([]string, c.peers)}
		if got := cfg.Quorum(); got != c.want {
			t.Fatalf("соседей %d: кворум %d, ждали %d", c.peers, got, c.want)
		}
	}
}

func TestNext_MasterAfterHold(t *testing.T) {
	sts := run(cluster3(), primary(0, "ps2"), primary(2, "ps2"), primary(4.9, "ps2"), primary(5, "ps2"))
	for i := 0; i < 3; i++ {
		if sts[i].Role != Fenced || sts[i].IsMaster() {
			t.Fatalf("шаг %d: до master_hold узел не мастер, получили %s", i, sts[i].Role)
		}
	}
	if !sts[3].IsMaster() || sts[3].Master != "ps1" {
		t.Fatalf("после master_hold - мастер, получили %+v", sts[3])
	}
}

// Асимметрия: A видит B, но не C - кворум 2 из 3 есть, мастер остаётся.
func TestNext_AsymmetricPartitionKeepsMaster(t *testing.T) {
	sts := run(cluster3(), primary(0, "ps2", "ps3"), primary(5, "ps2", "ps3"), primary(7, "ps2"), primary(60, "ps2"))
	if !sts[3].IsMaster() {
		t.Fatalf("видим одного соседа из двух - кворум есть, мастер должен остаться: %+v", sts[3])
	}
}

// Изоляция: никого не видим - мастер держится fence_after, потом fenced.
func TestNext_IsolatedFencedAfterTimeout(t *testing.T) {
	sts := run(cluster3(),
		primary(0, "ps2"), primary(5, "ps2"), // мастер
		primary(6), primary(15), // 10 с без кворума - ещё прощаем
		primary(15.1), // 10.1 с - fenced
	)
	if !sts[1].IsMaster() || !sts[3].IsMaster() {
		t.Fatalf("краткая потеря кворума (<= fence_after) прощается: %v %v", sts[1].Role, sts[3].Role)
	}
	if sts[4].Role != Fenced || sts[4].Quorum {
		t.Fatalf("без кворума дольше fence_after - fenced, получили %+v", sts[4])
	}
}

// Антифлаппинг: после возврата кворума мастер возвращается только через master_hold.
func TestNext_RegainRequiresHold(t *testing.T) {
	sts := run(cluster3(),
		primary(0, "ps2"), primary(5, "ps2"), // мастер
		primary(20),          // fenced
		primary(21, "ps3"),   // кворум вернулся - отсчёт заново
		primary(25.9, "ps3"), // ещё нет
		primary(26, "ps3"),   // мастер
	)
	if sts[2].Role != Fenced {
		t.Fatalf("ждали fenced, получили %s", sts[2].Role)
	}
	if sts[3].IsMaster() || sts[4].IsMaster() {
		t.Fatal("до истечения master_hold после возврата кворума - не мастер")
	}
	if !sts[5].IsMaster() {
		t.Fatalf("после master_hold - мастер, получили %+v", sts[5])
	}
}

// Мигание кворума во время выдержки не сбрасывает отсчёт, пока не превышен fence_after.
func TestNext_FlapDuringHoldWithinFenceAfter(t *testing.T) {
	sts := run(cluster3(), primary(0, "ps2"), primary(2), primary(5, "ps2"))
	if !sts[2].IsMaster() {
		t.Fatalf("кратковременная потеря кворума во время hold прощается: %+v", sts[2])
	}
}

func TestNext_FailClosed(t *testing.T) {
	cfg := cluster3()
	boom := errors.New("boom")
	cases := []struct {
		name string
		s    Sample
		want Role
	}{
		{"PG недоступен", Sample{At: at(0), PGErr: boom, PatroniLeader: true, Seen: []string{"ps2"}}, Unknown},
		{"PG реплика", Sample{At: at(0), InRecovery: true, PatroniLeader: true, Seen: []string{"ps2"}}, Replica},
		{"Patroni недоступен", Sample{At: at(0), PatroniErr: boom, Seen: []string{"ps2"}}, Fenced},
		{"Patroni не лидер", Sample{At: at(0), PatroniLeader: false, Seen: []string{"ps2"}}, Fenced},
	}
	for _, c := range cases {
		// Предыдущее состояние - устойчивый мастер: потеря любого условия - сразу не мастер.
		prev := run(cfg, primary(-10, "ps2"), primary(-4, "ps2"))[1]
		if !prev.IsMaster() {
			t.Fatal("подготовка: ждали мастера")
		}
		got := Next(cfg, prev, c.s)
		if got.Role != c.want || got.IsMaster() {
			t.Fatalf("%s: ждали %s, получили %+v", c.name, c.want, got)
		}
		if got.Role != Replica && got.Reason == "" {
			t.Fatalf("%s: у не-мастера должна быть причина", c.name)
		}
		// Причина уходит клиентам - без текста ошибок PG/Patroni.
		if strings.Contains(got.Reason, "boom") {
			t.Fatalf("%s: текст ошибки не должен попадать в reason: %q", c.name, got.Reason)
		}
	}
}

// Ручной promote мимо Patroni: PG primary, кворум есть, Patroni не лидер - не мастер.
func TestNext_PromoteBypassingPatroniIsNotMaster(t *testing.T) {
	cfg := cluster3()
	var st State
	for i := 0; i < 10; i++ {
		st = Next(cfg, st, Sample{At: at(float64(i * 2)), Seen: []string{"ps2", "ps3"}})
	}
	if st.IsMaster() {
		t.Fatal("без лидерства Patroni узел не должен становиться мастером")
	}
}

// Старт без кворума: до первого кворума - fail-closed, даже в окне fence_after.
func TestNext_NoQuorumAtStart(t *testing.T) {
	sts := run(cluster3(), primary(0), primary(6), primary(9))
	for i, st := range sts {
		if st.IsMaster() || st.Quorum {
			t.Fatalf("шаг %d: без единого кворума - не мастер, получили %+v", i, st)
		}
	}
}

func TestNext_SeenFiltersUnknownAndSelf(t *testing.T) {
	st := Next(cluster3(), State{}, primary(0, "ps9", "ps1", "ps3", "ps3"))
	if len(st.Seen) != 1 || st.Seen[0] != "ps3" {
		t.Fatalf("seen должен содержать только известных соседей без дублей: %v", st.Seen)
	}
	// Чужие коды не дают кворума.
	st = Next(cluster3(), State{}, primary(0, "ps8", "ps9"))
	if st.Quorum {
		t.Fatal("неизвестные узлы не голосуют")
	}
}

func TestNext_SingleNodeMode(t *testing.T) {
	cfg := Config{Self: "ps1"} // ни соседей, ни Patroni, hold 0
	st := Next(cfg, State{}, Sample{At: at(0)})
	if !st.IsMaster() {
		t.Fatalf("одиночный режим: PG primary = мастер, получили %+v", st)
	}
	st = Next(cfg, st, Sample{At: at(1), InRecovery: true})
	if st.Role != Replica {
		t.Fatalf("одиночный режим: PG реплика = replica, получили %s", st.Role)
	}
}

func TestNext_MasterHintAndSince(t *testing.T) {
	cfg := cluster3()
	s1 := Next(cfg, State{}, Sample{At: at(0), InRecovery: true, Seen: []string{"ps2"}, MasterHint: "ps2"})
	if s1.Master != "ps2" {
		t.Fatalf("реплика должна подсказывать мастера от соседей: %q", s1.Master)
	}
	s2 := Next(cfg, s1, Sample{At: at(3), InRecovery: true, Seen: []string{"ps2"}})
	if !s2.Since.Equal(s1.Since) {
		t.Fatal("since не меняется, пока роль та же")
	}
}

// Старт процесса: master_hold не выдерживается, но остальные условия - да.
func TestNext_InitialSkipsHoldOnly(t *testing.T) {
	cfg := cluster3()
	if st := Next(cfg, Initial(), primary(0, "ps2")); !st.IsMaster() {
		t.Fatalf("старт при кворуме и лидерстве Patroni - мастер сразу: %+v", st)
	}
	if st := Next(cfg, Initial(), primary(0)); st.IsMaster() {
		t.Fatal("старт без кворума - не мастер")
	}
	if st := Next(cfg, Initial(), Sample{At: at(0), Seen: []string{"ps2"}}); st.IsMaster() {
		t.Fatal("старт без лидерства Patroni - не мастер")
	}
	// После первого наблюдения признак старта пропадает: потеря и возврат - с выдержкой.
	st := Next(cfg, Initial(), primary(0))
	if st = Next(cfg, st, primary(1, "ps2")); st.IsMaster() || !st.Holding() {
		t.Fatalf("после первого наблюдения - обычная выдержка: %+v", st)
	}
}
