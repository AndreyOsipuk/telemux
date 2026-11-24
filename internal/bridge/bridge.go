// Package bridge - мост на машине-выходе (ADR 0001, этап 4 плана).
//
// Релей ходит к мастеру только через туннель своей пары. Мост на машине-выходе
// принимает запросы агента релея из туннеля и пересылает их мастеру с тем же
// токеном релея. Последний бандл каждого токена мост держит в памяти и отдаёт
// его, когда мастер недоступен; отчёты релея при недоступном мастере ставит в
// очередь и досылает. Своего доступа к чужим бандлам у моста нет: кэш отдаётся
// только тому токену, который мастер принял.
//
// Для установки агента на новый релей мост так же пересылает /v1/join/* (скрипт,
// бинарь потоком, обмен одноразового токена) - без кэша и очереди.
//
// Мост с кодом выхода (Config.Code) помечает каждый запрос к мастеру заголовком
// X-Telemux-Bridge и ведёт сводку релеев, которые через него ходят (GET /v1/relays,
// спецификация relay-via-exit, R1).
package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/tlspin"
)

// BridgeHeader - заголовок с кодом выхода в каждом запросе моста к мастеру (R1.1).
const BridgeHeader = "X-Telemux-Bridge"

// Заголовки доставки списка путей от мастера агенту через мост.
const (
	AcceptRelayMastersHeader = "X-Telemux-Accept-Relay-Masters"
	RelayMastersHeader       = "X-Telemux-Relay-Masters"
)

// bridgeCodeRe - код выхода (как код машины): иное в заголовок не ставится.
var bridgeCodeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Config - настройки моста.
type Config struct {
	Code       string        // код машины-выхода; пусто - заголовок не ставится, сводка без кода
	MasterURLs []string      // адреса мастера по приоритету
	MasterPin  string        // sha256 сертификата мастера для https-адресов (закрепление)
	IdleWait   time.Duration // пауза перед 304 из кэша (чтобы не гонять агента по кругу); по умолчанию 5 с
	Log        *slog.Logger
}

type cacheEntry struct {
	target string
	rev    int64
	body   []byte
}

// relayStat - учёт одного релея в мосте (R1.2).
type relayStat struct {
	lastSeen  time.Time
	requests  int64
	masterOK  bool
	lastError string
}

// RelayInfo - строка сводки моста (контракт 5.1).
type RelayInfo struct {
	Code      string     `json:"code"`
	LastSeen  *time.Time `json:"last_seen"`
	Requests  int64      `json:"requests"`
	MasterOK  bool       `json:"master_ok"`
	LastError string     `json:"last_error"`
}

// RelaysSummary - ответ GET /v1/relays: релеи по коду, без токенов и их хешей.
type RelaysSummary struct {
	Bridge string      `json:"bridge"`
	Relays []RelayInfo `json:"relays"`
}

type pendingReport struct {
	token string
	body  []byte
}

// Bridge - мост одной машины-выхода.
type Bridge struct {
	cfg    Config
	client *http.Client
	stream *http.Client // join: без общего таймаута - бинарь идёт потоком со скоростью туннеля

	mu      sync.Mutex
	cache   map[string]cacheEntry    // sha256(токен) → последний бандл
	pending map[string]pendingReport // sha256(токен) → последний неотправленный отчёт
	// relayOf - код релея токена, подтверждённый мастером на /v1/desired.
	relayOf map[string]string     // sha256(токен) → код релея
	relays  map[string]*relayStat // код релея → учёт
	now     func() time.Time      // часы учёта релеев (подменяются в тестах)
}

// relayTTL - релей, который не ходил через мост дольше этого, забывается (U23).
const relayTTL = 24 * time.Hour

// New проверяет конфиг и создаёт мост.
func New(cfg Config) (*Bridge, error) {
	if len(cfg.MasterURLs) == 0 {
		return nil, errors.New("нужен хотя бы один адрес мастера")
	}
	if cfg.Code != "" && !bridgeCodeRe.MatchString(cfg.Code) {
		return nil, fmt.Errorf("код моста %q: строчные латиница, цифры, - и _, до 64 символов", cfg.Code)
	}
	if cfg.IdleWait <= 0 {
		cfg.IdleWait = 5 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.MasterPin != "" {
		tr.TLSClientConfig = tlspin.Config(cfg.MasterPin)
	}
	streamTr := tr.Clone()
	streamTr.ResponseHeaderTimeout = 30 * time.Second
	return &Bridge{
		cfg:     cfg,
		client:  &http.Client{Timeout: 90 * time.Second, Transport: tr}, // long-poll мастера - до 60 с
		stream:  &http.Client{Transport: streamTr},                      // ограничен контекстом запроса
		cache:   map[string]cacheEntry{},
		pending: map[string]pendingReport{},
		relayOf: map[string]string{},
		relays:  map[string]*relayStat{},
		now:     time.Now,
	}, nil
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func bearer(r *http.Request) string {
	t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return t
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg, "error": msg})
}

