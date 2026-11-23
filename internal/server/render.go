package server

// Сборка конфигов серверов (ADR 0006, этап B2): цикл на мастере собирает
// telemt.config и mtg.config машин каждого сервера, сравнивает с текущей ревизией и
// пишет результат в server_render; серверам с config_source=telemux публикует
// кандидата сам. Ручки: статус с diff, переключение источника.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/render"
	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

// RenderStore - данные сборки конфигов серверов (реализует store.Store).
type RenderStore interface {
	RenderInput(ctx context.Context) (store.RenderInput, error)
	SaveRender(ctx context.Context, rows []store.RenderRow) error
	ListRender(ctx context.Context, no int) ([]store.RenderStatus, error)
	SetConfigSource(ctx context.Context, no int, source string, force bool) (store.Server, error)
	TelemuxServersOfMachine(ctx context.Context, target string) ([]store.Server, error)
}

// renderInterval - период цикла сборки по умолчанию.
const renderInterval = time.Minute

// renderState - будильник цикла сборки и его сериализация.
type renderState struct {
	once sync.Once
	wake chan struct{}
	mu   sync.Mutex // один проход за раз (тик, будильник, тесты)
}

func (s *Server) renderWake() chan struct{} {
	s.renderSt.once.Do(func() { s.renderSt.wake = make(chan struct{}, 1) })
	return s.renderSt.wake
}

// wakeRender просит цикл сборки пройти сейчас (после правки сервера или
// пользователей). Неблокирующе: повторный сигнал схлопывается.
func (s *Server) wakeRender() {
	if s.deps.Render == nil {
		return
	}
	select {
	case s.renderWake() <- struct{}{}:
	default:
	}
}

func (s *Server) renderLoop(ctx context.Context) {
	every := s.deps.RenderInterval
	if every <= 0 {
		every = renderInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.renderOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.renderWake():
		}
	}
}

// RenderNow - один проход цикла сборки сейчас, синхронно (тесты, отладка).
func (s *Server) RenderNow(ctx context.Context) {
	if s.deps.Render != nil && s.deps.Revisions != nil {
		s.renderOnce(ctx)
	}
}

// renderOnce - один проход: только на мастере (пишет server_render и ревизии).
func (s *Server) renderOnce(ctx context.Context) {
	st, err := s.currentRole(ctx)
	if err != nil || !st.IsMaster() {
		return
	}
	s.renderSt.mu.Lock()
	defer s.renderSt.mu.Unlock()
	in, err := s.deps.Render.RenderInput(ctx)
	if err != nil {
		s.deps.Log.Error("сборка конфигов: чтение", "err", err)
		return
	}
	rows, published := s.renderAll(ctx, in, time.Now())
	if err := s.deps.Render.SaveRender(ctx, rows); err != nil {
		s.deps.Log.Error("сборка конфигов: запись сравнения", "err", err)
	}
	if published {
		s.revs.bump()
	}
}

// renderAll собирает кандидатов всех серверов, публикует для config_source=telemux
// и возвращает строки сравнения. published - появилась хоть одна новая ревизия.
func (s *Server) renderAll(ctx context.Context, in store.RenderInput, now time.Time) ([]store.RenderRow, bool) {
	users := make([]render.User, len(in.Users))
	for i, u := range in.Users {
		users[i] = render.User{Key: u.Key, Secret: u.Secret, ExpiresAt: u.ExpiresAt,
			PreviousSecret: u.PreviousSecret, PreviousSecretExpiresAt: u.PreviousSecretExpiresAt}
	}
	// Ресурс машины, который собирают несколько серверов: кандидаты спорили бы
	// друг с другом, поэтому такой ресурс не собирается вовсе.
	owners := map[string][]int{}
	for _, srv := range in.Servers {
		for _, j := range render.Jobs(srv) {
			k := store.CurrentKey(j.Target, j.Resource)
			owners[k] = append(owners[k], srv.No)
		}
	}
	var rows []store.RenderRow
	published := false
	for _, srv := range in.Servers {
		for _, j := range render.Jobs(srv) {
			row := store.RenderRow{ServerNo: srv.No, Target: j.Target, Resource: j.Resource, RenderedAt: now}
			key := store.CurrentKey(j.Target, j.Resource)
			if sha, ok := in.Current[key]; ok {
				row.CurrentSHA = &sha
			}
			content, err := s.renderJob(srv, j, in.Machines, users, now)
			if err == nil && len(owners[key]) > 1 {
				err = &render.Error{Code: "shared_machine", Msg: fmt.Sprintf("ресурс %s машины %s собирают серверы %v", j.Resource, j.Target, owners[key])}
			}
			if content != nil {
				sha, text := telemtcfg.SHA256(content), string(content)
				row.CandidateSHA, row.Candidate = &sha, &text
				row.Equal = row.CurrentSHA != nil && *row.CurrentSHA == sha
			}
			if err != nil {
				row.Error = renderErrText(err)
			}
			if err == nil && !row.Equal && srv.ConfigSource == store.ConfigSourceTelemux {
				pub, perr := s.deps.Revisions.PublishResource(ctx, j.Target, j.Resource, content)
				switch {
				case errors.Is(perr, store.ErrMachineDisabled):
					// Машину выключили между чтением и публикацией: это не сбой.
					row.Error = render.CodeMachineDisabled + ": " + perr.Error()
				case errors.Is(perr, store.ErrResourceNotAssigned):
					row.Error = render.CodeNotAssigned + ": " + perr.Error()
				case perr != nil:
					s.deps.Log.Error("сборка конфигов: публикация", "server", srv.No, "target", j.Target, "resource", j.Resource, "err", perr)
					row.Error = "publish_failed: " + perr.Error()
				default:
					row.CurrentSHA, row.Equal = row.CandidateSHA, true
					if len(pub.Changed) > 0 {
						published = true
						s.deps.Log.Info("сборка конфигов: новая ревизия", "server", srv.No, "target", j.Target,
							"resource", j.Resource, "revision", pub.Revision)
					}
				}
			}
			rows = append(rows, row)
		}
	}
	return rows, published
}

