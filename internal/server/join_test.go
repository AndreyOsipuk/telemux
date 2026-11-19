package server

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/agentjoin"
	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/tlspin"
)

// fakeJoins - машины и токены установки в памяти, с той же семантикой, что store.
type fakeJoins struct {
	mu      sync.Mutex
	targets map[string]store.Target
	tokens  map[string]*fakeJoinToken // токен → состояние
	agent   map[string]string         // машина → токен агента
	n       int
	calls   int // обращения к записи (CreateTarget, IssueJoinToken, ExchangeJoin)
}

type fakeJoinToken struct {
	target  string
	expires time.Time
	used    bool
}

func newFakeJoins(targets ...store.Target) *fakeJoins {
	f := &fakeJoins{targets: map[string]store.Target{}, tokens: map[string]*fakeJoinToken{}, agent: map[string]string{}}
	for _, t := range targets {
		f.targets[t.Code] = t
	}
	return f
}

func (f *fakeJoins) newToken() string {
	f.n++
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(f.n + i)
	}
	return hex.EncodeToString(b)
}

func (f *fakeJoins) exits(relay string) []store.Target {
	out := []store.Target{}
	for _, t := range f.targets {
		if t.Kind == "exit" && t.RelayCode == relay && t.Enabled &&
			t.Settings[store.SettingAddress] != "" && t.Settings[store.SettingBridgeSocks] != "" {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

func (f *fakeJoins) joinExits(t store.Target) ([]store.Target, error) {
	switch t.Kind {
	case "exit":
		return []store.Target{}, nil
	case "relay":
		ex := f.exits(t.Code)
		if len(ex) == 0 {
			return nil, store.ErrRelayNoExits
		}
		return ex, nil
	}
	return nil, store.ErrJoinNotSupported
}

func (f *fakeJoins) issue(code string, ttl time.Duration) *store.JoinToken {
	for tok, st := range f.tokens {
		if st.target == code && !st.used {
			delete(f.tokens, tok)
		}
	}
	tok := f.newToken()
	exp := time.Now().Add(ttl)
	f.tokens[tok] = &fakeJoinToken{target: code, expires: exp}
	return &store.JoinToken{Token: tok, ExpiresAt: exp}
}

func (f *fakeJoins) TargetByCode(_ context.Context, code string) (store.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.targets[code]
	if !ok {
		return t, store.ErrUnknownTarget
	}
	return t, nil
}

func (f *fakeJoins) CreateTarget(_ context.Context, t store.Target, ttl time.Duration) (store.JoinIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	res := store.JoinIssue{Exits: []store.Target{}}
	if t.Kind == "exit" {
		r, ok := f.targets[t.RelayCode]
		if !ok || r.Kind != "relay" {
			return res, store.ErrBadRelay
		}
	}
	if _, ok := f.targets[t.Code]; ok {
		return res, store.ErrTargetExists
	}
	f.targets[t.Code] = t
	res.Target = t
	if t.Kind == "relay" {
		res.Exits = f.exits(t.Code)
	}
	if t.Kind == "exit" || len(res.Exits) > 0 {
		res.Join = f.issue(t.Code, ttl)
	}
	return res, nil
}

func (f *fakeJoins) IssueJoinToken(_ context.Context, code string, ttl time.Duration) (store.JoinIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	t, ok := f.targets[code]
	if !ok {
		return store.JoinIssue{}, store.ErrUnknownTarget
	}
	exits, err := f.joinExits(t)
	if err != nil {
		return store.JoinIssue{}, err
	}
	return store.JoinIssue{Target: t, Exits: exits, Join: f.issue(code, ttl)}, nil
}

func (f *fakeJoins) live(token string) (*fakeJoinToken, bool) {
	st, ok := f.tokens[token]
	if !ok || st.used || time.Now().After(st.expires) {
		return nil, false
	}
	return st, true
}

func (f *fakeJoins) JoinTarget(_ context.Context, token string) (store.JoinInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.live(token)
	if !ok {
		return store.JoinInfo{}, store.ErrBadJoinToken
	}
	t := f.targets[st.target]
	exits, err := f.joinExits(t)
	if err != nil {
		return store.JoinInfo{}, err
	}
	return store.JoinInfo{Target: t, Exits: exits}, nil
}

func (f *fakeJoins) ExchangeJoin(_ context.Context, token string) (store.JoinExchange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	st, ok := f.live(token)
	if !ok {
		return store.JoinExchange{}, store.ErrBadJoinToken
	}
	t := f.targets[st.target]
	exits, err := f.joinExits(t)
	if err != nil {
		return store.JoinExchange{}, err
	}
	st.used = true
	agent := f.newToken()
	f.agent[t.Code] = agent
	return store.JoinExchange{Target: t, Exits: exits, AgentToken: agent}, nil
}

func (f *fakeJoins) usedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, st := range f.tokens {
		if st.used {
			n++
		}
	}
	return n
}

// fakeFirewall - вызовы Allow и заданная ошибка.
type fakeFirewall struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeFirewall) Allow(_ context.Context, chain string, ip netip.Addr, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, chain+" "+ip.String()+" "+strconv.Itoa(port))
	return f.err
}

