package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// Встроенная веб-панель (собранная: internal/server/web/dist, исходники - web/src,
// React + antd + Vite). Сборка фронта: cd internal/server/web && npm ci && npm run build.
//
//go:embed all:web/dist
var distFS embed.FS

// handleIndex раздаёт статику панели + fallback на index.html для клиентских роутов.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		http.Error(w, "ui not built", http.StatusInternalServerError)
		return
	}
	// API/служебные пути не должны попадать в SPA-fallback - честный 404.
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/healthz") {
		http.NotFound(w, r)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	// Реальный файл (index.html, assets/*) - отдаём как есть.
	if f, err := sub.Open(p); err == nil {
		f.Close()
		if strings.HasPrefix(p, "assets/") {
			// В имени файла сборки - хеш содержимого, файл не меняется никогда.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.FileServerFS(sub).ServeHTTP(w, r)
		return
	}
	// Нет такого файла сборки (вкладка открыта до обновления бинаря и просит старый
	// чанк) - 404, а не index.html: HTML вместо JS ломает панель без понятной ошибки.
	if strings.HasPrefix(p, "assets/") {
		http.NotFound(w, r)
		return
	}
	// SPA-fallback: любой неизвестный GET → index.html (клиентский роутинг).
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "ui not built", http.StatusInternalServerError)
		return
	}
	// index.html ссылается на чанки с хешами - его нельзя кэшировать, иначе после
	// обновления бинаря браузер держит старую панель.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}
