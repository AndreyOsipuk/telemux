package bridge

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const joinTok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// joinMaster - мастер с join-ручками: считает запросы, отдаёт бинарь кусками.
type joinMaster struct {
	mu       sync.Mutex
	status   int // 0 - обычные ответы
	requests []string
	auth     []string
	binary   []byte
	gate     chan struct{} // если задан - вторая половина бинаря ждёт его закрытия
}

func (m *joinMaster) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.requests = append(m.requests, r.Method+" "+r.URL.RequestURI())
		m.auth = append(m.auth, r.Header.Get("Authorization"))
		status, bin, gate := m.status, m.binary, m.gate
		m.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/join/"+joinTok:
			w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
			_, _ = w.Write([]byte("#!/usr/bin/env bash\necho ok\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/join/"+joinTok+"/telemux":
			w.Header().Set("Content-Type", "application/octet-stream")
			half := len(bin) / 2
			_, _ = w.Write(bin[:half])
			w.(http.Flusher).Flush()
			if gate != nil {
				<-gate
			}
			_, _ = w.Write(bin[half:])
		case r.Method == http.MethodPost && r.URL.Path == "/v1/join/"+joinTok+"/agent":
			if r.URL.Query().Get("format") == "env" {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = w.Write([]byte("agenttoken\nTELEMUX_AGENT_TARGET=pr1\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"target":"pr1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"не найдено"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (m *joinMaster) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func TestJoin_ForwardsPaths(t *testing.T) {
	m := &joinMaster{binary: []byte("ELF-binary")}
	b := newBridge(t, m.server(t).URL)
	h := b.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/join/"+joinTok, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "#!/usr/bin/env bash") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/x-shellscript") {
		t.Fatalf("скрипт: %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/join/"+joinTok+"/telemux", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ELF-binary" || rec.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("бинарь: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/join/"+joinTok+"/agent?format=env", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "agenttoken\nTELEMUX_AGENT_TARGET=pr1\n" {
		t.Fatalf("обмен: %d %q", rec.Code, rec.Body.String())
	}
	// 404 мастера - как есть, без перебора.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/join/"+strings.Repeat("ab", 32)+"/agent", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("чужой токен: ждали 404, получили %d", rec.Code)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	want := []string{"GET /v1/join/" + joinTok, "GET /v1/join/" + joinTok + "/telemux",
		"POST /v1/join/" + joinTok + "/agent?format=env", "POST /v1/join/" + strings.Repeat("ab", 32) + "/agent"}
	if strings.Join(m.requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("мастер получил:\n%s\nждали\n%s", strings.Join(m.requests, "\n"), strings.Join(want, "\n"))
	}
	for _, a := range m.auth {
		if a != "" {
			t.Fatalf("join мостом идёт без токена агента, а пришло %q", a)
		}
	}
}

// Мусор в токене или параметрах мастеру не уходит.
func TestJoin_RejectsGarbage(t *testing.T) {
	m := &joinMaster{}
	b := newBridge(t, m.server(t).URL)
	for _, path := range []string{"/v1/join/abc", "/v1/join/" + strings.ToUpper(joinTok), "/v1/join/" + joinTok[:63] + "g/telemux"} {
		rec := httptest.NewRecorder()
		b.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: ждали 404, получили %d", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/join/"+joinTok+"/agent?format=x&evil=1", nil))
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) != 1 || m.requests[0] != "POST /v1/join/"+joinTok+"/agent" {
		t.Fatalf("лишнее ушло мастеру: %v", m.requests)
	}
}

// Перебор мастеров: недоступный и 5xx пропускаются, ответ - от первого живого.
func TestJoin_MasterFailover(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downURL := down.URL
	down.Close() // соединение отвергается
	broken := &joinMaster{status: http.StatusServiceUnavailable}
	good := &joinMaster{binary: []byte("ELF")}
	b := newBridge(t, downURL, broken.server(t).URL, good.server(t).URL)

	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/join/"+joinTok, nil))
	if rec.Code != http.StatusOK || broken.count() != 1 || good.count() != 1 {
		t.Fatalf("перебор: %d, сломанный %d, живой %d", rec.Code, broken.count(), good.count())
	}
	// Все мастера недоступны - 503.
	b2 := newBridge(t, downURL, broken.server(t).URL)
	rec = httptest.NewRecorder()
	b2.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/join/"+joinTok+"/agent", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("все мастера лежат: ждали 503, получили %d", rec.Code)
	}
}

// Реплика (409 not_master) на join пропускается, как в forward; прочий 409 -
// ответ мастера по делу, отдаётся как есть без перебора.
func TestJoin_NotMasterGoesToNext(t *testing.T) {
	var replicaHits int
	good := &joinMaster{binary: []byte("ELF")}
	b := newBridge(t, replicaServer(t, &replicaHits).URL, good.server(t).URL)
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/join/"+joinTok+"/agent?format=env", nil))
	if rec.Code != http.StatusOK || replicaHits != 1 || good.count() != 1 {
		t.Fatalf("реплика первой: %d, реплика %d, мастер %d", rec.Code, replicaHits, good.count())
	}

	// Только реплики - клиенту 409 not_master, а не 503.
	var hits int
	b2 := newBridge(t, replicaServer(t, &hits).URL, replicaServer(t, &hits).URL)
	rec = httptest.NewRecorder()
	b2.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/join/"+joinTok+"/agent", nil))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_master") || hits != 2 {
		t.Fatalf("все реплики: ждали 409 not_master, получили %d (%d запросов): %s", rec.Code, hits, rec.Body.String())
	}

	// Обычный 409 мастера - без перебора.
	conflict := &joinMaster{status: http.StatusConflict}
	next := &joinMaster{}
	b3 := newBridge(t, conflict.server(t).URL, next.server(t).URL)
	rec = httptest.NewRecorder()
	b3.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/join/"+joinTok, nil))
	if rec.Code != http.StatusConflict || conflict.count() != 1 || next.count() != 0 {
		t.Fatalf("409 мастера: %d, первый %d, второй %d", rec.Code, conflict.count(), next.count())
	}
}

// flushRecorder - ResponseWriter, который сообщает о первых записанных байтах.
type flushRecorder struct {
	mu     sync.Mutex
	header http.Header
	buf    bytes.Buffer
	first  chan struct{}
	once   sync.Once
}

func (f *flushRecorder) Header() http.Header { return f.header }
func (f *flushRecorder) WriteHeader(int)     {}
func (f *flushRecorder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.once.Do(func() { close(f.first) })
	return f.buf.Write(p)
}

// Бинарь идёт потоком: первая половина доходит до клиента раньше, чем мастер
// отдал вторую (мост не читает ответ в память целиком).
func TestJoin_BinaryStreams(t *testing.T) {
	bin := bytes.Repeat([]byte("0123456789"), 400_000) // 4 МБ
	gate := make(chan struct{})
	m := &joinMaster{binary: bin, gate: gate}
	b := newBridge(t, m.server(t).URL)
	w := &flushRecorder{header: http.Header{}, first: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		b.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/join/"+joinTok+"/telemux", nil))
		close(done)
	}()
	select {
	case <-w.first:
	case <-time.After(5 * time.Second):
		close(gate)
		t.Fatal("клиент не получил ни байта, пока мастер не дописал бинарь: мост буферизует")
	}
	close(gate)
	<-done
	w.mu.Lock()
	defer w.mu.Unlock()
	if !bytes.Equal(w.buf.Bytes(), bin) {
		t.Fatalf("бинарь искажён: %d байт из %d", w.buf.Len(), len(bin))
	}
}
