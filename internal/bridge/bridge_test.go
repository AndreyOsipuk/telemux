package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/tlspin"
)

// fakeMaster - мастер для тестов моста.
type fakeMaster struct {
	mu       sync.Mutex
	down     bool
	rev      int64
	token    string
	reports  []store.ObservedReport
	runtime  []store.RuntimeReport
	desired  int
	lastAuth string
}

func (m *fakeMaster) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		m.lastAuth = r.Header.Get("Authorization")
		if m.lastAuth != "Bearer "+m.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"x"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/desired":
			m.desired++
			if r.URL.Query().Get("rev") == itoa(m.rev) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_ = json.NewEncoder(w).Encode(store.Bundle{Target: "pr2", Revision: m.rev, Resources: []store.Desired{
				{Resource: store.ResourceTelemtUsers, Revision: m.rev, SHA256: "s", Content: []byte("users")},
			}})
		case "/v1/observed":
			var rep store.ObservedReport
			_ = json.NewDecoder(r.Body).Decode(&rep)
			m.reports = append(m.reports, rep)
			w.WriteHeader(http.StatusNoContent)
		case "/v1/runtime":
			var rep store.RuntimeReport
			_ = json.NewDecoder(r.Body).Decode(&rep)
			m.runtime = append(m.runtime, rep)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (m *fakeMaster) setDown(v bool) {
	m.mu.Lock()
	m.down = v
	m.mu.Unlock()
}

func newBridge(t *testing.T, masters ...string) *Bridge {
	t.Helper()
	b, err := New(Config{MasterURLs: masters, IdleWait: 30 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func get(b *Bridge, token, rev string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/desired?target=pr2&rev="+rev+"&wait=50ms", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	b.Handler().ServeHTTP(rec, req)
	return rec
}

func post(b *Bridge, token, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/observed", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	b.Handler().ServeHTTP(rec, req)
	return rec
}

const report = `{"target":"pr2","resources":[{"resource":"telemt.users","revision":3,"status":"applied"}]}`

// Мастер доступен: запрос и токен релея проходят насквозь, ответ - как у мастера.
func TestBridge_PassThrough(t *testing.T) {
	m := &fakeMaster{rev: 3, token: "relay-tok"}
	b := newBridge(t, m.server(t).URL)
	rec := get(b, "relay-tok", "1")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d", rec.Code)
	}
	var bundle store.Bundle
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil || bundle.Revision != 3 {
		t.Fatalf("не тот бандл: %+v, %v", bundle, err)
	}
	if m.lastAuth != "Bearer relay-tok" {
		t.Fatalf("мост не передал токен релея: %q", m.lastAuth)
	}
	if rec := get(b, "relay-tok", "3"); rec.Code != http.StatusNotModified {
		t.Fatalf("304 мастера должен пройти насквозь, получили %d", rec.Code)
	}
}

// Мастер недоступен: последний бандл отдаётся из кэша тому же токену.
func TestBridge_ServesCacheWhenMasterDown(t *testing.T) {
	m := &fakeMaster{rev: 3, token: "relay-tok"}
	b := newBridge(t, m.server(t).URL)
	if rec := get(b, "relay-tok", "0"); rec.Code != http.StatusOK {
		t.Fatalf("прогрев кэша: %d", rec.Code)
	}
	m.setDown(true)
	rec := get(b, "relay-tok", "1")
	if rec.Code != http.StatusOK {
		t.Fatalf("при недоступном мастере ждали бандл из кэша, получили %d", rec.Code)
	}
	if rec.Header().Get("X-Telemux-From-Cache") != "1" {
		t.Fatal("ответ из кэша должен быть помечен")
	}
	// Ревизия в кэше не новее известной - 304 после короткой паузы.
	start := time.Now()
	if rec := get(b, "relay-tok", "3"); rec.Code != http.StatusNotModified || rec.Header().Get("X-Telemux-From-Cache") != "1" {
		t.Fatalf("ждали 304 из кэша, получили %d, marker=%q", rec.Code, rec.Header().Get("X-Telemux-From-Cache"))
	}
	if time.Since(start) < 25*time.Millisecond {
		t.Fatal("без новой ревизии мост должен выдержать паузу, а не гнать агента по кругу")
	}
}

// Кэш не отдаётся чужому токену и без токена.
func TestBridge_CacheRequiresSameToken(t *testing.T) {
	m := &fakeMaster{rev: 3, token: "relay-tok"}
	b := newBridge(t, m.server(t).URL)
	get(b, "relay-tok", "0")
	m.setDown(true)
	for _, tok := range []string{"other", ""} {
		rec := get(b, tok, "0")
		if rec.Code == http.StatusOK {
			t.Fatalf("токен %q получил чужой бандл из кэша", tok)
		}
	}
}

// Мастер отверг токен - мост забывает кэш этого токена.
func TestBridge_UnauthorizedDropsCache(t *testing.T) {
	m := &fakeMaster{rev: 3, token: "relay-tok"}
	b := newBridge(t, m.server(t).URL)
	get(b, "relay-tok", "0")
	m.mu.Lock()
	m.token = "rotated"
	m.mu.Unlock()
	if rec := get(b, "relay-tok", "0"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ждали 401 от мастера, получили %d", rec.Code)
	}
	m.setDown(true)
	if rec := get(b, "relay-tok", "0"); rec.Code == http.StatusOK {
		t.Fatal("отозванный токен получил бандл из кэша")
	}
}

// Мастер недоступен и кэша нет - 503.
func TestBridge_NoCacheMasterDown(t *testing.T) {
	m := &fakeMaster{rev: 3, token: "relay-tok", down: true}
	b := newBridge(t, m.server(t).URL)
	if rec := get(b, "relay-tok", "0"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ждали 503, получили %d", rec.Code)
	}
}

// Первый мастер недоступен - мост идёт ко второму.
func TestBridge_FallsBackToNextMaster(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	m := &fakeMaster{rev: 3, token: "relay-tok"}
	b := newBridge(t, deadURL, m.server(t).URL)
	if rec := get(b, "relay-tok", "0"); rec.Code != http.StatusOK {
		t.Fatalf("ждали 200 через второй мастер, получили %d", rec.Code)
	}
}

// Отчёт уходит мастеру; при недоступном мастере мост принимает его (202) и
// дошлёт, когда мастер вернётся.
func TestBridge_ObservedQueuedWhenMasterDown(t *testing.T) {
	m := &fakeMaster{rev: 3, token: "relay-tok"}
	b := newBridge(t, m.server(t).URL)
	if rec := post(b, "relay-tok", report); rec.Code != http.StatusNoContent {
		t.Fatalf("при доступном мастере ждали 204, получили %d", rec.Code)
	}
	m.setDown(true)
	if rec := post(b, "relay-tok", report); rec.Code != http.StatusAccepted {
		t.Fatalf("при недоступном мастере ждали 202, получили %d", rec.Code)
	}
	m.setDown(false)
	b.FlushPending(t.Context())
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.reports) != 2 {
		t.Fatalf("отложенный отчёт не дошёл: %d отчётов", len(m.reports))
	}
}

// Отчёт с чужим для мастера токеном не ставится в очередь.
func TestBridge_ObservedUnauthorizedPassesThrough(t *testing.T) {
	m := &fakeMaster{rev: 3, token: "relay-tok"}
	b := newBridge(t, m.server(t).URL)
	if rec := post(b, "wrong", report); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ждали 401 от мастера, получили %d", rec.Code)
	}
}

