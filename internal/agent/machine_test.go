package agent

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Машина без ресурсов (выход без telemt и mtg): агента можно запустить без
// файлов, он отчитывается мастеру о себе и о локальных проверках.
func newMachineAgent(t *testing.T, master string, checks []Check, recheck time.Duration) *Agent {
	t.Helper()
	a, err := New(Config{
		MasterURLs: []string{master}, Token: "tok", Target: "vega", Version: "test",
		Mode: ModeApply, Checks: checks, Wait: 50 * time.Millisecond, RecheckEvery: recheck,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("агент без ресурсов не создан: %v", err)
	}
	return a
}

func TestAgent_MachineWithoutResourcesReportsItself(t *testing.T) {
	m := &fakeMaster{}
	srv := httptest.NewServer(m.handler(t, "tok", "vega"))
	defer srv.Close()
	bridgeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer bridgeUp.Close()

	a := newMachineAgent(t, srv.URL, []Check{{Name: "bridge", URL: bridgeUp.URL + "/healthz"}}, time.Hour)
	if _, err := a.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	reports := append(m.reports[:0:0], m.reports...)
	m.mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("ждали один отчёт о машине, получили %d", len(reports))
	}
	rep := reports[0]
	if rep.Target != "vega" || len(rep.Resources) != 0 {
		t.Fatalf("отчёт не о машине без ресурсов: %+v", rep)
	}
	if len(rep.Checks) != 1 || rep.Checks[0].Name != "bridge" || !rep.Checks[0].OK || rep.Checks[0].Error != "" {
		t.Fatalf("проверка моста не в отчёте: %+v", rep.Checks)
	}

	// Без новой ревизии и до срока перепроверки отчёт не повторяется.
	if _, err := a.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	n := len(m.reports)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("отчёт о машине ушёл раньше срока: %d", n)
	}
}

// Срок перепроверки прошёл - агент снова отчитывается (мастер видит, что машина жива).
func TestAgent_MachineHeartbeatEveryRecheck(t *testing.T) {
	m := &fakeMaster{}
	srv := httptest.NewServer(m.handler(t, "tok", "vega"))
	defer srv.Close()
	a := newMachineAgent(t, srv.URL, nil, time.Millisecond)
	for i := 0; i < 3; i++ {
		if _, err := a.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.reports) != 3 {
		t.Fatalf("ждали 3 отчёта о машине, получили %d", len(m.reports))
	}
}

// Мост лежит или отвечает ошибкой - проверка в отчёте не пройдена, с причиной.
func TestAgent_FailedCheckReported(t *testing.T) {
	m := &fakeMaster{}
	srv := httptest.NewServer(m.handler(t, "tok", "vega"))
	defer srv.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer bad.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	a := newMachineAgent(t, srv.URL, []Check{{Name: "bridge", URL: bad.URL}, {Name: "other", URL: deadURL}}, time.Hour)
	if _, err := a.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	checks := m.reports[0].Checks
	if len(checks) != 2 {
		t.Fatalf("ждали 2 проверки, получили %+v", checks)
	}
	for _, c := range checks {
		if c.OK || c.Error == "" {
			t.Fatalf("неудачная проверка отмечена как пройденная: %+v", c)
		}
	}
	if !strings.Contains(checks[0].Error, "502") {
		t.Fatalf("в причине нет кода ответа: %q", checks[0].Error)
	}
}

func TestParseChecks(t *testing.T) {
	got, err := ParseChecks("bridge=http://127.0.0.1:18480/healthz, x=http://h/y")
	if err != nil || len(got) != 2 || got[0].Name != "bridge" || got[1].URL != "http://h/y" {
		t.Fatalf("разбор проверок: %+v, %v", got, err)
	}
	if got, err := ParseChecks(""); err != nil || len(got) != 0 {
		t.Fatalf("пустая строка: %+v, %v", got, err)
	}
	for _, bad := range []string{"bridge", "=http://h", "b=ftp://h", "b=http://"} {
		if _, err := ParseChecks(bad); err == nil {
			t.Fatalf("принята битая проверка %q", bad)
		}
	}
}
