package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

const mtgHead = `[network]
bind-to = "0.0.0.0:8767"

[stats]
api-bind-to = "127.0.0.1:8081"

`

func mtgCfg(head string, secrets ...string) string {
	var b strings.Builder
	b.WriteString(head)
	b.WriteString("[secrets]\n")
	for i := 0; i+1 < len(secrets); i += 2 {
		b.WriteString(`"` + secrets[i] + `" = "` + secrets[i+1] + `"` + "\n")
	}
	return b.String()
}

// Отпечаток секретов - как у mtg-multi: sha256 строк "имя=секрет" по имени через \n.
func TestMtgSecretsDigest_MatchesMtg(t *testing.T) {
	cfg := mtgCfg(mtgHead, "bob_2", "BBBB", "alice_1", "aaaa")
	sum := sha256.Sum256([]byte("alice_1=aaaa\nbob_2=bbbb"))
	if got := MtgSecretsDigest([]byte(cfg)); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("отпечаток не совпадает с mtg: %s", got)
	}
	if MtgSecretsDigest([]byte(mtgHead)) != hex.EncodeToString(func() []byte { s := sha256.Sum256(nil); return s[:] }()) {
		t.Fatal("без [secrets] отпечаток - от пустого набора")
	}
}

func TestOnlySecretsChanged(t *testing.T) {
	a := mtgCfg(mtgHead, "alice_1", "aaaa")
	if !onlySecretsChanged(a, mtgCfg(mtgHead, "alice_1", "aaaa", "bob_2", "bbbb")) {
		t.Fatal("изменились только секреты")
	}
	if onlySecretsChanged(a, mtgCfg(strings.Replace(mtgHead, "8767", "8768", 1), "alice_1", "aaaa")) {
		t.Fatal("изменился порт - нужен рестарт")
	}
}

// fakeMtg - эмулятор mtg-multi под systemd: /stats отдаёт отпечаток того конфига,
// который процесс прочитал при последнем старте или SIGHUP.
type fakeMtg struct {
	mu        sync.Mutex
	file      string
	digest    string
	active    bool
	hups      int
	restarts  int
	hupWorks  bool                  // SIGHUP подхватывает секреты
	startable func(cfg string) bool // поднимется ли процесс на конфиге
	srv       *httptest.Server
}

func newFakeMtg(t *testing.T, file string) *fakeMtg {
	f := &fakeMtg{file: file, active: true, hupWorks: true, startable: func(string) bool { return true }}
	cur, _ := os.ReadFile(file)
	f.digest = MtgSecretsDigest(cur)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.active {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"secrets_sha256": f.digest})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMtg) HUP(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hups++
	if f.hupWorks {
		cur, _ := os.ReadFile(f.file)
		f.digest = MtgSecretsDigest(cur)
	}
	return nil
}

func (f *fakeMtg) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	cur, _ := os.ReadFile(f.file)
	f.active = f.startable(string(cur))
	if f.active {
		f.digest = MtgSecretsDigest(cur)
	}
	return nil
}

