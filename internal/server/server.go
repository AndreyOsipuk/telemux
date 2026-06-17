// Package server — демон telemux (`telemux serve`): HTTP API + health + дашборд +
// периодический автономный sync-loop (нода приводит свой telemt к локальному PG).
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/role"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// Store — то, что демон ждёт от локального PG.
type Store interface {
	role.RecoveryChecker                                              // IsInRecovery
	ListDesired(ctx context.Context) ([]telemtsync.DesiredUser, error)
}

// Deps — зависимости демона.
type Deps struct {
	Store    Store
	Node     syncpkg.NodeAPI
	Version  string
	Interval time.Duration   // период автосинхры (0 → 60с)
	SyncOpts syncpkg.Options // режим (shadow/apply) и пр.
	Log      *slog.Logger

	// Кластер (опционально; nil → одно-нодовый режим без реестра/add-node).
	Cluster       ClusterStore
	ClusterSecret string // Bearer для heartbeat/join-token
	PublicURL     string // внешний URL мастера (для join-команды)
}

// Server — HTTP-демон + фоновый sync-loop.
type Server struct {
	deps Deps
	mux  *http.ServeMux

	mu       sync.RWMutex
	lastSync syncStatus
}

type syncStatus struct {
	At      time.Time `json:"at"`
	Mode    string    `json:"mode"`
	Creates int       `json:"creates"`
	Patches int       `json:"patches"`
	Deletes int       `json:"deletes"`
	Applied int       `json:"applied"`
	Failed  int       `json:"failed"`
	Aborted bool      `json:"aborted"`
	Error   string    `json:"error,omitempty"`
}

// New собирает демон и маршруты.
func New(d Deps) *Server {
	if d.Interval == 0 {
		d.Interval = 60 * time.Second
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &Server{deps: d, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler — http.Handler демона (для httptest и реального ListenAndServe).
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"version": s.deps.Version})
	})
	s.mux.HandleFunc("GET /api/role", func(w http.ResponseWriter, r *http.Request) {
		rl, err := role.Detect(r.Context(), s.deps.Store)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		writeJSON(w, map[string]any{"role": string(rl), "is_master": rl.IsMaster()})
	})
	s.mux.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		des, err := s.deps.Store.ListDesired(r.Context())
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		writeJSON(w, map[string]any{"total": len(des)})
	})
	s.mux.HandleFunc("GET /api/sync/status", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		st := s.lastSync
		s.mu.RUnlock()
		writeJSON(w, st)
	})
	s.mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		st := s.runSync(r.Context())
		writeJSON(w, st)
	})
	if s.deps.Cluster != nil {
		s.routesCluster()
	}
	// Дашборд (встроенный, см. web.go).
	s.mux.HandleFunc("GET /", s.handleIndex)
}

// runSync выполняет один проход синхронизации и сохраняет статус.
func (s *Server) runSync(ctx context.Context) syncStatus {
	res, err := syncpkg.SyncNode(ctx, s.deps.Store, s.deps.Node, s.deps.SyncOpts)
	st := syncStatus{At: time.Now().UTC(), Mode: string(s.deps.SyncOpts.Mode), Applied: res.Applied, Failed: res.Failed, Aborted: res.Aborted}
	for _, op := range res.Ops {
		switch op.Kind {
		case telemtsync.OpCreate:
			st.Creates++
		case telemtsync.OpPatch:
			st.Patches++
		case telemtsync.OpDelete:
			st.Deletes++
		}
	}
	if err != nil {
		st.Error = err.Error()
		s.deps.Log.Error("sync", "err", err)
	}
	s.mu.Lock()
	s.lastSync = st
	s.mu.Unlock()
	return st
}

// Run запускает HTTP-сервер + фоновый sync-loop; блокирует до отмены ctx.
func (s *Server) Run(ctx context.Context, addr string) error {
	go s.syncLoop(ctx)
	srv := &http.Server{Addr: addr, Handler: s.mux}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	s.deps.Log.Info("telemux serve", "addr", addr, "interval", s.deps.Interval.String(), "mode", s.deps.SyncOpts.Mode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) syncLoop(ctx context.Context) {
	t := time.NewTicker(s.deps.Interval)
	defer t.Stop()
	s.runSync(ctx) // сразу при старте
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runSync(ctx)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error, log *slog.Logger) {
	log.Error("http", "code", code, "err", err) // реальную ошибку — в лог (не глотаем)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
