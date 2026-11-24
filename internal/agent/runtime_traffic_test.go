package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

func TestRuntime_TelemtTrafficFromMetrics(t *testing.T) {
	api := telemtAPI(t, nil)
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `# TYPE telemt_user_octets_from_client_total counter
telemt_user_octets_from_client_total{user="free_1"} 9007199254740993
telemt_user_octets_to_client_total{user="free_1"} 11
telemt_user_octets_from_client_total{user="free_2"} 0
telemt_user_octets_to_client_total{user="free_2"} 42
telemt_user_octets_to_client_total{user="free_3"} 42
telemt_user_octets_from_client_total{user="placeholder"} 99
`)
	}))
	defer metrics.Close()

	c, runner, _, now := newCollector(t, api.URL, "")
	runner.out["systemctl"] = "1234567890abcdef1234567890abcdef\n"
	c.Mtg = false
	c.TelemtMetricsURL = metrics.URL + "/metrics"
	rep := c.Collect(context.Background(), time.Minute)
	if rep.ObservedAt == nil || !rep.ObservedAt.Equal(*now) {
		t.Fatalf("время снимка: %v", rep.ObservedAt)
	}
	got := rep.Telemt
	if got.TrafficGeneration != "systemd:1234567890abcdef1234567890abcdef" {
		t.Errorf("поколение telemt: %q", got.TrafficGeneration)
	}
	if len(got.Users) != 4 || len(got.Users[0].ActiveIPs) != 1 || got.Users[2].Key != "free_1" || got.Users[2].BytesIn != 9007199254740993 ||
		got.Users[2].BytesOut != 11 || !got.Users[2].TrafficSample || got.Users[3].Key != "free_2" ||
		got.Users[3].Connections != 0 || len(got.Users[3].ActiveIPs) != 0 || got.Users[3].BytesOut != 42 {
		t.Fatalf("онлайн- и офлайн-строки telemt: %+v", got)
	}
}

func TestRuntime_MtgOfflineTrafficAfterDisconnect(t *testing.T) {
	var round atomic.Int32
	stats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats" {
			http.NotFound(w, r)
			return
		}
		switch round.Add(1) {
		case 1:
			io.WriteString(w, `{"total_connections":1,"users":{"free_7":{"connections":1,"bytes_in":10,"bytes_out":0}}}`)
		case 2:
			http.Error(w, "не доступен", http.StatusServiceUnavailable)
		default:
			io.WriteString(w, `{"started_at":"2026-11-19T09:00:00Z","total_connections":0,"users":{"free_7":{"connections":0,"bytes_in":77,"bytes_out":0,"last_seen":"2026-11-19T10:00:00Z"},"idle":{"connections":0}}}`)
		}
	}))
	defer stats.Close()

	c, _, _, _ := newCollector(t, "", stats.URL+"/stats")
	c.Telemt = false
	ctx := context.Background()
	if got := c.Collect(ctx, time.Minute).Mtg; !got.APIOK || len(got.Users) != 1 || got.Users[0].BytesIn != 10 {
		t.Fatalf("подключённый клиент: %+v", got)
	}
	if got := c.Collect(ctx, time.Minute).Mtg; got.APIOK || len(got.Users) != 0 {
		t.Fatalf("сбой источника: %+v", got)
	}
	for range 2 {
		got := c.Collect(ctx, time.Minute).Mtg
		if !got.APIOK || len(got.Users) != 1 || got.Users[0].Key != "free_7" || got.Users[0].Connections != 0 ||
			got.Users[0].BytesIn != 77 || !got.Users[0].TrafficSample || got.TrafficGeneration != "2026-11-19T09:00:00Z" || got.Users[0].LastSeen == nil {
			t.Fatalf("финальный счётчик офлайн-клиента: %+v", got)
		}
	}
}

func TestRuntime_TelemtMetricsFailureKeepsConnections(t *testing.T) {
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "недоступен", http.StatusServiceUnavailable)
	}))
	defer metrics.Close()

	c, _, _, _ := newCollector(t, telemtAPI(t, nil).URL, "")
	c.Mtg = false
	c.TelemtMetricsURL = metrics.URL + "/metrics"
	got := c.Collect(context.Background(), time.Minute).Telemt
	if !got.APIOK || !strings.Contains(got.Error, "503") || len(got.Users) != 2 || got.Users[0].TrafficSample {
		t.Fatalf("отчёт после сбоя /metrics: %+v", got)
	}
}

