//go:build integration

// Сквозной сценарий фейловера на реальном PostgreSQL: монитор роли + PG-fence +
// соседи-telemux по HTTP (httptest) + фейковый Patroni. Проверяет, что изолированный
// primary перестаёт принимать запись приложений и возвращает её только после
// восстановления кворума и выдержки master_hold.
package e2e

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndreyOsipuk/telemux/internal/patroni"
	"github.com/AndreyOsipuk/telemux/internal/peers"
	"github.com/AndreyOsipuk/telemux/internal/role"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

const failoverApp = "telemux_e2e_app"

// fakePeer - соседний telemux; up=false имитирует сетевую изоляцию.
type fakePeer struct {
	code string
	up   atomic.Bool
	srv  *httptest.Server
}

func newFakePeer(code string) *fakePeer {
	p := &fakePeer{code: code}
	p.up.Store(true)
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.up.Load() {
			// Обрываем соединение без ответа - как фильтр, режущий трафик.
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		if r.URL.Path != peers.PingPath || r.Header.Get("Authorization") != "Bearer sek" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"code":"` + p.code + `","role":"replica"}`))
	}))
	return p
}

func appExec(t *testing.T, dsn, q string) error {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = failoverApp, failoverApp
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("подключение приложения: %v", err)
	}
	defer c.Close(context.Background())
	_, err = c.Exec(context.Background(), q)
	return err
}

func isReadOnlyErr(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "25006"
}

// eventually ждёт выполнения условия (reload конфигурации PG асинхронен).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestE2E_IsolatedPrimaryFencedAndRecovers(t *testing.T) {
	dsn := os.Getenv("TELEMUX_TEST_DSN")
	if dsn == "" {
		t.Skip("TELEMUX_TEST_DSN не задан")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, q := range []string{
		`DO $$ BEGIN
		   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'telemux_e2e_app') THEN
		     CREATE ROLE telemux_e2e_app LOGIN PASSWORD 'telemux_e2e_app';
		   END IF;
		 END $$`,
		`CREATE TABLE IF NOT EXISTS failover_payments (id text PRIMARY KEY)`,
		`TRUNCATE failover_payments`,
		`GRANT INSERT, SELECT ON failover_payments TO telemux_e2e_app`,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("подготовка: %v", err)
		}
	}
	fencer := store.Fencer{Store: st, Roles: []string{failoverApp}}
	t.Cleanup(func() { _ = fencer.SetWriteFence(context.Background(), false) })

	// Patroni отвечает «я лидер» - роль PG здесь не меняется, проверяем контур telemux.
	pat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"state":"running","role":"primary"}`))
	}))
	defer pat.Close()

	ps2, ps3 := newFakePeer("ps2"), newFakePeer("ps3")
	defer ps2.srv.Close()
	defer ps3.srv.Close()
	peerMap := map[string]string{"ps2": ps2.srv.URL, "ps3": ps3.srv.URL}

	cfg := role.Config{
		Self: "ps1", Peers: peers.Codes(peerMap), RequirePatroni: true,
		FenceAfter: 600 * time.Millisecond, MasterHold: 300 * time.Millisecond,
	}
	mon := role.NewMonitor(cfg, role.Probes{
		PG: st, Patroni: patroni.New(pat.URL, nil), Standbys: st, Fence: fencer,
		Peers: &peers.Prober{Peers: peerMap, Secret: "sek", HTTP: &http.Client{Timeout: 200 * time.Millisecond}},
	}, 100*time.Millisecond, nil)
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go mon.Run(runCtx)

	pay := func(id string) error {
		return appExec(t, dsn, `INSERT INTO failover_payments VALUES ('`+id+`')`)
	}

	// 1. Кворум есть - после master_hold мастер, запись приложения проходит.
	eventually(t, "мастер при полном кворуме", func() bool { return mon.Current().IsMaster() })
	eventually(t, "запись после получения мастера", func() bool { return pay("p1") == nil })

	// 2. Асимметрия: пропал один сосед - кворум 2 из 3, мастер остаётся.
	ps3.up.Store(false)
	time.Sleep(1200 * time.Millisecond) // дольше fence_after
	if !mon.Current().IsMaster() {
		t.Fatalf("видим одного соседа из двух - мастер должен остаться: %+v", mon.Current())
	}
	if err := pay("p2"); err != nil {
		t.Fatalf("при кворуме 2 из 3 запись проходит: %v", err)
	}

	// 3. Изоляция: пропали оба - через fence_after fenced, PG отвергает запись приложения.
	ps2.up.Store(false)
	eventually(t, "fenced при изоляции", func() bool { return mon.Current().Role == role.Fenced })
	eventually(t, "PG read-only для приложения", func() bool { return isReadOnlyErr(pay("p3")) })

	// 4. Сосед вернулся - запись возвращается, но не раньше master_hold.
	ps2.up.Store(true)
	eventually(t, "мастер после возврата кворума", func() bool { return mon.Current().IsMaster() })
	if since := mon.Current().Since; time.Since(since) > 5*time.Second {
		t.Fatalf("since должен обновиться при смене роли: %v", since)
	}
	eventually(t, "запись после снятия fence", func() bool { return pay("p4") == nil })

	var n int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM failover_payments").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("ждали 3 записи (p1, p2, p4; p3 отвергнута fence), получили %d", n)
	}
}
