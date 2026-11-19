package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

// mainBase - telemt.config от бота (без профилей, include на users.toml машины).
func mainBase(dir, port string) string {
	return "[general]\nfast_mode = true\n\n[server]\nport = " + port + "\n\n[server.api]\nread_only = true\n\n" +
		"[[server.listeners]]\nip = \"0.0.0.0\"\nannounce_ip = \"198.51.100.2\"\n\n" +
		"[censorship]\nmask_port = 443\ntls_front_dir = \"/var/lib/telemt/tlsfront\"\n\n" +
		"[access]\ninclude = \"" + filepath.Join(dir, "users.toml") + "\"\n\n[[upstreams]]\ntype = \"direct\"\n"
}

// relayLocal - машинная часть WEB-релея.
const relayLocal = "[server]\nport = 8453\n\n[[server.listeners]]\nip = \"127.0.0.1\"\n\n[censorship]\nmask_port = 8444\n\n" +
	"[web]\nlisten = \"127.0.0.1:18080\"\n\n[[web.vhosts]]\nhost = \"relay.example.com\"\n"

// fakeTelemtProc - telemt под systemd: SIGHUP перечитывает файл с диска, часть
// полей применяется только рестартом (deferred), рестарт на плохом конфиге
// уходит в цикл перезапусков.
type fakeTelemtProc struct {
	mu        sync.Mutex
	cfgPath   string
	reloads   int
	restarts  int
	nrestarts int    // счётчик авто-перезапусков systemd
	loaded    string // конфиг, на котором работает процесс
	running   string // конфиг процесса после последнего рестарта
	phase     string // published | rejected
	invalid   func(cfg string) bool
	deferred  func(old, next string) []string // поля, которые требуют рестарта
	crashLoop func(cfg string) bool           // рестарт на этом конфиге не поднимается
	down      bool
	srv       *httptest.Server
}

