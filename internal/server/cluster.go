package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// ClusterStore — операции реестра кластера (реализует store.Store на master).
type ClusterStore interface {
	ListNodes(ctx context.Context) ([]store.Node, error)
	UpsertNode(ctx context.Context, n store.Node) error
	CreateJoinToken(ctx context.Context, token string, ttl time.Duration) error
	ConsumeJoinToken(ctx context.Context, token string) (bool, error)
}

// newToken — 32 случайных байта в hex (одноразовый join-token).
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// clusterAuthed проверяет Authorization: Bearer <ClusterSecret>.
func (s *Server) clusterAuthed(r *http.Request) bool {
	if s.deps.ClusterSecret == "" {
		return false
	}
	h := r.Header.Get("Authorization")
	return strings.TrimPrefix(h, "Bearer ") == s.deps.ClusterSecret
}

func (s *Server) routesCluster() {
	// Список нод — для веб-морды (за панель-авторизацией).
	s.mux.HandleFunc("GET /api/nodes", func(w http.ResponseWriter, r *http.Request) {
		nodes, err := s.deps.Cluster.ListNodes(r.Context())
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		writeJSON(w, map[string]any{"nodes": nodes})
	})

	// Heartbeat агента ноды → master обновляет реестр.
	s.mux.HandleFunc("POST /api/cluster/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !s.clusterAuthed(r) {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("неверный cluster-secret"), s.deps.Log)
			return
		}
		var n store.Node
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil || n.Code == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON с непустым code"), s.deps.Log)
			return
		}
		if err := s.deps.Cluster.UpsertNode(r.Context(), n); err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	// Создать одноразовый join-token (кнопка «Add node» в UI).
	s.mux.HandleFunc("POST /api/cluster/join-token", func(w http.ResponseWriter, r *http.Request) {
		if !s.clusterAuthed(r) {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("неверный cluster-secret"), s.deps.Log)
			return
		}
		tok, err := newToken()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err, s.deps.Log)
			return
		}
		if err := s.deps.Cluster.CreateJoinToken(r.Context(), tok, time.Hour); err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		base := s.deps.PublicURL
		writeJSON(w, map[string]any{
			"token":   tok,
			"expires": "1h",
			"command": fmt.Sprintf("curl -fsSL %s/join/%s | sudo bash", base, tok),
		})
	})

	// Нода забирает join-bundle по токену (одноразово).
	s.mux.HandleFunc("GET /join/{token}", func(w http.ResponseWriter, r *http.Request) {
		tok := r.PathValue("token")
		ok, err := s.deps.Cluster.ConsumeJoinToken(r.Context(), tok)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		if !ok {
			writeErr(w, http.StatusGone, fmt.Errorf("токен недействителен/использован/истёк"), s.deps.Log)
			return
		}
		// Bundle: то, что нужно ноде для присоединения. Поля Patroni/repl — TODO (этап 2).
		writeJSON(w, map[string]any{
			"master_url": s.deps.PublicURL,
			"version":    s.deps.Version,
			"note":       "join-bundle: дальше — конфиг Patroni/реплики + mTLS-cert (в разработке)",
		})
	})
}
