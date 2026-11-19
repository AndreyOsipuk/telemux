package stackapply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	oldBin = "telemt 3.5.16\nold-binary"
	newBin = "telemt 3.5.17\nnew-binary-payload"
)

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// fakeSys - systemctl и `<бинарь> --version` без systemd: версия - первая строка файла,
// юнит активен, если в установленном бинаре нет слова BROKEN.
type fakeSys struct {
	t   *testing.T
	bin string

	mu       sync.Mutex
	restarts int
	down     bool // сервис остановлен (убит), пока его не перезапустят
	calls    []string
}

func (f *fakeSys) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if len(args) == 1 && args[0] == "--version" {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		return []byte(strings.SplitN(string(data), "\n", 2)[0] + "\n"), nil
	}
	if name != "systemctl" {
		f.t.Errorf("неожиданная команда %s %v", name, args)
		return nil, errors.New("неожиданная команда")
	}
	switch args[0] {
	case "restart":
		f.restarts++
		f.down = false
		return nil, nil
	case "show":
		return []byte("0\n"), nil
	case "is-active":
		data, _ := os.ReadFile(f.bin)
		if f.down {
			return []byte("inactive\n"), errors.New("exit status 3")
		}
		if strings.Contains(string(data), "BROKEN") {
			return []byte("failed\n"), errors.New("exit status 3")
		}
		return []byte("active\n"), nil
	}
	return nil, errors.New("неожиданный systemctl")
}

// fakeMaster - мастер с одним бинарём.
type fakeMaster struct {
	blob string
	hits int
	mu   sync.Mutex
}

func (m *fakeMaster) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits++
		m.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("target") != "pr2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/stack/blobs/"+sum(m.blob) {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(m.blob))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type env struct {
	dir string
	bin string
	sys *fakeSys
	cfg Config
}

func newEnv(t *testing.T, masters []string, blob string) *env {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "telemt")
	if err := os.WriteFile(bin, []byte(oldBin), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o750); err != nil {
		t.Fatal(err)
	}
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(health.Close)
	sys := &fakeSys{t: t, bin: bin}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	return &env{dir: dir, bin: bin, sys: sys, cfg: Config{
		Product: "telemt", SHA256: sum(blob), Version: "3.5.17", Size: int64(len(blob)),
		Masters: masters, Token: "tok", Target: "pr2",
		Bin: bin, Unit: "telemt", HealthURL: health.URL + "/v1/health",
		Runner: sys,
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
			return nil
		},
	}}
}

func (e *env) read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// В каталоге бинаря не остаётся временных файлов.
func (e *env) noTemp(t *testing.T) {
	t.Helper()
	entries, _ := os.ReadDir(e.dir)
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), ".telemux-") {
			t.Fatalf("остался временный файл %s", de.Name())
		}
	}
}

// Бинарь на месте и прежний; .bak не создан.
func (e *env) untouched(t *testing.T) {
	t.Helper()
	if got := e.read(t, e.bin); got != oldBin {
		t.Fatalf("бинарь изменён: %q", got)
	}
	if _, err := os.Stat(e.bin + ".bak"); !os.IsNotExist(err) {
		t.Fatal("создан .bak, хотя замены не было")
	}
	if e.sys.restarts != 0 {
		t.Fatalf("сервис перезапускался: %d", e.sys.restarts)
	}
	e.noTemp(t)
}

