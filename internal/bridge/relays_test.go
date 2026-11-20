package bridge

// Тесты спецификации relay-via-exit, раздел R1: мост знает свой релей.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// tokenMaster - мастер с несколькими токенами: токен -> код машины. Запоминает
// заголовок X-Telemux-Bridge каждого запроса.
type tokenMaster struct {
	mu      sync.Mutex
	down    bool
	tokens  map[string]string
	headers []string // X-Telemux-Bridge по порядку запросов ("-" - заголовка нет)
}

func (m *tokenMaster) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		h := "-"
		if v, ok := r.Header[http.CanonicalHeaderKey(BridgeHeader)]; ok {
			h = strings.Join(v, ",")
		}
		m.headers = append(m.headers, h)
		if m.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/join/") {
			_, _ = w.Write([]byte("#!/bin/sh\n"))
			return
		}
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		code, ok := m.tokens[tok]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"x"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/desired":
			if r.URL.Query().Get("target") != code {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if r.URL.Query().Get("rev") == "7" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_ = json.NewEncoder(w).Encode(store.Bundle{Target: code, Revision: 7, Resources: []store.Desired{}})
		case "/v1/observed", "/v1/runtime":
			w.WriteHeader(http.StatusNoContent)
		case "/v1/stack/blobs/" + strings.Repeat("a", 64):
			_, _ = w.Write([]byte("bin"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (m *tokenMaster) setDown(v bool) {
	m.mu.Lock()
	m.down = v
	m.mu.Unlock()
}

func (m *tokenMaster) lastHeaders() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.headers...)
}

func newCodedBridge(t *testing.T, code string, masters ...string) *Bridge {
	t.Helper()
	b, err := New(Config{Code: code, MasterURLs: masters, IdleWait: 20 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// call - запрос агента релея к мосту.
func call(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// relaysOf - сводка моста через GET /v1/relays.
func relaysOf(t *testing.T, h http.Handler) (RelaysSummary, string) {
	t.Helper()
	rec := call(t, h, http.MethodGet, "/v1/relays", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/relays: %d %s", rec.Code, rec.Body.String())
	}
	var s RelaysSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("сводка не JSON: %v", err)
	}
	return s, rec.Body.String()
}

func TestBridge_R1_1_HeaderWithCode(t *testing.T) {
	m := &tokenMaster{tokens: map[string]string{"tok-pr2": "pr2"}}
	srv := m.server(t)
	h := newCodedBridge(t, "ps2", srv.URL).Handler()

	call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=0&wait=1s", "tok-pr2", "")
	call(t, h, http.MethodPost, "/v1/observed", "tok-pr2", `{"target":"pr2"}`)
	call(t, h, http.MethodPost, "/v1/runtime", "tok-pr2", `{"target":"pr2"}`)
	call(t, h, http.MethodGet, "/v1/join/"+strings.Repeat("b", 64), "", "")
	call(t, h, http.MethodGet, "/v1/stack/blobs/"+strings.Repeat("a", 64)+"?target=pr2", "tok-pr2", "")

	got := m.lastHeaders()
	if len(got) != 5 {
		t.Fatalf("ждали 5 запросов к мастеру, было %d: %v", len(got), got)
	}
	for i, v := range got {
		if v != "ps2" {
			t.Fatalf("запрос %d: %s = %q, ждали ps2", i, BridgeHeader, v)
		}
	}
}

func TestBridge_R1_1_NoHeaderWithoutCode(t *testing.T) {
	m := &tokenMaster{tokens: map[string]string{"tok-pr2": "pr2"}}
	srv := m.server(t)
	h := newCodedBridge(t, "", srv.URL).Handler()

	call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=0&wait=1s", "tok-pr2", "")
	call(t, h, http.MethodPost, "/v1/observed", "tok-pr2", `{"target":"pr2"}`)
	call(t, h, http.MethodGet, "/v1/join/"+strings.Repeat("b", 64), "", "")
	for i, v := range m.lastHeaders() {
		if v != "-" {
			t.Fatalf("запрос %d: без кода моста заголовка быть не должно, есть %q", i, v)
		}
	}
}

func TestBridge_R1_1_BadCodeRejected(t *testing.T) {
	for _, code := range []string{"PS2", "ps 2", "ps2\r\nX: y", strings.Repeat("a", 65)} {
		if _, err := New(Config{Code: code, MasterURLs: []string{"http://127.0.0.1:1"}}); err == nil {
			t.Fatalf("код моста %q должен быть отвергнут", code)
		}
	}
}

func TestBridge_R1_2_CodeOnlyFromDesiredAnswer(t *testing.T) {
	m := &tokenMaster{tokens: map[string]string{"tok-pr2": "pr2"}}
	srv := m.server(t)
	h := newCodedBridge(t, "ps2", srv.URL).Handler()

	// Отчёт до первого desired: код релея мосту ещё неизвестен - в сводке пусто.
	call(t, h, http.MethodPost, "/v1/observed", "tok-pr2", `{"target":"pr2"}`)
	if s, _ := relaysOf(t, h); len(s.Relays) != 0 {
		t.Fatalf("до ответа мастера на desired релей не должен попасть в сводку: %+v", s.Relays)
	}

	call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=0&wait=1s", "tok-pr2", "")
	call(t, h, http.MethodPost, "/v1/observed", "tok-pr2", `{"target":"pr2"}`)
	call(t, h, http.MethodPost, "/v1/runtime", "tok-pr2", `{"target":"pr2"}`)

	s, _ := relaysOf(t, h)
	if len(s.Relays) != 1 {
		t.Fatalf("ждали один релей, сводка: %+v", s.Relays)
	}
	r := s.Relays[0]
	if r.Code != "pr2" || r.Requests != 3 || !r.MasterOK || r.LastError != "" || r.LastSeen == nil {
		t.Fatalf("сводка релея: %+v", r)
	}
	if time.Since(*r.LastSeen) > time.Minute {
		t.Fatalf("last_seen не свежий: %v", r.LastSeen)
	}
}

func TestBridge_R1_2_NotModifiedConfirmsTarget(t *testing.T) {
	// Мост перезапустился, агент уже знает ревизию: мастер отвечает 304 на target
	// из запроса (токен и target он проверил) - релей учитывается.
	m := &tokenMaster{tokens: map[string]string{"tok-pr2": "pr2"}}
	srv := m.server(t)
	h := newCodedBridge(t, "ps2", srv.URL).Handler()
	rec := call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=7&wait=1s", "tok-pr2", "")
	if rec.Code != http.StatusNotModified {
		t.Fatalf("ждали 304, %d", rec.Code)
	}
	s, _ := relaysOf(t, h)
	if len(s.Relays) != 1 || s.Relays[0].Code != "pr2" || s.Relays[0].Requests != 1 {
		t.Fatalf("сводка после 304: %+v", s.Relays)
	}
}

func TestBridge_R1_2_UnknownTokenNotCounted(t *testing.T) {
	m := &tokenMaster{tokens: map[string]string{"tok-pr2": "pr2"}}
	srv := m.server(t)
	h := newCodedBridge(t, "ps2", srv.URL).Handler()

	// Чужой токен с target pr2: мастер его не признал.
	rec := call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=0&wait=1s", "tok-evil", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("ждали 401, %d", rec.Code)
	}
	call(t, h, http.MethodPost, "/v1/observed", "tok-evil", `{"target":"pr2"}`)
	if s, _ := relaysOf(t, h); len(s.Relays) != 0 {
		t.Fatalf("непризнанный токен попал в сводку: %+v", s.Relays)
	}

	// Токен отозвали после признания: дальнейшие запросы не считаются.
	call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=0&wait=1s", "tok-pr2", "")
	m.mu.Lock()
	delete(m.tokens, "tok-pr2")
	m.mu.Unlock()
	call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=0&wait=1s", "tok-pr2", "")
	call(t, h, http.MethodPost, "/v1/observed", "tok-pr2", `{"target":"pr2"}`)
	s, _ := relaysOf(t, h)
	if len(s.Relays) != 1 || s.Relays[0].Requests != 1 {
		t.Fatalf("после отзыва токена запросы считаться не должны: %+v", s.Relays)
	}
}

func TestBridge_R1_3_MasterDownRelayCounted(t *testing.T) {
	m := &tokenMaster{tokens: map[string]string{"tok-pr2": "pr2"}}
	srv := m.server(t)
	h := newCodedBridge(t, "ps2", srv.URL).Handler()
	call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=0&wait=1s", "tok-pr2", "")
	first, _ := relaysOf(t, h)

	m.setDown(true)
	time.Sleep(5 * time.Millisecond)
	call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=7&wait=10ms", "tok-pr2", "") // из кэша
	call(t, h, http.MethodPost, "/v1/observed", "tok-pr2", `{"target":"pr2"}`)          // в очередь
	call(t, h, http.MethodPost, "/v1/runtime", "tok-pr2", `{"target":"pr2"}`)           // 503

	s, _ := relaysOf(t, h)
	if len(s.Relays) != 1 {
		t.Fatalf("сводка: %+v", s.Relays)
	}
	r := s.Relays[0]
	if r.MasterOK || r.LastError == "" || r.Requests != 4 {
		t.Fatalf("мастер лёг: ждали master_ok=false, ошибку и 4 запроса, %+v", r)
	}
	if !r.LastSeen.After(*first.Relays[0].LastSeen) {
		t.Fatalf("last_seen должен сдвинуться и при лежащем мастере: %v -> %v", first.Relays[0].LastSeen, r.LastSeen)
	}

	m.setDown(false)
	call(t, h, http.MethodPost, "/v1/runtime", "tok-pr2", `{"target":"pr2"}`)
	s, _ = relaysOf(t, h)
	if !s.Relays[0].MasterOK {
		t.Fatalf("мастер вернулся - master_ok=true: %+v", s.Relays[0])
	}
}

func TestBridge_R1_4_RelaysContract(t *testing.T) {
	m := &tokenMaster{tokens: map[string]string{"tok-a": "pr3", "tok-b": "pr1"}}
	srv := m.server(t)
	h := newCodedBridge(t, "ps2", srv.URL).Handler()

	// Пустой мост - relays: [] (не null).
	_, raw := relaysOf(t, h)
	if !strings.Contains(raw, `"relays":[]`) || !strings.Contains(raw, `"bridge":"ps2"`) {
		t.Fatalf("пустая сводка: %s", raw)
	}

	call(t, h, http.MethodGet, "/v1/desired?target=pr3&rev=0&wait=1s", "tok-a", "")
	call(t, h, http.MethodGet, "/v1/desired?target=pr1&rev=0&wait=1s", "tok-b", "")
	s, raw := relaysOf(t, h)
	if s.Bridge != "ps2" || len(s.Relays) != 2 || s.Relays[0].Code != "pr1" || s.Relays[1].Code != "pr3" {
		t.Fatalf("сводка по коду: %s", raw)
	}
	// Поля контракта 5.1 и ничего о токенах.
	var generic struct {
		Relays []map[string]any `json:"relays"`
	}
	_ = json.Unmarshal([]byte(raw), &generic)
	for _, k := range []string{"code", "last_seen", "requests", "master_ok", "last_error"} {
		if _, ok := generic.Relays[0][k]; !ok {
			t.Fatalf("в сводке нет поля %s: %s", k, raw)
		}
	}
	for _, secret := range []string{"tok-a", "tok-b", hashToken("tok-a"), hashToken("tok-b")} {
		if strings.Contains(raw, secret) {
			t.Fatalf("в сводке токен или его хеш: %s", raw)
		}
	}
}

// Заголовок X-Telemux-Bridge от клиента моста до мастера не доходит: мост с кодом
// ставит свой, без кода - не ставит. Иначе релей мог бы назваться чужим мостом.
func TestBridge_R1_1_ClientHeaderNotForwarded(t *testing.T) {
	for _, code := range []string{"ps2", ""} {
		m := &tokenMaster{tokens: map[string]string{"tok-pr2": "pr2"}}
		srv := m.server(t)
		h := newCodedBridge(t, code, srv.URL).Handler()
		spoof := func(method, path, token, body string) {
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			req.Header.Set(BridgeHeader, "ps9")
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
		spoof(http.MethodGet, "/v1/desired?target=pr2&rev=0&wait=1s", "tok-pr2", "")
		spoof(http.MethodPost, "/v1/observed", "tok-pr2", `{"target":"pr2"}`)
		spoof(http.MethodPost, "/v1/runtime", "tok-pr2", `{"target":"pr2"}`)
		spoof(http.MethodGet, "/v1/join/"+strings.Repeat("b", 64), "", "")
		spoof(http.MethodGet, "/v1/stack/blobs/"+strings.Repeat("a", 64)+"?target=pr2", "tok-pr2", "")
		want := code
		if want == "" {
			want = "-"
		}
		got := m.lastHeaders()
		if len(got) != 5 {
			t.Fatalf("код %q: ждали 5 запросов к мастеру, было %v", code, got)
		}
		for i, v := range got {
			if v != want {
				t.Fatalf("код моста %q, запрос %d: мастер получил %s = %q, ждали %q", code, i, BridgeHeader, v, want)
			}
		}
	}
}

// U23: релей, который не ходил через мост 24 ч, из сводки забывается - при чтении
// сводки и в цикле досылки отчётов.
func TestBridge_U23_ForgetsSilentRelayAfter24h(t *testing.T) {
	m := &tokenMaster{tokens: map[string]string{"tok-a": "pr1", "tok-b": "pr2"}}
	srv := m.server(t)
	b := newCodedBridge(t, "ps2", srv.URL)
	clock := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return clock }
	h := b.Handler()
	call(t, h, http.MethodGet, "/v1/desired?target=pr1&rev=0&wait=1s", "tok-a", "")
	call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=0&wait=1s", "tok-b", "")

	clock = clock.Add(23 * time.Hour)
	call(t, h, http.MethodPost, "/v1/runtime", "tok-b", `{"target":"pr2"}`) // pr2 жив
	if s, _ := relaysOf(t, h); len(s.Relays) != 2 {
		t.Fatalf("23 ч - ещё помним оба релея: %+v", s.Relays)
	}

	clock = clock.Add(time.Hour + time.Second) // pr1 молчит 24 ч с секундой
	s, _ := relaysOf(t, h)
	if len(s.Relays) != 1 || s.Relays[0].Code != "pr2" {
		t.Fatalf("pr1 молчит больше 24 ч - забыт при чтении: %+v", s.Relays)
	}
	// Токен забытого релея тоже забыт: без подтверждения кода его запрос не учитывается.
	call(t, h, http.MethodPost, "/v1/observed", "tok-a", `{"target":"pr1"}`)
	if s, _ := relaysOf(t, h); len(s.Relays) != 1 {
		t.Fatalf("отчёт без подтверждённого кода не возвращает релей: %+v", s.Relays)
	}

	// Чистка в цикле досылки, без чтения сводки.
	clock = clock.Add(25 * time.Hour)
	b.FlushPending(context.Background())
	b.mu.Lock()
	n, k := len(b.relays), len(b.relayOf)
	b.mu.Unlock()
	if n != 0 || k != 0 {
		t.Fatalf("FlushPending забывает молчащие релеи и их токены: relays=%d relayOf=%d", n, k)
	}
}

// U24: last_error - класс ошибки, без адресов мастера, URL и query.
func TestBridge_U24_LastErrorWithoutAddresses(t *testing.T) {
	m := &tokenMaster{tokens: map[string]string{"tok-pr2": "pr2"}}
	srv := m.server(t)
	dead := "http://127.0.0.1:1"
	b := newCodedBridge(t, "ps2", srv.URL)
	h := b.Handler()
	call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=0&wait=1s", "tok-pr2", "")
	b.cfg.MasterURLs = []string{dead} // мастер пропал
	call(t, h, http.MethodGet, "/v1/desired?target=pr2&rev=7&wait=10ms", "tok-pr2", "")
	s, _ := relaysOf(t, h)
	if len(s.Relays) != 1 || s.Relays[0].LastError == "" {
		t.Fatalf("ждали ошибку мастера: %+v", s.Relays)
	}
	e := s.Relays[0].LastError
	for _, bad := range []string{"127.0.0.1", "http", "/v1/", "target=", "?", ":1"} {
		if strings.Contains(e, bad) {
			t.Fatalf("last_error содержит %q: %q", bad, e)
		}
	}
	if e != "мастер не ответил: соединение отклонено" {
		t.Fatalf("класс ошибки: %q", e)
	}
}

func TestBridge_U24_ErrClass(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "таймаут"},
		{&url.Error{Op: "Get", URL: "http://10.0.0.1/v1/desired?target=pr2", Err: &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}}, "таймаут"},
		{&url.Error{Op: "Get", URL: "http://x", Err: &net.DNSError{Err: "no such host", Name: "x"}}, "ошибка DNS"},
		{&url.Error{Op: "Get", URL: "http://x", Err: syscall.ECONNREFUSED}, "соединение отклонено"},
		{&url.Error{Op: "Get", URL: "http://x", Err: syscall.ECONNRESET}, "соединение сброшено"},
		{&url.Error{Op: "Get", URL: "https://x", Err: &tls.CertificateVerificationError{Err: errors.New("x")}}, "ошибка TLS"},
		{context.Canceled, "запрос отменён"},
		{errors.New("http://10.0.0.1:443 что-то ещё"), "сетевая ошибка"},
	}
	for _, c := range cases {
		if got := errClass(c.err); got != c.want {
			t.Fatalf("%v: %q, ждали %q", c.err, got, c.want)
		}
	}
}