func TestNew_RequiresMasters(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("мост без адресов мастера принят")
	}
}

// Мост ходит к мастеру по HTTPS с закреплённым сертификатом.
func TestBridge_PinnedMaster(t *testing.T) {
	m := &fakeMaster{rev: 3, token: "relay-tok"}
	srv := httptest.NewTLSServer(m.server(t).Config.Handler)
	defer srv.Close()
	b, err := New(Config{MasterURLs: []string{srv.URL}, MasterPin: tlspin.Fingerprint(srv.Certificate().Raw),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(b, "relay-tok", "0"); rec.Code != http.StatusOK {
		t.Fatalf("с верным отпечатком ждали 200, получили %d", rec.Code)
	}
	bad, _ := New(Config{MasterURLs: []string{srv.URL}, MasterPin: strings.Repeat("0", 64),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if rec := get(bad, "relay-tok", "0"); rec.Code == http.StatusOK {
		t.Fatal("мост принял чужой сертификат мастера")
	}
}

// Runtime-отчёт проходит насквозь; при недоступном мастере - 503 без очереди
// (снимок устареет, агент пришлёт свежий).
func TestBridge_RuntimePassThroughNoQueue(t *testing.T) {
	m := &fakeMaster{rev: 3, token: "relay-tok"}
	b := newBridge(t, m.server(t).URL)
	send := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/runtime", strings.NewReader(`{"target":"pr2","host":{"cpu_pct":1}}`))
		req.Header.Set("Authorization", "Bearer relay-tok")
		b.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send(); code != http.StatusNoContent || len(m.runtime) != 1 || m.runtime[0].Host.CPUPct != 1 {
		t.Fatalf("ждали 204 и отчёт у мастера, получили %d %+v", code, m.runtime)
	}
	m.setDown(true)
	if code := send(); code != http.StatusServiceUnavailable {
		t.Fatalf("мастер недоступен: ждали 503, получили %d", code)
	}
	m.setDown(false)
	b.FlushPending(context.Background())
	if len(m.runtime) != 1 {
		t.Fatalf("runtime-отчёт не должен копиться и досылаться, у мастера %d", len(m.runtime))
	}
}

// replicaServer - узел-реплика: на запись отвечает 409 not_master, как сервер telemux.
func replicaServer(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"not_master","error":"not_master","message":"узел в роли replica"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Первый адрес - реплика (409 not_master): мост идёт к следующему мастеру, как агент.
func TestBridge_NotMasterGoesToNext(t *testing.T) {
	var replicaHits int
	m := &fakeMaster{rev: 3, token: "relay-tok"}
	b := newBridge(t, replicaServer(t, &replicaHits).URL, m.server(t).URL)
	if rec := post(b, "relay-tok", report); rec.Code != http.StatusNoContent {
		t.Fatalf("observed: ждали 204 от второго мастера, получили %d: %s", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/runtime", strings.NewReader(`{"target":"pr2","host":{"cpu_pct":1}}`))
	req.Header.Set("Authorization", "Bearer relay-tok")
	b.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("runtime: ждали 204 от второго мастера, получили %d: %s", rec.Code, rec.Body.String())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.reports) != 1 || len(m.runtime) != 1 || replicaHits != 2 {
		t.Fatalf("отчёты должны дойти до мастера: observed=%d runtime=%d, реплика %d", len(m.reports), len(m.runtime), replicaHits)
	}
}

// Все адреса - реплики: агент получает 409 not_master как есть (это не сбой мастера).
func TestBridge_AllNotMasterPassesConflict(t *testing.T) {
	var hits int
	b := newBridge(t, replicaServer(t, &hits).URL, replicaServer(t, &hits).URL)
	rec := post(b, "relay-tok", report)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_master") || hits != 2 {
		t.Fatalf("ждали 409 not_master после обхода обоих адресов, получили %d (%d запросов): %s", rec.Code, hits, rec.Body.String())
	}
}