// Handler - HTTP-обработчик моста (слушает loopback, куда приходит туннель).
func (b *Bridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	// Сводка релеев для агента этого выхода (R1.4). Мост слушает loopback.
	mux.HandleFunc("GET /v1/relays", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(b.Relays())
	})
	mux.HandleFunc("GET /v1/desired", b.handleDesired)
	mux.HandleFunc("POST /v1/observed", b.handleObserved)
	mux.HandleFunc("POST /v1/runtime", b.handleRuntime)
	// Установка агента релея: скрипт, бинарь и обмен токена. Токена агента у
	// релея ещё нет - запрос авторизует одноразовый токен в пути, его проверяет мастер.
	mux.HandleFunc("GET /v1/join/{token}", b.handleJoin(http.MethodGet, ""))
	mux.HandleFunc("GET /v1/join/{token}/telemux", b.handleJoin(http.MethodGet, "/telemux"))
	mux.HandleFunc("POST /v1/join/{token}/agent", b.handleJoin(http.MethodPost, "/agent"))
	// Бинарь обновления telemt / mtg-multi для stack-apply на релее (ADR 0008).
	mux.HandleFunc("GET /v1/stack/blobs/{sha256}", b.handleStackBlob)
	return mux
}

// joinTokenRe - одноразовый токен установки: 64 hex. Иное мастеру не пересылается.
var joinTokenRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// handleJoin пересылает join-запрос мастерам по очереди и отдаёт ответ потоком
// (бинарь - около 20 МБ, в память целиком не читается). Сбой сети, 5xx или
// 409 not_master (адрес оказался репликой) - следующий мастер; если ответили
// только реплики, клиенту уходит последний 409. Кэша и очереди у join нет.
func (b *Bridge) handleJoin(method, suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		if !joinTokenRe.MatchString(token) {
			writeError(w, http.StatusNotFound, "not_found", "не найдено")
			return
		}
		pathQuery := "/v1/join/" + token + suffix
		if q := r.URL.Query().Get("format"); q == "env" || q == "json" {
			pathQuery += "?format=" + q
		}
		b.streamProxy(w, r, method, pathQuery, "", "join")
	}
}

// stackTargetRe - код машины в запросе бинаря обновления; иное мастеру не пересылается.
var stackTargetRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// handleStackBlob пересылает мастеру запрос бинаря обновления (ресурс stack, ADR 0008)
// с токеном агента и отдаёт ответ потоком: бинарь telemt или mtg-multi - десятки МБ.
// Что машине можно скачать, решает мастер по токену; мост проверяет только формат.
func (b *Bridge) handleStackBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha256")
	target := r.URL.Query().Get("target")
	if !joinTokenRe.MatchString(sha) || !stackTargetRe.MatchString(target) {
		writeError(w, http.StatusNotFound, "not_found", "не найдено")
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "unauthorized", "нужен токен агента")
		return
	}
	b.streamProxy(w, r, http.MethodGet, "/v1/stack/blobs/"+sha+"?target="+target, auth, "stack")
}

