package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// Встроенная React-SPA (собранная: internal/server/web/dist, см. web/ — Vite+React+TS).
// Сборка фронта: cd internal/server/web && npm install && npm run build.
//
//go:embed all:web/dist
var distFS embed.FS

// handleIndex раздаёт статику SPA + fallback на index.html для клиентских роутов.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		http.Error(w, "ui not built", http.StatusInternalServerError)
		return
	}
	// API/служебные пути не должны попадать в SPA-fallback — честный 404.
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/healthz") {
		http.NotFound(w, r)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	// Реальный файл (index.html, assets/*) — отдаём как есть.
	if f, err := sub.Open(p); err == nil {
		f.Close()
		http.FileServerFS(sub).ServeHTTP(w, r)
		return
	}
	// SPA-fallback: любой неизвестный GET → index.html (клиентский роутинг).
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "ui not built", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}
