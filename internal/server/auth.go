package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// auth — простая сессионная авторизация веб-панели (admin login → cookie).
// Если пароль не задан — auth ВЫКЛЮЧЕН (открытый режим за фаерволом, как было).
// Машинные эндпоинты (/api/cluster/*, /join/*) имеют свою Bearer/token-авторизацию
// и не требуют сессии. Статика SPA публична; защищены человеческие /api/*.
type auth struct {
	user, pass string
	enabled    bool
	ttl        time.Duration

	mu       sync.Mutex
	sessions map[string]time.Time // token → истечение
}

const sessionCookie = "telemux_session"

func newAuth(user, pass string) *auth {
	a := &auth{user: user, pass: pass, enabled: pass != "", ttl: 24 * time.Hour, sessions: map[string]time.Time{}}
	if user == "" {
		a.user = "admin"
	}
	return a
}

func (a *auth) newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// valid — есть ли действующая сессия в запросе.
func (a *auth) valid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

// isPublic — пути, не требующие сессии.
func (a *auth) isPublic(path string) bool {
	switch {
	case path == "/healthz",
		path == "/api/login", path == "/api/me",
		strings.HasPrefix(path, "/api/cluster/"), // Bearer cluster-secret
		strings.HasPrefix(path, "/join/"):         // одноразовый token
		return true
	case !strings.HasPrefix(path, "/api/"): // статика SPA (/, /assets/*)
		return true
	}
	return false
}

// wrap — middleware: защищает человеческие /api/*, остальное пропускает.
func (a *auth) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.enabled || a.isPublic(r.URL.Path) || a.valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
	})
}

func (a *auth) routes(mux *http.ServeMux) {
	// Состояние авторизации — публично (SPA решает, показывать ли логин).
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"auth_enabled": a.enabled, "authed": !a.enabled || a.valid(r)})
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		uOK := subtle.ConstantTimeCompare([]byte(body.Username), []byte(a.user)) == 1
		pOK := subtle.ConstantTimeCompare([]byte(body.Password), []byte(a.pass)) == 1
		if !a.enabled || !uOK || !pOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "неверный логин или пароль"})
			return
		}
		tok := a.newToken()
		a.mu.Lock()
		a.sessions[tok] = time.Now().Add(a.ttl)
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteLaxMode, MaxAge: int(a.ttl.Seconds()),
		})
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			a.mu.Lock()
			delete(a.sessions, c.Value)
			a.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
		writeJSON(w, map[string]bool{"ok": true})
	})
}
