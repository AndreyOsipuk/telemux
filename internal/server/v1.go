package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
	"github.com/AndreyOsipuk/telemux/internal/xui"
)

// RevisionStore - ревизии желаемого состояния (реализует store.Store).
type RevisionStore interface {
	UpsertUser(ctx context.Context, r store.ImportRow) (bool, error)
	PublishTelemtUsers(ctx context.Context, defaultMaxConns int) (store.PublishResult, error)
	RevisionStatus(ctx context.Context, rev int64) ([]store.TargetStatus, error)
	PublishResource(ctx context.Context, target, resource string, content []byte) (store.PublishResult, error)
}

// Предел тела PUT /api/v1/users: MaxImportBatch пользователей с запасом.
const maxUsersBody = 32 << 20

// v1User - пользователь в API бота.
type v1User struct {
	Key                     string     `json:"key"`
	Secret                  string     `json:"secret"`
	ExpiresAt               *time.Time `json:"expires_at"`
	MaxTCPConns             *int       `json:"max_tcp_conns"`
	PreviousSecret          string     `json:"previous_secret"`
	PreviousSecretExpiresAt *time.Time `json:"previous_secret_expires_at"`
}

func (u v1User) validate() error {
	if u.MaxTCPConns != nil && *u.MaxTCPConns < 1 {
		return fmt.Errorf("пользователь %q: max_tcp_conns должен быть больше 0", u.Key)
	}
	return telemtcfg.ValidateRenderUser(telemtcfg.RenderUser{Key: u.Key, Secret: u.Secret, PreviousSecret: u.PreviousSecret})
}

func (u v1User) row() store.ImportRow {
	return store.ImportRow{
		Username: u.Key, Secret: u.Secret, ExpirationAt: u.ExpiresAt, MaxTCPConns: u.MaxTCPConns,
		PreviousSecret: u.PreviousSecret, PreviousSecretExpiresAt: u.PreviousSecretExpiresAt,
	}
}

// botAuthed - Bearer-токен бота, сравнение за постоянное время.
func (s *Server) botAuthed(r *http.Request) bool {
	if s.deps.BotToken == "" {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(token), []byte(s.deps.BotToken)) == 1
}

// requireBotOrSession пускает бота по токену или человека по сессии панели.
func (s *Server) requireBotOrSession(w http.ResponseWriter, r *http.Request) bool {
	if s.botAuthed(r) || (s.auth.enabled && s.auth.valid(r)) || (!s.auth.enabled && s.deps.BotToken == "") {
		return true
	}
	writeErr(w, http.StatusUnauthorized, errors.New("нужен токен бота или вход в панель"), s.deps.Log)
	return false
}

// publish публикует ревизию после изменения пользователей и будит sync-loop.
func (s *Server) publish(w http.ResponseWriter, r *http.Request) (store.PublishResult, bool) {
	res, err := s.deps.Revisions.PublishTelemtUsers(r.Context(), s.deps.DefaultMaxConns)
	if err != nil {
		s.writeStoreErr(w, err)
		return res, false
	}
	s.markDirty()
	if len(res.Changed) > 0 {
		s.revs.bump()
	}
	return res, true
}

