package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AndreyOsipuk/telemux/internal/peers"
	"github.com/AndreyOsipuk/telemux/internal/role"
	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
)

type fixedRole struct{ st role.State }

func (f fixedRole) Current() role.State { return f.st }

func failoverServer(st role.State, inRecovery bool) (*Server, *fakeUsers, *fakeCluster) {
	fu := newFakeUsers(0)
	fc := &fakeCluster{}
	s := New(Deps{
		Store: fakeStore{inRecovery: inRecovery}, Users: fu, Node: &fakeNode{},
		SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}, Cluster: fc,
		AdminPassword: "pw", // auth включён: /api/leader должен быть доступен без сессии
		ClusterSecret: "sek", SelfCode: "ps1",
		Role:  fixedRole{st},
		Peers: map[string]string{"ps2": "http://ps2:8080", "ps3": "http://ps3:8080"},
	})
	return s, fu, fc
}

func TestLeader_Master200(t *testing.T) {
	s, _, _ := failoverServer(role.State{Role: role.Master, Master: "ps1", Quorum: true}, false)
	rec := do(s, "GET", "/api/leader", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("мастер → 200 без сессии, получили %d %s", rec.Code, rec.Body)
	}
}

func TestLeader_NotMaster503WithHint(t *testing.T) {
	// Роль по монитору важнее PG: PG primary, но узел fenced → 503.
	s, _, _ := failoverServer(role.State{Role: role.Fenced, Reason: "нет кворума", Master: "ps2"}, false)
	rec := do(s, "GET", "/api/leader", "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fenced → 503, получили %d", rec.Code)
	}
	if rec.Header().Get("X-Telemux-Master") != "http://ps2:8080" {
		t.Fatalf("ждали подсказку мастера в заголовке, получили %q", rec.Header().Get("X-Telemux-Master"))
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "not_master" || body["master_url"] != "http://ps2:8080" {
		t.Fatalf("тело 503: %v", body)
	}
}

// Без монитора /api/leader работает по роли PG (одиночный режим).
func TestLeader_FallbackToPG(t *testing.T) {
	s := New(Deps{Store: fakeStore{inRecovery: true}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}})
	if rec := do(s, "GET", "/api/leader", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("реплика без монитора → 503, получили %d", rec.Code)
	}
	s = New(Deps{Store: fakeStore{}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}})
	if rec := do(s, "GET", "/api/leader", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("primary без монитора → 200, получили %d", rec.Code)
	}
}

// Главный инвариант для приложений: fenced primary не принимает запись,
// хотя локальный PG ещё primary и запись в него технически прошла бы.
func TestWrites_RejectedWhenFenced(t *testing.T) {
	s, fu, _ := failoverServer(role.State{Role: role.Fenced, Master: "ps3"}, false)
	rec := do(s, "POST", "/api/cluster/users-sync", "sek", `{"users":[]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("users-sync на fenced → 409, получили %d", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "not_master" || body["code"] != "not_master" || body["master_url"] != "http://ps3:8080" {
		t.Fatalf("тело 409: %v", body)
	}
	if rec.Header().Get("X-Telemux-Master") != "http://ps3:8080" {
		t.Fatal("нет заголовка X-Telemux-Master")
	}
	if rec := do(s, "POST", "/api/cluster/join-token", "sek", ""); rec.Code != http.StatusConflict {
		t.Fatalf("join-token на fenced → 409, получили %d", rec.Code)
	}
	if rec := do(s, "POST", "/api/cluster/heartbeat", "sek", `{"code":"ps2"}`); rec.Code != http.StatusConflict {
		t.Fatalf("приём heartbeat на fenced → 409, получили %d", rec.Code)
	}
	if len(fu.m) != 0 {
		t.Fatal("на fenced-узле ничего не должно записаться")
	}
}

func TestPeerPing(t *testing.T) {
	s, _, _ := failoverServer(role.State{Role: role.Replica}, true)
	if rec := do(s, "GET", peers.PingPath, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("пинг без секрета → 401, получили %d", rec.Code)
	}
	rec := do(s, "GET", peers.PingPath, "sek", "")
	var p peers.Ping
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 200 || p.Code != "ps1" || p.Role != role.Replica {
		t.Fatalf("пинг → %d %+v", rec.Code, p)
	}
}

// Пинг сервера читается Prober'ом соседа - сквозная проверка контракта.
func TestPeerPing_ProberContract(t *testing.T) {
	s, _, _ := failoverServer(role.State{Role: role.Master, Master: "ps1"}, false)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	pr := &peers.Prober{Peers: map[string]string{"ps1": ts.URL}, Secret: "sek"}
	res := pr.Probe(context.Background())
	if len(res) != 1 || !res[0].OK || res[0].Role != role.Master {
		t.Fatalf("сосед должен увидеть ps1 мастером: %+v", res)
	}
}

func TestRole_ExposesReason(t *testing.T) {
	s, _, _ := failoverServer(role.State{Role: role.Fenced, Reason: "нет кворума", Seen: []string{}}, false)
	rec := do(s, "GET", "/api/role", "", "")
	// /api/role за сессией панели.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/role без сессии при включённом auth → 401, получили %d", rec.Code)
	}
	s2 := New(Deps{Store: fakeStore{}, Node: &fakeNode{}, Role: fixedRole{role.State{Role: role.Fenced, Reason: "нет кворума"}}})
	got := get(t, s2, "/api/role")
	if got["role"] != "fenced" || got["is_master"] != false || got["reason"] != "нет кворума" {
		t.Fatalf("/api/role: %v", got)
	}
}

// Реплика шлёт heartbeat мастеру, найденному через соседей, а не на устаревший MasterURL.
func TestHeartbeat_GoesToDiscoveredMaster(t *testing.T) {
	var got store.Node
	newMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(200)
	}))
	defer newMaster.Close()
	oldMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("heartbeat ушёл на старый мастер")
	}))
	defer oldMaster.Close()

	s := New(Deps{
		Store: fakeStore{inRecovery: true}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		Cluster: &fakeCluster{}, SelfCode: "ps1", ClusterSecret: "sek",
		MasterURL: oldMaster.URL,
		Role:      fixedRole{role.State{Role: role.Replica, Master: "ps3"}},
		Peers:     map[string]string{"ps2": oldMaster.URL, "ps3": newMaster.URL},
	})
	s.reportHeartbeat(context.Background())
	if got.Code != "ps1" || got.Role != "replica" {
		t.Fatalf("новый мастер должен получить heartbeat: %+v", got)
	}
}