// renderErrText - «код: текст» ошибки сборки.
func renderErrText(err error) string {
	var e *render.Error
	if errors.As(err, &e) {
		return e.Error()
	}
	return "internal: " + err.Error()
}

// renderJob - кандидат ресурса машины. Ресурс не назначен - кандидат собирается
// (виден в diff), но с ошибкой not_assigned: публиковать его некуда.
func (s *Server) renderJob(srv store.Server, j render.Job, machines map[string]store.RenderMachine,
	users []render.User, now time.Time) ([]byte, error) {
	m, ok := machines[j.Target]
	if !ok {
		return nil, &render.Error{Code: render.CodeUnknown, Msg: fmt.Sprintf("машины %s нет", j.Target)}
	}
	machine := render.Machine{Code: m.Code, Kind: j.Kind, Address: m.Address}
	var content []byte
	var err error
	switch j.Resource {
	case render.ResourceTelemtConfig:
		content, err = render.TelemtMain(srv, machine)
	case render.ResourceMtgConfig, store.ResourceMtgSecondaryConfig:
		content, err = render.MtgConfig(srv, machine, users, render.MtgOpts{Now: now})
	default:
		return nil, &render.Error{Code: "internal", Msg: "неизвестный ресурс " + j.Resource}
	}
	if err != nil {
		return nil, err
	}
	if !m.Enabled {
		return content, &render.Error{Code: render.CodeMachineDisabled,
			Msg: fmt.Sprintf("машина %s выключена: новые ревизии ей не публикуются", j.Target)}
	}
	if !slices.Contains(m.Resources, j.Resource) {
		return content, &render.Error{Code: render.CodeNotAssigned,
			Msg: fmt.Sprintf("ресурс %s не назначен машине %s (targets.resources)", j.Resource, j.Target)}
	}
	return content, nil
}

// managedByTelemux - ресурс машины собирает сервер с config_source=telemux (тогда
// публикация извне, от бота, отвергается).
func (s *Server) managedByTelemux(ctx context.Context, target, resource string) (int, bool, error) {
	if s.deps.Render == nil || (resource != render.ResourceTelemtConfig && resource != render.ResourceMtgConfig && resource != store.ResourceMtgSecondaryConfig) {
		return 0, false, nil
	}
	list, err := s.deps.Render.TelemuxServersOfMachine(ctx, target)
	if err != nil {
		return 0, false, err
	}
	for _, srv := range list {
		for _, j := range render.Jobs(srv) {
			if j.Target == target && j.Resource == resource {
				return srv.No, true, nil
			}
		}
	}
	return 0, false, nil
}

// renderItem - строка GET /api/v1/servers/{no}/render.
type renderItem struct {
	Target       string    `json:"target"`
	Resource     string    `json:"resource"`
	CandidateSHA *string   `json:"candidate_sha"`
	CurrentSHA   *string   `json:"current_sha"`
	Equal        bool      `json:"equal"`
	Error        *string   `json:"error"`
	RenderedAt   time.Time `json:"rendered_at"`
	Diff         string    `json:"diff"`
}

func (s *Server) routesRender() {
	// Последнее сравнение сборки telemux с текущими конфигами машин сервера.
	s.mux.HandleFunc("GET /api/v1/servers/{no}/render", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		no, ok := serverNo(r)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет сервера %q", r.PathValue("no")), s.deps.Log)
			return
		}
		list, err := s.deps.Render.ListRender(r.Context(), no)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		out := make([]renderItem, 0, len(list))
		for _, st := range list {
			it := renderItem{Target: st.Target, Resource: st.Resource, CandidateSHA: st.CandidateSHA,
				CurrentSHA: st.CurrentSHA, Equal: st.Equal, RenderedAt: st.RenderedAt}
			if st.Error != "" {
				e := st.Error
				it.Error = &e
			}
			if st.Candidate != nil && !st.Equal {
				cur := ""
				if st.Current != nil {
					cur = *st.Current
				}
				name := st.Target + "/" + st.Resource
				it.Diff = render.SecretDiff(cur, *st.Candidate, name+" (текущий)", name+" (telemux)")
			}
			out = append(out, it)
		}
		writeJSON(w, map[string]any{"data": out})
	})

	// Источник конфигов сервера: bot или telemux. На telemux - только если сборка
	// совпала по всем машинам (иначе 409 render_mismatch), с force - всегда.
	s.mux.HandleFunc("PUT /api/v1/servers/{no}/config-source", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		no, ok := serverNo(r)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет сервера %q", r.PathValue("no")), s.deps.Log)
			return
		}
		var body struct {
			Source string `json:"source"`
			Force  bool   `json:"force"`
		}
		if !s.decodeBody(w, r, 4<<10, &body, "источника конфигов ({source, force})") {
			return
		}
		if body.Source != store.ConfigSourceBot && body.Source != store.ConfigSourceTelemux {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("source %q: допустимы bot, telemux", body.Source), s.deps.Log)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		srv, err := s.deps.Render.SetConfigSource(r.Context(), no, body.Source, body.Force)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		s.deps.Log.Info("источник конфигов сервера", "server", no, "source", body.Source, "force", body.Force)
		s.wakeRender()
		list := []store.Server{srv}
		s.fillServerLive(list, time.Now())
		writeJSON(w, list[0])
	})
}