func (s *Server) routesV1() {
	// Полный набор пользователей от бота: сверка + публикация ревизии.
	s.mux.HandleFunc("PUT /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) || !s.requireMaster(w, r) {
			return
		}
		var body struct {
			Users []v1User `json:"users"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxUsersBody)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON {users: [...]}: %w", err), s.deps.Log)
			return
		}
		if len(body.Users) > store.MaxImportBatch {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("слишком много пользователей: %d > %d", len(body.Users), store.MaxImportBatch), s.deps.Log)
			return
		}
		rows := make([]store.ImportRow, 0, len(body.Users))
		seen := make(map[string]bool, len(body.Users))
		for _, u := range body.Users {
			if err := u.validate(); err != nil {
				writeErr(w, http.StatusBadRequest, err, s.deps.Log)
				return
			}
			if seen[u.Key] {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("ключ %q повторяется", u.Key), s.deps.Log)
				return
			}
			seen[u.Key] = true
			rows = append(rows, u.row())
		}
		force := r.URL.Query().Get("force") == "true"
		rec, err := s.deps.Users.ReconcileUsers(r.Context(), rows, force)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if rec.Aborted {
			writeJSONStatus(w, http.StatusConflict, errorBody{
				Code:    "mass_delete_guard",
				Message: "набор удалил бы слишком много пользователей; изменения не применены (повторите с force=true, если это намеренно)",
				Error:   "mass_delete_guard",
			})
			return
		}
		pub, ok := s.publish(w, r)
		if !ok {
			return
		}
		writeJSON(w, map[string]any{
			"revision": pub.Revision, "changed": pub.Changed,
			"inserted": rec.Inserted, "updated": rec.Updated, "deleted": rec.Deleted,
		})
	})

	// Точечное создание или изменение пользователя (смена ключа, продление).
	s.mux.HandleFunc("PUT /api/v1/users/{key}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) || !s.requireMaster(w, r) {
			return
		}
		var u v1User
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&u); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON пользователя: %w", err), s.deps.Log)
			return
		}
		u.Key = r.PathValue("key")
		if err := u.validate(); err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		created, err := s.deps.Revisions.UpsertUser(r.Context(), u.row())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		pub, ok := s.publish(w, r)
		if !ok {
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSONStatus(w, status, map[string]any{"revision": pub.Revision, "changed": pub.Changed, "created": created})
	})

	// Удаление пользователя (блокировка, возврат денег).
	s.mux.HandleFunc("DELETE /api/v1/users/{key}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) || !s.requireMaster(w, r) {
			return
		}
		ok, err := s.deps.Users.DeleteUser(r.Context(), r.PathValue("key"))
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет пользователя %q", r.PathValue("key")), s.deps.Log)
			return
		}
		pub, ok := s.publish(w, r)
		if !ok {
			return
		}
		writeJSON(w, map[string]any{"revision": pub.Revision, "changed": pub.Changed})
	})

	// Готовый ресурс одной машины от бота (config.toml mtg-multi): тело - текст файла.
	s.mux.HandleFunc("PUT /api/v1/targets/{code}/resources/{resource}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) || !s.requireMaster(w, r) {
			return
		}
		resource := r.PathValue("resource")
		if !store.PublishableResources[resource] {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("ресурс %q не публикуется извне (допустимы: %s, %s)", resource, store.ResourceMtgConfig, store.ResourceXuiClients), s.deps.Log)
			return
		}
		content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil || len(bytes.TrimSpace(content)) == 0 {
			writeErr(w, http.StatusBadRequest, errors.New("нужно непустое содержимое ресурса, не больше 4 МБ"), s.deps.Log)
			return
		}
		if resource == store.ResourceXuiClients {
			var clients []xui.Client
			if err := json.Unmarshal(content, &clients); err != nil {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("xui.clients - JSON-массив клиентов: %w", err), s.deps.Log)
				return
			}
		}
		pub, err := s.deps.Revisions.PublishResource(r.Context(), r.PathValue("code"), resource, content)
		switch {
		case errors.Is(err, store.ErrUnknownTarget):
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", r.PathValue("code")), s.deps.Log)
			return
		case errors.Is(err, store.ErrResourceNotAssigned):
			writeErr(w, http.StatusBadRequest, fmt.Errorf("ресурс %q не назначен машине %q", resource, r.PathValue("code")), s.deps.Log)
			return
		case err != nil:
			s.writeStoreErr(w, err)
			return
		}
		if len(pub.Changed) > 0 {
			s.revs.bump()
		}
		writeJSON(w, map[string]any{"revision": pub.Revision, "changed": pub.Changed})
	})

	// Дошла ли ревизия до машин (бот выдаёт ссылку после применения).
	s.mux.HandleFunc("GET /api/v1/revisions/{rev}/status", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		rev, err := strconv.ParseInt(r.PathValue("rev"), 10, 64)
		if err != nil || rev < 1 {
			writeErr(w, http.StatusBadRequest, errors.New("номер ревизии - целое больше 0"), s.deps.Log)
			return
		}
		targets, err := s.deps.Revisions.RevisionStatus(r.Context(), rev)
		if errors.Is(err, store.ErrUnknownRevision) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("ревизии %d нет", rev), s.deps.Log)
			return
		}
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		applied := 0
		for _, t := range targets {
			if t.Applied {
				applied++
			}
		}
		if targets == nil {
			targets = []store.TargetStatus{}
		}
		writeJSON(w, map[string]any{"revision": rev, "applied": applied, "total": len(targets), "targets": targets})
	})
}