func isLinuxAmd64() bool { return runtime.GOOS == "linux" && runtime.GOARCH == "amd64" }

const publicURL = "https://203.0.113.1:18443"

type joinOpts struct {
	publicURL  string
	chain      string
	noCert     bool
	inRecovery bool
}

func relayTarget(code string) store.Target {
	return store.Target{Code: code, Kind: "relay", Resources: []string{"telemt.users"}, Enabled: true}
}

func exitT(code, relay, addr, socks string) store.Target {
	return store.Target{Code: code, Kind: "exit", RelayCode: relay, Resources: []string{}, Enabled: true,
		Settings: map[string]string{store.SettingAddress: addr, store.SettingBridgeSocks: socks}}
}

// joinServer - мастер с сертификатом API агентов, флот: релей pr1 с выходами
// ps1, ps2, пустой релей pr2, машина panel pan1.
func joinServer(t *testing.T, o joinOpts) (*Server, *fakeJoins, *fakeFirewall, string) {
	t.Helper()
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	spki := ""
	if !o.noCert {
		if _, err := tlspin.Generate(cert, filepath.Join(dir, "key.pem"), "telemux-master"); err != nil {
			t.Fatal(err)
		}
		var err error
		if spki, err = tlspin.SPKIPinFile(cert); err != nil {
			t.Fatal(err)
		}
	}
	fj := newFakeJoins(relayTarget("pr1"), relayTarget("pr2"),
		exitT("ps2", "pr1", "203.0.113.12", "socks5://127.0.0.1:10872"),
		exitT("ps1", "pr1", "203.0.113.11", "socks5://127.0.0.1:10871"),
		store.Target{Code: "pan1", Kind: "panel", Resources: []string{}, Enabled: true},
	)
	fw := &fakeFirewall{}
	fu := newFakeUsers(0)
	fr := &fakeRevisions{users: fu, status: map[int64][]store.TargetStatus{}}
	s := New(Deps{
		Store: fakeStore{inRecovery: o.inRecovery}, Users: fu, Revisions: fr, Node: &fakeNode{},
		Agents:  &fakeAgents{rev: fr, tokens: map[string]string{}},
		Version: "v1", SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		BotToken: botToken, AdminUser: "admin", AdminPassword: "pw",
		Joins: fj, AgentPublicURL: o.publicURL, AgentFWChain: o.chain, Firewall: fw,
		TLSListen: "0.0.0.0:18443", TLSCertFile: cert,
		Certs: newFakeCerts(),
	})
	return s, fj, fw, spki
}

func defaultJoin(t *testing.T) (*Server, *fakeJoins, *fakeFirewall, string) {
	return joinServer(t, joinOpts{publicURL: publicURL, chain: "TELEMUX-AGENTS"})
}

func decodeCmd(t *testing.T, rec *httptest.ResponseRecorder) joinCommand {
	t.Helper()
	var c joinCommand
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("не JSON: %v: %s", err, rec.Body.String())
	}
	return c
}

// tokenOf - токен из команды установки.
func tokenOf(t *testing.T, cmd string) string {
	t.Helper()
	i := strings.Index(cmd, "/v1/join/")
	if i < 0 || len(cmd) < i+9+64 {
		t.Fatalf("в команде нет токена: %q", cmd)
	}
	return cmd[i+9 : i+9+64]
}

