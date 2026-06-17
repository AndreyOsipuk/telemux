package telemt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// captures — записывает последний запрос мутации для проверки.
type capture struct {
	method  string
	path    string
	ifMatch string
	body    map[string]any
}

func mutServer(t *testing.T, cap *capture, status int, errBody string) *httptest.Server {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.ifMatch = r.Header.Get("If-Match")
		cap.body = nil
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &cap.body)
		}
		if status != 0 && status != http.StatusOK {
			http.Error(w, errBody, status)
			return
		}
		w.Write([]byte(`{"ok":true,"data":{},"revision":"rev-new"}`))
	}
	return httptest.NewServer(http.HandlerFunc(h))
}

func TestApplyOp_Create(t *testing.T) {
	var cap capture
	srv := mutServer(t, &cap, 0, "")
	defer srv.Close()
	exp := "2026-07-01T00:00:00Z"
	rev, err := New(srv.URL, "").ApplyOp(context.Background(),
		telemtsync.SyncOp{Kind: telemtsync.OpCreate, Username: "sub_1", Secret: "eeabc", ExpirationRFC3339: &exp}, "rev-cur")
	if err != nil {
		t.Fatal(err)
	}
	if rev != "rev-new" {
		t.Fatalf("ждали новый revision, получили %q", rev)
	}
	if cap.method != http.MethodPost || cap.path != "/v1/users" || cap.ifMatch != "rev-cur" {
		t.Fatalf("create: метод/путь/If-Match неверны: %+v", cap)
	}
	if cap.body["username"] != "sub_1" || cap.body["secret"] != "eeabc" || cap.body["enabled"] != true {
		t.Fatalf("create body неверно: %+v", cap.body)
	}
}

func TestApplyOp_PatchNullRemovesExpiration(t *testing.T) {
	var cap capture
	srv := mutServer(t, &cap, 0, "")
	defer srv.Close()
	// SetExpiration с nil → JSON null (Remove срока).
	_, err := New(srv.URL, "").ApplyOp(context.Background(),
		telemtsync.SyncOp{Kind: telemtsync.OpPatch, Username: "sub_1",
			Fields: telemtsync.PatchFields{SetExpiration: true, ExpirationRFC3339: nil}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if cap.method != http.MethodPatch || cap.path != "/v1/users/sub_1" {
		t.Fatalf("patch метод/путь неверны: %+v", cap)
	}
	v, ok := cap.body["expiration_rfc3339"]
	if !ok || v != nil {
		t.Fatalf("ждали expiration_rfc3339:null (Remove), получили %#v (present=%v)", v, ok)
	}
	if _, has := cap.body["max_tcp_conns"]; has {
		t.Fatalf("неизменённые поля не должны слаться: %+v", cap.body)
	}
}

func TestApplyOp_Delete(t *testing.T) {
	var cap capture
	srv := mutServer(t, &cap, 0, "")
	defer srv.Close()
	_, err := New(srv.URL, "").ApplyOp(context.Background(),
		telemtsync.SyncOp{Kind: telemtsync.OpDelete, Username: "sub_9"}, "rev-cur")
	if err != nil {
		t.Fatal(err)
	}
	if cap.method != http.MethodDelete || cap.path != "/v1/users/sub_9" || cap.ifMatch != "rev-cur" {
		t.Fatalf("delete неверно: %+v", cap)
	}
}

func TestApplyOp_ErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(*APIError) bool
		name   string
	}{
		{409, `{"error":{"code":"revision_conflict"}}`, (*APIError).RevisionConflict, "revision_conflict"},
		{403, `{"error":{"code":"read_only"}}`, (*APIError).ReadOnly, "read_only"},
		{409, `{"error":{"code":"user_exists"}}`, (*APIError).AlreadyDone, "user_exists"},
		{404, `not found`, (*APIError).AlreadyDone, "404→AlreadyDone"},
	}
	for _, tc := range cases {
		var cap capture
		srv := mutServer(t, &cap, tc.status, tc.body)
		_, err := New(srv.URL, "").ApplyOp(context.Background(),
			telemtsync.SyncOp{Kind: telemtsync.OpDelete, Username: "x"}, "")
		srv.Close()
		ae, ok := err.(*APIError)
		if !ok {
			t.Fatalf("%s: ждали *APIError, получили %T", tc.name, err)
		}
		if !tc.check(ae) {
			t.Fatalf("%s: предикат не сработал на %+v", tc.name, ae)
		}
	}
}
