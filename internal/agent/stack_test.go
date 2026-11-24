package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/stack"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

const (
	shaTelemt = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaMtg    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// versionRunner - `<bin> --version` по пути из таблицы.
type versionRunner struct {
	mu  sync.Mutex
	out map[string]string
	n   int
}

func (r *versionRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	if len(args) != 1 || args[0] != "--version" {
		return nil, errors.New("неожиданная команда")
	}
	out, ok := r.out[name]
	if !ok {
		return nil, errors.New("нет бинаря")
	}
	return []byte(out), nil
}

func (r *versionRunner) set(path, out string) {
	r.mu.Lock()
	r.out[path] = out
	r.mu.Unlock()
}

// systemdRunStub - запись вызовов systemd-run; err - ответ.
type systemdRunStub struct {
	mu    sync.Mutex
	calls [][]string
	out   string
	err   error
	after func() // что «сделал» stack-apply (например, заменил версию)
}

func (s *systemdRunStub) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string{name}, args...))
	s.mu.Unlock()
	if s.err == nil && s.after != nil {
		s.after()
	}
	return []byte(s.out), s.err
}

func (s *systemdRunStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type stackEnv struct {
	a         *Agent
	versions  *versionRunner
	run       *systemdRunStub
	telemtBin string
	mtgBin    string
}

// newStackAgent - агент с telemt (и mtg, если withMtg) на «машине»; бинари - временные файлы.
func newStackAgent(t *testing.T, mode string, withMtg bool) *stackEnv {
	t.Helper()
	dir := t.TempDir()
	e := &stackEnv{
		versions:  &versionRunner{out: map[string]string{}},
		run:       &systemdRunStub{},
		telemtBin: filepath.Join(dir, "telemt"),
		mtgBin:    filepath.Join(dir, "mtg-multi"),
	}
	for _, p := range []string{e.telemtBin, e.mtgBin} {
		if err := os.WriteFile(p, []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.versions.set(e.telemtBin, "telemt 3.5.16\n")
	e.versions.set(e.mtgBin, "1.15.0 (go1.26.5: linux/amd64)\n")
	cfg := Config{
		MasterURLs: []string{"http://127.0.0.1:1"}, Token: "tok", Target: "pr2", Mode: mode,
		UsersFile: filepath.Join(dir, "users.toml"),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Runtime: &RuntimeCollector{
			TelemtBin: e.telemtBin, MtgBin: e.mtgBin, Runner: e.versions,
			LookPath: func(f string) (string, error) { return f, nil },
		},
		StackApplier: &StackApplier{
			Runner:     e.run,
			Executable: func() (string, error) { return "/usr/local/bin/telemux", nil },
		},
	}
	if withMtg {
		cfg.MtgFile = filepath.Join(dir, "config.toml")
	}
	if mode == ModeApply {
		cfg.Applier = &Applier{}
		if withMtg {
			cfg.MtgApplier = &MtgApplier{}
		}
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.a = a
	return e
}

func stackDesired(t *testing.T, rev int64, c stack.Content) *store.Desired {
	t.Helper()
	b, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return &store.Desired{Resource: stack.Resource, Revision: rev, SHA256: "x", Content: b}
}

// Ресурс stack агент ведёт всегда - мастер должен видеть ответ и там, где продукта нет.
func TestAgent_HandlesStack(t *testing.T) {
	e := newStackAgent(t, ModeCompare, false)
	if !e.a.handles(stack.Resource) {
		t.Fatal("агент не ведёт ресурс stack")
	}
}

// compare: только разница версий, systemd-run не вызывается; нет продукта - failed.
func TestAgent_StackCompareReportsDiff(t *testing.T) {
	e := newStackAgent(t, ModeCompare, true)
	d := stackDesired(t, 5, stack.Content{
		stack.ProductTelemt: {Version: "3.5.17", SHA256: shaTelemt, Size: 10},
		stack.ProductMtg:    {Version: "1.15.0", SHA256: shaMtg, Size: 10},
	})
	res := e.a.compare(context.Background(), d)
	if res.Status != "compared" || res.Error != "telemt: на машине 3.5.16, нужно 3.5.17" {
		t.Fatalf("ждали разницу только по telemt, получили %+v", res)
	}
	if e.run.count() != 0 {
		t.Fatal("в режиме compare запущен systemd-run")
	}

	noMtg := newStackAgent(t, ModeCompare, false)
	res = noMtg.a.compare(context.Background(), stackDesired(t, 5, stack.Content{
		stack.ProductMtg: {Version: "1.16.0", SHA256: shaMtg, Size: 10},
	}))
	if res.Status != "failed" || !strings.Contains(res.Error, "mtg: продукта на машине нет") {
		t.Fatalf("нет mtg на машине: ждали failed, получили %+v", res)
	}
}

// apply: systemd-run с точными аргументами; после обновления версия читается заново.
func TestAgent_StackApplyRunsSystemdRun(t *testing.T) {
	e := newStackAgent(t, ModeApply, true)
	e.run.after = func() { e.versions.set(e.telemtBin, "telemt 3.5.17\n") }
	d := stackDesired(t, 6, stack.Content{
		stack.ProductTelemt: {Version: "3.5.17", SHA256: shaTelemt, Size: 4242},
		stack.ProductMtg:    {Version: "v1.15.0", SHA256: shaMtg, Size: 10}, // уже стоит
	})
	res := e.a.apply(context.Background(), d)
	if res.Status != "applied" || res.Error != "" {
		t.Fatalf("ждали applied, получили %+v", res)
	}
	want := []string{"systemd-run", "--wait", "--pipe", "--collect", "--quiet",
		"--unit=telemux-stack-telemt-6", "-p", "EnvironmentFile=/etc/telemux/agent.env",
		"-p", "RuntimeMaxSec=540",
		"/usr/local/bin/telemux", "stack-apply",
		"--product", "telemt", "--sha256", shaTelemt, "--version", "3.5.17", "--size", "4242",
		"--bin", e.telemtBin}
	if len(e.run.calls) != 1 || strings.Join(e.run.calls[0], " ") != strings.Join(want, " ") {
		t.Fatalf("неверный вызов systemd-run:\n%v\nждали\n%v", e.run.calls, want)
	}

	// Следующая сверка той же ревизии: версия новая (кэш сброшен) - повторного вызова нет.
	if res := e.a.apply(context.Background(), d); res.Status != "applied" {
		t.Fatalf("повторная сверка: %+v", res)
	}
	if e.run.count() != 1 {
		t.Fatalf("после обновления systemd-run вызван снова: %d", e.run.count())
	}
}

// Ревизия stack - разовая команда: после applied человек поставил другую версию вручную,
// перепроверка той же ревизии её не откатывает (stack-apply не вызывается), итог прежний.
func TestAgent_StackRecheckDoesNotReapply(t *testing.T) {
	e := newStackAgent(t, ModeApply, true)
	e.run.after = func() { e.versions.set(e.mtgBin, "1.16.0 (go1.26.5)\n") }
	d := stackDesired(t, 20, stack.Content{stack.ProductMtg: {Version: "1.16.0", SHA256: shaMtg, Size: 10}})
	if res := e.a.apply(context.Background(), d); res.Status != "applied" || e.run.count() != 1 {
		t.Fatalf("первое применение: %+v, вызовов %d", res, e.run.count())
	}
	// Вручную - своя сборка; файл другой, кэш версии видит замену.
	if err := os.WriteFile(e.mtgBin, []byte("custom-build"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.versions.set(e.mtgBin, "1.16.0-webalias (go1.26.5)\n")
	for i := 0; i < 3; i++ {
		e.a.ForceRecheck()
		if res := e.a.apply(context.Background(), d); res.Status != "applied" || res.Error != "" {
			t.Fatalf("перепроверка: ждали прежний итог applied, получили %+v", res)
		}
	}
	if e.run.count() != 1 {
		t.Fatalf("перепроверка той же ревизии откатила ручную версию: вызовов %d", e.run.count())
	}
	// Новая ревизия - новый запуск, юнит с её номером.
	d2 := stackDesired(t, 21, stack.Content{stack.ProductMtg: {Version: "1.16.0", SHA256: shaMtg, Size: 10}})
	e.a.apply(context.Background(), d2)
	if e.run.count() != 2 || !strings.Contains(strings.Join(e.run.calls[1], " "), "--unit=telemux-stack-mtg-21 ") ||
		!strings.Contains(strings.Join(e.run.calls[1], " "), "--bin "+e.mtgBin) {
		t.Fatalf("новая ревизия: %v", e.run.calls)
	}
}

// Бинаря нет на машине - failed без запуска.
func TestAgent_StackBinaryMissing(t *testing.T) {
	e := newStackAgent(t, ModeApply, false)
	e.a.versions.LookPath = func(string) (string, error) { return "", errors.New("нет") }
	res := e.a.apply(context.Background(), stackDesired(t, 3, stack.Content{stack.ProductTelemt: {Version: "3.5.17", SHA256: shaTelemt, Size: 10}}))
	if res.Status != "failed" || !strings.Contains(res.Error, "бинарь на машине не найден") || e.run.count() != 0 {
		t.Fatalf("нет бинаря: %+v, вызовов %d", res, e.run.count())
	}
}

// Та же версия (суффикс тоже совпадает) - ничего не запускается.
func TestAgent_StackApplySameVersionNoop(t *testing.T) {
	e := newStackAgent(t, ModeApply, true)
	e.versions.set(e.mtgBin, "1.15.0-webalias (go1.26.5: linux/amd64)\n")
	res := e.a.apply(context.Background(), stackDesired(t, 7, stack.Content{
		stack.ProductTelemt: {Version: "v3.5.16", SHA256: shaTelemt, Size: 10},
		stack.ProductMtg:    {Version: "1.15.0-webalias", SHA256: shaMtg, Size: 10},
	}))
	if res.Status != "applied" || e.run.count() != 0 {
		t.Fatalf("версии те же: ждали applied без вызовов, получили %+v, вызовов %d", res, e.run.count())
	}
}

// Провал systemd-run - failed с хвостом вывода; та же ревизия не перезапускается,
// новая - запускается.
func TestAgent_StackApplyFailureNotRetried(t *testing.T) {
	e := newStackAgent(t, ModeApply, false)
	e.run.err = errors.New("exit status 1")
	e.run.out = strings.Repeat("шум ", 300) + "stack-apply: telemt не поднялся за 30s; прежний бинарь возвращён"
	d := stackDesired(t, 8, stack.Content{stack.ProductTelemt: {Version: "3.5.17", SHA256: shaTelemt, Size: 10}})

	res := e.a.apply(context.Background(), d)
	if res.Status != "failed" || !strings.Contains(res.Error, "прежний бинарь возвращён") ||
		!strings.Contains(res.Error, "telemt: обновление до 3.5.17 не удалось") {
		t.Fatalf("ждали failed с причиной, получили %+v", res)
	}
	if len(res.Error) > 700 {
		t.Fatalf("хвост вывода не обрезан: %d байт", len(res.Error))
	}
	for i := 0; i < 3; i++ {
		if again := e.a.apply(context.Background(), d); again.Status != "failed" || again.Error != res.Error {
			t.Fatalf("повтор той же ревизии: %+v", again)
		}
	}
	if e.run.count() != 1 {
		t.Fatalf("отвергнутая ревизия запускалась снова: %d вызовов", e.run.count())
	}

	e.run.err = nil
	res = e.a.apply(context.Background(), stackDesired(t, 9, stack.Content{stack.ProductTelemt: {Version: "3.5.17", SHA256: shaTelemt, Size: 10}}))
	if res.Status != "applied" || e.run.count() != 2 {
		t.Fatalf("новая ревизия должна запуститься: %+v, вызовов %d", res, e.run.count())
	}
}

// Битый ресурс и отсутствующий продукт - failed без запуска.
func TestAgent_StackApplyBadContentAndMissingProduct(t *testing.T) {
	e := newStackAgent(t, ModeApply, false)
	res := e.a.apply(context.Background(), &store.Desired{Resource: stack.Resource, Revision: 1, Content: []byte(`{"xray":{}}`)})
	if res.Status != "failed" || !strings.Contains(res.Error, "неизвестный продукт") {
		t.Fatalf("битый ресурс: %+v", res)
	}
	res = e.a.apply(context.Background(), stackDesired(t, 2, stack.Content{
		stack.ProductMtg:    {Version: "1.16.0", SHA256: shaMtg, Size: 10},
		stack.ProductTelemt: {Version: "3.5.17", SHA256: shaTelemt, Size: 10},
	}))
	if res.Status != "failed" || !strings.Contains(res.Error, "mtg: продукта на машине нет") {
		t.Fatalf("нет mtg: %+v", res)
	}
	// telemt при этом обновлялся: продукты независимы.
	if e.run.count() != 1 || !strings.Contains(strings.Join(e.run.calls[0], " "), "--product telemt ") {
		t.Fatalf("telemt должен обновиться и без mtg: %v", e.run.calls)
	}
}

// Сквозной цикл: бандл с ресурсом stack от мастера -> compare -> отчёт с разницей.
func TestAgent_StackThroughMaster(t *testing.T) {
	e := newStackAgent(t, ModeCompare, false)
	d := stackDesired(t, 11, stack.Content{stack.ProductTelemt: {Version: "3.5.17", SHA256: shaTelemt, Size: 10}})
	m := &fakeMaster{bundle: store.Bundle{Target: "pr2", Revision: 11, Resources: []store.Desired{*d}}}
	srv := httptest.NewServer(m.handler(t, "tok", "pr2"))
	defer srv.Close()
	a2, err := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2", Mode: ModeCompare,
		UsersFile: e.a.cfg.UsersFile, Runtime: e.a.cfg.Runtime, Wait: 50 * time.Millisecond, Log: e.a.cfg.Log})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a2.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.reports) != 1 {
		t.Fatalf("ждали отчёт, получили %d", len(m.reports))
	}
	var got *store.ObservedResource
	for i := range m.reports[0].Resources {
		if m.reports[0].Resources[i].Resource == stack.Resource {
			got = &m.reports[0].Resources[i]
		}
	}
	if got == nil || got.Status != "compared" || got.Error != "telemt: на машине 3.5.16, нужно 3.5.17" {
		t.Fatalf("в отчёте нет разницы stack: %+v", m.reports[0].Resources)
	}
}

func TestOutputTail(t *testing.T) {
	if got := outputTail([]byte("  коротко \n"), 500); got != "коротко" {
		t.Fatalf("короткий вывод: %q", got)
	}
	long := strings.Repeat("я", 400) + "КОНЕЦ"
	got := outputTail([]byte(long), 100)
	if !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, "КОНЕЦ") || len(got) > 103 {
		t.Fatalf("хвост: %q", got)
	}
	if !strings.HasPrefix(got[3:], "я") { // не с середины символа
		t.Fatalf("хвост начат с середины символа: %q", got[:8])
	}
}

// После итога обновления мастер публикует ревизию {} (продукт ушёл из ресурса): агент
// отвечает applied без вызовов, иначе дрейф показывал бы машину отстающей.
func TestAgent_StackEmptyRevisionApplied(t *testing.T) {
	for _, mode := range []string{ModeApply, ModeCompare} {
		e := newStackAgent(t, mode, true)
		res := e.a.apply(context.Background(), &store.Desired{Resource: stack.Resource, Revision: 9, Content: []byte(`{}`)})
		// И в compare: менять нечего - ревизия выполнена.
		const want = "applied"
		if res.Status != want || res.Error != "" || e.run.count() != 0 {
			t.Fatalf("%s: ждали %s без вызовов, получили %+v, вызовов %d", mode, want, res, e.run.count())
		}
	}
}

func TestAgent_StackApplyUsesManagedMasterFirst(t *testing.T) {
	e := newStackAgent(t, ModeApply, false)
	e.run.after = func() { e.versions.set(e.telemtBin, "telemt 3.5.17\n") }
	const entry = "http://192.0.2.41:18480|socks5://127.0.0.1:10874"
	paths := []relayPath{{Exit: "ps1", Master: entry}}
	masters, err := e.a.parseRelayPaths(paths)
	if err != nil {
		t.Fatal(err)
	}
	e.a.relayPaths, e.a.relayMasters = paths, masters
	d := stackDesired(t, 6, stack.Content{stack.ProductTelemt: {Version: "3.5.17", SHA256: shaTelemt, Size: 4242}})
	if got := e.a.apply(context.Background(), d); got.Status != "applied" {
		t.Fatalf("применение: %+v", got)
	}
	call := e.run.calls[0]
	for i := 0; i < len(call)-1; i++ {
		if call[i] == "--master" && strings.HasPrefix(call[i+1], entry+",") {
			return
		}
	}
	t.Fatal("stack-apply не получил приоритетный путь из реестра")
}
