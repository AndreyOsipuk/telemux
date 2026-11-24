package bridge

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBridgeRelayMastersOnlyFromLiveMaster(t *testing.T) {
	var offline atomic.Bool
	var forwarded atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("X-Telemux-Accept-Relay-Masters") == "1" {
			forwarded.Store(true)
		}
		w.Header().Set("X-Telemux-Relay-Masters", "eyJwYXRocyI6W119")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer up.Close()
	b := newBridge(t, up.URL)
	for _, down := range []bool{false, true} {
		offline.Store(down)
		req := httptest.NewRequest("GET", "/v1/desired?target=pr1&rev=0&wait=0s", nil)
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("X-Telemux-Accept-Relay-Masters", "1")
		rec := httptest.NewRecorder()
		b.Handler().ServeHTTP(rec, req)
		got := rec.Header().Get("X-Telemux-Relay-Masters")
		if !down && (!forwarded.Load() || rec.Code != http.StatusNotModified || got != "eyJwYXRocyI6W119") {
			t.Fatalf("HTTP %d, запрос переслан=%v, заголовок=%q", rec.Code, forwarded.Load(), got)
		}
		if down && got != "" {
			t.Fatalf("при недоступном мастере отдан список путей: %q", got)
		}
	}
}
