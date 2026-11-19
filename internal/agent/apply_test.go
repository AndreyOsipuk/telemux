package agent

import (
	"context"
	"errors"
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

// fakeTelemt - эмулятор telemt: перезагрузка по сигналу и ответ /v1/config/state.
// accept решает, принять ли конфиг на диске после перезагрузки.
type fakeTelemt struct {
	mu      sync.Mutex
	reloads int
	state   string // ответ API: accepted | rejected | pending | down
	accept  func(users, config string) bool
	dir     string
	srv     *httptest.Server
}

func newFakeTelemt(t *testing.T, dir string) *fakeTelemt {
	f := &fakeTelemt{dir: dir, state: "accepted", accept: func(string, string) bool { return true }}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		st := f.state
		f.mu.Unlock()
		switch st {
		case "down":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "rejected":
			_, _ = w.Write([]byte(`{"ok":true,"data":{"desired_status":"valid","watcher":{"phase":"rejected","error_code":"web_profile_unknown_user"},"watcher_matches_desired":true,"comparison":{"status":"pending_publication"}}}`))
		case "invalid":
			_, _ = w.Write([]byte(`{"ok":true,"data":{"desired_status":"invalid","desired_error":{"path":"web.vhosts.profiles[1].user","code":"unknown_user","message":"references unknown access user"}}}`))
		case "pending":
			_, _ = w.Write([]byte(`{"ok":true,"data":{"desired_status":"valid","watcher":{"phase":"idle"},"watcher_matches_desired":false,"comparison":{"status":"pending_publication"}}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"data":{"desired_status":"valid","watcher":{"phase":"published"},"watcher_matches_desired":true,"comparison":{"status":"in_sync"}}}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// Reload - «SIGHUP»: telemt перечитывает файлы и решает, принять ли их.
func (f *fakeTelemt) Reload(context.Context) error {
	users, _ := os.ReadFile(filepath.Join(f.dir, "users.toml"))
	cfg, _ := os.ReadFile(filepath.Join(f.dir, "telemt.toml"))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	if f.state == "down" || f.state == "pending" {
		return nil
	}
	if f.accept(string(users), string(cfg)) {
		f.state = "accepted"
	} else {
		f.state = "rejected"
	}
	return nil
}

const webCfg = `[[web.vhosts]]
domain = "web.example.com"

[[web.vhosts.profiles]]
user = "alice_1"
secret_mode = "ee"
max_sessions = 4
max_streams = 256
max_streams_per_session = 32

[server.api]
listen = "127.0.0.1:9091"
`

func setupMachine(t *testing.T) (string, *fakeTelemt, *Applier) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "users.toml"), render(telemtcfg.RenderUser{Key: "alice_1", Secret: sec1}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "telemt.toml"), []byte(webCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ft := newFakeTelemt(t, dir)
	ap := &Applier{
		UsersFile: filepath.Join(dir, "users.toml"), ConfigFile: filepath.Join(dir, "telemt.toml"),
		StateURL: ft.srv.URL, Reloader: ft, ConfirmTimeout: 300 * time.Millisecond, PollEvery: 20 * time.Millisecond,
		FreeSpace: func(string) (uint64, error) { return 1 << 30, nil },
	}
	return dir, ft, ap
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Принятый конфиг: оба файла заменены, профили пересобраны под новый список, одна перезагрузка.
func TestApply_Accepted(t *testing.T) {
	dir, ft, ap := setupMachine(t)
	desired := render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2})
	res := ap.Apply(context.Background(), desired)
	if res.Status != "applied" || res.Error != "" {
		t.Fatalf("ждали applied, получили %+v", res)
	}
	if readFile(t, filepath.Join(dir, "users.toml")) != string(desired) {
		t.Fatal("users.toml не заменён")
	}
	cfg := readFile(t, filepath.Join(dir, "telemt.toml"))
	if !strings.Contains(cfg, `user = "bob_2"`) || strings.Contains(cfg, `user = "alice_1"`) {
		t.Fatalf("профили не пересобраны:\n%s", cfg)
	}
	if ft.reloads != 1 {
		t.Fatalf("ждали одну перезагрузку, было %d", ft.reloads)
	}
	if _, err := os.Stat(filepath.Join(dir, "users.toml.telemux-tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("временный файл не убран")
	}
}

// Отвергнутый конфиг: оба файла возвращаются, telemt перезагружается на старых.
func TestApply_RejectedRollsBack(t *testing.T) {
	dir, ft, ap := setupMachine(t)
	ft.accept = func(string, string) bool { return false }
	oldUsers := readFile(t, filepath.Join(dir, "users.toml"))
	res := ap.Apply(context.Background(), render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2}))
	if res.Status != "failed" || !strings.Contains(res.Error, "отверг") {
		t.Fatalf("ждали failed с причиной отказа, получили %+v", res)
	}
	if readFile(t, filepath.Join(dir, "users.toml")) != oldUsers || readFile(t, filepath.Join(dir, "telemt.toml")) != webCfg {
		t.Fatal("после отказа файлы не возвращены")
	}
	if ft.reloads != 2 {
		t.Fatalf("ждали перезагрузку на новых и на возвращённых файлах, было %d", ft.reloads)
	}
}

// Конфиг на диске не проходит проверку telemt (desired_status invalid) - тоже откат.
func TestApply_InvalidRollsBack(t *testing.T) {
	dir, ft, ap := setupMachine(t)
	ft.accept = func(string, string) bool { return false }
	ft.state = "accepted"
	reload := ft.Reload
	_ = reload
	ap.Reloader = reloaderFunc(func(ctx context.Context) error {
		ft.mu.Lock()
		ft.reloads++
		if ft.reloads == 1 {
			ft.state = "invalid"
		} else {
			ft.state = "accepted"
		}
		ft.mu.Unlock()
		return nil
	})
	res := ap.Apply(context.Background(), render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2}))
	if res.Status != "failed" || !strings.Contains(res.Error, "unknown_user") {
		t.Fatalf("ждали failed с причиной из API, получили %+v", res)
	}
	if readFile(t, filepath.Join(dir, "telemt.toml")) != webCfg {
		t.Fatal("файлы не возвращены")
	}
}

type reloaderFunc func(context.Context) error

func (f reloaderFunc) Reload(ctx context.Context) error { return f(ctx) }

// telemt не подтвердил за отведённое время, API молчит - откат (без подтверждения не оставляем).
func TestApply_UnconfirmedRollsBack(t *testing.T) {
	dir, ft, ap := setupMachine(t)
	ft.state = "down"
	oldUsers := readFile(t, filepath.Join(dir, "users.toml"))
	res := ap.Apply(context.Background(), render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2}))
	if res.Status != "failed" || !strings.Contains(res.Error, "не подтвердил") {
		t.Fatalf("ждали failed без подтверждения, получили %+v", res)
	}
	if readFile(t, filepath.Join(dir, "users.toml")) != oldUsers {
		t.Fatal("без подтверждения файлы должны вернуться")
	}
}

// Нет места на диске - ничего не пишется.
func TestApply_NoSpace(t *testing.T) {
	dir, ft, ap := setupMachine(t)
	ap.FreeSpace = func(string) (uint64, error) { return 100, nil }
	oldUsers := readFile(t, filepath.Join(dir, "users.toml"))
	res := ap.Apply(context.Background(), render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2}))
	if res.Status != "failed" || !strings.Contains(res.Error, "места") {
		t.Fatalf("ждали failed из-за места, получили %+v", res)
	}
	if readFile(t, filepath.Join(dir, "users.toml")) != oldUsers || ft.reloads != 0 {
		t.Fatal("при нехватке места файлы и telemt трогать нельзя")
	}
}

// Записанный файл не совпал с желаемым (обрезался) - рабочие файлы не трогаются.
func TestApply_TruncatedWriteDetected(t *testing.T) {
	dir, ft, ap := setupMachine(t)
	ap.writeFile = func(p string, data []byte, perm os.FileMode) error {
		return os.WriteFile(p, data[:len(data)/2], perm)
	}
	oldUsers := readFile(t, filepath.Join(dir, "users.toml"))
	res := ap.Apply(context.Background(), render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2}))
	if res.Status != "failed" || !strings.Contains(res.Error, "обрез") {
		t.Fatalf("ждали failed из-за обрезанного файла, получили %+v", res)
	}
	if readFile(t, filepath.Join(dir, "users.toml")) != oldUsers || ft.reloads != 0 {
		t.Fatal("обрезанный файл не должен попасть на место рабочего")
	}
}

// Профилей больше лимита - конфиг не пишется вообще (telemt ушёл бы в перезапуски).
func TestApply_ProfilesOverLimit(t *testing.T) {
	dir, ft, ap := setupMachine(t)
	cfg := strings.Replace(webCfg, "[server.api]", "[web.limits]\nmax_profiles = 1\n\n[server.api]", 1)
	_ = os.WriteFile(filepath.Join(dir, "telemt.toml"), []byte(cfg), 0o600)
	res := ap.Apply(context.Background(), render(
		telemtcfg.RenderUser{Key: "bob_2", Secret: sec2}, telemtcfg.RenderUser{Key: "carol_3", Secret: sec1}))
	if res.Status != "failed" || !strings.Contains(res.Error, "max_profiles") {
		t.Fatalf("ждали failed из-за лимита профилей, получили %+v", res)
	}
	if ft.reloads != 0 {
		t.Fatal("при превышении лимита telemt трогать нельзя")
	}
}

// Уже совпадает - ничего не пишется и не перезагружается.
func TestApply_AlreadyInSync(t *testing.T) {
	dir, ft, ap := setupMachine(t)
	current := []byte(readFile(t, filepath.Join(dir, "users.toml")))
	before, _ := os.Stat(filepath.Join(dir, "users.toml"))
	res := ap.Apply(context.Background(), current)
	after, _ := os.Stat(filepath.Join(dir, "users.toml"))
	if res.Status != "applied" || ft.reloads != 0 || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("совпадающее состояние трогать нельзя: %+v, перезагрузок %d", res, ft.reloads)
	}
}

// Пустой список при непустом текущем - отказ (похоже на недогруженную базу мастера).
func TestApply_RefusesEmptyOverNonEmpty(t *testing.T) {
	_, ft, ap := setupMachine(t)
	res := ap.Apply(context.Background(), telemtcfg.RenderUsersToml(nil, 50))
	if res.Status != "failed" || ft.reloads != 0 {
		t.Fatalf("пустой список поверх непустого применять нельзя: %+v", res)
	}
}

func applyAgent(t *testing.T, masterURL string, ap *Applier) *Agent {
	t.Helper()
	a, err := New(Config{
		MasterURLs: []string{masterURL}, Token: "tok", Target: "pr2", Version: "test",
		UsersFile: ap.UsersFile, Mode: ModeApply, Applier: ap, Wait: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Режим применения: агент получает бандл, применяет и отчитывается applied.
func TestAgent_ApplyMode(t *testing.T) {
	dir, _, ap := setupMachine(t)
	desired := render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2})
	m := &fakeMaster{bundle: bundleOf(4, desired)}
	srv := httptest.NewServer(m.handler(t, "tok", "pr2"))
	defer srv.Close()

	a := applyAgent(t, srv.URL, ap)
	if _, err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(dir, "users.toml")) != string(desired) {
		t.Fatal("агент не применил users.toml")
	}
	r := m.reports[len(m.reports)-1].Resources[0]
	if r.Status != "applied" || r.Revision != 4 {
		t.Fatalf("ждали applied ревизии 4, получили %+v", r)
	}
}

// Отвергнутую ревизию агент не применяет по кругу: только новая ревизия
// пробует снова. В отчёте остаётся failed с причиной.
func TestAgent_FailedRevisionNotRetried(t *testing.T) {
	_, ft, ap := setupMachine(t)
	ft.accept = func(string, string) bool { return false }
	m := &fakeMaster{bundle: bundleOf(5, render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2}))}
	srv := httptest.NewServer(m.handler(t, "tok", "pr2"))
	defer srv.Close()

	a := applyAgent(t, srv.URL, ap)
	if _, err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ft.reloads != 2 {
		t.Fatalf("ждали попытку и откат (2 перезагрузки), было %d", ft.reloads)
	}
	for i := 0; i < 3; i++ {
		a.ForceRecheck()
		if _, err := a.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if ft.reloads != 2 {
		t.Fatalf("отвергнутая ревизия применялась повторно: перезагрузок %d", ft.reloads)
	}
	last := m.reports[len(m.reports)-1].Resources[0]
	if last.Status != "failed" || last.Revision != 5 || last.Error == "" {
		t.Fatalf("в отчёте должен остаться failed с причиной, получили %+v", last)
	}

	// Новая ревизия - новая попытка.
	ft.accept = func(string, string) bool { return true }
	m.mu.Lock()
	m.bundle = bundleOf(6, render(telemtcfg.RenderUser{Key: "carol_3", Secret: sec1}))
	m.mu.Unlock()
	if _, err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := m.reports[len(m.reports)-1].Resources[0]; r.Status != "applied" || r.Revision != 6 {
		t.Fatalf("новая ревизия должна примениться, получили %+v", r)
	}
}

func bundleOf(rev int64, content []byte) store.Bundle {
	return store.Bundle{Target: "pr2", Revision: rev, Resources: []store.Desired{
		{Resource: store.ResourceTelemtUsers, Revision: rev, SHA256: telemtcfg.SHA256(content), Content: content},
	}}
}

// Откат возвращает только файлы, которые менялись в этой попытке. Раньше откат
// поднимал и бэкап telemt.toml, оставшийся от прошлого успешного применения: при
// смене одного секрета (профили не меняются) на машину возвращались профили
// старого списка, а с ресурсом telemt.config - и старый основной конфиг.
func TestApply_RollbackRestoresOnlyChangedFiles(t *testing.T) {
	dir, ft, ap := setupMachine(t)
	bob := render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2})
	if r := ap.Apply(context.Background(), bob); r.Status != "applied" {
		t.Fatalf("первое применение: %+v", r)
	}
	cfgAfter := readFile(t, filepath.Join(dir, "telemt.toml"))
	if !strings.Contains(cfgAfter, `user = "bob_2"`) {
		t.Fatal("профили не пересобраны")
	}
	// Тот же ключ, другой секрет: меняется только users.toml; telemt отвергает.
	ft.accept = func(string, string) bool { return false }
	if r := ap.Apply(context.Background(), render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec1})); r.Status != "failed" {
		t.Fatalf("ждали failed: %+v", r)
	}
	if got := readFile(t, filepath.Join(dir, "telemt.toml")); got != cfgAfter {
		t.Fatalf("откат вернул устаревший telemt.toml из бэкапа прошлого применения:\n%s", got)
	}
	if readFile(t, filepath.Join(dir, "users.toml")) != string(bob) {
		t.Fatal("users.toml не возвращён")
	}
}

// users.toml не было: откат удаляет новый файл, а не поднимает давний бэкап.
func TestApply_RollbackRemovesNewFile(t *testing.T) {
	dir, ft, ap := setupMachine(t)
	users := filepath.Join(dir, "users.toml")
	stale := "# давний бэкап\n"
	_ = os.WriteFile(users+bakSuffix, []byte(stale), 0o600)
	_ = os.Remove(users)
	ft.accept = func(string, string) bool { return false }
	if r := ap.Apply(context.Background(), render(telemtcfg.RenderUser{Key: "alice_1", Secret: sec2})); r.Status != "failed" {
		t.Fatalf("ждали failed: %+v", r)
	}
	if b, err := os.ReadFile(users); err == nil {
		t.Fatalf("откат поднял давний бэкап вместо удаления нового файла: %q", b)
	}
}
