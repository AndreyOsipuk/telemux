package telemt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// стаб telemt-API: повторяет envelope {ok,data,revision} и проверяет Authorization.
func stubServer(t *testing.T, wantAuth string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			http.Error(w, `{"ok":false}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"ok":true,"data":{"status":"ok","read_only":true},"revision":"rev1"}`))
	})
	mux.HandleFunc("/v1/users", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"revision":"rev2","data":[
			{"username":"sub_1","enabled":true,"max_tcp_conns":8,"expiration_rfc3339":"2026-07-01T00:00:00Z","current_connections":2},
			{"username":"sub_2","enabled":false,"max_tcp_conns":null,"expiration_rfc3339":null,"current_connections":0}
		]}`))
	})
	return httptest.NewServer(mux)
}

func TestHealth(t *testing.T) {
	srv := stubServer(t, "secret123")
	defer srv.Close()
	c := New(srv.URL, "secret123")
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || !h.ReadOnly {
		t.Fatalf("ждали status=ok read_only=true, получили %+v", h)
	}
}

func TestHealth_AuthRejected(t *testing.T) {
	srv := stubServer(t, "secret123")
	defer srv.Close()
	c := New(srv.URL, "wrong")
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("ждали ошибку при неверном Authorization")
	}
}

func TestListUsers(t *testing.T) {
	srv := stubServer(t, "")
	defer srv.Close()
	c := New(srv.URL, "")
	users, rev, err := c.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rev != "rev2" {
		t.Fatalf("revision: ждали rev2, получили %q", rev)
	}
	if len(users) != 2 {
		t.Fatalf("ждали 2 юзера, получили %d", len(users))
	}
	if users[0].Username != "sub_1" || !users[0].Enabled || users[0].MaxTCPConns == nil || *users[0].MaxTCPConns != 8 {
		t.Fatalf("sub_1 распарсен неверно: %+v", users[0])
	}
	if users[1].Enabled || users[1].MaxTCPConns != nil || users[1].ExpirationRFC3339 != nil {
		t.Fatalf("sub_2 (disabled, null-поля) распарсен неверно: %+v", users[1])
	}
}
