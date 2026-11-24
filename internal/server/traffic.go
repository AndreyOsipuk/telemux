package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

type trafficUsageReader interface {
	TrafficUsage(context.Context, string, time.Time, time.Time, time.Time, time.Time) (store.TrafficUsage, error)
}

func validateTrafficSample(observedAt *time.Time, generation string, users []store.RuntimeUser) error {
	if len(generation) > maxRuntimeKeyLen || strings.ContainsRune(generation, 0) {
		return errors.New("traffic_generation: требуется идентификатор до 128 байт")
	}
	if observedAt == nil {
		if generation != "" {
			return errors.New("traffic_generation: требуется observed_at")
		}
		for _, user := range users {
			if user.TrafficSample {
				return errors.New("traffic_sample: требуется observed_at")
			}
		}
	}
	return nil
}

func (s *Server) routesTraffic(reader trafficUsageReader) {
	s.mux.HandleFunc("GET /api/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		if !s.botAuthed(r) {
			writeErr(w, http.StatusUnauthorized, errors.New("нужен токен бота"), s.deps.Log)
			return
		}

		query := r.URL.Query()
		key := query.Get("user_key")
		since, sinceErr := time.Parse(time.RFC3339Nano, query.Get("since"))
		windowSince, windowErr := time.Parse(time.RFC3339Nano, query.Get("window_since"))
		now := time.Now()
		if key == "" || len(key) > maxRuntimeKeyLen || strings.ContainsRune(key, 0) {
			writeErr(w, http.StatusBadRequest, errors.New("user_key: требуется стабильный ключ до 128 байт"), s.deps.Log)
			return
		}
		if sinceErr != nil || windowErr != nil || windowSince.Before(since) || windowSince.After(now) {
			writeErr(w, http.StatusBadRequest, errors.New("since и window_since: нужны даты RFC3339, since ≤ window_since ≤ текущее время"), s.deps.Log)
			return
		}
		usage, err := reader.TrafficUsage(r.Context(), key, since, windowSince, now, now.Add(-s.staleAfter()))
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, usage)
	})
}