func newFakeTelemtProc(t *testing.T, cfgPath string) *fakeTelemtProc {
	cur, _ := os.ReadFile(cfgPath)
	f := &fakeTelemtProc{cfgPath: cfgPath, loaded: string(cur), running: string(cur), phase: "published",
		invalid:   func(string) bool { return false },
		deferred:  func(string, string) []string { return nil },
		crashLoop: func(string) bool { return false },
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		disk, _ := os.ReadFile(f.cfgPath)
		data := map[string]any{}
		if f.invalid(string(disk)) {
			data["desired_status"] = "invalid"
			data["desired_error"] = map[string]string{"path": "server.port", "code": "bad", "message": "invalid value"}
		} else {
			data["desired_status"] = "valid"
			matches := string(disk) == f.loaded
			data["watcher"] = map[string]string{"phase": f.phase}
			data["watcher_matches_desired"] = matches || f.phase == "rejected"
			cmp := map[string]any{"status": "published_match", "process_deferred_paths": f.deferred(f.running, f.loaded)}
			if !matches && f.phase != "rejected" {
				cmp["status"] = "pending_publication"
			}
			data["comparison"] = cmp
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTelemtProc) Reload(ctx context.Context) error {
	if ctx.Err() != nil { // как exec.CommandContext: с отменённым ctx systemctl не запустится
		return ctx.Err()
	}
	disk, _ := os.ReadFile(f.cfgPath)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	if f.invalid(string(disk)) {
		f.phase = "rejected"
		return nil
	}
	f.phase, f.loaded = "published", string(disk)
	return nil
}

func (f *fakeTelemtProc) Restart(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	disk, _ := os.ReadFile(f.cfgPath)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	if f.crashLoop(string(disk)) {
		f.down = true
		f.nrestarts += 3
		return nil
	}
	f.down, f.phase, f.loaded, f.running = false, "published", string(disk), string(disk)
	return nil
}

func (f *fakeTelemtProc) Active(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.down
}

func (f *fakeTelemtProc) Restarts(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nrestarts, nil
}

// setupMain - машина с users.toml (alice_1), telemt.toml и, если local != "", машинной частью.
func setupMain(t *testing.T, current, local string) (string, *fakeTelemtProc, *ConfigApplier) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "users.toml"), render(telemtcfg.RenderUser{Key: "alice_1", Secret: sec1}), 0o600); err != nil {
		t.Fatal(err)
	}
	if current == "" {
		current = mainBase(dir, "443")
	}
	if err := os.WriteFile(filepath.Join(dir, "telemt.toml"), []byte(strings.ReplaceAll(current, "DIR", dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	if local != "" {
		if err := os.WriteFile(filepath.Join(dir, "telemt.local.toml"), []byte(local), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fp := newFakeTelemtProc(t, filepath.Join(dir, "telemt.toml"))
	ca := &ConfigApplier{
		ConfigFile: filepath.Join(dir, "telemt.toml"), LocalFile: filepath.Join(dir, "telemt.local.toml"),
		UsersFile: filepath.Join(dir, "users.toml"), StateURL: fp.srv.URL, Service: fp,
		ConfirmTimeout: 300 * time.Millisecond, PollEvery: 10 * time.Millisecond, SettleTime: 20 * time.Millisecond,
		FreeSpace: func(string) (uint64, error) { return 1 << 30, nil },
	}
	return dir, fp, ca
}

// Изменение, которое telemt применяет на лету: SIGHUP без рестарта.
func TestConfigApply_HotReload(t *testing.T) {
	dir, fp, ca := setupMain(t, "", "")
	next := strings.Replace(mainBase(dir, "443"), "fast_mode = true", "fast_mode = false", 1)
	if r := ca.Apply(context.Background(), []byte(next)); r.Status != "applied" {
		t.Fatalf("ждали applied, получили %+v", r)
	}
	if readFile(t, ca.ConfigFile) != next || fp.reloads != 1 || fp.restarts != 0 {
		t.Fatalf("файл/перезагрузки: reloads=%d restarts=%d", fp.reloads, fp.restarts)
	}
}

// Поле, которое telemt применяет только рестартом (порт): после принятой
// перезагрузки агент перезапускает telemt и проверяет, что тот поднялся.
func TestConfigApply_DeferredFieldsRestart(t *testing.T) {
	dir, fp, ca := setupMain(t, "", "")
	fp.deferred = func(running, loaded string) []string {
		if strings.Contains(running, "\nport = 443\n") != strings.Contains(loaded, "\nport = 443\n") {
			return []string{"server.port"}
		}
		return nil
	}
	next := mainBase(dir, "444")
	if r := ca.Apply(context.Background(), []byte(next)); r.Status != "applied" {
		t.Fatalf("ждали applied, получили %+v", r)
	}
	if fp.restarts != 1 || !strings.Contains(fp.running, "port = 444") {
		t.Fatalf("ждали один рестарт на новом конфиге: restarts=%d", fp.restarts)
	}
}

// telemt не принял конфиг при перезагрузке - рестарта не было, прежний файл возвращён.
func TestConfigApply_InvalidNeverRestarts(t *testing.T) {
	dir, fp, ca := setupMain(t, "", "")
	before := readFile(t, ca.ConfigFile)
	fp.invalid = func(cfg string) bool { return strings.Contains(cfg, "port = 99999") }
	r := ca.Apply(context.Background(), []byte(mainBase(dir, "99999")))
	if r.Status != "failed" || !strings.Contains(r.Error, "server.port") {
		t.Fatalf("ждали failed с причиной от telemt, получили %+v", r)
	}
	if fp.restarts != 0 {
		t.Fatal("telemt перезапущен на невалидном конфиге")
	}
	if readFile(t, ca.ConfigFile) != before {
		t.Fatal("прежний telemt.toml не возвращён")
	}
}

// Рестарт на новом конфиге ушёл в цикл перезапусков (порт занят) - откат и рестарт на прежнем.
func TestConfigApply_CrashLoopRollsBack(t *testing.T) {
	dir, fp, ca := setupMain(t, "", "")
	before := readFile(t, ca.ConfigFile)
	fp.deferred = func(running, loaded string) []string {
		if running != loaded {
			return []string{"server.port"}
		}
		return nil
	}
	fp.crashLoop = func(cfg string) bool { return strings.Contains(cfg, "port = 445") }
	r := ca.Apply(context.Background(), []byte(mainBase(dir, "445")))
	if r.Status != "failed" || !strings.Contains(r.Error, "прежний") {
		t.Fatalf("ждали failed с откатом, получили %+v", r)
	}
	if readFile(t, ca.ConfigFile) != before || fp.restarts != 2 || fp.down {
		t.Fatalf("ждали возврат прежнего и рестарт на нём: restarts=%d down=%v", fp.restarts, fp.down)
	}
}

// WEB-релей: машинная часть (порт 8453, listener, mask_port, WEB) накладывается на
// базу, профили собираются из users.toml машины.
func TestConfigApply_RelayLocalPartAndProfiles(t *testing.T) {
	dir, _, ca := setupMain(t, "", relayLocal)
	if r := ca.Apply(context.Background(), []byte(mainBase(dir, "443"))); r.Status != "applied" {
		t.Fatalf("ждали applied, получили %+v", r)
	}
	got := readFile(t, ca.ConfigFile)
	for _, want := range []string{"port = 8453", `ip = "127.0.0.1"`, "mask_port = 8444", "[web]", "[[web.vhosts.profiles]]", `user = "alice_1"`} {
		if !strings.Contains(got, want) {
			t.Errorf("нет %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `ip = "0.0.0.0"`) {
		t.Errorf("listener базы не заменён машинным:\n%s", got)
	}
}

// Эталон без WEB-режима на WEB-релее без машинной части не применяется (инцидент
// с «Address in use»): файл не тронут, причина - в отчёте.
func TestConfigApply_RefusesToDropWebMode(t *testing.T) {
	dir := t.TempDir()
	cur, err := telemtcfg.ComposeMainConfig(mainBase(dir, "443"), relayLocal, []string{"alice_1"})
	if err != nil {
		t.Fatal(err)
	}
	_, fp, ca := setupMain(t, cur, "")
	before := readFile(t, ca.ConfigFile)
	r := ca.Apply(context.Background(), []byte(mainBase(filepath.Dir(ca.ConfigFile), "443")))
	if r.Status != "failed" || !strings.Contains(r.Error, "WEB") {
		t.Fatalf("ждали отказ из-за WEB-режима, получили %+v", r)
	}
	if readFile(t, ca.ConfigFile) != before || fp.reloads+fp.restarts != 0 {
		t.Fatal("конфиг WEB-релея тронут")
	}
}

// include не на users.toml агента - отказ до записи.
func TestConfigApply_RefusesForeignInclude(t *testing.T) {
	_, fp, ca := setupMain(t, "", "")
	r := ca.Apply(context.Background(), []byte(mainBase("/srv/other", "443")))
	if r.Status != "failed" || !strings.Contains(r.Error, "include") || fp.reloads != 0 {
		t.Fatalf("ждали отказ из-за include, получили %+v (reloads=%d)", r, fp.reloads)
	}
}

func TestConfigApply_InSyncDoesNothing(t *testing.T) {
	dir, fp, ca := setupMain(t, "", "")
	if r := ca.Apply(context.Background(), []byte(mainBase(dir, "443"))); r.Status != "applied" || fp.reloads != 0 {
		t.Fatalf("совпадающий конфиг: %+v reloads=%d", r, fp.reloads)
	}
}

func TestConfigApply_NoSpace(t *testing.T) {
	dir, fp, ca := setupMain(t, "", "")
	ca.FreeSpace = func(string) (uint64, error) { return 10, nil }
	r := ca.Apply(context.Background(), []byte(mainBase(dir, "444")))
	if r.Status != "failed" || !strings.Contains(r.Error, "места") || fp.reloads != 0 {
		t.Fatalf("ждали отказ по месту: %+v", r)
	}
}

func TestConfigApply_APIDownRollsBack(t *testing.T) {
	dir, fp, ca := setupMain(t, "", "")
	before := readFile(t, ca.ConfigFile)
	fp.mu.Lock()
	fp.down = true
	fp.mu.Unlock()
	r := ca.Apply(context.Background(), []byte(mainBase(dir, "444")))
	if r.Status != "failed" || readFile(t, ca.ConfigFile) != before || fp.restarts != 0 {
		t.Fatalf("без подтверждения ждали откат без рестарта: %+v restarts=%d", r, fp.restarts)
	}
}

func TestSystemdTelemtRestartsParse(t *testing.T) {
	if n, err := parseNRestarts("3\n"); err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := parseNRestarts("[not set]"); err == nil {
		t.Fatal("мусор принят")
	}
}

// Агент: telemt.users и telemt.config на одной машине - оба применяются, профили
// в telemt.toml соответствуют новому списку, отчёт по обоим ресурсам.
func TestAgent_UsersAndMainConfig(t *testing.T) {
	dir, fp, ca := setupMain(t, "", relayLocal)
	// Сначала на машине собранный конфиг под alice_1.
	start, _ := telemtcfg.ComposeMainConfig(mainBase(dir, "443"), relayLocal, []string{"alice_1"})
	if err := os.WriteFile(ca.ConfigFile, []byte(start), 0o600); err != nil {
		t.Fatal(err)
	}
	fp.loaded, fp.running = start, start
	ap := &Applier{ConfigFile: ca.ConfigFile, StateURL: fp.srv.URL + "?sections=access", Reloader: fp,
		ConfirmTimeout: 300 * time.Millisecond, PollEvery: 10 * time.Millisecond,
		FreeSpace: func(string) (uint64, error) { return 1 << 30, nil }}
	users := render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2})
	base := strings.Replace(mainBase(dir, "443"), "fast_mode = true", "fast_mode = false", 1)
	m := &fakeMaster{bundle: store.Bundle{Target: "pr2", Revision: 11, Resources: []store.Desired{
		{Resource: store.ResourceTelemtConfig, Revision: 11, SHA256: "c", Content: []byte(base)},
		{Resource: store.ResourceTelemtUsers, Revision: 10, SHA256: "u", Content: users},
	}}}
	srv := httptest.NewServer(m.handler(t, "tok", "pr2"))
	defer srv.Close()

	// Сравнение: ничего не меняет, расхождение основного конфига названо по секциям.
	cmp, err := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2", Mode: ModeCompare,
		UsersFile: ca.UsersFile, TelemtConfigFile: ca.ConfigFile, TelemtLocalFile: ca.LocalFile, Wait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cmp.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep := m.reports[len(m.reports)-1]
	if len(rep.Resources) != 2 || rep.Resources[0].Resource != store.ResourceTelemtUsers || rep.Resources[1].Resource != store.ResourceTelemtConfig {
		t.Fatalf("ждали отчёт users, затем config: %+v", rep.Resources)
	}
	if r := rep.Resources[1]; r.Status != "compared" || !strings.Contains(r.Error, "[general]") {
		t.Fatalf("расхождение [general] не названо: %+v", r)
	}
	if readFile(t, ca.ConfigFile) != start {
		t.Fatal("режим сравнения тронул telemt.toml")
	}

	app, err := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2", Mode: ModeApply,
		UsersFile: ca.UsersFile, Applier: ap, TelemtConfigFile: ca.ConfigFile, TelemtLocalFile: ca.LocalFile,
		ConfigApplier: ca, Wait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep = m.reports[len(m.reports)-1]
	for _, r := range rep.Resources {
		if r.Status != "applied" {
			t.Fatalf("ждали applied по обоим: %+v", rep.Resources)
		}
	}
	want, _ := telemtcfg.ComposeMainConfig(base, relayLocal, []string{"bob_2"})
	if got := readFile(t, ca.ConfigFile); got != want {
		t.Fatalf("telemt.toml != база + машинная часть + профили bob_2:\n%s", got)
	}

	// Повторная сверка без изменений ничего не трогает.
	reloads := fp.reloads
	app.ForceRecheck()
	if _, err := app.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fp.reloads != reloads {
		t.Fatal("сверка без изменений перезагрузила telemt - ресурсы перетирают друг друга")
	}
}

func TestNew_MainConfigNeedsApplier(t *testing.T) {
	_, err := New(Config{MasterURLs: []string{"http://x"}, Token: "t", Target: "pr2", Mode: ModeApply,
		TelemtConfigFile: "/etc/telemt/telemt.toml"})
	if err == nil {
		t.Fatal("apply с telemt.config без ConfigApplier принят")
	}
}

// Отмена ctx агента (SIGTERM) посреди применения: откат всё равно перечитывает
// telemt на прежнем файле, а не оставляет его работать на новом.
func TestConfigApply_CancelStillRollsBack(t *testing.T) {
	dir, fp, ca := setupMain(t, "", "")
	before := readFile(t, ca.ConfigFile)
	fp.mu.Lock()
	fp.down = true // подтверждения не будет - агент ждёт
	fp.mu.Unlock()
	ca.ConfirmTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	r := ca.Apply(ctx, []byte(mainBase(dir, "444")))
	if r.Status != "failed" || readFile(t, ca.ConfigFile) != before {
		t.Fatalf("ждали откат файла: %+v", r)
	}
	if fp.reloads != 2 {
		t.Fatalf("откат не перечитал telemt после отмены ctx: reloads=%d", fp.reloads)
	}
}

// Неудача telemt.config из-за машинной части не запоминается навсегда: исправили
// telemt.local.toml - та же ревизия применяется при следующей сверке.
func TestAgent_MainConfigRetriedAfterLocalFix(t *testing.T) {
	dir, _, ca := setupMain(t, "", "[server\n")
	m := &fakeMaster{bundle: store.Bundle{Target: "pr2", Revision: 3, Resources: []store.Desired{
		{Resource: store.ResourceTelemtConfig, Revision: 3, SHA256: "c", Content: []byte(mainBase(dir, "443") + "\n[general.links]\nshow = []\n")},
	}}}
	srv := httptest.NewServer(m.handler(t, "tok", "pr2"))
	defer srv.Close()
	a, err := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2", Mode: ModeApply,
		UsersFile: ca.UsersFile, Applier: &Applier{}, TelemtConfigFile: ca.ConfigFile, TelemtLocalFile: ca.LocalFile,
		ConfigApplier: ca, Wait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := m.reports[len(m.reports)-1].Resources[0]; r.Status != "failed" {
		t.Fatalf("битая машинная часть: ждали failed, получили %+v", r)
	}
	if err := os.WriteFile(ca.LocalFile, []byte("[general]\nfast_mode = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ForceRecheck()
	if _, err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := m.reports[len(m.reports)-1].Resources[0]; r.Status != "applied" {
		t.Fatalf("после правки машинной части ждали applied той же ревизии, получили %+v", r)
	}
}