// streamProxy пересылает запрос мастерам по очереди и отдаёт ответ потоком (бинарь - около
// 20 МБ, в память целиком не читается). Сбой сети, 5xx или 409 not_master (адрес оказался
// репликой) - следующий мастер; если ответили только реплики, клиенту уходит последний 409.
// Кэша и очереди нет. auth - заголовок Authorization для мастера (пусто - без него).
func (b *Bridge) streamProxy(w http.ResponseWriter, r *http.Request, method, pathQuery, auth, what string) {
	var notMaster *upstream
	for _, base := range b.cfg.MasterURLs {
		req, err := http.NewRequestWithContext(r.Context(), method, strings.TrimRight(base, "/")+pathQuery, nil)
		if err != nil {
			continue
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		b.markBridge(req)
		resp, err := b.stream.Do(req)
		if err != nil {
			b.cfg.Log.Warn("мост: мастер не ответил", "what", what, "master", base, "err", err)
			continue
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			b.cfg.Log.Warn("мост: мастер ответил сбоем", "what", what, "master", base, "status", resp.StatusCode)
			continue
		}
		if resp.StatusCode == http.StatusConflict {
			// 409 - короткий JSON, его можно прочитать целиком и проверить код.
			data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if err != nil {
				b.cfg.Log.Warn("мост: ответ мастера не читается", "what", what, "master", base, "err", err)
				continue
			}
			up := &upstream{status: resp.StatusCode, header: resp.Header, body: data}
			if isNotMaster(resp.StatusCode, data) {
				b.cfg.Log.Warn("мост: адрес не мастер, запрос идёт к следующему", "what", what, "master", base)
				notMaster = up
				continue
			}
			writeUpstream(w, up)
			return
		}
		for _, h := range []string{"Content-Type", "Content-Length", "Cache-Control"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			b.cfg.Log.Warn("мост: ответ оборвался при передаче", "what", what, "master", base, "err", err)
		}
		_ = resp.Body.Close()
		return
	}
	if notMaster != nil {
		writeUpstream(w, notMaster)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "unavailable", "мастер недоступен")
}

// writeUpstream отдаёт клиенту прочитанный ответ мастера.
func writeUpstream(w http.ResponseWriter, up *upstream) {
	if ct := up.header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(up.status)
	_, _ = w.Write(up.body)
}

// upstream - один ответ мастера.
type upstream struct {
	status int
	header http.Header
	body   []byte
}

// isNotMaster - ответ узла-реплики на запись: 409 с кодом not_master.
func isNotMaster(status int, body []byte) bool {
	if status != http.StatusConflict {
		return false
	}
	var e struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(body, &e) == nil && e.Code == "not_master"
}

// forward отправляет запрос мастерам по очереди. Возвращает первый ответ, который
// не является сбоем сети, 5xx или 409 not_master (адрес оказался репликой - идём
// к следующему, как агент); nil - ни один мастер не ответил по делу. Если все
// ответившие - реплики, возвращается последний 409: агент сам решит, что делать.
func (b *Bridge) forward(ctx context.Context, method, pathQuery, token string, body []byte) *upstream {
	up, _ := b.forwardErr(ctx, method, pathQuery, token, body)
	return up
}

// forwardErr - forward и текст последней ошибки мастера (пусто - ответ по делу).
func (b *Bridge) forwardErr(ctx context.Context, method, pathQuery, token string, body []byte) (*upstream, string) {
	return b.forwardRequest(ctx, method, pathQuery, token, body, false)
}

func (b *Bridge) forwardRequest(ctx context.Context, method, pathQuery, token string, body []byte, wantRelayMasters bool) (*upstream, string) {
	var notMaster *upstream
	lastErr := "адрес мастера не задан"
	for _, base := range b.cfg.MasterURLs {
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+pathQuery, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if wantRelayMasters {
			req.Header.Set(AcceptRelayMastersHeader, "1")
		}
		b.markBridge(req)
		resp, err := b.client.Do(req)
		if err != nil {
			b.cfg.Log.Warn("мост: мастер не ответил", "master", base, "err", err)
			lastErr = "мастер не ответил: " + errClass(err) // в сводку - без адресов (U24)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil || resp.StatusCode >= 500 {
			b.cfg.Log.Warn("мост: мастер ответил сбоем", "master", base, "status", resp.StatusCode)
			lastErr = fmt.Sprintf("мастер ответил сбоем: HTTP %d", resp.StatusCode)
			continue
		}
		up := &upstream{status: resp.StatusCode, header: resp.Header, body: data}
		if isNotMaster(resp.StatusCode, data) {
			b.cfg.Log.Warn("мост: адрес не мастер, пробуем следующий", "master", base)
			notMaster = up
			lastErr = "ответили только реплики (not_master)"
			continue
		}
		return up, ""
	}
	return notMaster, lastErr
}

// errClass - класс сетевой ошибки для last_error сводки (U24): без адреса мастера,
// URL и query, которые есть в тексте *url.Error. Полная ошибка - только в лог.
func errClass(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var recErr tls.RecordHeaderError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "запрос отменён"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
		errors.As(err, &netErr) && netErr.Timeout():
		return "таймаут"
	case errors.As(err, &dnsErr):
		return "ошибка DNS"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "соединение отклонено"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "соединение сброшено"
	case errors.As(err, &certErr), errors.As(err, &recErr):
		return "ошибка TLS"
	}
	return "сетевая ошибка"
}

// markBridge ставит заголовок с кодом выхода (R1.1); без кода - ничего.
func (b *Bridge) markBridge(req *http.Request) {
	if b.cfg.Code != "" {
		req.Header.Set(BridgeHeader, b.cfg.Code)
	}
}

