package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/agentjoin"
	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/tlspin"
)

// Добавление машины одной командой (по аналогии с 3x-ui, но pull-моделью):
// мастер выдаёт curl ... | sudo bash, скрипт на новой машине скачивает бинарь,
// меняет одноразовый токен на токен агента и запускает агента в режиме compare.

// JoinStore - машины и одноразовые токены установки (реализует store.Store).
type JoinStore interface {
	TargetByCode(ctx context.Context, code string) (store.Target, error)
	CreateTarget(ctx context.Context, t store.Target, ttl time.Duration) (store.JoinIssue, error)
	IssueJoinToken(ctx context.Context, code string, ttl time.Duration) (store.JoinIssue, error)
	JoinTarget(ctx context.Context, token string) (store.JoinInfo, error)
	ExchangeJoin(ctx context.Context, token string) (store.JoinExchange, error)
}

// Результат открытия адреса выхода на файрволе мастера (поле firewall).
const (
	fwOpened        = "opened"
	fwNotConfigured = "not_configured"
	fwFailed        = "failed"
	fwNotNeeded     = "not_needed"
)

// newTargetBody - тело POST /api/v1/targets (схема NewTarget).
type newTargetBody struct {
	Code        string   `json:"code"`
	Kind        string   `json:"kind"`
	RelayCode   string   `json:"relay_code"`
	Address     string   `json:"address"`
	BridgeSocks string   `json:"bridge_socks"`
	Resources   []string `json:"resources"`
}

// joinCommand - ответ JoinCommand.
type joinCommand struct {
	Target    string    `json:"target"`
	Command   string    `json:"command"`
	ExpiresAt time.Time `json:"expires_at"`
	Firewall  string    `json:"firewall,omitempty"`
}