const exitBody = `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"203.0.113.13","bridge_socks":"socks5://127.0.0.1:10873","resources":["telemt.users"]}`

func TestJoin_CreateTargetValidation(t *testing.T) {
	s, fj, fw, _ := defaultJoin(t)
	cases := map[string]string{
		"не JSON":                `{`,
		"код пустой":             `{"code":"","kind":"relay","resources":[]}`,
		"код с заглавной":        `{"code":"Pr3","kind":"relay","resources":[]}`,
		"код из одной буквы":     `{"code":"p","kind":"relay","resources":[]}`,
		"код с подчёркиванием":   `{"code":"pr_3","kind":"relay","resources":[]}`,
		"код длиннее 32":         `{"code":"p` + strings.Repeat("a", 32) + `","kind":"relay","resources":[]}`,
		"kind panel":             `{"code":"pr3","kind":"panel","resources":[]}`,
		"kind пустой":            `{"code":"pr3","resources":[]}`,
		"релей с адресом":        `{"code":"pr3","kind":"relay","address":"203.0.113.9","resources":[]}`,
		"релей с relay_code":     `{"code":"pr3","kind":"relay","relay_code":"pr1","resources":[]}`,
		"без resources":          `{"code":"pr3","kind":"relay"}`,
		"чужой ресурс":           `{"code":"pr3","kind":"relay","resources":["xui.clients"]}`,
		"ресурс дважды":          `{"code":"pr3","kind":"relay","resources":["telemt.users","telemt.users"]}`,
		"выход без relay_code":   `{"code":"ps3","kind":"exit","address":"203.0.113.13","bridge_socks":"socks5://127.0.0.1:1","resources":[]}`,
		"релея нет":              `{"code":"ps3","kind":"exit","relay_code":"pr9","address":"203.0.113.13","bridge_socks":"socks5://127.0.0.1:1","resources":[]}`,
		"релей - это выход":      `{"code":"ps3","kind":"exit","relay_code":"ps1","address":"203.0.113.13","bridge_socks":"socks5://127.0.0.1:1","resources":[]}`,
		"адрес с мусором":        `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"1.2.3.4;rm","bridge_socks":"socks5://127.0.0.1:1","resources":[]}`,
		"адрес IPv6":             `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"2001:db8::1","bridge_socks":"socks5://127.0.0.1:1","resources":[]}`,
		"адрес пустой":           `{"code":"ps3","kind":"exit","relay_code":"pr1","bridge_socks":"socks5://127.0.0.1:1","resources":[]}`,
		"адрес loopback":         `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"127.0.0.1","bridge_socks":"socks5://127.0.0.1:1","resources":[]}`,
		"socks не loopback":      `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"203.0.113.13","bridge_socks":"socks5://10.0.0.1:1080","resources":[]}`,
		"socks порт 0":           `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"203.0.113.13","bridge_socks":"socks5://127.0.0.1:0","resources":[]}`,
		"socks порт 65536":       `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"203.0.113.13","bridge_socks":"socks5://127.0.0.1:65536","resources":[]}`,
		"socks пустой":           `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"203.0.113.13","resources":[]}`,
		"socks с мусором":        `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"203.0.113.13","bridge_socks":"socks5://127.0.0.1:1;id","resources":[]}`,
		"socks другая схема":     `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"203.0.113.13","bridge_socks":"http://127.0.0.1:1080","resources":[]}`,
		"resources не массив":    `{"code":"pr3","kind":"relay","resources":"telemt.users"}`,
		"релей с bridge_socks":   `{"code":"pr3","kind":"relay","bridge_socks":"socks5://127.0.0.1:1","resources":[]}`,
		"выход с чужим ресурсом": `{"code":"ps3","kind":"exit","relay_code":"pr1","address":"203.0.113.13","bridge_socks":"socks5://127.0.0.1:1","resources":["panel"]}`,
	}
	for name, body := range cases {
		rec := do(s, "POST", "/api/v1/targets", botToken, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: ждали 400, получили %d: %s", name, rec.Code, rec.Body.String())
			continue
		}
		var e errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != "bad_request" || e.Message == "" {
			t.Errorf("%s: ответ без понятного текста: %s", name, rec.Body.String())
		}
	}
	if len(fw.calls) != 0 {
		t.Fatalf("при ошибке ввода файрвол трогать нельзя: %v", fw.calls)
	}
	if _, ok := fj.targets["ps3"]; ok {
		t.Fatal("машина с ошибкой ввода заведена")
	}
}