func TestRun_Success(t *testing.T) {
	m := &fakeMaster{blob: newBin}
	e := newEnv(t, []string{m.server(t).URL}, newBin)
	res, err := Run(context.Background(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.String() != "telemt: 3.5.16 -> 3.5.17" {
		t.Fatalf("итог: %q", res.String())
	}
	if got := e.read(t, e.bin); got != newBin {
		t.Fatalf("бинарь не заменён: %q", got)
	}
	if got := e.read(t, e.bin+".bak"); got != oldBin {
		t.Fatalf(".bak не прежний бинарь: %q", got)
	}
	if fi, _ := os.Stat(e.bin + ".bak"); fi.Mode().Perm() != 0o750 {
		t.Fatalf("права .bak не сохранены: %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(e.bin); fi.Mode().Perm() != 0o755 {
		t.Fatalf("новый бинарь не исполняемый: %v", fi.Mode().Perm())
	}
	if e.sys.restarts != 1 {
		t.Fatalf("ждали один перезапуск, было %d", e.sys.restarts)
	}
	e.noTemp(t)
}

// Нужная версия уже стоит - к мастеру не ходим, ничего не меняем.
func TestRun_AlreadyInstalled(t *testing.T) {
	m := &fakeMaster{blob: newBin}
	e := newEnv(t, []string{m.server(t).URL}, newBin)
	e.cfg.Version = "v3.5.16"
	res, err := Run(context.Background(), e.cfg)
	if err != nil || !res.Already {
		t.Fatalf("ждали «уже стоит», получили %+v, %v", res, err)
	}
	if m.hits != 0 {
		t.Fatal("ходили к мастеру за уже стоящей версией")
	}
	e.untouched(t)
}

// Мастер отдал другой файл того же размера - сумма не сходится, бинарь не тронут.
// Прошлый stack-apply убили после замены, до проверки: новая версия на диске, сервис
// не жив. Повторный запуск не отвечает «уже стоит», а перезапускает и проверяет;
// не поднялся - откат из .bak.
func TestRun_KilledMidReplace_RollsBack(t *testing.T) {
	broken := "telemt 3.5.17\nBROKEN"
	m := &fakeMaster{blob: broken}
	e := newEnv(t, []string{m.server(t).URL}, broken)
	if err := os.WriteFile(e.bin+".bak", []byte(oldBin), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.bin, []byte(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), e.cfg)
	if err == nil || res.Already || !strings.Contains(err.Error(), "прежний бинарь возвращён") {
		t.Fatalf("ждали откат, получили %+v, %v", res, err)
	}
	if got := e.read(t, e.bin); got != oldBin {
		t.Fatalf("после отката бинарь не прежний: %q", got)
	}
	if m.hits != 0 {
		t.Fatal("версия уже на диске - скачивать не нужно")
	}
	if e.sys.restarts != 2 {
		t.Fatalf("ждали restart с новым и с прежним бинарём, было %d", e.sys.restarts)
	}
}

// Версия на диске, сервис остановлен, но после restart живой - успех без скачивания и отката.
func TestRun_AlreadyButStopped_Restarts(t *testing.T) {
	m := &fakeMaster{blob: newBin}
	e := newEnv(t, []string{m.server(t).URL}, newBin)
	if err := os.WriteFile(e.bin, []byte(newBin), 0o755); err != nil {
		t.Fatal(err)
	}
	e.sys.down = true
	res, err := Run(context.Background(), e.cfg)
	if err != nil || res.Already || e.sys.restarts != 1 || m.hits != 0 {
		t.Fatalf("ждали restart без скачивания: %+v, %v, restarts %d, hits %d", res, err, e.sys.restarts, m.hits)
	}
	if got := e.read(t, e.bin); got != newBin {
		t.Fatalf("бинарь изменён: %q", got)
	}
}

// Общий дедлайн: зависший мастер не держит stack-apply дольше Deadline.
func TestRun_Deadline(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer hang.Close()
	e := newEnv(t, []string{hang.URL, hang.URL}, newBin)
	e.cfg.Deadline = 200 * time.Millisecond
	start := time.Now()
	_, err := Run(context.Background(), e.cfg)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("дедлайн не сработал: %v за %s", err, time.Since(start))
	}
	e.untouched(t)
}

func TestRun_WrongSHA(t *testing.T) {
	forged := newBin[:len(newBin)-1] + "Y"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(forged))
	}))
	defer srv.Close()
	e := newEnv(t, []string{srv.URL}, newBin)
	_, err := Run(context.Background(), e.cfg)
	if err == nil || !strings.Contains(err.Error(), "sha256 не сошёлся") {
		t.Fatalf("ждали ошибку суммы, получили %v", err)
	}
	e.untouched(t)
}

func TestRun_LargerThanSize(t *testing.T) {
	m := &fakeMaster{blob: newBin}
	e := newEnv(t, []string{m.server(t).URL}, newBin)
	e.cfg.Size = int64(len(newBin)) - 1
	_, err := Run(context.Background(), e.cfg)
	if err == nil || !strings.Contains(err.Error(), "больше заявленных") {
		t.Fatalf("ждали ошибку размера, получили %v", err)
	}
	e.untouched(t)

	// Без Content-Length (поток): лимит чтения всё равно срабатывает.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(newBin))
	}))
	defer srv.Close()
	e.cfg.Masters = []string{srv.URL}
	_, err = Run(context.Background(), e.cfg)
	if err == nil || !strings.Contains(err.Error(), "больше заявленных") {
		t.Fatalf("поток: ждали ошибку размера, получили %v", err)
	}
	e.untouched(t)
}

// Скачанный бинарь сообщает не ту версию - бинарь на машине не меняется.
func TestRun_VersionMismatch(t *testing.T) {
	m := &fakeMaster{blob: newBin}
	e := newEnv(t, []string{m.server(t).URL}, newBin)
	e.cfg.Version = "3.5.18"
	_, err := Run(context.Background(), e.cfg)
	if err == nil || !strings.Contains(err.Error(), "ждали 3.5.18") {
		t.Fatalf("ждали ошибку версии, получили %v", err)
	}
	e.untouched(t)
}