func TestRuntime_TrimKeepsOfflineTraffic(t *testing.T) {
	sample := store.RuntimeUser{Key: "free_7", BytesIn: 77, TrafficSample: true}
	rep := store.RuntimeReport{Mtg: &store.RuntimeMtg{Users: []store.RuntimeUser{
		sample, {Key: strings.Repeat("x", 1000), Connections: 10},
	}}}
	body, err := encodeRuntime(rep, 512)
	if err != nil {
		t.Fatal(err)
	}
	var result store.RuntimeReport
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if got := result.Mtg.Users; len(got) != 1 || got[0].Key != sample.Key || !got[0].TrafficSample || got[0].BytesIn != 77 {
		t.Fatalf("строка трафика потерялась: %+v", got)
	}
	rep.Mtg.Users = []store.RuntimeUser{{Key: strings.Repeat("x", 1000), TrafficSample: true}}
	if _, err := encodeRuntime(rep, 512); err == nil {
		t.Fatal("обязательная строка не помещается, ждали ошибку")
	}
}

func TestRuntime_CollectionLimitKeepsOfflineTraffic(t *testing.T) {
	users := map[string]*store.RuntimeUser{
		"free_7": {Key: "free_7", TrafficSample: true},
	}
	for i := range runtimeMaxUsers {
		key := fmt.Sprintf("online_%d", i)
		users[key] = &store.RuntimeUser{Key: key, Connections: 1}
	}
	got := sortedUsers(users)
	if len(got) != runtimeMaxUsers || got[0].Key != "free_7" || !got[0].TrafficSample {
		t.Fatalf("лимит сбора отбросил строку трафика: строк=%d, первая=%+v", len(got), got[0])
	}
}

func TestRuntime_TelemtIgnoresMetricsDuringRestart(t *testing.T) {
	api := telemtAPI(t, nil)
	c, runner, _, _ := newCollector(t, api.URL, "")
	c.Mtg = false
	c.TelemtUnit = "telemt-pilot.service"
	runner.out["systemctl"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		runner.mu.Lock()
		runner.out["systemctl"] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		runner.mu.Unlock()
		io.WriteString(w, `telemt_user_octets_from_client_total{user="free_7"} 10
telemt_user_octets_to_client_total{user="free_7"} 20
`)
	}))
	defer metrics.Close()
	c.TelemtMetricsURL = metrics.URL + "/metrics"

	rep := c.Collect(context.Background(), time.Minute).Telemt
	if !rep.APIOK || rep.TrafficGeneration != "" || len(rep.Users) != 2 || !strings.Contains(rep.Error, "перезапустился") {
		t.Fatalf("замер между запусками юнита: %+v", rep)
	}
	if n := runner.count("systemctl show --property=InvocationID --value -- telemt-pilot.service"); n != 2 {
		t.Fatalf("проверок InvocationID нужного юнита: %d", n)
	}
}

func TestRuntime_RejectsTooManyTrafficSamples(t *testing.T) {
	users := make(map[string]*store.RuntimeUser)
	for i := range runtimeMaxUsers + 1 {
		key := fmt.Sprintf("free_%d", i)
		users[key] = &store.RuntimeUser{Key: key, TrafficSample: true}
	}
	rep := store.RuntimeReport{Telemt: &store.RuntimeTelemt{Users: sortedUsers(users)}}
	if len(rep.Telemt.Users) != runtimeMaxUsers+1 {
		t.Fatalf("строки трафика потерялись до формирования отчёта: %d", len(rep.Telemt.Users))
	}
	if _, err := encodeRuntime(rep, runtimeBodyBudget); err == nil {
		t.Fatal("ожидали отказ из-за числа обязательных строк")
	}
}