// Мусор в адресе до iptables не доходит - ни через ручку, ни через openFirewall.
func TestJoin_GarbageAddressNeverReachesFirewall(t *testing.T) {
	s, _, fw, _ := defaultJoin(t)
	for _, addr := range []string{"1.2.3.4;rm", "1.2.3.4 -j DROP", "$(id)", "1.2.3.4/0", "0.0.0.0"} {
		body := `{"code":"ps3","kind":"exit","relay_code":"pr1","address":` + jsonStr(addr) + `,"bridge_socks":"socks5://127.0.0.1:1","resources":[]}`
		if rec := do(s, "POST", "/api/v1/targets", botToken, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: ждали 400, получили %d", addr, rec.Code)
		}
	}
	// Машина из CLI с мусором в settings: openFirewall отказывает до вызова.
	got := s.openFirewall(context.Background(), exitT("ps9", "pr1", "1.2.3.4;rm", "socks5://127.0.0.1:1"))
	if got != fwFailed || len(fw.calls) != 0 {
		t.Fatalf("ждали failed без вызова iptables, получили %s, вызовы %v", got, fw.calls)
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestJoin_CreateExit(t *testing.T) {
	s, fj, fw, spki := defaultJoin(t)
	rec := do(s, "POST", "/api/v1/targets", botToken, exitBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ждали 201, получили %d: %s", rec.Code, rec.Body.String())
	}
	c := decodeCmd(t, rec)
	tok := tokenOf(t, c.Command)
	want := "curl -fsSLk --pinnedpubkey 'sha256//" + spki + "' " + publicURL + "/v1/join/" + tok + " | sudo bash"
	if c.Target != "ps3" || c.Command != want {
		t.Fatalf("команда выхода:\n%s\nждали\n%s", c.Command, want)
	}
	if d := time.Until(c.ExpiresAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("срок команды не час: %v", c.ExpiresAt)
	}
	if c.Firewall != fwOpened || len(fw.calls) != 1 || fw.calls[0] != "TELEMUX-AGENTS 203.0.113.13 18443" {
		t.Fatalf("файрвол: %s, вызовы %v", c.Firewall, fw.calls)
	}
	got := fj.targets["ps3"]
	if got.Settings[store.SettingAddress] != "203.0.113.13" || got.Settings[store.SettingBridgeSocks] != "socks5://127.0.0.1:10873" || !got.Enabled {
		t.Fatalf("машина сохранена неверно: %+v", got)
	}

	// Занятый код - 409, файрвол больше не трогается.
	if rec := do(s, "POST", "/api/v1/targets", botToken, exitBody); rec.Code != http.StatusConflict {
		t.Fatalf("занятый код: ждали 409, получили %d", rec.Code)
	}
	if len(fw.calls) != 1 {
		t.Fatal("на занятый код файрвол трогать нельзя")
	}
}

func TestJoin_Firewall(t *testing.T) {
	// Цепочка не задана.
	s, _, fw, _ := joinServer(t, joinOpts{publicURL: publicURL})
	if c := decodeCmd(t, do(s, "POST", "/api/v1/targets", botToken, exitBody)); c.Firewall != fwNotConfigured || len(fw.calls) != 0 {
		t.Fatalf("без цепочки: %s, вызовы %v", c.Firewall, fw.calls)
	}
	// iptables упал: машина заведена, команда выдана, firewall=failed.
	s, _, fw, _ = defaultJoin(t)
	fw.err = errors.New("iptables: No chain/target/match by that name")
	rec := do(s, "POST", "/api/v1/targets", botToken, exitBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ждали 201, получили %d", rec.Code)
	}
	if c := decodeCmd(t, rec); c.Firewall != fwFailed || c.Command == "" {
		t.Fatalf("ждали failed и команду, получили %+v", c)
	}
	// Релею файрвол не нужен.
	rec = do(s, "POST", "/api/v1/targets", botToken, `{"code":"pr3","kind":"relay","resources":[]}`)
	if c := decodeCmd(t, rec); rec.Code != http.StatusCreated || c.Firewall != fwNotNeeded {
		t.Fatalf("релей: %d %+v", rec.Code, c)
	}
	// Порт не определить из TELEMUX_TLS_LISTEN.
	s.deps.TLSListen = ""
	fw.err = nil
	rec = do(s, "POST", "/api/v1/targets", botToken, strings.Replace(exitBody, "ps3", "ps4", 1))
	if c := decodeCmd(t, rec); c.Firewall != fwFailed {
		t.Fatalf("без порта: ждали failed, получили %+v", c)
	}
}

// Новый релей без выходов: машина заведена, команды пока нет.
func TestJoin_CreateRelayWithoutExits(t *testing.T) {
	s, fj, _, _ := defaultJoin(t)
	rec := do(s, "POST", "/api/v1/targets", botToken, `{"code":"pr3","kind":"relay","resources":["telemt.users","mtg.config"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ждали 201, получили %d: %s", rec.Code, rec.Body.String())
	}
	if c := decodeCmd(t, rec); c.Command != "" || c.Target != "pr3" {
		t.Fatalf("у релея без выходов команды быть не должно: %+v", c)
	}
	if len(fj.tokens) != 0 {
		t.Fatal("токен релею без выходов не выпускается")
	}
	if rec := do(s, "POST", "/api/v1/targets/pr3/join", botToken, ""); rec.Code != http.StatusConflict {
		t.Fatalf("join релея без выходов: ждали 409, получили %d", rec.Code)
	}
}

func TestJoin_NotConfigured(t *testing.T) {
	s, fj, _, _ := joinServer(t, joinOpts{})
	for _, req := range [][2]string{{"/api/v1/targets", exitBody}, {"/api/v1/targets", `{"code":"pr3","kind":"relay","resources":[]}`}, {"/api/v1/targets/ps1/join", ""}, {"/api/v1/targets/pr1/join", ""}} {
		rec := do(s, "POST", req[0], botToken, req[1])
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "TELEMUX_AGENT_PUBLIC_URL") {
			t.Fatalf("%s: ждали 409 с подсказкой, получили %d: %s", req[0], rec.Code, rec.Body.String())
		}
	}
	if fj.calls != 0 {
		t.Fatal("без настройки мастер ничего не должен заводить")
	}
	// Нет сертификата - выход не заводится, релею сертификат не нужен.
	s, fj, _, _ = joinServer(t, joinOpts{publicURL: publicURL, noCert: true})
	if rec := do(s, "POST", "/api/v1/targets", botToken, exitBody); rec.Code != http.StatusConflict || fj.calls != 0 {
		t.Fatalf("без сертификата: ждали 409 без записи, получили %d", rec.Code)
	}
	if rec := do(s, "POST", "/api/v1/targets/pr1/join", botToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("релей без сертификата: ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	// Мусорный адрес мастера.
	s, _, _, _ = joinServer(t, joinOpts{publicURL: "http://203.0.113.1:18443"})
	if rec := do(s, "POST", "/api/v1/targets/ps1/join", botToken, ""); rec.Code != http.StatusConflict {
		t.Fatalf("http вместо https: ждали 409, получили %d", rec.Code)
	}
}

func TestJoin_Auth(t *testing.T) {
	s, fj, _, _ := defaultJoin(t)
	for _, req := range [][3]string{
		{"POST", "/api/v1/targets", exitBody},
		{"POST", "/api/v1/targets/ps1/join", ""},
	} {
		for _, auth := range []string{"", "nope"} {
			if rec := do(s, req[0], req[1], auth, req[2]); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s без токена (%q): ждали 401, получили %d", req[0], req[1], auth, rec.Code)
			}
		}
	}
	if fj.calls != 0 {
		t.Fatal("без авторизации ничего не пишется")
	}
	// Сессия панели пускает.
	login := do(s, "POST", "/api/login", "", `{"username":"admin","password":"pw"}`)
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("нет сессии")
	}
	req := httptest.NewRequest("POST", "/api/v1/targets/ps1/join", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("по сессии: ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
}

func TestJoin_Replica(t *testing.T) {
	s, fj, _, _ := joinServer(t, joinOpts{publicURL: publicURL, inRecovery: true})
	if rec := do(s, "POST", "/api/v1/targets", botToken, exitBody); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_master") {
		t.Fatalf("реплика: ждали 409 not_master, получили %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(s, "POST", "/api/v1/targets/ps1/join", botToken, ""); rec.Code != http.StatusConflict {
		t.Fatalf("реплика join: ждали 409, получили %d", rec.Code)
	}
	if fj.calls != 0 {
		t.Fatal("реплика не пишет")
	}
}

func TestJoin_CreateJoin(t *testing.T) {
	s, _, _, _ := defaultJoin(t)
	// Релей - через мост первого выхода с адресом (ps1 по коду).
	rec := do(s, "POST", "/api/v1/targets/pr1/join", botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	c := decodeCmd(t, rec)
	tok := tokenOf(t, c.Command)
	want := "curl -fsSL -x socks5://127.0.0.1:10871 http://203.0.113.11:18480/v1/join/" + tok + " | sudo bash"
	if c.Command != want {
		t.Fatalf("команда релея:\n%s\nждали\n%s", c.Command, want)
	}
	if c.Firewall != "" {
		t.Fatalf("join не трогает файрвол, а firewall=%q", c.Firewall)
	}
	// Новый выпуск гасит старый токен.
	c2 := decodeCmd(t, do(s, "POST", "/api/v1/targets/pr1/join", botToken, ""))
	if tok2 := tokenOf(t, c2.Command); tok2 == tok {
		t.Fatal("новый токен совпал со старым")
	}
	if rec := do(s, "GET", "/v1/join/"+tok, "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("старый токен после нового выпуска: ждали 404, получили %d", rec.Code)
	}
	for path, want := range map[string]int{
		"/api/v1/targets/nobody/join": http.StatusNotFound,
		"/api/v1/targets/BAD/join":    http.StatusNotFound,
		"/api/v1/targets/pr2/join":    http.StatusConflict, // релей без выходов
		"/api/v1/targets/pan1/join":   http.StatusConflict, // panel
	} {
		if rec := do(s, "POST", path, botToken, ""); rec.Code != want {
			t.Fatalf("%s: ждали %d, получили %d: %s", path, want, rec.Code, rec.Body.String())
		}
	}
}

func TestJoin_Script(t *testing.T) {
	s, _, _, spki := defaultJoin(t)
	exitCmd := decodeCmd(t, do(s, "POST", "/api/v1/targets/ps1/join", botToken, ""))
	relayCmd := decodeCmd(t, do(s, "POST", "/api/v1/targets/pr1/join", botToken, ""))
	for name, c := range map[string]struct {
		cmd     string
		want    []string
		notWant []string
	}{
		"выход": {exitCmd.Command, []string{"BASE='" + publicURL + "'", "'--pinnedpubkey' 'sha256//" + spki + "'", "TARGET='ps1'"}, []string{"socks5"}},
		"релей": {relayCmd.Command, []string{"BASE='http://203.0.113.11:18480'", "'-x' 'socks5://127.0.0.1:10871'", "TARGET='pr1'"}, []string{"pinnedpubkey", publicURL}},
	} {
		tok := tokenOf(t, c.cmd)
		rec := do(s, "GET", "/v1/join/"+tok, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: ждали 200, получили %d: %s", name, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
			t.Fatalf("%s: Content-Type %q", name, ct)
		}
		body := rec.Body.String()
		for _, w := range append(c.want, "TOKEN='"+tok+"'", "set -euo pipefail") {
			if !strings.Contains(body, w) {
				t.Errorf("%s: в скрипте нет %q", name, w)
			}
		}
		for _, nw := range c.notWant {
			if strings.Contains(body, nw) {
				t.Errorf("%s: в скрипте лишнее %q", name, nw)
			}
		}
		if bash, err := exec.LookPath("bash"); err == nil {
			f := filepath.Join(t.TempDir(), "join.sh")
			if err := os.WriteFile(f, rec.Body.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(bash, "-n", f).CombinedOutput(); err != nil {
				t.Fatalf("%s: bash -n: %v\n%s", name, err, out)
			}
		}
		// Скрипт токен не тратит.
		if rec := do(s, "GET", "/v1/join/"+tok, "", ""); rec.Code != http.StatusOK {
			t.Fatalf("%s: повторный скрипт: ждали 200, получили %d", name, rec.Code)
		}
	}
}

func TestJoin_BadTokens(t *testing.T) {
	s, fj, _, _ := defaultJoin(t)
	c := decodeCmd(t, do(s, "POST", "/api/v1/targets/ps1/join", botToken, ""))
	expired := tokenOf(t, c.Command)
	fj.tokens[expired].expires = time.Now().Add(-time.Second)
	c = decodeCmd(t, do(s, "POST", "/api/v1/targets/ps2/join", botToken, ""))
	used := tokenOf(t, c.Command)
	if rec := do(s, "POST", "/v1/join/"+used+"/agent", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("обмен: %d %s", rec.Code, rec.Body.String())
	}
	for name, tok := range map[string]string{
		"чужой":          strings.Repeat("ab", 32),
		"просроченный":   expired,
		"использованный": used,
		"не hex":         strings.Repeat("zz", 32),
		"короткий":       "abc",
	} {
		for _, req := range [][2]string{{"GET", "/v1/join/" + tok}, {"GET", "/v1/join/" + tok + "/telemux"}, {"POST", "/v1/join/" + tok + "/agent"}} {
			rec := do(s, req[0], req[1], "", "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s %s: ждали 404, получили %d", name, req[0], req[1], rec.Code)
			}
			if strings.Contains(rec.Body.String(), "просроч") || strings.Contains(rec.Body.String(), "использ") {
				t.Fatalf("%s: 404 без подробностей, а тело %s", name, rec.Body.String())
			}
		}
	}
}

func TestJoin_Binary(t *testing.T) {
	s, fj, _, _ := defaultJoin(t)
	tok := tokenOf(t, decodeCmd(t, do(s, "POST", "/api/v1/targets/ps1/join", botToken, "")).Command)
	bin := filepath.Join(t.TempDir(), "telemux")
	payload := strings.Repeat("ELF-telemux-", 100000)
	if err := os.WriteFile(bin, []byte(payload), 0o755); err != nil {
		t.Fatal(err)
	}
	s.joinBinary = func() (string, error) { return bin, nil }
	rec := do(s, "GET", "/v1/join/"+tok+"/telemux", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != payload {
		t.Fatalf("бинарь: %d, %d байт", rec.Code, rec.Body.Len())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type %q", ct)
	}
	if fj.usedCount() != 0 {
		t.Fatal("скачивание бинаря не тратит токен")
	}
	s.joinBinary = func() (string, error) { return "", errors.New("мастер собран под darwin/arm64") }
	rec = do(s, "GET", "/v1/join/"+tok+"/telemux", "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("не linux/amd64: ждали 503, получили %d", rec.Code)
	}
	// Настоящая проверка платформы.
	if _, err := selfBinary(); (err == nil) != (isLinuxAmd64()) {
		t.Fatalf("selfBinary на этой платформе: %v", err)
	}
}

func TestJoin_Exchange(t *testing.T) {
	s, fj, _, _ := defaultJoin(t)
	tok := tokenOf(t, decodeCmd(t, do(s, "POST", "/api/v1/targets/ps1/join", botToken, "")).Command)
	rec := do(s, "POST", "/v1/join/"+tok+"/agent", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	var ex struct {
		Target     string `json:"target"`
		AgentToken string `json:"agent_token"`
		AgentEnv   string `json:"agent_env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ex); err != nil {
		t.Fatal(err)
	}
	pin, _ := tlspin.FingerprintFile(s.deps.TLSCertFile)
	if ex.Target != "ps1" || ex.AgentToken != fj.agent["ps1"] {
		t.Fatalf("обмен: %+v", ex)
	}
	for _, line := range []string{"TELEMUX_AGENT_TARGET=ps1\n", "TELEMUX_AGENT_MASTERS=" + publicURL + "\n",
		"TELEMUX_AGENT_MASTER_PIN=" + pin + "\n", "TELEMUX_AGENT_USERS_FILE=none\n", "TELEMUX_AGENT_MTG_FILE=none\n", "TELEMUX_AGENT_MODE=compare\n"} {
		if !strings.Contains(ex.AgentEnv, line) {
			t.Errorf("в agent.env нет %q:\n%s", line, ex.AgentEnv)
		}
	}
	// Второй обмен - 404.
	if rec := do(s, "POST", "/v1/join/"+tok+"/agent", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("второй обмен: ждали 404, получили %d", rec.Code)
	}

	// Релей, format=env: первая строка - токен, дальше agent.env со всеми мостами.
	tok = tokenOf(t, decodeCmd(t, do(s, "POST", "/api/v1/targets/pr1/join", botToken, "")).Command)
	rec = do(s, "POST", "/v1/join/"+tok+"/agent?format=env", "", "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("format=env: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	first, env, _ := strings.Cut(rec.Body.String(), "\n")
	if first != fj.agent["pr1"] || !agentjoin.ValidToken(first) {
		t.Fatalf("первая строка - не токен агента: %q", first)
	}
	if !strings.Contains(env, "TELEMUX_AGENT_MASTERS=http://203.0.113.11:18480|socks5://127.0.0.1:10871,http://203.0.113.12:18480|socks5://127.0.0.1:10872\n") ||
		!strings.Contains(env, "TELEMUX_AGENT_USERS_FILE=/etc/telemt/users.toml\n") || strings.Contains(env, "MASTER_PIN") {
		t.Fatalf("agent.env релея:\n%s", env)
	}
}

// На реплике обмен - 409 not_master, токен не тратится.
func TestJoin_ExchangeOnReplica(t *testing.T) {
	s, fj, _, _ := defaultJoin(t)
	tok := tokenOf(t, decodeCmd(t, do(s, "POST", "/api/v1/targets/ps1/join", botToken, "")).Command)
	replica, _, _, _ := joinServer(t, joinOpts{publicURL: publicURL, inRecovery: true})
	replica.deps.Joins = fj
	replica.mux = http.NewServeMux()
	replica.routes()
	if rec := do(replica, "POST", "/v1/join/"+tok+"/agent", "", ""); rec.Code != http.StatusConflict {
		t.Fatalf("реплика: ждали 409, получили %d", rec.Code)
	}
	if fj.usedCount() != 0 {
		t.Fatal("реплика потратила токен")
	}
	// Скрипт отдаёт и реплика (только чтение).
	if rec := do(replica, "GET", "/v1/join/"+tok, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("скрипт на реплике: %d", rec.Code)
	}
}

// Внешний TLS-слушатель отдаёт join, но не API панели.
func TestJoin_AgentHandler(t *testing.T) {
	s, _, _, _ := defaultJoin(t)
	tok := tokenOf(t, decodeCmd(t, do(s, "POST", "/api/v1/targets/ps1/join", botToken, "")).Command)
	h := s.AgentHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/join/"+tok, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("скрипт через TLS-слушатель: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/targets/ps1/join", nil)
	req.Header.Set("Authorization", "Bearer "+botToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("API панели наружу не отдаётся: %d", rec.Code)
	}
}

// Команда выхода работает с настоящим curl: --pinnedpubkey принимает ключ
// сертификата мастера и отвергает чужой.
func TestJoin_CurlPinnedPubkey(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl не найден")
	}
	s, _, _, spki := defaultJoin(t)
	cert, err := tls.LoadX509KeyPair(s.deps.TLSCertFile, filepath.Join(filepath.Dir(s.deps.TLSCertFile), "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(s.AgentHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	s.deps.AgentPublicURL = srv.URL
	cmd := decodeCmd(t, do(s, "POST", "/api/v1/targets/ps1/join", botToken, "")).Command
	tok := tokenOf(t, cmd)
	// Команда без "| sudo bash": только скачивание скрипта.
	args := []string{"-fsSLk", "--pinnedpubkey", "sha256//" + spki, srv.URL + "/v1/join/" + tok}
	out, err := exec.Command(curl, args...).CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "#!/usr/bin/env bash") {
		t.Fatalf("curl с верным ключом: %v\n%s", err, out)
	}
	args[2] = "sha256//" + strings.Repeat("A", 43) + "="
	if out, err := exec.Command(curl, args...).CombinedOutput(); err == nil {
		t.Fatalf("curl с чужим ключом должен отказать, а скачал:\n%.80s", out)
	}
}
