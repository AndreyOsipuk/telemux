package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

type fakeStore struct {
	users      []telemtsync.DesiredUser
	inRecovery bool
}

func (f fakeStore) IsInRecovery(context.Context) (bool, error) { return f.inRecovery, nil }
func (f fakeStore) ListDesired(context.Context) ([]telemtsync.DesiredUser, error) {
	return f.users, nil
}

type fakeNode struct{ applied int }

func (n *fakeNode) ListUsers(context.Context) ([]telemtsync.RemoteUser, string, error) {
	return nil, "rev1", nil
}
func (n *fakeNode) ApplyOp(context.Context, telemtsync.SyncOp, string) (string, error) {
	n.applied++
	return "rev2", nil
}

func newTestServer(inRecovery bool, n int) *Server {
	users := make([]telemtsync.DesiredUser, n)
	for i := range users {
		users[i] = telemtsync.DesiredUser{Username: "sub_" + string(rune('1'+i)), Secret: "ee"}
	}
	return New(Deps{
		Store:    fakeStore{users: users, inRecovery: inRecovery},
		Node:     &fakeNode{},
		Version:  "v9.9.9",
		SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
	})
}

func get(t *testing.T, s *Server, path string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: код %d", path, rec.Code)
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestHealthz(t *testing.T) {
	s := newTestServer(false, 0)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("/healthz: %d %q", rec.Code, rec.Body.String())
	}
}

func TestAPIVersionRoleUsers(t *testing.T) {
	s := newTestServer(false, 3) // primary → master
	if get(t, s, "/api/version")["version"] != "v9.9.9" {
		t.Fatal("version")
	}
	role := get(t, s, "/api/role")
	if role["role"] != "master" || role["is_master"] != true {
		t.Fatalf("role: %v", role)
	}
	if get(t, s, "/api/users")["total"].(float64) != 3 {
		t.Fatal("users total")
	}

	sr := newTestServer(true, 0) // standby → replica
	if get(t, sr, "/api/role")["role"] != "replica" {
		t.Fatal("replica")
	}
}

func TestSyncEndpoint(t *testing.T) {
	s := newTestServer(false, 2) // 2 desired, нода пустая (shadow) → 2 create в diff
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	if rec.Code != 200 {
		t.Fatalf("POST /api/sync: %d", rec.Code)
	}
	var st syncStatus
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Creates != 2 || st.Mode != "shadow" {
		t.Fatalf("ждали 2 create в shadow, получили %+v", st)
	}
}

func TestIndexServed(t *testing.T) {
	s := newTestServer(false, 0)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 || rec.Body.Len() < 100 {
		t.Fatalf("/ дашборд: код=%d len=%d", rec.Code, rec.Body.Len())
	}
}
