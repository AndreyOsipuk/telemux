package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// fakeUsers — in-memory реализация UserAdmin.
type fakeUsers struct {
	mu sync.Mutex
	m  map[string]store.User
}

func newFakeUsers(n int) *fakeUsers {
	fu := &fakeUsers{m: map[string]store.User{}}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("sub_%d", i)
		fu.m[name] = store.User{Username: name, Secret: "ee", Enabled: true}
	}
	return fu
}

func (f *fakeUsers) CreateUser(_ context.Context, username, secret string, exp *time.Time, mc *int) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.m[username]; ok {
		return store.User{}, store.ErrUserExists
	}
	if secret == "" {
		secret = "eegenerated"
	}
	u := store.User{Username: username, Secret: secret, ExpirationAt: exp, MaxTCPConns: mc, Enabled: true}
	f.m[username] = u
	return u, nil
}
func (f *fakeUsers) DeleteUser(_ context.Context, username string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.m[username]; !ok {
		return false, nil
	}
	delete(f.m, username)
	return true, nil
}
func (f *fakeUsers) SetExpiration(_ context.Context, username string, exp *time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.m[username]
	if !ok {
		return false, nil
	}
	u.ExpirationAt = exp
	f.m[username] = u
	return true, nil
}
func (f *fakeUsers) SetEnabled(_ context.Context, username string, en bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.m[username]
	if !ok {
		return false, nil
	}
	u.Enabled = en
	f.m[username] = u
	return true, nil
}
func (f *fakeUsers) ListUsersPage(_ context.Context, limit, offset int) ([]store.User, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.m))
	for n := range f.m {
		names = append(names, n)
	}
	sort.Strings(names)
	total := len(names)
	out := []store.User{}
	for i := offset; i < total && len(out) < limit; i++ {
		out = append(out, f.m[names[i]])
	}
	return out, total, nil
}

func usersServer(inRecovery bool) (*Server, *fakeUsers) {
	fu := newFakeUsers(0)
	s := New(Deps{
		Store: fakeStore{inRecovery: inRecovery}, Users: fu, Node: &fakeNode{},
		Version: "v1", SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
	})
	return s, fu
}

func TestUsers_ListPaged(t *testing.T) {
	s := New(Deps{Store: fakeStore{}, Users: newFakeUsers(5), Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}})
	rec := do(s, "GET", "/api/users?limit=2&offset=0", "", "")
	if rec.Code != 200 {
		t.Fatalf("list → %d", rec.Code)
	}
	var out struct {
		Data   []store.User   `json:"data"`
		Paging map[string]int `json:"paging"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Paging["total"] != 5 || len(out.Data) != 2 {
		t.Fatalf("ждали total=5, на странице 2, получили total=%d len=%d", out.Paging["total"], len(out.Data))
	}
}

func TestUsers_CreateMaster(t *testing.T) {
	s, fu := usersServer(false) // master
	rec := do(s, "POST", "/api/users", "", `{"username":"sub_new"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create → %d", rec.Code)
	}
	if _, ok := fu.m["sub_new"]; !ok {
		t.Fatal("юзер не создан")
	}
	// create должен дёрнуть немедленную синхру (dirty-сигнал)
	select {
	case <-s.dirty:
	default:
		t.Fatal("успешный create должен пометить dirty (немедленная синхра)")
	}
	// дубль → 409
	if do(s, "POST", "/api/users", "", `{"username":"sub_new"}`).Code != http.StatusConflict {
		t.Fatal("дубль username → 409")
	}
}

func TestUsers_WriteBlockedOnReplica(t *testing.T) {
	s, _ := usersServer(true) // replica
	for _, c := range []struct{ m, p, b string }{
		{"POST", "/api/users", `{"username":"x"}`},
		{"DELETE", "/api/users/x", ""},
		{"POST", "/api/users/x/renew", `{"expiration_at":null}`},
		{"POST", "/api/users/x/disable", ""},
	} {
		if rec := do(s, c.m, c.p, "", c.b); rec.Code != http.StatusConflict {
			t.Fatalf("%s %s на replica → ждали 409, получили %d", c.m, c.p, rec.Code)
		}
	}
}

func TestUsers_DeleteRenewEnable(t *testing.T) {
	s, fu := usersServer(false)
	fu.CreateUser(context.Background(), "sub_1", "ee", nil, nil)

	if do(s, "POST", "/api/users/sub_1/disable", "", "").Code != 200 || fu.m["sub_1"].Enabled {
		t.Fatal("disable не сработал")
	}
	if do(s, "POST", "/api/users/sub_1/renew", "", `{"expiration_at":"2027-01-01T00:00:00Z"}`).Code != 200 || fu.m["sub_1"].ExpirationAt == nil {
		t.Fatal("renew не сработал")
	}
	if do(s, "DELETE", "/api/users/sub_1", "", "").Code != 200 {
		t.Fatal("delete не сработал")
	}
	if do(s, "DELETE", "/api/users/sub_1", "", "").Code != http.StatusNotFound {
		t.Fatal("повторный delete → 404")
	}
}
