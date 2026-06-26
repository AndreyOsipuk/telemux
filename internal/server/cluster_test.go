package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
)

type fakeCluster struct {
	nodes  []store.Node
	tokens map[string]bool // token → ещё валиден
}

func (f *fakeCluster) ListNodes(context.Context) ([]store.Node, error) { return f.nodes, nil }
func (f *fakeCluster) UpsertNode(_ context.Context, n store.Node) error {
	f.nodes = append(f.nodes, n)
	return nil
}
func (f *fakeCluster) CreateJoinToken(_ context.Context, tok string, _ time.Duration) error {
	if f.tokens == nil {
		f.tokens = map[string]bool{}
	}
	f.tokens[tok] = true
	return nil
}
func (f *fakeCluster) ConsumeJoinToken(_ context.Context, tok string) (bool, error) {
	if f.tokens[tok] {
		f.tokens[tok] = false
		return true, nil
	}
	return false, nil
}

func clusterServer(secret string) (*Server, *fakeCluster) {
	fc := &fakeCluster{}
	s := New(Deps{
		Store: fakeStore{}, Node: &fakeNode{}, Version: "v1",
		SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		Cluster:  fc, ClusterSecret: secret, PublicURL: "https://master.example",
	})
	return s, fc
}

func do(s *Server, method, path, auth, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestCluster_HeartbeatAuth(t *testing.T) {
	s, fc := clusterServer("sek")
	if do(s, "POST", "/api/cluster/heartbeat", "wrong", `{"code":"ps1"}`).Code != http.StatusUnauthorized {
		t.Fatal("неверный секрет → 401")
	}
	if rec := do(s, "POST", "/api/cluster/heartbeat", "sek", `{"code":"ps1","role":"replica"}`); rec.Code != 200 {
		t.Fatalf("валидный heartbeat → 200, получили %d", rec.Code)
	}
	if len(fc.nodes) != 1 || fc.nodes[0].Code != "ps1" {
		t.Fatalf("нода не зарегистрирована: %+v", fc.nodes)
	}
}

func TestCluster_NodesList(t *testing.T) {
	s, fc := clusterServer("sek")
	fc.nodes = []store.Node{{Code: "ps1", Role: "master"}, {Code: "ps2", Role: "replica"}}
	rec := do(s, "GET", "/api/nodes", "", "")
	if rec.Code != 200 {
		t.Fatalf("/api/nodes → %d", rec.Code)
	}
	var out struct {
		Nodes []store.Node `json:"nodes"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Nodes) != 2 {
		t.Fatalf("ждали 2 ноды, получили %d", len(out.Nodes))
	}
}

func TestCluster_JoinTokenFlow(t *testing.T) {
	s, _ := clusterServer("sek")
	// без авторизации — 401
	if do(s, "POST", "/api/cluster/join-token", "", "").Code != http.StatusUnauthorized {
		t.Fatal("join-token без секрета → 401")
	}
	rec := do(s, "POST", "/api/cluster/join-token", "sek", "")
	if rec.Code != 200 {
		t.Fatalf("join-token → %d", rec.Code)
	}
	var resp struct {
		Token, Command string
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Token == "" || !strings.Contains(resp.Command, resp.Token) {
		t.Fatalf("ответ join-token неверен: %+v", resp)
	}
	// токен валиден один раз
	if do(s, "GET", "/join/"+resp.Token, "", "").Code != 200 {
		t.Fatal("первый /join → 200")
	}
	if do(s, "GET", "/join/"+resp.Token, "", "").Code != http.StatusGone {
		t.Fatal("повторный /join → 410 (одноразовый)")
	}
	if do(s, "GET", "/join/неизвестный", "", "").Code != http.StatusGone {
		t.Fatal("неизвестный токен → 410")
	}
}

func TestHeartbeat_MasterSelfRegisters(t *testing.T) {
	fc := &fakeCluster{}
	s := New(Deps{
		Store: fakeStore{inRecovery: false}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		Cluster: fc, SelfCode: "ps1", SelfAddress: "10.0.0.1",
	})
	s.reportHeartbeat(context.Background())
	if len(fc.nodes) != 1 || fc.nodes[0].Code != "ps1" || fc.nodes[0].Role != "master" {
		t.Fatalf("master должен зарегистрировать себя как master: %+v", fc.nodes)
	}
}

func TestHeartbeat_ReplicaPostsToMaster(t *testing.T) {
	var got store.Node
	var gotAuth string
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(200)
	}))
	defer master.Close()
	s := New(Deps{
		Store: fakeStore{inRecovery: true}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		Cluster: &fakeCluster{}, SelfCode: "ps2", SelfAddress: "10.0.0.2",
		MasterURL: master.URL, ClusterSecret: "sek",
	})
	s.reportHeartbeat(context.Background())
	if got.Code != "ps2" || got.Role != "replica" {
		t.Fatalf("replica должна слать свой статус мастеру: %+v", got)
	}
	if gotAuth != "Bearer sek" {
		t.Fatalf("heartbeat должен нести Bearer-секрет, получили %q", gotAuth)
	}
}

func TestHeartbeat_NoCodeNoop(t *testing.T) {
	fc := &fakeCluster{}
	s := New(Deps{Store: fakeStore{}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}, Cluster: fc})
	s.reportHeartbeat(context.Background()) // SelfCode пуст → no-op
	if len(fc.nodes) != 0 {
		t.Fatal("без SelfCode heartbeat не должен ничего регистрировать")
	}
}

func TestCluster_DisabledWhenNoStore(t *testing.T) {
	// Без Cluster кластер-маршруты не смонтированы.
	s := New(Deps{Store: fakeStore{}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}})
	if do(s, "GET", "/api/nodes", "", "").Code != http.StatusNotFound {
		t.Fatal("без Cluster /api/nodes должен быть 404")
	}
}

func TestCluster_UsersSync(t *testing.T) {
	fu := newFakeUsers(20) // 20 текущих юзеров (sub_0..sub_19)
	fc := &fakeCluster{}
	s := New(Deps{
		Store: fakeStore{}, Node: &fakeNode{}, Version: "v1",
		SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		Users:    fu, Cluster: fc, ClusterSecret: "sek",
	})

	// неверный bearer → 401
	if do(s, "POST", "/api/cluster/users-sync", "wrong", `{"users":[]}`).Code != http.StatusUnauthorized {
		t.Fatal("неверный секрет → 401")
	}

	// нормальный reconcile: 20 старых → 21 новый (20 upsert + 1 новый), удалений 0
	body := `{"users":[`
	for i := 0; i < 21; i++ {
		if i > 0 {
			body += ","
		}
		body += `{"username":"sub_` + itoa(i) + `","secret":"ee` + itoa(i) + `"}`
	}
	body += `]}`
	rec := do(s, "POST", "/api/cluster/users-sync", "sek", body)
	if rec.Code != 200 {
		t.Fatalf("reconcile → ждали 200, получили %d (%s)", rec.Code, rec.Body.String())
	}
	if len(fu.m) != 21 {
		t.Fatalf("ждали 21 юзера, получили %d", len(fu.m))
	}

	// guard массового сноса: прислали 1 юзера при 21 текущем (удалили бы 20/21 > 20%) → 409, без изменений
	rec = do(s, "POST", "/api/cluster/users-sync", "sek", `{"users":[{"username":"sub_0","secret":"ee0"}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("массовый снос без force → ждали 409, получили %d", rec.Code)
	}
	if len(fu.m) != 21 {
		t.Fatalf("после заблокированного reconcile набор не должен меняться, получили %d", len(fu.m))
	}

	// с force → проходит, остаётся 1
	rec = do(s, "POST", "/api/cluster/users-sync", "sek", `{"force":true,"users":[{"username":"sub_0","secret":"ee0"}]}`)
	if rec.Code != 200 || len(fu.m) != 1 {
		t.Fatalf("force reconcile → 200 и 1 юзер, получили code=%d len=%d", rec.Code, len(fu.m))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
