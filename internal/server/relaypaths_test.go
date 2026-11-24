package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

type fakeRelayPathAgents struct {
	*fakeAgents
	managed bool
	exits   []store.Target
}

func (f *fakeRelayPathAgents) RelayMasterTargets(context.Context, string) (bool, []store.Target, error) {
	return f.managed, f.exits, nil
}

func TestDesiredRelayMastersOn200And304(t *testing.T) {
	s, revs, agents := agentServer(false)
	fake := &fakeRelayPathAgents{fakeAgents: agents, managed: true, exits: []store.Target{
		{Code: "ps1", Settings: map[string]string{"address": "203.0.113.7", "bridge_socks": "socks5://127.0.0.1:10874"}},
	}}
	s.deps.Agents = fake
	revs.rev = 2
	for _, test := range []struct {
		rev    string
		status int
	}{{"1", http.StatusOK}, {"2", http.StatusNotModified}} {
		req := httptest.NewRequest(http.MethodGet, "/v1/desired?target=pr2&wait=0s&rev="+test.rev, nil)
		req.Header.Set("Authorization", "Bearer "+pr2Token)
		req.Header.Set("X-Telemux-Accept-Relay-Masters", "1")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Fatalf("rev=%s: HTTP %d, ждали %d", test.rev, rec.Code, test.status)
		}
		data, err := base64.RawURLEncoding.DecodeString(rec.Header().Get("X-Telemux-Relay-Masters"))
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Paths []struct{ Exit, Master string }
		}
		if err := json.Unmarshal(data, &body); err != nil || len(body.Paths) != 1 ||
			body.Paths[0].Exit != "ps1" || body.Paths[0].Master != "http://203.0.113.7:18480|socks5://127.0.0.1:10874" {
			t.Fatalf("плохой список путей: %s (%v)", data, err)
		}
	}
}

func TestRelayPathsManagedSettingOnlyForRelay(t *testing.T) {
	for _, item := range []struct {
		kind, value string
		valid       bool
	}{
		{"relay", "1", true},
		{"relay", "0", true},
		{"relay", "yes", false},
		{"exit", "1", false},
	} {
		err := checkTargetSettings(item.kind, map[string]string{"relay_paths_managed": item.value})
		if item.valid != (err == nil) {
			t.Errorf("%s:%s, неожиданная ошибка: %v", item.kind, item.value, err)
		}
	}
}

func TestDesiredRelayMastersRespectsOptInAndLeadership(t *testing.T) {
	for _, item := range []struct {
		name                              string
		replica, disabled, managed, optIn bool
		want                              string
	}{
		{name: "включённое реле без выходов", managed: true, optIn: true, want: "eyJwYXRocyI6W119"},
		{name: "старый агент", managed: true},
		{name: "реплика", replica: true, managed: true, optIn: true},
		{name: "цель отключена", disabled: true, managed: true, optIn: true},
		{name: "управление приостановлено", optIn: true},
	} {
		t.Run(item.name, func(t *testing.T) {
			s, _, agents := agentServer(item.replica)
			agents.disabled = map[string]bool{"pr2": item.disabled}
			s.deps.Agents = &fakeRelayPathAgents{fakeAgents: agents, managed: item.managed}
			req := httptest.NewRequest(http.MethodGet, "/v1/desired?target=pr2&wait=0s&rev=0", nil)
			req.Header.Set("Authorization", "Bearer "+pr2Token)
			if item.optIn {
				req.Header.Set("X-Telemux-Accept-Relay-Masters", "1")
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if got := rec.Header().Get("X-Telemux-Relay-Masters"); rec.Code != http.StatusNotModified || got != item.want {
				t.Fatalf("HTTP %d, список путей %q, ждали %q", rec.Code, got, item.want)
			}
		})
	}
}