// touch учитывает запрос релея с токеном key (R1.2, R1.3). Токен, код которого
// мастер не подтвердил, не учитывается. masterErr - пусто, если мастер ответил.
func (b *Bridge) touch(key, masterErr string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	code, ok := b.relayOf[key]
	if !ok {
		return
	}
	st := b.relays[code]
	if st == nil {
		st = &relayStat{}
		b.relays[code] = st
	}
	st.lastSeen = b.now()
	st.requests++
	st.masterOK = masterErr == ""
	if masterErr != "" {
		st.lastError = masterErr
	}
}

// learn запоминает код релея токена по ответу мастера на /v1/desired.
func (b *Bridge) learn(key, code string) {
	if code == "" {
		return
	}
	b.mu.Lock()
	b.relayOf[key] = code
	b.mu.Unlock()
}

// forget - мастер отверг токен: дальше его запросы не учитываются.
func (b *Bridge) forget(key string) {
	b.mu.Lock()
	delete(b.relayOf, key)
	b.mu.Unlock()
}

// pruneRelaysLocked забывает релеи, которые не ходили через мост дольше relayTTL,
// и токены этих релеев (U23): иначе сводка росла бы вечно, а выведенный из работы
// релей висел бы в ней. Вернётся - его код снова подтвердит ответ на /v1/desired.
// Вызывается под b.mu.
func (b *Bridge) pruneRelaysLocked() {
	cut := b.now().Add(-relayTTL)
	for code, st := range b.relays {
		if st.lastSeen.Before(cut) {
			delete(b.relays, code)
		}
	}
	for key, code := range b.relayOf {
		if _, ok := b.relays[code]; !ok {
			delete(b.relayOf, key)
		}
	}
}

// Relays - сводка релеев моста по коду (контракт 5.1); пустой мост - relays: [].
// Релеи, молчащие дольше relayTTL, забываются (U23).
func (b *Bridge) Relays() RelaysSummary {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneRelaysLocked()
	out := RelaysSummary{Bridge: b.cfg.Code, Relays: make([]RelayInfo, 0, len(b.relays))}
	for code, st := range b.relays {
		seen := st.lastSeen.UTC()
		out.Relays = append(out.Relays, RelayInfo{Code: code, LastSeen: &seen, Requests: st.requests,
			MasterOK: st.masterOK, LastError: st.lastError})
	}
	sort.Slice(out.Relays, func(i, j int) bool { return out.Relays[i].Code < out.Relays[j].Code })
	return out
}

// masterErrOf - ошибка мастера для учёта релея: нет ответа по делу или ответили
// только реплики - ошибка; иначе мастер ответил (master_ok).
func masterErrOf(up *upstream, lastErr string) string {
	if up == nil {
		if lastErr == "" {
			lastErr = "мастер недоступен"
		}
		return lastErr
	}
	if isNotMaster(up.status, up.body) {
		return "ответили только реплики (not_master)"
	}
	return ""
}

func (b *Bridge) handleDesired(w http.ResponseWriter, r *http.Request) {
	token := bearer(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "нужен токен агента")
		return
	}
	key := hashToken(token)
	target := r.URL.Query().Get("target")

	wantRelayMasters := r.Header.Get(AcceptRelayMastersHeader) == "1"
	up, lastErr := b.forwardRequest(r.Context(), http.MethodGet, "/v1/desired?"+r.URL.RawQuery, token, nil, wantRelayMasters)
	if up != nil {
		switch {
		case up.status == http.StatusOK:
			var head struct {
				Target   string `json:"target"`
				Revision int64  `json:"revision"`
			}
			if err := json.Unmarshal(up.body, &head); err == nil && head.Target == target {
				b.mu.Lock()
				b.cache[key] = cacheEntry{target: target, rev: head.Revision, body: up.body}
				b.mu.Unlock()
				b.learn(key, head.Target) // код релея - из ответа мастера (R1.2)
			}
		case up.status == http.StatusNotModified:
			// 304 мастер отдаёт, только проверив токен и target запроса.
			b.learn(key, target)
		case up.status == http.StatusUnauthorized || up.status == http.StatusForbidden:
			b.mu.Lock()
			delete(b.cache, key) // токен отозван - кэш ему больше не положен
			b.mu.Unlock()
			b.forget(key)
		}
		b.touch(key, masterErrOf(up, lastErr))
		if ct := up.header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if wantRelayMasters && (up.status == http.StatusOK || up.status == http.StatusNotModified) {
			if paths := up.header.Get(RelayMastersHeader); paths != "" {
				w.Header().Set(RelayMastersHeader, paths)
			}
		}
		w.WriteHeader(up.status)
		_, _ = w.Write(up.body)
		return
	}

	// Мастер недоступен - кэш того же токена; релей при этом жив (R1.3).
	b.touch(key, masterErrOf(nil, lastErr))
	b.mu.Lock()
	entry, ok := b.cache[key]
	b.mu.Unlock()
	if !ok || entry.target != target {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "мастер недоступен, сохранённого состояния нет")
		return
	}
	known, _ := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
	if entry.rev > known {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Telemux-From-Cache", "1")
		_, _ = w.Write(entry.body)
		return
	}
	wait := b.cfg.IdleWait
	if d, err := time.ParseDuration(r.URL.Query().Get("wait")); err == nil && d < wait {
		wait = d
	}
	select {
	case <-time.After(wait):
	case <-r.Context().Done():
		return
	}
	w.WriteHeader(http.StatusNotModified)
}

