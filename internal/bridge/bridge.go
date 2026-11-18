// Package bridge - мост на машине-выходе (ADR 0001, этап 4 плана).
//
// Релей ходит к мастеру только через туннель своей пары. Мост на машине-выходе
// принимает запросы агента релея из туннеля и пересылает их мастеру с тем же
// токеном релея. Последний бандл каждого токена мост держит в памяти и отдаёт
// его, когда мастер недоступен; отчёты релея при недоступном мастере ставит в
// очередь и досылает. Своего доступа к чужим бандлам у моста нет: кэш отдаётся
// только тому токену, который мастер принял.
package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config - настройки моста.
type Config struct {
	MasterURLs []string      // адреса мастера по приоритету
	IdleWait   time.Duration // пауза перед 304 из кэша (чтобы не гонять агента по кругу); по умолчанию 5 с
	Log        *slog.Logger
}

type cacheEntry struct {
	target string
	rev    int64
	body   []byte
}

type pendingReport struct {
	token string
	body  []byte
}

// Bridge - мост одной машины-выхода.
type Bridge struct {
	cfg    Config
	client *http.Client

	mu      sync.Mutex
	cache   map[string]cacheEntry    // sha256(токен) → последний бандл
	pending map[string]pendingReport // sha256(токен) → последний неотправленный отчёт
}

// New проверяет конфиг и создаёт мост.
func New(cfg Config) (*Bridge, error) {
	if len(cfg.MasterURLs) == 0 {
		return nil, errors.New("нужен хотя бы один адрес мастера")
	}
	if cfg.IdleWait <= 0 {
		cfg.IdleWait = 5 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Bridge{
		cfg:     cfg,
		client:  &http.Client{Timeout: 90 * time.Second}, // long-poll мастера - до 60 с
		cache:   map[string]cacheEntry{},
		pending: map[string]pendingReport{},
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
	mux.HandleFunc("GET /v1/desired", b.handleDesired)
	mux.HandleFunc("POST /v1/observed", b.handleObserved)
	return mux
}

// upstream - один ответ мастера.
type upstream struct {
	status int
	header http.Header
	body   []byte
}

// forward отправляет запрос мастерам по очереди. Возвращает первый ответ, который
// не является сбоем сети или 5xx; nil - ни один мастер не ответил по делу.
func (b *Bridge) forward(ctx context.Context, method, pathQuery, token string, body []byte) *upstream {
	for _, base := range b.cfg.MasterURLs {
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+pathQuery, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := b.client.Do(req)
		if err != nil {
			b.cfg.Log.Warn("мост: мастер не ответил", "master", base, "err", err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil || resp.StatusCode >= 500 {
			b.cfg.Log.Warn("мост: мастер ответил сбоем", "master", base, "status", resp.StatusCode)
			continue
		}
		return &upstream{status: resp.StatusCode, header: resp.Header, body: data}
	}
	return nil
}

func (b *Bridge) handleDesired(w http.ResponseWriter, r *http.Request) {
	token := bearer(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "нужен токен агента")
		return
	}
	key := hashToken(token)
	target := r.URL.Query().Get("target")

	if up := b.forward(r.Context(), http.MethodGet, "/v1/desired?"+r.URL.RawQuery, token, nil); up != nil {
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
			}
		case up.status == http.StatusUnauthorized || up.status == http.StatusForbidden:
			b.mu.Lock()
			delete(b.cache, key) // токен отозван - кэш ему больше не положен
			b.mu.Unlock()
		}
		if ct := up.header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(up.status)
		_, _ = w.Write(up.body)
		return
	}

	// Мастер недоступен - кэш того же токена.
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
	if up := b.forward(r.Context(), http.MethodPost, "/v1/observed", token, body); up != nil {
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

// FlushPending досылает отложенные отчёты. Отчёт снимается с очереди при любом
// ответе мастера, кроме сбоя: 4xx повторять бессмысленно.
func (b *Bridge) FlushPending(ctx context.Context) {
	b.mu.Lock()
	items := make(map[string]pendingReport, len(b.pending))
	for k, v := range b.pending {
		items[k] = v
	}
	b.mu.Unlock()
	for key, p := range items {
		up := b.forward(ctx, http.MethodPost, "/v1/observed", p.token, p.body)
		if up == nil {
			continue
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
