package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
)

func authedServer(pass string) *Server {
	return New(Deps{
		Store: fakeStore{}, Users: newFakeUsers(1), Node: &fakeNode{},
		Version: "v1", SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		AdminUser: "admin", AdminPassword: pass,
	})
}

func TestAuth_DisabledPassesThrough(t *testing.T) {
	s := authedServer("") // пароль не задан → auth off
	if do(s, "GET", "/api/users", "", "").Code != 200 {
		t.Fatal("без пароля auth должен быть выключен → 200")
	}
	me := get(t, s, "/api/me")
	if me["auth_enabled"] != false || me["authed"] != true {
		t.Fatalf("/api/me при выключенном auth: %v", me)
	}
}

func TestAuth_BlocksWithoutSession(t *testing.T) {
	s := authedServer("secret")
	if do(s, "GET", "/api/users", "", "").Code != http.StatusUnauthorized {
		t.Fatal("защищённый /api/users без сессии → 401")
	}
	// статика и /api/me — публичны
	if do(s, "GET", "/", "", "").Code != 200 {
		t.Fatal("статика SPA должна быть публична")
	}
	if do(s, "GET", "/api/me", "", "").Code != 200 {
		t.Fatal("/api/me публичен")
	}
}

func TestAuth_LoginFlow(t *testing.T) {
	s := authedServer("secret")

	// неверный пароль → 401
	if do(s, "POST", "/api/login", "", `{"username":"admin","password":"wrong"}`).Code != http.StatusUnauthorized {
		t.Fatal("неверный пароль → 401")
	}

	// верный → 200 + cookie
	rec := do(s, "POST", "/api/login", "", `{"username":"admin","password":"secret"}`)
	if rec.Code != 200 {
		t.Fatalf("логин → %d", rec.Code)
	}
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 || cookie[0].Name != sessionCookie {
		t.Fatal("логин должен установить session-cookie")
	}

	// с cookie → защищённый ресурс доступен
	req := httptest.NewRequest("GET", "/api/users", nil)
	req.AddCookie(cookie[0])
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("с сессией /api/users → %d", rec2.Code)
	}

	// /api/me с cookie → authed
	reqMe := httptest.NewRequest("GET", "/api/me", nil)
	reqMe.AddCookie(cookie[0])
	recMe := httptest.NewRecorder()
	s.Handler().ServeHTTP(recMe, reqMe)
	var me map[string]bool
	json.Unmarshal(recMe.Body.Bytes(), &me)
	if !me["authed"] {
		t.Fatalf("/api/me с сессией должен быть authed: %v", me)
	}

	// logout → сессия убита
	reqOut := httptest.NewRequest("POST", "/api/logout", nil)
	reqOut.AddCookie(cookie[0])
	s.Handler().ServeHTTP(httptest.NewRecorder(), reqOut)
	req3 := httptest.NewRequest("GET", "/api/users", nil)
	req3.AddCookie(cookie[0])
	rec3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatal("после logout сессия не должна работать")
	}
}

func TestAuth_ClusterEndpointsBypassSession(t *testing.T) {
	// машинный /api/cluster/heartbeat защищён Bearer-секретом, а не сессией —
	// session-middleware его не должен блокировать (иначе реплики не достучатся).
	fc := &fakeCluster{}
	s := New(Deps{
		Store: fakeStore{}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		AdminPassword: "secret", Cluster: fc, ClusterSecret: "sek",
	})
	if do(s, "POST", "/api/cluster/heartbeat", "sek", `{"code":"ps9"}`).Code != 200 {
		t.Fatal("cluster/heartbeat с Bearer должен проходить мимо session-auth")
	}
}