// Сервис с новым бинарём не поднялся - .bak на место, перезапуск, ошибка.
func TestRun_UnhealthyRollsBack(t *testing.T) {
	broken := "telemt 3.5.17\nBROKEN"
	m := &fakeMaster{blob: broken}
	e := newEnv(t, []string{m.server(t).URL}, broken)
	_, err := Run(context.Background(), e.cfg)
	if err == nil || !strings.Contains(err.Error(), "не поднялся за 30s") || !strings.Contains(err.Error(), "прежний бинарь возвращён") {
		t.Fatalf("ждали откат, получили %v", err)
	}
	if got := e.read(t, e.bin); got != oldBin {
		t.Fatalf("после отката бинарь не прежний: %q", got)
	}
	if fi, _ := os.Stat(e.bin); fi.Mode().Perm() != 0o750 {
		t.Fatalf("после отката права не прежние: %v", fi.Mode().Perm())
	}
	if e.sys.restarts != 2 {
		t.Fatalf("ждали перезапуск с новым и с прежним бинарём, было %d", e.sys.restarts)
	}
	e.noTemp(t)
}

// API не отвечает, хотя юнит active, - тоже откат.
func TestRun_APIDownRollsBack(t *testing.T) {
	m := &fakeMaster{blob: newBin}
	e := newEnv(t, []string{m.server(t).URL}, newBin)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	e.cfg.HealthURL = down.URL
	_, err := Run(context.Background(), e.cfg)
	if err == nil || !strings.Contains(err.Error(), "API: HTTP 503") {
		t.Fatalf("ждали откат из-за API, получили %v", err)
	}
	if got := e.read(t, e.bin); got != oldBin {
		t.Fatalf("после отката бинарь не прежний: %q", got)
	}
}

// Первый мастер недоступен, второй отвечает 404 (бинаря нет у реплики), третий отдаёт.
func TestRun_FallsBackToNextMaster(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	empty := &fakeMaster{blob: "другое"}
	m := &fakeMaster{blob: newBin}
	e := newEnv(t, []string{deadURL, empty.server(t).URL, m.server(t).URL}, newBin)
	res, err := Run(context.Background(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "3.5.16" || e.read(t, e.bin) != newBin || m.hits != 1 || empty.hits != 1 {
		t.Fatalf("не дошли до рабочего мастера: %+v, хиты %d/%d", res, empty.hits, m.hits)
	}
}

func TestRun_AllMastersFail(t *testing.T) {
	empty := &fakeMaster{blob: "другое"}
	e := newEnv(t, []string{empty.server(t).URL}, newBin)
	_, err := Run(context.Background(), e.cfg)
	if err == nil || !strings.Contains(err.Error(), "мастер ответил 404") {
		t.Fatalf("ждали ошибку с ответом мастера, получили %v", err)
	}
	e.untouched(t)
}

func TestRun_Invalid(t *testing.T) {
	e := newEnv(t, []string{"http://127.0.0.1:1"}, newBin)
	for name, mut := range map[string]func(*Config){
		"продукт": func(c *Config) { c.Product = "xray" },
		"sha":     func(c *Config) { c.SHA256 = "ABC" },
		"размер":  func(c *Config) { c.Size = 0 },
		"путь":    func(c *Config) { c.Bin = "telemt" },
	} {
		c := e.cfg
		mut(&c)
		if _, err := Run(context.Background(), c); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: ждали ErrInvalid, получили %v", name, err)
		}
	}
	e.untouched(t)
}

func TestDefaults(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	tg, _ := Defaults("telemt", get)
	if tg.Bin != "telemt" || tg.Unit != "telemt" || tg.HealthURL != "http://127.0.0.1:9091/v1/health" {
		t.Fatalf("telemt по умолчанию: %+v", tg)
	}
	mg, _ := Defaults("mtg", get)
	if mg.Bin != "mtg-multi" || mg.Unit != "mtg-multi" || mg.HealthURL != "http://127.0.0.1:8081/stats" {
		t.Fatalf("mtg по умолчанию: %+v", mg)
	}
	env["TELEMUX_AGENT_TELEMT_API"] = "http://127.0.0.1:9999/"
	env["TELEMUX_AGENT_MTG_BIN"] = "/opt/mtg"
	env["TELEMUX_AGENT_MTG_UNIT"] = "mtg"
	tg, _ = Defaults("telemt", get)
	mg, _ = Defaults("mtg", get)
	if tg.HealthURL != "http://127.0.0.1:9999/v1/health" || mg.Bin != "/opt/mtg" || mg.Unit != "mtg" {
		t.Fatalf("переопределение: %+v %+v", tg, mg)
	}
	if _, err := Defaults("xray", get); err == nil {
		t.Fatal("неизвестный продукт принят")
	}
}

func TestResolveBin(t *testing.T) {
	if p, err := ResolveBin("/usr/local/bin/telemt", nil); err != nil || p != "/usr/local/bin/telemt" {
		t.Fatalf("абсолютный путь: %q %v", p, err)
	}
	p, err := ResolveBin("mtg-multi", func(string) (string, error) { return "/usr/local/bin/mtg-multi", nil })
	if err != nil || p != "/usr/local/bin/mtg-multi" {
		t.Fatalf("через PATH: %q %v", p, err)
	}
	if _, err := ResolveBin("nope", func(string) (string, error) { return "", errors.New("нет") }); err == nil {
		t.Fatal("ненайденный бинарь принят")
	}
}