// validate проверяет новую машину; ошибки - тексты для 400.
func (b newTargetBody) validate() error {
	if !agentjoin.ValidCode(b.Code) {
		return fmt.Errorf("code %q: нужен вид ^[a-z][a-z0-9-]{1,31}$", b.Code)
	}
	switch b.Kind {
	case "exit":
		if b.RelayCode == "" {
			return errors.New("для выхода нужен relay_code - код его релея")
		}
		if !agentjoin.ValidCode(b.RelayCode) {
			return fmt.Errorf("relay_code %q: нет такого релея", b.RelayCode)
		}
		if _, err := agentjoin.ParseExitAddress(b.Address); err != nil {
			return err
		}
		if err := agentjoin.ValidBridgeSocks(b.BridgeSocks); err != nil {
			return err
		}
	case "relay":
		if b.RelayCode != "" || b.BridgeSocks != "" {
			return errors.New("relay_code и bridge_socks задаются только для выхода (kind=exit)")
		}
		// Адрес релея необязателен: он нужен сборке telemt.config (announce_ip, ADR 0006).
		if b.Address != "" {
			if _, err := agentjoin.ParseExitAddress(b.Address); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("kind %q: допустимы exit и relay", b.Kind)
	}
	if b.Resources == nil {
		return errors.New("нужен resources - список, можно пустой")
	}
	for _, r := range b.Resources {
		if r == agentjoin.ResourceMtgSecondaryConfig && b.Kind != "relay" {
			return errors.New("второй mtg назначается релею")
		}
	}
	return agentjoin.ValidateResources(b.Resources)
}

// masterPins - отпечатки сертификата мастера: base64 SPKI для curl и hex для агента.
type masterPins struct{ spki, cert string }

// joinConfig проверяет, что мастер настроен выдавать команды; needPins - нужен
// ли сертификат (машине-выходу). Ошибка - текст для 409 (или 503 на ручках /v1/join).
func (s *Server) joinConfig(needPins bool) (masterPins, error) {
	var p masterPins
	if s.deps.AgentPublicURL == "" {
		return p, errors.New("на мастере не задан TELEMUX_AGENT_PUBLIC_URL - внешний https-адрес API агентов, например https://<адрес мастера>:18443")
	}
	if err := agentjoin.ValidPublicURL(s.deps.AgentPublicURL); err != nil {
		return p, err
	}
	if !needPins {
		return p, nil
	}
	var err error
	if p.spki, err = tlspin.SPKIPinFile(s.deps.TLSCertFile); err != nil {
		s.deps.Log.Error("join: сертификат мастера не читается", "file", s.deps.TLSCertFile, "err", err)
		return p, errors.New("на мастере нет сертификата API агентов (TELEMUX_TLS_CERT, telemux tls-cert)")
	}
	if p.cert, err = tlspin.FingerprintFile(s.deps.TLSCertFile); err != nil {
		s.deps.Log.Error("join: сертификат мастера не читается", "file", s.deps.TLSCertFile, "err", err)
		return p, errors.New("на мастере нет сертификата API агентов (TELEMUX_TLS_CERT, telemux tls-cert)")
	}
	return p, nil
}

// bridgeExits - выходы релея, годные как мосты (проверенные адрес и socks).
// Машины, заведённые командой target с мусором в settings, пропускаются.
func (s *Server) bridgeExits(exits []store.Target) []agentjoin.Exit {
	out := []agentjoin.Exit{}
	for _, e := range exits {
		x := agentjoin.Exit{Address: e.Settings[store.SettingAddress], Socks: e.Settings[store.SettingBridgeSocks]}
		if _, err := agentjoin.BridgeRoute(x.Address, x.Socks); err != nil {
			s.deps.Log.Warn("join: выход не годится мостом", "exit", e.Code, "err", err)
			continue
		}
		out = append(out, x)
	}
	return out
}

// joinRoute - как машина пойдёт к мастеру при установке.
func (s *Server) joinRoute(t store.Target, exits []store.Target, pins masterPins) (agentjoin.Route, error) {
	if t.Kind == "exit" {
		return agentjoin.DirectRoute(s.deps.AgentPublicURL, pins.spki)
	}
	bridges := s.bridgeExits(exits)
	if len(bridges) == 0 {
		return agentjoin.Route{}, store.ErrRelayNoExits
	}
	return agentjoin.BridgeRoute(bridges[0].Address, bridges[0].Socks)
}

// agentEnv - содержимое agent.env новой машины.
func (s *Server) agentEnv(t store.Target, exits []store.Target, pins masterPins) (string, error) {
	return agentjoin.Env(agentjoin.EnvParams{
		Target: t.Code, Kind: t.Kind, Resources: t.Resources,
		PublicURL: s.deps.AgentPublicURL, MasterPin: pins.cert, Exits: s.bridgeExits(exits),
	})
}

// openFirewall открывает адрес выхода на TLS-порту агентов (если настроена цепочка).
func (s *Server) openFirewall(ctx context.Context, t store.Target) string {
	if t.Kind != "exit" {
		return fwNotNeeded
	}
	if s.deps.AgentFWChain == "" {
		return fwNotConfigured
	}
	ip, err := agentjoin.ParseExitAddress(t.Settings[store.SettingAddress])
	if err != nil {
		s.deps.Log.Error("файрвол: адрес выхода", "target", t.Code, "err", err)
		return fwFailed
	}
	_, portStr, err := net.SplitHostPort(s.deps.TLSListen)
	port, perr := strconv.Atoi(portStr)
	if err != nil || perr != nil || port < 1 || port > 65535 {
		s.deps.Log.Error("файрвол: порт API агентов не определить из TELEMUX_TLS_LISTEN", "listen", s.deps.TLSListen)
		return fwFailed
	}
	fw := s.deps.Firewall
	if fw == nil {
		fw = agentjoin.IPTables{}
	}
	if err := fw.Allow(ctx, s.deps.AgentFWChain, ip, port); err != nil {
		s.deps.Log.Error("файрвол: адрес выхода не открыт", "target", t.Code, "address", ip.String(), "chain", s.deps.AgentFWChain, "err", err)
		return fwFailed
	}
	s.deps.Log.Info("файрвол: адрес выхода открыт", "target", t.Code, "address", ip.String(), "port", port)
	return fwOpened
}

// commandFor собирает JoinCommand по выпущенному токену.
func (s *Server) commandFor(iss store.JoinIssue, pins masterPins) (joinCommand, error) {
	cmd := joinCommand{Target: iss.Target.Code}
	if iss.Join == nil {
		// Новый релей без выходов: команды пока нет (сначала выходы, потом .../join).
		cmd.ExpiresAt = time.Now().UTC()
		return cmd, nil
	}
	route, err := s.joinRoute(iss.Target, iss.Exits, pins)
	if err != nil {
		return cmd, err
	}
	if cmd.Command, err = agentjoin.Command(route, iss.Join.Token); err != nil {
		return cmd, err
	}
	cmd.ExpiresAt = iss.Join.ExpiresAt.UTC()
	return cmd, nil
}

// selfBinary - бинарь этого мастера, если он годится для новой машины (linux/amd64).
func selfBinary() (string, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("мастер собран под %s/%s, а машинам нужен linux/amd64", runtime.GOOS, runtime.GOARCH)
	}
	return os.Executable()
}

