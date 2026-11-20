package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
)

// fakeAgents - машины и их бандлы поверх fakeRevisions (ревизия общая).
type fakeAgents struct {
	mu       sync.Mutex
	rev      *fakeRevisions
	tokens   map[string]string // токен → машина
	disabled map[string]bool   // выключенные машины (отчитываются, ревизий не получают)
	observed []store.ObservedReport

	machines              []store.Machine
	pairs                 []store.Pair
	drift                 []store.DriftItem
	applyLog              map[string][]store.ApplyLogEntry
	fail                  error // ошибка базы для Pairs
	lastLimit, lastOffset int
	lastFilter            store.MachineFilter

	runtime    []store.RuntimeReport
	runtimeErr error
	snapshots  map[string]store.MachineRuntime
	conns      []store.ConnRow
	summary    []store.ConnSummary
	logErrors  []store.LogErrorSum
	lastConn   store.ConnFilter
	lastSince  time.Time
	lastMach   string

	totals     store.FleetTotals
	totalsErr  error
	lastFresh  time.Time
	lastSilent time.Time
	byUserKeys []string

	relayVia      []relayViaCall
	relayViaErr   error
	pruned        int
	prunedBefore  time.Time
	lastThreshold time.Duration
}

func (f *fakeAgents) AgentTargetByToken(_ context.Context, token string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code, ok := f.tokens[token]
	if !ok {
		return "", false, store.ErrBadAgentToken
	}
	return code, !f.disabled[code], nil
}

func (f *fakeAgents) MaxRevision(_ context.Context, _ string) (int64, error) {
	f.rev.mu.Lock()
	defer f.rev.mu.Unlock()
	return f.rev.rev, nil
}

func (f *fakeAgents) DesiredBundle(_ context.Context, target string) (store.Bundle, error) {
	f.rev.mu.Lock()
	defer f.rev.mu.Unlock()
	return store.Bundle{Target: target, Revision: f.rev.rev, Resources: []store.Desired{
		{Resource: store.ResourceTelemtUsers, Revision: f.rev.rev, SHA256: "abc", Content: []byte("[access.users]\n")},
	}}, nil
}

func (f *fakeAgents) RecordObserved(_ context.Context, rep store.ObservedReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = append(f.observed, rep)
	return nil
}

const pr2Token = "token-pr2"

func agentServer(inRecovery bool) (*Server, *fakeRevisions, *fakeAgents) {
	fu := newFakeUsers(0)
	fr := &fakeRevisions{users: fu, status: map[int64][]store.TargetStatus{}}
	fa := &fakeAgents{rev: fr, tokens: map[string]string{pr2Token: "pr2", "token-ps2": "ps2"}}
	s := New(Deps{
		Store: fakeStore{inRecovery: inRecovery}, Users: fu, Revisions: fr, Agents: fa, Node: &fakeNode{},
		Version: "v1", SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		BotToken: botToken, DefaultMaxConns: 50, DesiredPollInterval: 20 * time.Millisecond,
		AdminUser: "admin", AdminPassword: "pw",
	})
	return s, fr, fa
}

func TestAgentAPI_Auth(t *testing.T) {
	s, _, _ := agentServer(false)
	if rec := do(s, "GET", "/v1/desired?target=pr2&rev=0", "nope", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("чужой токен → ждали 401, получили %d", rec.Code)
	}
	if rec := do(s, "GET", "/v1/desired?target=pr2&rev=0", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена → ждали 401, получили %d", rec.Code)
	}
	// Токен pr2 не даёт бандл ps2: релей не видит чужих конфигов.
	if rec := do(s, "GET", "/v1/desired?target=ps2&rev=0", pr2Token, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("чужая машина → ждали 403, получили %d", rec.Code)
	}
	if rec := do(s, "GET", "/v1/desired?target=pr2&rev=x", pr2Token, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("мусор в rev → ждали 400, получили %d", rec.Code)
	}
}