func (f *fakeMtg) Active(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

func setupMtg(t *testing.T, initial string) (string, *fakeMtg, *MtgApplier) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(file, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	fm := newFakeMtg(t, file)
	ap := &MtgApplier{
		ConfigFile: file, StatsURL: fm.srv.URL, Service: fm, HotReload: true,
		ConfirmTimeout: 300 * time.Millisecond, PollEvery: 20 * time.Millisecond,
		FreeSpace: func(string) (uint64, error) { return 1 << 30, nil },
	}
	return file, fm, ap
}

// Изменились только секреты - SIGHUP без рестарта.
func TestMtgApply_SecretsOnlyHotReload(t *testing.T) {
	file, fm, ap := setupMtg(t, mtgCfg(mtgHead, "alice_1", "aaaa"))
	next := mtgCfg(mtgHead, "alice_1", "aaaa", "bob_2", "bbbb")
	res := ap.Apply(context.Background(), []byte(next))
	if res.Status != "applied" || fm.hups != 1 || fm.restarts != 0 {
		t.Fatalf("ждали applied через SIGHUP без рестарта: %+v, hup=%d restart=%d", res, fm.hups, fm.restarts)
	}
	if readFile(t, file) != next {
		t.Fatal("конфиг не заменён")
	}
}

// Изменилось что-то кроме секретов - рестарт.
func TestMtgApply_OtherChangeRestarts(t *testing.T) {
	_, fm, ap := setupMtg(t, mtgCfg(mtgHead, "alice_1", "aaaa"))
	res := ap.Apply(context.Background(), []byte(mtgCfg(strings.Replace(mtgHead, "8767", "8768", 1), "alice_1", "aaaa")))
	if res.Status != "applied" || fm.hups != 0 || fm.restarts != 1 {
		t.Fatalf("ждали applied через рестарт: %+v, hup=%d restart=%d", res, fm.hups, fm.restarts)
	}
}

// SIGHUP не подхватил секреты - откат к рестарту.
func TestMtgApply_HotReloadFailsFallsBackToRestart(t *testing.T) {
	_, fm, ap := setupMtg(t, mtgCfg(mtgHead, "alice_1", "aaaa"))
	fm.hupWorks = false
	res := ap.Apply(context.Background(), []byte(mtgCfg(mtgHead, "bob_2", "bbbb")))
	if res.Status != "applied" || fm.hups != 1 || fm.restarts != 1 {
		t.Fatalf("ждали SIGHUP, затем рестарт: %+v, hup=%d restart=%d", res, fm.hups, fm.restarts)
	}
}

// Без hot reload - сразу рестарт.
func TestMtgApply_HotReloadDisabled(t *testing.T) {
	_, fm, ap := setupMtg(t, mtgCfg(mtgHead, "alice_1", "aaaa"))
	ap.HotReload = false
	if res := ap.Apply(context.Background(), []byte(mtgCfg(mtgHead, "bob_2", "bbbb"))); res.Status != "applied" || fm.hups != 0 || fm.restarts != 1 {
		t.Fatalf("без hot reload ждали только рестарт: %+v", res)
	}
}

// mtg не поднялся на новом конфиге - прежний возвращается, mtg снова запущен.
func TestMtgApply_RestartFailsRollsBack(t *testing.T) {
	old := mtgCfg(mtgHead, "alice_1", "aaaa")
	file, fm, ap := setupMtg(t, old)
	fm.startable = func(cfg string) bool { return !strings.Contains(cfg, "9999") }
	res := ap.Apply(context.Background(), []byte(mtgCfg(strings.Replace(mtgHead, "8767", "9999", 1), "alice_1", "aaaa")))
	if res.Status != "failed" || !strings.Contains(res.Error, "возвращ") {
		t.Fatalf("ждали failed с откатом, получили %+v", res)
	}
	if readFile(t, file) != old || !fm.Active(context.Background()) {
		t.Fatal("после отказа прежний конфиг не возвращён или mtg не поднят")
	}
}

// Уже совпадает - ничего не делаем.
func TestMtgApply_InSync(t *testing.T) {
	cfg := mtgCfg(mtgHead, "alice_1", "aaaa")
	_, fm, ap := setupMtg(t, cfg)
	if res := ap.Apply(context.Background(), []byte(cfg)); res.Status != "applied" || fm.hups+fm.restarts != 0 {
		t.Fatalf("совпадающее состояние трогать нельзя: %+v", res)
	}
}

// Нет места - ничего не пишется.
func TestMtgApply_NoSpace(t *testing.T) {
	old := mtgCfg(mtgHead, "alice_1", "aaaa")
	file, fm, ap := setupMtg(t, old)
	ap.FreeSpace = func(string) (uint64, error) { return 10, nil }
	if res := ap.Apply(context.Background(), []byte(mtgCfg(mtgHead, "bob_2", "bbbb"))); res.Status != "failed" || readFile(t, file) != old || fm.hups+fm.restarts != 0 {
		t.Fatalf("при нехватке места трогать нельзя: %+v", res)
	}
}

// Пустой набор секретов поверх непустого не применяем.
func TestMtgApply_RefusesEmptySecrets(t *testing.T) {
	_, fm, ap := setupMtg(t, mtgCfg(mtgHead, "alice_1", "aaaa"))
	if res := ap.Apply(context.Background(), []byte(mtgCfg(mtgHead))); res.Status != "failed" || fm.hups+fm.restarts != 0 {
		t.Fatalf("пустой набор поверх непустого применять нельзя: %+v", res)
	}
}

// Машина с telemt и mtg: оба ресурса из одного бандла, один отчёт на оба.
func TestAgent_BothResources(t *testing.T) {
	dir, _, ap := setupMachine(t)
	mtgFile, fm, mtgAp := setupMtg(t, mtgCfg(mtgHead, "alice_1", "aaaa"))
	users := render(telemtcfg.RenderUser{Key: "bob_2", Secret: sec2})
	mtgNext := mtgCfg(mtgHead, "bob_2", "bbbb")
	m := &fakeMaster{bundle: store.Bundle{Target: "pr2", Revision: 9, Resources: []store.Desired{
		{Resource: store.ResourceTelemtUsers, Revision: 8, SHA256: "u", Content: users},
		{Resource: store.ResourceMtgConfig, Revision: 9, SHA256: "m", Content: []byte(mtgNext)},
	}}}
	srv := httptest.NewServer(m.handler(t, "tok", "pr2"))
	defer srv.Close()

	// Сначала режим сравнения: ничего не меняется, расхождение mtg - в отчёте.
	cmp, err := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2",
		UsersFile: filepath.Join(dir, "users.toml"), MtgFile: mtgFile, Mode: ModeCompare, Wait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cmp.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep := m.reports[len(m.reports)-1]
	if len(rep.Resources) != 2 {
		t.Fatalf("ждали отчёт по двум ресурсам, получили %+v", rep)
	}
	for _, r := range rep.Resources {
		if r.Status != "compared" || r.Error == "" {
			t.Fatalf("оба ресурса отличаются - ждали compared с описанием: %+v", r)
		}
	}
	if fm.hups+fm.restarts != 0 || readFile(t, mtgFile) == mtgNext {
		t.Fatal("режим сравнения тронул mtg")
	}

	// Режим применения: оба ресурса применены.
	app, err := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2",
		UsersFile: filepath.Join(dir, "users.toml"), MtgFile: mtgFile, Mode: ModeApply,
		Applier: ap, MtgApplier: mtgAp, Wait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep = m.reports[len(m.reports)-1]
	for _, r := range rep.Resources {
		if r.Status != "applied" {
			t.Fatalf("ждали applied по обоим ресурсам: %+v", rep.Resources)
		}
	}
	if readFile(t, mtgFile) != mtgNext || readFile(t, filepath.Join(dir, "users.toml")) != string(users) {
		t.Fatal("файлы не применены")
	}
}

// Машина только с mtg (без telemt) - допустима.
func TestNew_MtgOnlyMachine(t *testing.T) {
	if _, err := New(Config{MasterURLs: []string{"http://m"}, Token: "t", Target: "ps5", MtgFile: "/etc/mtg-multi/config.toml", Mode: ModeCompare}); err != nil {
		t.Fatalf("машина только с mtg отвергнута: %v", err)
	}
	if _, err := New(Config{MasterURLs: []string{"http://m"}, Token: "t", Target: "ps5", MtgFile: "/x", Mode: ModeApply}); err == nil {
		t.Fatal("apply без MtgApplier принят")
	}
}

// Обновление одного экземпляра не затрагивает конфиг и процесс его соседа.
func TestAgent_SecondaryMtgIsolation(t *testing.T) {
	initial := mtgCfg(mtgHead, "alice_1", "aaaa")
	mainFile, mainService, mainApply := setupMtg(t, initial)
	secondFile, secondService, secondApply := setupMtg(t, initial)
	next := mtgCfg(mtgHead, "bob_2", "bbbb")
	m := &fakeMaster{bundle: store.Bundle{Target: "relay", Revision: 12, Resources: []store.Desired{
		{Resource: "mtg.config", Revision: 8, SHA256: "a", Content: []byte(initial)},
		{Resource: "mtg.secondary.config", Revision: 12, SHA256: "b", Content: []byte(next)},
	}}}
	srv := httptest.NewServer(m.handler(t, "tok", "relay"))
	defer srv.Close()
	cfg := Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "relay", Mode: ModeApply,
		MtgFile: mainFile, MtgApplier: mainApply, MtgSecondaryFile: secondFile, MtgSecondaryApplier: secondApply}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	report := m.reports[len(m.reports)-1].Resources
	if len(report) != 2 || report[1].Resource != "mtg.secondary.config" || report[1].Revision != 12 || report[1].Status != "applied" {
		t.Fatalf("нет отдельного подтверждения второго процесса: %+v", report)
	}
	if readFile(t, mainFile) != initial || readFile(t, secondFile) != next || mainService.hups+mainService.restarts != 0 || secondService.hups != 1 {
		t.Fatal("обновление второго экземпляра должно затронуть только его файл и процесс")
	}
}

