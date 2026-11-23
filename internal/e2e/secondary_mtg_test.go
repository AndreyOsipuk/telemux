//go:build integration

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/agent"
	"github.com/AndreyOsipuk/telemux/internal/server"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// Только systemd и /stats заменены локальной моделью процесса, читающего файл при перезагрузке.
type secondaryMtgService struct {
	file   string
	digest atomic.Value
}

func (s *secondaryMtgService) HUP(ctx context.Context) error { return s.Restart(ctx) }
func (s *secondaryMtgService) Active(context.Context) bool   { return s.digest.Load() != nil }
func (s *secondaryMtgService) Restart(context.Context) error {
	b, err := os.ReadFile(s.file)
	if err == nil {
		s.digest.Store(agent.MtgSecretsDigest(b))
	}
	return err
}

func TestE2E_SecondaryMtgAutosync(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool := renderDB(t)
	defer pool.Close()
	st, err := store.Open(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const primary = store.ResourceMtgConfig
	const secondary = store.ResourceMtgSecondaryConfig
	relay := "pr1"
	if err := st.UpsertTarget(ctx, store.Target{Code: relay, Kind: "relay", Enabled: true, Resources: []string{primary, secondary}}); err != nil {
		t.Fatal(err)
	}
	_, err = st.CreateServer(ctx, store.ServerWrite{No: 6, Name: "Тест", Engine: "mtg", RelayCode: &relay, NodeRetired: true, TLSDomain: "example.org",
		Settings: map[string]string{"RELAY_MODE": "local-mtg", "TELEMUX_RELAY_RESOURCE": secondary, "RELAY_API_BIND": "127.0.0.1:8083", "RELAY_PROMETHEUS_BIND": "127.0.0.1:3130"}})
	if err != nil {
		t.Fatal(err)
	}

	master := server.New(server.Deps{Store: st, Users: st, Servers: st, Render: st, Revisions: st, Agents: st, BotToken: renderBotToken})
	e := renderEnv{t: t, st: st, pool: pool, srv: master}
	put := func(path, body string) {
		t.Helper()
		r := e.do("PUT", path, body)
		if r.Code != http.StatusOK && r.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
		}
	}
	const firstConfig = "[secrets]\n\"first\" = \"aaaa\"\n"
	put("/api/v1/targets/pr1/resources/"+primary, firstConfig)
	// Начальное назначение владельца только для изолированной тестовой карточки.
	put("/api/v1/servers/6/config-source", `{"source":"telemux","force":true}`)
	token, err := st.IssueAgentToken(ctx, relay)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(master.AgentHandler())
	defer endpoint.Close()
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.toml"), filepath.Join(dir, "second.toml")
	if err := os.WriteFile(first, []byte(firstConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, nil, 0600); err != nil {
		t.Fatal(err)
	}
	process := &secondaryMtgService{file: second}
	stats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"secrets_sha256": process.digest.Load()})
	}))
	defer stats.Close()
	a, err := agent.New(agent.Config{MasterURLs: []string{endpoint.URL}, Token: token, Target: relay, Mode: agent.ModeApply,
		MtgFile: first, MtgApplier: &agent.MtgApplier{}, MtgSecondaryFile: second, MtgSecondaryApplier: &agent.MtgApplier{Service: process, StatsURL: stats.URL, HotReload: true}, Wait: time.Millisecond * 100})
	if err != nil {
		t.Fatal(err)
	}
	var previous int64
	for _, key := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		put("/api/v1/users/sub_1", `{"secret":"`+key+`"}`)
		master.RenderNow(ctx)
		if _, err := a.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		d, err := st.LatestDesired(ctx, relay, secondary)
		if err != nil {
			t.Fatal(err)
		}
		var matches bool
		err = pool.QueryRow(ctx, "SELECT revision=$3 AND sha256=$4 AND status='applied' FROM observed_state WHERE target=$1 AND resource=$2", relay, secondary, d.Revision, d.SHA256).Scan(&matches)
		if err != nil || !matches || d.Revision <= previous || process.digest.Load() != agent.MtgSecretsDigest(d.Content) {
			t.Fatalf("применение второго mtg на ревизии %d: %v", d.Revision, err)
		}
		previous = d.Revision
	}
	b, err := os.ReadFile(first)
	if err != nil || string(b) != firstConfig || e.revisions(relay, primary) != 1 {
		t.Fatal("изменился основной процесс релея")
	}
}