// Есть ревизия новее известной агенту - бандл отдаётся сразу.
func TestAgentAPI_DesiredImmediateWhenNewer(t *testing.T) {
	s, fr, _ := agentServer(false)
	fr.rev = 3
	start := time.Now()
	rec := do(s, "GET", "/v1/desired?target=pr2&rev=1&wait=5s", pr2Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	if time.Since(start) > time.Second {
		t.Fatal("при наличии новой ревизии ответ должен быть сразу")
	}
	var b store.Bundle
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Revision != 3 || len(b.Resources) != 1 || string(b.Resources[0].Content) != "[access.users]\n" {
		t.Fatalf("не тот бандл: %+v", b)
	}
}

// Новее нет - ждём до таймаута и отвечаем 304.
func TestAgentAPI_DesiredNotModifiedAfterWait(t *testing.T) {
	s, fr, _ := agentServer(false)
	fr.rev = 3
	start := time.Now()
	rec := do(s, "GET", "/v1/desired?target=pr2&rev=3&wait=150ms", pr2Token, "")
	if rec.Code != http.StatusNotModified {
		t.Fatalf("ждали 304, получили %d", rec.Code)
	}
	if el := time.Since(start); el < 140*time.Millisecond || el > 2*time.Second {
		t.Fatalf("ожидание должно длиться около wait, длилось %v", el)
	}
}

// Ожидающий запрос просыпается сразу после публикации новой ревизии.
func TestAgentAPI_DesiredWakesOnPublish(t *testing.T) {
	s, fr, _ := agentServer(false)
	fr.rev = 3
	go func() {
		time.Sleep(100 * time.Millisecond)
		do(s, "PUT", "/api/v1/users", botToken, usersBody("alice_1"))
	}()
	start := time.Now()
	rec := do(s, "GET", "/v1/desired?target=pr2&rev=3&wait=10s", pr2Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200 после публикации, получили %d", rec.Code)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("ответ должен прийти вскоре после публикации, а не по таймауту")
	}
}

// Клиент ушёл - обработчик не висит до конца wait.
func TestAgentAPI_DesiredStopsWhenClientGone(t *testing.T) {
	s, fr, _ := agentServer(false)
	fr.rev = 3
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/desired?target=pr2&rev=3&wait=30s", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+pr2Token)
	done := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("обработчик не завершился после ухода клиента")
	}
}

func TestAgentAPI_Observed(t *testing.T) {
	s, _, fa := agentServer(false)
	body := `{"target":"pr2","agent_version":"v0.9","resources":[{"resource":"telemt.users","revision":3,"sha256":"abc","status":"compared"}]}`
	if rec := do(s, "POST", "/v1/observed", pr2Token, body); rec.Code != http.StatusNoContent {
		t.Fatalf("ждали 204, получили %d: %s", rec.Code, rec.Body.String())
	}
	if len(fa.observed) != 1 || fa.observed[0].Target != "pr2" {
		t.Fatalf("отчёт не сохранён: %+v", fa.observed)
	}
	for _, bad := range []string{
		`{"target":"pr2","resources":[{"resource":"telemt.users","revision":3,"status":"ok"}]}`,
		`{"target":"pr2","resources":[{"resource":"","revision":3,"status":"applied"}]}`,
		`{"target":"pr2","resources":`,
	} {
		if rec := do(s, "POST", "/v1/observed", pr2Token, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → ждали 400, получили %d", bad, rec.Code)
		}
	}
	other := strings.Replace(body, `"target":"pr2"`, `"target":"ps2"`, 1)
	if rec := do(s, "POST", "/v1/observed", pr2Token, other); rec.Code != http.StatusForbidden {
		t.Fatalf("отчёт за чужую машину → ждали 403, получили %d", rec.Code)
	}
}

// Отчёты пишет только мастер: на реплике база только для чтения.
func TestAgentAPI_ObservedOnReplica(t *testing.T) {
	s, _, _ := agentServer(true)
	body := `{"target":"pr2","resources":[{"resource":"telemt.users","revision":3,"status":"compared"}]}`
	rec := do(s, "POST", "/v1/observed", pr2Token, body)
	if rec.Code != http.StatusConflict || decodeErr(t, rec).Code != "not_master" {
		t.Fatalf("ждали 409 not_master, получили %d %s", rec.Code, rec.Body.String())
	}
}