func TestSecondaryMtgRejectsSameFile(t *testing.T) {
	main, _, _ := setupMtg(t, mtgCfg(mtgHead))
	link := filepath.Join(t.TempDir(), "linked.toml")
	if err := os.Link(main, link); err != nil {
		t.Fatal(err)
	}
	if err := validateMtgFiles(main, link); err == nil {
		t.Fatal("два имени одного файла приняты")
	}
}

// Совпадения файла недостаточно для передачи доставки от бота агенту.
func TestSecondaryMtgRequiresRuntimeConfirmation(t *testing.T) {
	initial := mtgCfg(mtgHead, "alice_1", "aaaa")
	file, service, apply := setupMtg(t, initial)
	service.digest = "устаревший набор"
	service.hupWorks = false
	service.startable = func(string) bool { return false }
	mainFile, _, mainApply := setupMtg(t, initial)
	cfg := Config{MasterURLs: []string{"http://master"}, Target: "relay", Token: "token", Mode: ModeApply, MtgFile: mainFile, MtgApplier: mainApply,
		MtgSecondaryFile: file, MtgSecondaryApplier: apply}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res := a.apply(context.Background(), &store.Desired{Resource: "mtg.secondary.config", Revision: 3, Content: []byte(initial)})
	if res.Status != "failed" {
		t.Fatal("устаревший процесс получил applied за совпадение файла")
	}
	service.hupWorks = true
	res = a.apply(context.Background(), &store.Desired{Resource: "mtg.secondary.config", Revision: 3, Content: []byte(initial)})
	if res.Status != "applied" {
		t.Fatal("устранённый сбой /stats должен восстановиться на прежней ревизии")
	}
	if readFile(t, mainFile) != initial {
		t.Fatal("неожиданно изменён конфиг")
	}
}

func TestSecondaryMtgMissingLocalSettingsReported(t *testing.T) {
	m := &fakeMaster{bundle: store.Bundle{Target: "pr1", Revision: 9, Resources: []store.Desired{
		{Resource: "mtg.secondary.config", Revision: 9, Content: []byte("[secrets]\n")},
	}}}
	srv := httptest.NewServer(m.handler(t, "tok", "pr1"))
	defer srv.Close()
	a, err := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr1", Mode: ModeApply})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := m.reports[len(m.reports)-1].Resources
	if len(got) != 1 || got[0].Status != "failed" || got[0].Error == "" {
		t.Fatalf("мастер не видит пропавшие настройки второго процесса: %+v", got)
	}
}
