package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

func TestTrafficRuntimeRejectsMissingObservationTime(t *testing.T) {
	s, _, _ := agentServer(false)
	rec := do(s, "POST", "/v1/runtime", pr2Token,
		`{"target":"pr2","telemt":{"api_ok":true,"traffic_generation":"process-1"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ждали 400, получили %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTrafficRuntimeRejectsFutureObservation(t *testing.T) {
	s, _, _ := agentServer(false)
	body := fmt.Sprintf(`{"target":"pr2","observed_at":%q,"mtg":{"api_ok":true,"traffic_generation":"process-1"}}`,
		time.Now().Add(time.Hour).Format(time.RFC3339))
	rec := do(s, "POST", "/v1/runtime", pr2Token, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ждали 400, получили %d: %s", rec.Code, rec.Body.String())
	}
}

func (f *fakeAgents) TrafficUsage(_ context.Context, key string, since, windowSince, until, freshSince time.Time) (store.TrafficUsage, error) {
	if key != "free_123" || since.Format(time.RFC3339) != "2026-11-20T09:00:00Z" ||
		windowSince.Sub(since) != 24*time.Hour || until.Sub(freshSince) != runtimeStaleDefault(RuntimeEveryDefault) {
		return store.TrafficUsage{}, fmt.Errorf("неверные параметры запроса расхода")
	}
	return store.TrafficUsage{TotalBytes: "320", WindowBytes: "120", MeasuredAt: &windowSince, Complete: true}, nil
}

func TestTrafficUsageRoute(t *testing.T) {
	s, _, _ := agentServer(false)
	const endpoint = "/api/v1/usage?user_key=free_123&since=2026-11-20T09:00:00Z&window_since=2026-11-21T09:00:00Z"
	if rec := do(s, "GET", endpoint, pr2Token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("токен агента: ждали 401, получили %d", rec.Code)
	}
	if rec := do(s, "GET", endpoint, botToken, ""); rec.Code != http.StatusOK ||
		strings.TrimSpace(rec.Body.String()) != `{"total_bytes":320,"window_bytes":120,"measured_at":"2026-11-21T09:00:00Z","complete":true}` {
		t.Fatalf("ответ: %d %s", rec.Code, rec.Body.String())
	}
	for _, query := range []string{
		"?user_key=free_123&since=bad&window_since=2026-11-21T09:00:00Z",
		"?user_key=free_123&since=2026-11-21T09:00:00Z&window_since=2026-11-20T09:00:00Z",
		"?since=2026-11-20T09:00:00Z&window_since=2026-11-21T09:00:00Z",
	} {
		if rec := do(s, "GET", "/api/v1/usage"+query, botToken, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("запрос %s: ждали 400, получили %d", query, rec.Code)
		}
	}
}
