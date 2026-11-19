package bridge

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Бинарь обновления (ресурс stack) на релее идёт через мост выхода: мост пересылает
// запрос с токеном агента и отдаёт тело потоком. Без этого stack-apply на релее получал 404.
func TestStackBlob_ForwardsWithAgentToken(t *testing.T) {
	bin := bytes.Repeat([]byte("B"), 300<<10)
	var gotAuth, gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotURI = r.Header.Get("Authorization"), r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(bin)
	}))
	t.Cleanup(srv.Close)
	h := newBridge(t, srv.URL).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/stack/blobs/"+joinTok+"?target=pr1", nil)
	req.Header.Set("Authorization", "Bearer agenttok")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), bin) {
		t.Fatalf("код %d, тело %d байт", rec.Code, rec.Body.Len())
	}
	if gotAuth != "Bearer agenttok" || gotURI != "/v1/stack/blobs/"+joinTok+"?target=pr1" {
		t.Fatalf("мастеру ушло %q %q", gotAuth, gotURI)
	}
}

func TestStackBlob_RejectsBadInputWithoutForwarding(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	t.Cleanup(srv.Close)
	h := newBridge(t, srv.URL).Handler()
	for _, c := range []struct {
		path, auth string
		want       int
	}{
		{"/v1/stack/blobs/abc?target=pr1", "Bearer x", http.StatusNotFound},
		{"/v1/stack/blobs/" + joinTok + "?target=../x", "Bearer x", http.StatusNotFound},
		{"/v1/stack/blobs/" + joinTok, "Bearer x", http.StatusNotFound},
		{"/v1/stack/blobs/" + joinTok + "?target=pr1", "", http.StatusUnauthorized},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", c.path, nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%s: код %d, ждали %d", c.path, rec.Code, c.want)
		}
	}
	if calls != 0 {
		t.Fatalf("мастеру ушло %d запросов", calls)
	}
}

// 404 мастера (бинарь не этой машины) доходит до агента как есть - stack-apply пробует
// следующий адрес; 5xx - мост сам идёт к следующему мастеру.
func TestStackBlob_PassesNotFoundAndSkipsFailedMaster(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	t.Cleanup(bad.Close)
	nf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	t.Cleanup(nf.Close)
	h := newBridge(t, bad.URL, nf.URL).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/stack/blobs/"+joinTok+"?target=pr1", nil)
	req.Header.Set("Authorization", "Bearer x")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("код %d", rec.Code)
	}
}