func (b *Bridge) handleObserved(w http.ResponseWriter, r *http.Request) {
	token := bearer(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "нужен токен агента")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "отчёт не читается")
		return
	}
	up, lastErr := b.forwardErr(r.Context(), http.MethodPost, "/v1/observed", token, body)
	b.touchReply(token, up, lastErr)
	if up != nil {
		if up.status < 300 {
			b.mu.Lock()
			delete(b.pending, hashToken(token)) // свежий отчёт дошёл - старый не нужен
			b.mu.Unlock()
		}
		if ct := up.header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(up.status)
		_, _ = w.Write(up.body)
		return
	}
	b.mu.Lock()
	b.pending[hashToken(token)] = pendingReport{token: token, body: body}
	b.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

// handleRuntime пересылает runtime-отчёт мастеру как есть. В очередь не кладётся:
// снимок через минуту устареет, агент пришлёт свежий.
func (b *Bridge) handleRuntime(w http.ResponseWriter, r *http.Request) {
	token := bearer(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "нужен токен агента")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "отчёт не читается")
		return
	}
	if len(body) > 2<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "отчёт больше 2 МБ")
		return
	}
	up, lastErr := b.forwardErr(r.Context(), http.MethodPost, "/v1/runtime", token, body)
	b.touchReply(token, up, lastErr)
	if up == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "мастер недоступен")
		return
	}
	if ct := up.header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(up.status)
	_, _ = w.Write(up.body)
}

// touchReply учитывает отчёт релея по ответу мастера; токен, который мастер
// отверг (401/403), забывается и не учитывается.
func (b *Bridge) touchReply(token string, up *upstream, lastErr string) {
	key := hashToken(token)
	if up != nil && (up.status == http.StatusUnauthorized || up.status == http.StatusForbidden) {
		if up.status == http.StatusUnauthorized {
			b.forget(key)
		}
		return
	}
	b.touch(key, masterErrOf(up, lastErr))
}

// FlushPending досылает отложенные отчёты. Отчёт снимается с очереди при любом
// ответе мастера, кроме сбоя: 4xx повторять бессмысленно.
func (b *Bridge) FlushPending(ctx context.Context) {
	b.mu.Lock()
	b.pruneRelaysLocked() // молчащие релеи забываются и без чтения сводки (U23)
	items := make(map[string]pendingReport, len(b.pending))
	for k, v := range b.pending {
		items[k] = v
	}
	b.mu.Unlock()
	for key, p := range items {
		up := b.forward(ctx, http.MethodPost, "/v1/observed", p.token, p.body)
		if up == nil || isNotMaster(up.status, up.body) {
			continue // мастер не найден (идёт смена мастера) - дошлём позже
		}
		if up.status >= 300 {
			b.cfg.Log.Warn("мост: мастер отклонил отложенный отчёт", "status", up.status)
		}
		b.mu.Lock()
		if cur, ok := b.pending[key]; ok && bytes.Equal(cur.body, p.body) {
			delete(b.pending, key)
		}
		b.mu.Unlock()
	}
}

// Run слушает addr и раз в 30 с досылает отложенные отчёты; до отмены ctx.
func (b *Bridge) Run(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: b.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(sh)
				return
			case <-t.C:
				b.FlushPending(ctx)
			}
		}
	}()
	b.cfg.Log.Info("мост запущен", "addr", addr, "masters", strings.Join(b.cfg.MasterURLs, ","))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("мост: %w", err)
	}
	return nil
}
