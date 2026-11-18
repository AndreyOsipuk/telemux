package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Первый файл из web/dist/assets - имя с хешем меняется при каждой сборке.
func someAsset(t *testing.T) string {
	t.Helper()
	entries, err := fs.ReadDir(distFS, "web/dist/assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("в web/dist/assets нет файлов: %v", err)
	}
	return "/assets/" + entries[0].Name()
}

func serve(s *Server, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Клиентские роуты панели отдают index.html, и его нельзя кэшировать: после
// обновления бинаря вкладка должна получить новые имена чанков.
func TestWebSPAFallbackNoCache(t *testing.T) {
	s := newTestServer(false, 0)
	for _, p := range []string{"/", "/pairs", "/drift", "/users", "/api-docs"} {
		rec := serve(s, p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<div id="root">`) {
			t.Fatalf("%s: код=%d, ждали index.html", p, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: Cache-Control=%q, ждали no-cache", p, cc)
		}
	}
}

// Файлы сборки с хешем в имени не меняются - кэшируются навсегда.
func TestWebAssetsImmutable(t *testing.T) {
	s := newTestServer(false, 0)
	rec := serve(s, someAsset(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("asset: код=%d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset: Cache-Control=%q, ждали immutable", cc)
	}
}

// Устаревший чанк (вкладка открыта до обновления) - честный 404, а не index.html:
// иначе браузер пытается выполнить HTML как JS и панель молча ломается.
func TestWebMissingAssetIs404(t *testing.T) {
	s := newTestServer(false, 0)
	rec := serve(s, "/assets/PairsPage-old.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("устаревший чанк: код=%d, ждали 404", rec.Code)
	}
}

// API и служебные пути не попадают в SPA-fallback.
func TestWebAPINotFallback(t *testing.T) {
	s := newTestServer(false, 0)
	for _, p := range []string{"/api/no-such-endpoint", "/healthzz"} {
		if rec := serve(s, p); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: код=%d, ждали 404", p, rec.Code)
		}
	}
}