func TestRuntime_TwoMtgProcesses(t *testing.T) {
	main := mtgServer(t, `{"started_at":"2026-11-19T09:00:00Z","total_connections":1,"users":{
		"free_7":{"connections":1,"bytes_in":10,"bytes_out":20}}}`)
	var down atomic.Bool
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats" {
			http.NotFound(w, r)
			return
		}
		if down.Load() {
			http.Error(w, "недоступен", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"started_at":"2026-11-19T09:05:00Z","users":{
			"free_7":{"connections":0,"bytes_in":77,"bytes_out":88}}}`)
	}))
	defer secondary.Close()

	c, _, _, _ := newCollector(t, "", main.URL+"/stats")
	c.Telemt = false
	ctx := context.Background()
	collect := func() map[string]json.RawMessage {
		t.Helper()
		body, err := encodeRuntime(c.Collect(ctx, time.Minute), runtimeBodyBudget)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		return wire
	}
	decode := func(wire map[string]json.RawMessage, field string) store.RuntimeMtg {
		t.Helper()
		var value store.RuntimeMtg
		if err := json.Unmarshal(wire[field], &value); err != nil {
			t.Fatalf("%s: %v", field, err)
		}
		return value
	}
	if _, ok := collect()["mtg_secondary"]; ok {
		t.Fatal("без вторичного источника поле mtg_secondary должно отсутствовать")
	}
	c.MtgSecondaryStatsURL = secondary.URL + "/stats"
	wire := collect()
	first, second := decode(wire, "mtg"), decode(wire, "mtg_secondary")
	if !first.APIOK || first.TrafficGeneration != "2026-11-19T09:00:00Z" || len(first.Users) != 1 ||
		first.Users[0].Key != "free_7" || first.Users[0].BytesIn != 10 || !first.Users[0].TrafficSample {
		t.Fatalf("основной mtg: %+v", first)
	}
	if !second.APIOK || second.TrafficGeneration != "2026-11-19T09:05:00Z" || len(second.Users) != 1 ||
		second.Users[0].Key != "free_7" || second.Users[0].Connections != 0 ||
		second.Users[0].BytesIn != 77 || second.Users[0].BytesOut != 88 || !second.Users[0].TrafficSample {
		t.Fatalf("вторичный mtg: %+v", second)
	}
	down.Store(true)
	wire = collect()
	if first, second := decode(wire, "mtg"), decode(wire, "mtg_secondary"); !first.APIOK ||
		second.APIOK || !strings.Contains(second.Error, "503") || len(second.Users) != 0 || second.TrafficGeneration != "" {
		t.Fatalf("при сбое второго источника: основной=%+v второй=%+v", first, second)
	}
}

func TestRuntime_TrimKeepsSecondaryMtgTraffic(t *testing.T) {
	rep := store.RuntimeReport{MtgSecondary: &store.RuntimeMtg{Users: []store.RuntimeUser{
		{Key: strings.Repeat("x", 1000), Connections: 10},
		{Key: "free_7", TrafficSample: true, BytesIn: 77},
	}}}
	body, err := encodeRuntime(rep, 512)
	if err != nil {
		t.Fatal(err)
	}
	var got store.RuntimeReport
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if m := got.MtgSecondary; m == nil || len(m.Users) != 1 || !m.Users[0].TrafficSample || m.Users[0].BytesIn != 77 {
		t.Fatalf("вторичный mtg после урезания: %+v", m)
	}
}

func TestRuntime_TelemtExplicitZeroWithoutMetricsSeries(t *testing.T) {
	var metricsDown atomic.Bool
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/users":
			io.WriteString(w, `{"ok":true,"data":[
				{"username":"free_zero","total_octets":0},
				{"username":"free_missing"},
				{"username":"free_null","total_octets":null},
				{"username":"free_positive","total_octets":15},
				{"username":"free_partial","total_octets":0},
				{"username":"paid_zero","total_octets":0}
			]}`)
		case "/v1/stats/users/active-ips":
			io.WriteString(w, `{"ok":true,"data":[]}`)
		case "/metrics":
			if metricsDown.Load() {
				http.Error(w, "недоступен", http.StatusServiceUnavailable)
				return
			}
			io.WriteString(w, `telemt_user_octets_to_client_total{user="free_partial"} 0
telemt_user_octets_to_client_total{user="free_unconfigured"} 0
`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer source.Close()

	c, runner, _, _ := newCollector(t, source.URL, "")
	c.Mtg = false
	runner.out["systemctl"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ctx := context.Background()
	got := c.Collect(ctx, time.Minute).Telemt
	if !got.APIOK || got.TrafficGeneration == "" || len(got.Users) != 1 {
		t.Fatalf("ожидали один подтверждённый нулевой замер: %+v", got)
	}
	if u := got.Users[0]; u.Key != "free_zero" || !u.TrafficSample || u.BytesIn != 0 || u.BytesOut != 0 {
		t.Fatalf("нулевой замер: %+v", u)
	}
	metricsDown.Store(true)
	got = c.Collect(ctx, time.Minute).Telemt
	if !got.APIOK || len(got.Users) != 0 || !strings.Contains(got.Error, "503") {
		t.Fatalf("сбой /metrics не должен создавать нулевой замер: %+v", got)
	}
}
