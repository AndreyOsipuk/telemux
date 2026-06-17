package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/store"
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

func TestCluster_DisabledWhenNoStore(t *testing.T) {
	// Без Cluster кластер-маршруты не смонтированы.
	s := New(Deps{Store: fakeStore{}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}})
	if do(s, "GET", "/api/nodes", "", "").Code != http.StatusNotFound {
		t.Fatal("без Cluster /api/nodes должен быть 404")
	}
}