// joinNotFound - 404 без подробностей (токен чужой, просрочен или использован).
func (s *Server) joinNotFound(w http.ResponseWriter, err error) {
	s.deps.Log.Warn("join: токен отклонён", "err", err)
	writeJSONStatus(w, http.StatusNotFound, errorBody{Code: "not_found", Message: "не найдено", Error: "не найдено"})
}

// joinLookup - машина живого токена из пути; на ошибку пишет ответ и возвращает ok=false.
func (s *Server) joinLookup(w http.ResponseWriter, r *http.Request) (string, store.JoinInfo, bool) {
	token := r.PathValue("token")
	if !agentjoin.ValidToken(token) {
		s.joinNotFound(w, errors.New("токен не по формату"))
		return "", store.JoinInfo{}, false
	}
	info, err := s.deps.Joins.JoinTarget(r.Context(), token)
	switch {
	case errors.Is(err, store.ErrBadJoinToken), errors.Is(err, store.ErrRelayNoExits), errors.Is(err, store.ErrJoinNotSupported):
		s.joinNotFound(w, err)
		return "", info, false
	case err != nil:
		s.writeStoreErr(w, err)
		return "", info, false
	}
	return token, info, true
}

func (s *Server) routesJoin() {
	// Добавить машину: завести её и сразу выдать команду установки агента.
	s.mux.HandleFunc("POST /api/v1/targets", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		var body newTargetBody
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON машины (NewTarget): %w", err), s.deps.Log)
			return
		}
		if err := body.validate(); err != nil {
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		pins, err := s.joinConfig(body.Kind == "exit")
		if err != nil {
			writeErr(w, http.StatusConflict, err, s.deps.Log)
			return
		}
		t := store.Target{Code: body.Code, Kind: body.Kind, RelayCode: body.RelayCode, Resources: body.Resources, Enabled: true}
		if body.Kind == "exit" {
			t.Settings = map[string]string{store.SettingAddress: body.Address, store.SettingBridgeSocks: body.BridgeSocks}
		}
		if body.Kind == "relay" && body.Address != "" {
			t.Settings = map[string]string{store.SettingAddress: body.Address}
		}
		iss, err := s.deps.Joins.CreateTarget(r.Context(), t, agentjoin.TTL)
		switch {
		case errors.Is(err, store.ErrTargetExists):
			writeErr(w, http.StatusConflict, fmt.Errorf("машина %q уже есть; новая команда установки - POST /api/v1/targets/%s/join", body.Code, body.Code), s.deps.Log)
			return
		case errors.Is(err, store.ErrBadRelay):
			// Релея нет или машина не того вида - 400 unknown_machine (R5.2).
			writeErr(w, http.StatusBadRequest, codedErr("unknown_machine", err), s.deps.Log)
			return
		case err != nil:
			s.writeStoreErr(w, err)
			return
		}
		cmd, err := s.commandFor(iss, pins)
		if err != nil {
			// Машина уже заведена: команду можно получить через .../join.
			writeErr(w, http.StatusConflict, fmt.Errorf("машина %q заведена, но команду не собрать: %w", body.Code, err), s.deps.Log)
			return
		}
		cmd.Firewall = s.openFirewall(r.Context(), iss.Target)
		writeJSONStatus(w, http.StatusCreated, cmd)
	})

	// Новая одноразовая команда установки агента (прежняя сгорает).
	s.mux.HandleFunc("POST /api/v1/targets/{code}/join", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		code := r.PathValue("code")
		if !agentjoin.ValidCode(code) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		t, err := s.deps.Joins.TargetByCode(r.Context(), code)
		if errors.Is(err, store.ErrUnknownTarget) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if t.Kind != "exit" && t.Kind != "relay" {
			writeErr(w, http.StatusConflict, fmt.Errorf("машине %q (%s) агент не ставится", code, t.Kind), s.deps.Log)
			return
		}
		pins, err := s.joinConfig(t.Kind == "exit")
		if err != nil {
			writeErr(w, http.StatusConflict, err, s.deps.Log)
			return
		}
		iss, err := s.deps.Joins.IssueJoinToken(r.Context(), code, agentjoin.TTL)
		switch {
		case errors.Is(err, store.ErrUnknownTarget):
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		case errors.Is(err, store.ErrRelayNoExits):
			writeErr(w, http.StatusConflict, fmt.Errorf("релей %q ходит к мастеру через мост своего выхода, а %w; сначала добавьте выход", code, err), s.deps.Log)
			return
		case errors.Is(err, store.ErrJoinNotSupported):
			writeErr(w, http.StatusConflict, fmt.Errorf("машине %q агент не ставится", code), s.deps.Log)
			return
		case err != nil:
			s.writeStoreErr(w, err)
			return
		}
		cmd, err := s.commandFor(iss, pins)
		if errors.Is(err, store.ErrRelayNoExits) {
			writeErr(w, http.StatusConflict, fmt.Errorf("у релея %q нет выхода с корректными address и bridge_socks", code), s.deps.Log)
			return
		}
		if err != nil {
			writeErr(w, http.StatusConflict, err, s.deps.Log)
			return
		}
		writeJSON(w, cmd)
	})

	// Скрипт установки (выполняется на новой машине). Токен не тратится.
	s.mux.HandleFunc("GET /v1/join/{token}", func(w http.ResponseWriter, r *http.Request) {
		token, info, ok := s.joinLookup(w, r)
		if !ok {
			return
		}
		pins, err := s.joinConfig(info.Target.Kind == "exit")
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		route, err := s.joinRoute(info.Target, info.Exits, pins)
		if err != nil {
			s.joinNotFound(w, err)
			return
		}
		script, err := agentjoin.Script(route, token, info.Target.Code)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err, s.deps.Log)
			return
		}
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(script)
	})

	// Бинарь telemux для новой машины - бинарь самого мастера. Токен не тратится.
	s.mux.HandleFunc("GET /v1/join/{token}/telemux", func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := s.joinLookup(w, r); !ok {
			return
		}
		path, err := s.joinBinary()
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("бинарь мастера: %w", err), s.deps.Log)
			return
		}
		defer f.Close() //nolint:errcheck // только чтение
		st, err := f.Stat()
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("бинарь мастера: %w", err), s.deps.Log)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "telemux", st.ModTime(), f) // потоком, с Content-Length
	})

	// Обмен одноразового токена на токен агента. Только мастер, один раз.
	s.mux.HandleFunc("POST /v1/join/{token}/agent", func(w http.ResponseWriter, r *http.Request) {
		token, info, ok := s.joinLookup(w, r)
		if !ok {
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		// Проверка настроек до траты токена: неудача не сжигает команду.
		pins, err := s.joinConfig(info.Target.Kind == "exit")
		if err != nil {
			writeErr(w, http.StatusConflict, err, s.deps.Log)
			return
		}
		if _, err := s.agentEnv(info.Target, info.Exits, pins); err != nil {
			writeErr(w, http.StatusConflict, fmt.Errorf("agent.env машины %q не собрать: %w", info.Target.Code, err), s.deps.Log)
			return
		}
		ex, err := s.deps.Joins.ExchangeJoin(r.Context(), token)
		switch {
		case errors.Is(err, store.ErrBadJoinToken):
			s.joinNotFound(w, err)
			return
		case errors.Is(err, store.ErrRelayNoExits), errors.Is(err, store.ErrJoinNotSupported):
			writeErr(w, http.StatusConflict, err, s.deps.Log)
			return
		case err != nil:
			s.writeStoreErr(w, err)
			return
		}
		env, err := s.agentEnv(ex.Target, ex.Exits, pins)
		if err != nil {
			// Токен уже потрачен: нужна новая команда.
			writeErr(w, http.StatusConflict, fmt.Errorf("agent.env машины %q не собрать, выпустите новую команду: %w", ex.Target.Code, err), s.deps.Log)
			return
		}
		s.deps.Log.Info("join: агент машины получил токен", "target", ex.Target.Code)
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Query().Get("format") == "env" {
			// Для скрипта без jq: первая строка - токен агента, дальше agent.env.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = fmt.Fprintf(w, "%s\n%s", ex.AgentToken, env)
			return
		}
		writeJSON(w, map[string]string{"target": ex.Target.Code, "agent_token": ex.AgentToken, "agent_env": env})
	})
}
