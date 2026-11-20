package agent

// Тесты спецификации relay-via-exit, раздел R2: агент выхода шлёт сводку моста.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/bridge"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// relayMaster - мастер, который признаёт токен релея pr2.
func relayMaster(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-pr2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(store.Bundle{Target: "pr2", Revision: 1, Resources: []store.Desired{}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRuntime_R2_1_BridgeSummaryInReport(t *testing.T) {
	b, err := bridge.New(bridge.Config{Code: "ps2", MasterURLs: []string{relayMaster(t).URL}, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	bsrv := httptest.NewServer(b.Handler())
	t.Cleanup(bsrv.Close)
	// Релей pr2 сходил через мост.
	req, _ := http.NewRequest(http.MethodGet, bsrv.URL+"/v1/desired?target=pr2&rev=0&wait=1s", nil)
	req.Header.Set("Authorization", "Bearer tok-pr2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	c, _, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	c.BridgeURL = bsrv.URL + "/" // хвостовой слэш не мешает
	rep := c.Collect(context.Background(), time.Minute)
	if rep.Bridge == nil {
		t.Fatal("с адресом моста в отчёте должно быть поле bridge")
	}
	if rep.Bridge.Code != "ps2" || rep.Bridge.Error != "" || len(rep.Bridge.Relays) != 1 {
		t.Fatalf("сводка моста: %+v", rep.Bridge)
	}
	r := rep.Bridge.Relays[0]
	if r.Code != "pr2" || r.Requests != 1 || !r.MasterOK || r.LastSeen == nil {
		t.Fatalf("релей в сводке: %+v", r)
	}
	if rep.Host == nil {
		t.Fatal("остальной отчёт тоже должен быть")
	}
	// Поле в JSON - контракт 5.2: {"bridge": {"code", "relays", "error"}}.
	raw, _ := json.Marshal(rep)
	for _, k := range []string{`"bridge":{"code":"ps2"`, `"relays":[{"code":"pr2"`, `"error":""`} {
		if !strings.Contains(string(raw), k) {
			t.Fatalf("в JSON отчёта нет %s: %s", k, raw)
		}
	}
}

func TestRuntime_R2_1_EmptyBridgeRelaysNotNull(t *testing.T) {
	b, err := bridge.New(bridge.Config{Code: "ps2", MasterURLs: []string{"http://127.0.0.1:1"}, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	bsrv := httptest.NewServer(b.Handler())
	t.Cleanup(bsrv.Close)
	c, _, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	c.BridgeURL = bsrv.URL
	rep := c.Collect(context.Background(), time.Minute)
	raw, _ := json.Marshal(rep.Bridge)
	if string(raw) != `{"code":"ps2","relays":[],"error":""}` {
		t.Fatalf("пустой мост: %s", raw)
	}
}

func TestRuntime_R2_2_BridgeTimeoutReportStillSent(t *testing.T) {
	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(hang); slow.Close() })

	c, _, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	c.BridgeURL = slow.URL
	c.BridgeTimeout = 50 * time.Millisecond
	start := time.Now()
	rep := c.Collect(context.Background(), time.Minute)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("молчащий мост задержал отчёт на %s", d)
	}
	if rep.Bridge == nil || rep.Bridge.Error == "" || rep.Bridge.Relays == nil || len(rep.Bridge.Relays) != 0 {
		t.Fatalf("мост молчит - ждали bridge.error и пустые relays: %+v", rep.Bridge)
	}
	if rep.Host == nil {
		t.Fatal("остальной отчёт должен уйти как обычно")
	}
}

func TestRuntime_R2_2_BridgeDefaultTimeout3s(t *testing.T) {
	c := &RuntimeCollector{}
	c.defaults()
	if c.BridgeTimeout != 3*time.Second {
		t.Fatalf("таймаут моста по умолчанию - 3 с, а %s", c.BridgeTimeout)
	}
}

func TestRuntime_R2_2_BridgeBadAnswer(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // старый мост без /v1/relays
	}))
	t.Cleanup(bad.Close)
	c, _, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	c.BridgeURL = bad.URL
	rep := c.Collect(context.Background(), time.Minute)
	if rep.Bridge == nil || !strings.Contains(rep.Bridge.Error, "404") {
		t.Fatalf("старый мост - ошибка с кодом ответа: %+v", rep.Bridge)
	}
}

func TestRuntime_R2_3_NoBridgeURLNoField(t *testing.T) {
	c, _, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	rep := c.Collect(context.Background(), time.Minute)
	if rep.Bridge != nil {
		t.Fatalf("без адреса моста поля bridge быть не должно: %+v", rep.Bridge)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), `"bridge"`) {
		t.Fatalf("без адреса моста ключа bridge в JSON быть не должно: %s", raw)
	}
}

// Релей с двумя своими нодами ходит к мастеру через первую доступную; чтобы мост
// второй ноды тоже видел релей (иначе пара навсегда relay_silent, U1), агент своим
// тикером (U17) спрашивает desired без ожидания через каждый адрес мастера.
func TestAgent_U1_U17_ProbesEveryBridge(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]string{}
	mk := func(name string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name] = r.URL.Path + "?" + r.URL.RawQuery
			mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	a1, a2 := mk("ps2"), mk("ps1")
	a, err := New(Config{MasterURLs: []string{a1.URL, a2.URL}, Token: "tok", Target: "pr2",
		Mode: ModeCompare, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	a.ProbeMasters(context.Background())
	for _, name := range []string{"ps2", "ps1"} {
		q := hits[name]
		if !strings.HasPrefix(q, "/v1/desired?") || !strings.Contains(q, "target=pr2") || !strings.Contains(q, "wait=0s") {
			t.Fatalf("мост %s: ждали desired без ожидания, было %q", name, q)
		}
	}
}

func TestAgent_U1_SingleMasterNoProbe(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(srv.Close)
	a, err := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "ps2", Mode: ModeCompare, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	a.ProbeMasters(context.Background())
	if n != 0 {
		t.Fatalf("один адрес мастера - проб нет, было %d", n)
	}
}

// U17: адреса опрашиваются параллельно; мёртвый адрес (не отвечает) не задерживает
// остальные, а сама проба ограничена таймаутом.
func TestAgent_U17_DeadAddressDoesNotBlockOthers(t *testing.T) {
	release := make(chan struct{})
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); dead.Close() })
	start := time.Now()
	var mu sync.Mutex
	liveAt := map[string]time.Duration{}
	mk := func(name string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			liveAt[name] = time.Since(start)
			mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	l1, l2 := mk("ps1"), mk("ps3")
	// Мёртвый адрес первым: при последовательном опросе он задержал бы остальные.
	a, err := New(Config{MasterURLs: []string{dead.URL, l1.URL, l2.URL}, Token: "tok", Target: "pr2",
		Mode: ModeCompare, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	a.probeTimeout = 300 * time.Millisecond
	start = time.Now()
	a.ProbeMasters(context.Background())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("проба ждала мёртвый адрес дольше таймаута: %s", d)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"ps1", "ps3"} {
		at, ok := liveAt[name]
		if !ok {
			t.Fatalf("живой адрес %s не опрошен", name)
		}
		if at >= a.probeTimeout {
			t.Fatalf("живой адрес %s опрошен через %s - его задержал мёртвый", name, at)
		}
	}
}

// U17: таймаут пробы одного адреса - 5 с, период тикера - не реже раза в 60 с.
func TestAgent_U17_ProbeDefaults(t *testing.T) {
	for _, every := range []time.Duration{0, time.Hour} {
		a, err := New(Config{MasterURLs: []string{"http://127.0.0.1:1", "http://127.0.0.1:2"}, Token: "tok",
			Target: "pr2", Mode: ModeCompare, ProbeEvery: every, Log: discardLog()})
		if err != nil {
			t.Fatal(err)
		}
		if a.probeTimeout != 5*time.Second {
			t.Fatalf("таймаут пробы - 5 с, а %s", a.probeTimeout)
		}
		if a.cfg.ProbeEvery <= 0 || a.cfg.ProbeEvery > time.Minute {
			t.Fatalf("ProbeEvery=%s: период пробы должен быть в (0, 60 с], а %s", every, a.cfg.ProbeEvery)
		}
	}
}

// U17: тикер пробы запускается из Run сам по себе - без runtime-сборщика
// (RuntimeEvery = 0, как и при паузе ErrRuntimeUnsupported runtime-цикл не шлёт пробы).
func TestAgent_U17_ProbeLoopRunsWithoutRuntime(t *testing.T) {
	// Первый адрес держит long-poll агента до отмены - так ходит обычный цикл.
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wait") == "0s" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(first.Close)
	var mu sync.Mutex
	probes := 0
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wait") == "0s" {
			mu.Lock()
			probes++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(second.Close)
	a, err := New(Config{MasterURLs: []string{first.URL, second.URL}, Token: "tok", Target: "pr2",
		Mode: ModeCompare, ProbeEvery: 20 * time.Millisecond, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := probes
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("тикер пробы не запустился из Run: проб через второй адрес %d", probes)
}

// U17: runtime-цикл сам проб не шлёт - они только в своём тикере.
func TestAgent_U17_RuntimeLoopDoesNotProbe(t *testing.T) {
	var mu sync.Mutex
	probes := 0
	srv := func() *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("wait") == "0s" {
				mu.Lock()
				probes++
				mu.Unlock()
			}
			w.WriteHeader(http.StatusNotFound) // старый мастер: ErrRuntimeUnsupported
		}))
		t.Cleanup(s.Close)
		return s
	}
	m1, m2 := srv(), srv()
	c, _, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	a, err := New(Config{MasterURLs: []string{m1.URL, m2.URL}, Token: "tok", Target: "pr2",
		Mode: ModeCompare, Runtime: c, RuntimeEvery: time.Hour, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.runtimeLoop(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if probes != 0 {
		t.Fatalf("runtime-цикл не должен слать пробы, было %d", probes)
	}
}
