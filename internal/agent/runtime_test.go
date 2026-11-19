package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// --- фикстуры ---

// fakeRunner - внешние команды по имени: ответ и счётчик вызовов.
type fakeRunner struct {
	mu    sync.Mutex
	out   map[string]string
	fail  map[string]bool
	calls []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if f.fail[name] {
		return nil, errors.New(name + ": упало")
	}
	return []byte(f.out[name]), nil
}

func (f *fakeRunner) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type fakeFileInfo struct {
	mtime time.Time
	size  int64
}

func (f fakeFileInfo) Name() string       { return "bin" }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0o755 }
func (f fakeFileInfo) ModTime() time.Time { return f.mtime }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

// fakeFS - файлы по пути (/proc, users.toml, telemt.toml) и mtime бинарей.
type fakeFS struct {
	mu    sync.Mutex
	files map[string]string
	mtime map[string]time.Time
}

func (f *fakeFS) read(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(v), nil
}

func (f *fakeFS) set(p, v string) {
	f.mu.Lock()
	f.files[p] = v
	f.mu.Unlock()
}

func (f *fakeFS) stat(p string) (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.mtime[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return fakeFileInfo{mtime: m, size: 100}, nil
}

const usersTomlFixture = `[access.users]
placeholder = "00000000000000000000000000000000"
a_1 = "11111111111111111111111111111111"
b_2 = "22222222222222222222222222222222"
c_3 = "33333333333333333333333333333333"

[access.user_max_tcp_conns]
placeholder = 1
a_1 = 10
b_2 = 30
c_3 = 50

[access.user_expirations]
a_1 = "2026-01-01T00:00:00.000Z"
b_2 = "2027-01-01T00:00:00Z"
`

const telemtTomlFixture = `[general]
client_keepalive = 30
client_ack = 90

[censorship]
tls_domain = "vk.com"
tls_domains = ["a.example"]
`

func manyIPs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("10.0.0.%d", i+1)
	}
	return out
}

// telemtAPI - эмулятор API telemt: /v1/users и /v1/stats/users/active-ips в конверте.
func telemtAPI(t *testing.T, down *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down != nil && *down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var data any
		switch r.URL.Path {
		case "/v1/users":
			data = []map[string]any{
				{"username": "placeholder", "current_connections": 9},
				{"username": "a_1", "current_connections": 2, "expiration_rfc3339": "2026-01-01T00:00:00Z"},
				{"username": "b_2", "current_connections": 0},
				{"username": "c_3", "current_connections": 5},
			}
		case "/v1/stats/users/active-ips":
			data = []map[string]any{
				{"username": "placeholder", "active_ips": []string{"9.9.9.9"}},
				{"username": "a_1", "active_ips": []string{"1.2.3.4"}},
				{"username": "b_2", "active_ips": []string{}},
				{"username": "c_3", "active_ips": manyIPs(25)},
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data, "revision": "r"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

const mtgStatsNew = `{"total_connections":7,"secrets_sha256":"x","users":{
 "vasya_1":{"connections":5,"bytes_in":1024,"bytes_out":8192,"last_seen":"2026-11-19T10:00:00Z","active_ips":["203.0.113.15","95.0.0.1"]},
 "petya_2":{"connections":2,"bytes_in":512,"bytes_out":2048,"last_seen":"2026-11-19T09:59:00Z"},
 "idle_3":{"connections":0,"bytes_in":1,"bytes_out":1,"last_seen":null}}}`

func newCollector(t *testing.T, telemtURL, mtgURL string) (*RuntimeCollector, *fakeRunner, *fakeFS, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	ffs := &fakeFS{files: map[string]string{
		"/etc/telemt/users.toml":  usersTomlFixture,
		"/etc/telemt/telemt.toml": telemtTomlFixture,
	}, mtime: map[string]time.Time{"/usr/bin/telemt": now, "/usr/bin/mtg": now}}
	fr := &fakeRunner{out: map[string]string{
		"/usr/bin/telemt": "telemt 3.5.17\n",
		"/usr/bin/mtg":    "mtg-multi 2510861 (go1.24)\n",
	}, fail: map[string]bool{}}
	c := &RuntimeCollector{
		Telemt: true, Mtg: true, TelemtAPI: telemtURL, MtgStatsURL: mtgURL,
		UsersFile: "/etc/telemt/users.toml", CPUSampleGap: time.Millisecond,
		Runner: fr, ReadFile: ffs.read, Stat: ffs.stat,
		LookPath:  func(f string) (string, error) { return "/usr/bin/" + f, nil },
		DiskUsage: func(string) (float64, error) { return 55.04, nil },
		Now:       func() time.Time { return now },
	}
	return c, fr, ffs, &now
}

func mtgServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	t.Cleanup(srv.Close)
	return srv
}

// --- telemt ---

func TestRuntime_TelemtFixture(t *testing.T) {
	c, fr, ffs, _ := newCollector(t, telemtAPI(t, nil).URL, mtgServer(t, mtgStatsNew).URL)
	rep := c.Collect(context.Background(), time.Minute)
	tm := rep.Telemt
	if tm == nil || !tm.APIOK || tm.Error != "" {
		t.Fatalf("telemt: %+v", tm)
	}
	if tm.Version != "3.5.17" {
		t.Fatalf("версия: %q", tm.Version)
	}
	// placeholder не считается; a_1 истёк по users.toml.
	if tm.UsersConfigured != 3 || tm.UsersEffective != 2 || tm.ConnectionsTotal != 7 {
		t.Fatalf("счётчики: configured=%d effective=%d total=%d", tm.UsersConfigured, tm.UsersEffective, tm.ConnectionsTotal)
	}
	if tm.Keepalive != 30 || tm.Ack != 90 || tm.TLSDomain != "vk.com" || tm.MaxTCPConnsMedian != 30 || len(tm.ConfigSHA256) != 64 {
		t.Fatalf("сводка конфига: %+v", tm)
	}
	// Только пользователи в сети, адресов не больше 20.
	if len(tm.Users) != 2 || tm.Users[0].Key != "a_1" || tm.Users[0].Connections != 2 ||
		len(tm.Users[0].ActiveIPs) != 1 || tm.Users[1].Key != "c_3" || len(tm.Users[1].ActiveIPs) != 20 {
		t.Fatalf("пользователи: %+v", tm.Users)
	}
	// Версия кешируется по mtime бинаря: второй отчёт не запускает --version.
	c.Collect(context.Background(), time.Minute)
	if n := fr.count("/usr/bin/telemt --version"); n != 1 {
		t.Fatalf("--version при неизменном бинаре: ждали 1 вызов, было %d", n)
	}
	ffs.mu.Lock()
	ffs.mtime["/usr/bin/telemt"] = ffs.mtime["/usr/bin/telemt"].Add(time.Hour)
	ffs.mu.Unlock()
	fr.out["/usr/bin/telemt"] = "telemt 3.5.18\n"
	if v := c.Collect(context.Background(), time.Minute).Telemt.Version; v != "3.5.18" || fr.count("/usr/bin/telemt --version") != 2 {
		t.Fatalf("бинарь заменён: ждали 3.5.18 и второй вызов, получили %q", v)
	}
}

// API telemt лежит: api_ok=false и причина, остальной отчёт (host, mtg, сводка конфига) уходит.
func TestRuntime_TelemtDownDoesNotDropReport(t *testing.T) {
	down := true
	c, _, ffs, _ := newCollector(t, telemtAPI(t, &down).URL, mtgServer(t, mtgStatsNew).URL)
	ffs.set("/proc/meminfo", "MemTotal: 1000 kB\nMemAvailable: 250 kB\n")
	rep := c.Collect(context.Background(), time.Minute)
	if rep.Telemt == nil || rep.Telemt.APIOK || !strings.Contains(rep.Telemt.Error, "502") {
		t.Fatalf("ждали api_ok=false с причиной: %+v", rep.Telemt)
	}
	if len(rep.Telemt.Users) != 0 || rep.Telemt.Keepalive != 30 || rep.Telemt.MaxTCPConnsMedian != 30 {
		t.Fatalf("сводка конфига должна прийти и без API: %+v", rep.Telemt)
	}
	if rep.Host == nil || rep.Host.MemPct != 75 || rep.Host.DiskPct != 55 {
		t.Fatalf("host: %+v", rep.Host)
	}
	if rep.Mtg == nil || !rep.Mtg.APIOK || rep.Mtg.ConnectionsTotal != 7 {
		t.Fatalf("mtg должен прийти: %+v", rep.Mtg)
	}
}

// Нет telemt и mtg на машине - в отчёте null, журнал не читается.
func TestRuntime_NoProducts(t *testing.T) {
	c, fr, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	rep := c.Collect(context.Background(), time.Minute)
	if rep.Telemt != nil || rep.Mtg != nil || rep.LogErrors != nil || rep.Host == nil {
		t.Fatalf("ждали только host: %+v", rep)
	}
	if fr.count("journalctl") != 0 {
		t.Fatal("журнал telemt без telemt читать не нужно")
	}
}

// --- mtg ---

func TestParseMtgStats(t *testing.T) {
	total, users, err := ParseMtgStats([]byte(mtgStatsNew))
	if err != nil || total != 7 || len(users) != 2 {
		t.Fatalf("total=%d users=%+v err=%v", total, users, err)
	}
	p, v := users[0], users[1] // по ключу: petya_2, vasya_1
	if p.Key != "petya_2" || p.ActiveIPs == nil || len(p.ActiveIPs) != 0 {
		t.Fatalf("старая сборка без active_ips → пустой список, получили %+v", p)
	}
	if v.Key != "vasya_1" || v.Connections != 5 || v.BytesIn != 1024 || v.BytesOut != 8192 ||
		v.LastSeen == nil || !v.LastSeen.Equal(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)) || len(v.ActiveIPs) != 2 {
		t.Fatalf("vasya_1: %+v", v)
	}
	// Старый mtg целиком: без active_ips и без total_connections.
	_, old, err := ParseMtgStats([]byte(`{"users":{"x_1":{"connections":1}}}`))
	if err != nil || len(old) != 1 || old[0].ActiveIPs == nil {
		t.Fatalf("старый формат: %+v %v", old, err)
	}
	if _, _, err := ParseMtgStats([]byte(`<html>`)); err == nil {
		t.Fatal("мусор вместо JSON - ошибка")
	}
}

func TestRuntime_MtgFixture(t *testing.T) {
	c, _, _, _ := newCollector(t, telemtAPI(t, nil).URL, mtgServer(t, mtgStatsNew).URL)
	m := c.Collect(context.Background(), time.Minute).Mtg
	if m == nil || !m.APIOK || m.Version != "mtg-multi 2510861 (go1.24)" || m.ConnectionsTotal != 7 || len(m.Users) != 2 {
		t.Fatalf("mtg: %+v", m)
	}
	c2, _, _, _ := newCollector(t, telemtAPI(t, nil).URL, mtgServer(t, `oops`).URL)
	if m := c2.Collect(context.Background(), time.Minute).Mtg; m.APIOK || m.Error == "" {
		t.Fatalf("битый /stats → api_ok=false: %+v", m)
	}
}

func TestParseVersion(t *testing.T) {
	cases := map[string]string{
		"telemt 3.5.17\n":           "3.5.17",
		"telemt v3.5.16-rc1 (abc)":  "v3.5.16-rc1",
		"mtg-multi 2510861":         "mtg-multi 2510861",
		"":                          "",
		"\n  1.2\nsecond line 9.9 ": "1.2",
	}
	for in, want := range cases {
		if got := ParseVersion([]byte(in)); got != want {
			t.Fatalf("%q → ждали %q, получили %q", in, want, got)
		}
	}
}

// --- хост ---

const statA = "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 1 1 1 1 1 1 1\n"
const statB = "cpu  160 0 140 780 120 0 0 0 0 0\ncpu0 1 1 1 1 1 1 1 1\n"

func TestCPUPercent_TwoSamples(t *testing.T) {
	a, err := ParseCPUStat([]byte(statA))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ParseCPUStat([]byte(statB))
	// Прирост total 200, из них idle+iowait 100 → 50%.
	if got := CPUPercent(a, b); got != 50 {
		t.Fatalf("ждали 50%%, получили %v", got)
	}
	if CPUPercent(b, a) != 0 {
		t.Fatal("счётчики назад (перезагрузка) → 0")
	}
	if _, err := ParseCPUStat([]byte("intr 1 2 3")); err == nil {
		t.Fatal("без строки cpu - ошибка")
	}
}

func TestRuntime_HostBetweenReports(t *testing.T) {
	c, _, ffs, now := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	ffs.set("/proc/stat", statA)
	ffs.set("/proc/loadavg", "0.31 0.2 0.1 1/100 123\n")
	ffs.set("/proc/uptime", "12345.67 999.0\n")
	ffs.set("/proc/net/route", "Iface\tDestination\tGateway\neth0\t0000FEA9\t00000000\nens3\t00000000\t0101A8C0\n")
	ffs.set("/proc/net/dev", "Inter-|   Receive\n face |bytes packets\n    lo: 5 0 0 0 0 0 0 0 5 0 0 0 0 0 0 0\n  ens3: 1000000 10 0 0 0 0 0 0 2000000 20 0 0 0 0 0 0\n")
	first := c.Collect(context.Background(), time.Minute).Host
	if first.CPUPct != 0 || first.Load1 != 0.31 || first.UptimeS != 12345 || first.NetRxMbps != 0 {
		t.Fatalf("первый отчёт (сети ещё не с чем сравнить): %+v", first)
	}
	// Через 10 с: CPU по разнице с прошлым отчётом, сеть - +12.5 МБ и +25 МБ.
	*now = now.Add(10 * time.Second)
	ffs.set("/proc/stat", statB)
	ffs.set("/proc/net/dev", "  ens3: 13500000 10 0 0 0 0 0 0 27000000 20 0 0 0 0 0 0\n")
	second := c.Collect(context.Background(), time.Minute).Host
	if second.CPUPct != 50 || second.NetRxMbps != 10 || second.NetTxMbps != 20 {
		t.Fatalf("второй отчёт: %+v", second)
	}
	// Сброс счётчика интерфейса - 0, а не огромное число.
	*now = now.Add(10 * time.Second)
	ffs.set("/proc/net/dev", "  ens3: 5 10 0 0 0 0 0 0 5 20 0 0 0 0 0 0\n")
	if h := c.Collect(context.Background(), time.Minute).Host; h.NetRxMbps != 0 || h.NetTxMbps != 0 {
		t.Fatalf("сброс счётчика: %+v", h)
	}
}

func TestParseMemInfo(t *testing.T) {
	if v, err := ParseMemInfo([]byte("MemTotal: 2000 kB\nMemFree: 100 kB\nMemAvailable: 500 kB\n")); err != nil || v != 75 {
		t.Fatalf("ждали 75, получили %v %v", v, err)
	}
	if _, err := ParseMemInfo([]byte("MemTotal: 2000 kB\n")); err == nil {
		t.Fatal("без MemAvailable - ошибка")
	}
}

// --- журнал ---

func TestClassifyLogLine(t *testing.T) {
	cases := []struct{ line, kind, key string }{
		{"WARN user=andrey_1135 connection limit exceeded", "limit", "andrey_1135"},
		{`WARN handshake failed peer=1.2.3.4 user="tg_408715770_12"`, "handshake", "tg_408715770_12"},
		{"ERROR auth failed: bad secret", "auth", ""},
		{"WARN user=sub_5 upstream timed out", "timeout", "sub_5"},
		{"WARN user=sub_5 read timeout", "timeout", "sub_5"},
		{"INFO handshake ok user=a_1", "", ""},
		{"INFO config reloaded", "", ""},
	}
	for _, c := range cases {
		kind, key, ok := ClassifyLogLine(c.line)
		if c.kind == "" {
			if ok {
				t.Fatalf("%q - не ошибка подключения, получили %s", c.line, kind)
			}
			continue
		}
		if !ok || kind != c.kind || key != c.key {
			t.Fatalf("%q → ждали %s/%q, получили %s/%q ok=%v", c.line, c.kind, c.key, kind, key, ok)
		}
	}
	items := CountLogErrors([]byte("user=a_1 limit exceeded\nuser=a_1 limit exceeded\nhandshake closed\nnoise\n"))
	if len(items) != 2 || items[0] != (store.LogErrorItem{Key: "a_1", Kind: "limit", Count: 2}) ||
		items[1] != (store.LogErrorItem{Key: "", Kind: "handshake", Count: 1}) {
		t.Fatalf("счётчики: %+v", items)
	}
}

// Окно журнала: первое - период отчётов назад, следующее - с конца прошлого
// принятого; сбой journalctl окно не сдвигает.
func TestRuntime_JournalWindow(t *testing.T) {
	c, fr, _, now := newCollector(t, telemtAPI(t, nil).URL, mtgServer(t, mtgStatsNew).URL)
	fr.out["journalctl"] = "user=a_1 limit exceeded\n"
	t0 := *now
	le := c.Collect(context.Background(), time.Minute).LogErrors
	if le == nil || !le.PeriodStart.Equal(t0.Add(-time.Minute)) || !le.PeriodEnd.Equal(t0) || len(le.Items) != 1 {
		t.Fatalf("первое окно: %+v", le)
	}
	want := fmt.Sprintf("journalctl -u telemt --since @%d --until @%d -o cat --no-pager", t0.Add(-time.Minute).Unix(), t0.Unix())
	if fr.calls[len(fr.calls)-1] != want && !strings.Contains(strings.Join(fr.calls, "\n"), want) {
		t.Fatalf("команда журнала: ждали %q, были %v", want, fr.calls)
	}
	c.CommitLogWindow(le) // мастер принял отчёт
	*now = now.Add(time.Minute)
	fr.fail["journalctl"] = true
	if le := c.Collect(context.Background(), time.Minute).LogErrors; le != nil {
		t.Fatalf("сбой журнала → log_errors null, получили %+v", le)
	}
	*now = now.Add(time.Minute)
	fr.fail["journalctl"] = false
	le = c.Collect(context.Background(), time.Minute).LogErrors
	if le == nil || !le.PeriodStart.Equal(t0) || !le.PeriodEnd.Equal(*now) {
		t.Fatalf("после сбоя окно должно начаться с конца последнего удачного: %+v", le)
	}
}

// --- отправка ---

func TestSendRuntime_FallbackAndTarget(t *testing.T) {
	var mu sync.Mutex
	var got []store.RuntimeReport
	var auth string
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var rep store.RuntimeReport
		_ = json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		got, auth = append(got, rep), r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	c, _, _, _ := newCollector(t, telemtAPI(t, nil).URL, mtgServer(t, mtgStatsNew).URL)
	a, err := New(Config{MasterURLs: []string{down.URL, up.URL}, Token: "tok", Target: "pr2", Version: "v9",
		Mode: ModeCompare, Runtime: c, RuntimeEvery: time.Minute, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SendRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Target != "pr2" || got[0].AgentVersion != "v9" || auth != "Bearer tok" ||
		got[0].Telemt == nil || got[0].Mtg == nil || got[0].Host == nil {
		t.Fatalf("отчёт: %+v auth=%q", got, auth)
	}
}

// 4xx от мастера - отчёт отвергнут, другие адреса не перебираются.
func TestSendRuntime_RejectedStops(t *testing.T) {
	var hits int
	var mu sync.Mutex
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	})
	m1, m2 := httptest.NewServer(h), httptest.NewServer(h)
	defer m1.Close()
	defer m2.Close()
	c, _, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	a, _ := New(Config{MasterURLs: []string{m1.URL, m2.URL}, Token: "tok", Target: "pr2", Mode: ModeCompare,
		Runtime: c, RuntimeEvery: time.Minute, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := a.SendRuntime(context.Background()); err == nil || hits != 1 {
		t.Fatalf("ждали ошибку после первого 400, hits=%d err=%v", hits, err)
	}
}

// Runtime-отчёт идёт своим тикером: long-poll висит, а отчёты приходят.
func TestRun_RuntimeNotBlockedByLongPoll(t *testing.T) {
	var mu sync.Mutex
	runtimeHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/desired":
			<-r.Context().Done() // long-poll не отвечает
		case "/v1/runtime":
			mu.Lock()
			runtimeHits++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c, _, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	a, _ := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2", Mode: ModeCompare,
		Runtime: c, RuntimeEvery: 30 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	a.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if runtimeHits < 2 {
		t.Fatalf("при висящем long-poll ждали несколько runtime-отчётов, было %d", runtimeHits)
	}
}

// --- доработки после ревью ---

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Версия mtg-multi на релеях - вида 1.15.0-webalias: суффикс сборки сохраняется.
func TestParseVersion_MtgWebalias(t *testing.T) {
	for in, want := range map[string]string{
		"1.15.0-webalias\n":                                    "1.15.0-webalias",
		"mtg-multi 1.15.0-webalias":                            "1.15.0-webalias",
		"mtg 1.15.0-webalias (go1.24.1) abcdef":                "1.15.0-webalias",
		"mtg-multi version v1.15.0-webalias, built 2026-11-01": "v1.15.0-webalias",
	} {
		if got := ParseVersion([]byte(in)); got != want {
			t.Fatalf("%q → ждали %q, получили %q", in, want, got)
		}
	}
}

// Неудача --version тоже кешируется по (mtime, size): битый бинарь не
// запускается каждую минуту. Замена бинаря - повторная попытка.
func TestRuntime_VersionFailureCached(t *testing.T) {
	c, fr, ffs, _ := newCollector(t, telemtAPI(t, nil).URL, mtgServer(t, mtgStatsNew).URL)
	fr.fail["/usr/bin/mtg"] = true
	for i := 0; i < 3; i++ {
		if v := c.Collect(context.Background(), time.Minute).Mtg.Version; v != "" {
			t.Fatalf("--version упал → версия пустая, получили %q", v)
		}
	}
	if n := fr.count("/usr/bin/mtg --version"); n != 1 {
		t.Fatalf("неудача должна кешироваться: ждали 1 вызов, было %d", n)
	}
	ffs.mu.Lock()
	ffs.mtime["/usr/bin/mtg"] = ffs.mtime["/usr/bin/mtg"].Add(time.Minute)
	ffs.mu.Unlock()
	fr.mu.Lock()
	fr.fail["/usr/bin/mtg"] = false
	fr.out["/usr/bin/mtg"] = "mtg-multi 1.15.0-webalias\n"
	fr.mu.Unlock()
	if v := c.Collect(context.Background(), time.Minute).Mtg.Version; v != "1.15.0-webalias" || fr.count("/usr/bin/mtg --version") != 2 {
		t.Fatalf("бинарь заменён: ждали новую попытку и 1.15.0-webalias, получили %q", v)
	}
}

// Сбой journalctl не глотается: предупреждение в лог и строка в telemt.error.
func TestRuntime_JournalFailureReported(t *testing.T) {
	c, fr, _, _ := newCollector(t, telemtAPI(t, nil).URL, mtgServer(t, mtgStatsNew).URL)
	var logBuf strings.Builder
	var logMu sync.Mutex
	c.Log = slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logBuf.Write(p)
	}), nil))
	fr.fail["journalctl"] = true
	rep := c.Collect(context.Background(), time.Minute)
	if rep.LogErrors != nil {
		t.Fatalf("сбой журнала → log_errors null, получили %+v", rep.LogErrors)
	}
	if !strings.Contains(rep.Telemt.Error, "journalctl") {
		t.Fatalf("сбой журнала должен попасть в telemt.error, получили %q", rep.Telemt.Error)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(logBuf.String(), "level=WARN") || !strings.Contains(logBuf.String(), "journalctl") {
		t.Fatalf("ждали предупреждение в логе, получили %q", logBuf.String())
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Окно журнала сдвигается только после 2xx мастера: не дошедший отчёт не теряет счётчики.
func TestSendRuntime_JournalWindowMovesOnlyOn2xx(t *testing.T) {
	var mu sync.Mutex
	status := http.StatusInternalServerError
	var got []store.RuntimeReport
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep store.RuntimeReport
		_ = json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		defer mu.Unlock()
		got = append(got, rep)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c, fr, _, now := newCollector(t, telemtAPI(t, nil).URL, mtgServer(t, mtgStatsNew).URL)
	fr.out["journalctl"] = "user=a_1 limit exceeded\n"
	a, _ := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2", Mode: ModeCompare,
		Runtime: c, RuntimeEvery: time.Minute, Log: discardLog()})
	t0 := *now
	if err := a.SendRuntime(context.Background()); err == nil {
		t.Fatal("мастер ответил 500 - ждали ошибку")
	}
	*now = now.Add(time.Minute)
	mu.Lock()
	status = http.StatusNoContent
	mu.Unlock()
	if err := a.SendRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := a.SendRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("ждали 3 отчёта, было %d", len(got))
	}
	second, third := got[1].LogErrors, got[2].LogErrors
	if second == nil || !second.PeriodStart.Equal(t0.Add(-time.Minute)) || !second.PeriodEnd.Equal(t0.Add(time.Minute)) {
		t.Fatalf("после 500 окно не должно сдвигаться: второй отчёт %+v", second)
	}
	if third == nil || !third.PeriodStart.Equal(t0.Add(time.Minute)) {
		t.Fatalf("после 2xx окно начинается с конца принятого: третий отчёт %+v", third)
	}
}

// Журнал читается построчно: длинная строка не ломает разбор.
func TestExecRunner_RunLines(t *testing.T) {
	long := strings.Repeat("x", 300<<10)
	var lines []string
	err := ExecRunner{}.RunLines(context.Background(), func(line []byte) { lines = append(lines, string(line)) },
		"sh", "-c", "printf 'user=a_1 limit exceeded\\n"+long+"\\nhandshake closed\\n'")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0] != "user=a_1 limit exceeded" || len(lines[1]) != len(long) || lines[2] != "handshake closed" {
		t.Fatalf("строки: %d %q", len(lines), lines[0])
	}
	if err := (ExecRunner{}).RunLines(context.Background(), func([]byte) {}, "sh", "-c", "exit 3"); err == nil {
		t.Fatal("ненулевой код выхода - ошибка")
	}
}

// Старый мастер или мост без /v1/runtime (404/405): пауза растёт до 30 мин,
// после 2xx возвращается к периоду отчёта.
func TestRuntimePause_BackoffOnUnsupported(t *testing.T) {
	every := time.Minute
	p := every
	var seen []time.Duration
	for i := 0; i < 8; i++ {
		p = nextRuntimePause(p, every, fmt.Errorf("x: %w", ErrRuntimeUnsupported))
		seen = append(seen, p)
	}
	if seen[0] != 2*time.Minute || seen[1] != 4*time.Minute || seen[len(seen)-1] != 30*time.Minute {
		t.Fatalf("паузы: %v", seen)
	}
	if p := nextRuntimePause(30*time.Minute, every, nil); p != every {
		t.Fatalf("после 2xx пауза = период, получили %v", p)
	}
	if p := nextRuntimePause(30*time.Minute, every, errors.New("сеть")); p != every {
		t.Fatalf("обычный сбой - без роста паузы, получили %v", p)
	}
}

func TestSendRuntime_UnsupportedError(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		c, _, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
		c.Telemt, c.Mtg = false, false
		a, _ := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2", Mode: ModeCompare,
			Runtime: c, RuntimeEvery: time.Minute, Log: discardLog()})
		if err := a.SendRuntime(context.Background()); !errors.Is(err, ErrRuntimeUnsupported) {
			t.Fatalf("%d → ждали ErrRuntimeUnsupported, получили %v", code, err)
		}
		srv.Close()
	}
}

// Цикл отчётов со старым мастером не долбит его каждый период.
func TestRuntimeLoop_BacksOffOn404(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runtime" {
			mu.Lock()
			hits++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, _, _, _ := newCollector(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	c.Telemt, c.Mtg = false, false
	a, _ := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2", Mode: ModeCompare,
		Runtime: c, RuntimeEvery: 20 * time.Millisecond, Log: discardLog()})
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	a.runtimeLoop(ctx)
	mu.Lock()
	defer mu.Unlock()
	// Без паузы было бы ~20 отчётов; с удвоением 20, 40, 80, 160 мс - не больше 5.
	if hits > 5 || hits == 0 {
		t.Fatalf("ждали 1..5 попыток за 400 мс, было %d", hits)
	}
}

// Худший реалистичный случай: максимум пользователей в обоих продуктах, у каждого
// 20 адресов IPv6, полный журнал. Отчёт урезается до лимита мастера, остаются
// пользователи с наибольшим числом подключений, в error - пометка.
func TestSendRuntime_WorstCaseFitsLimit(t *testing.T) {
	key := func(i int) string { return fmt.Sprintf("tg_%0125d", i) } // 128 символов
	ips := func(i int) []string {
		out := make([]string, 20)
		for j := range out {
			out[j] = fmt.Sprintf("2001:db8:%04x:%04x:%04x:%04x:%04x:%04x", i, j, i, j, i, j)
		}
		return out
	}
	const n = 5000
	tapi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/v1/users":
			users := make([]map[string]any, n)
			for i := range users {
				users[i] = map[string]any{"username": key(i), "current_connections": i + 1}
			}
			data = users
		case "/v1/stats/users/active-ips":
			list := make([]map[string]any, n)
			for i := range list {
				list[i] = map[string]any{"username": key(i), "active_ips": ips(i)}
			}
			data = list
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	}))
	defer tapi.Close()
	mstats := map[string]any{}
	musers := map[string]any{}
	for i := 0; i < n; i++ {
		musers[key(i)] = map[string]any{"connections": i + 1, "bytes_in": 1 << 40, "bytes_out": 1 << 40,
			"last_seen": "2026-11-19T10:00:00Z", "active_ips": ips(i)}
	}
	mstats["total_connections"], mstats["users"] = n, musers
	mbody, _ := json.Marshal(mstats)
	c, fr, _, _ := newCollector(t, tapi.URL, mtgServer(t, string(mbody)).URL)
	var journal strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&journal, "WARN user=%s connection limit exceeded\nWARN handshake failed user=%s\n", key(i), key(i))
	}
	fr.out["journalctl"] = journal.String()

	var mu sync.Mutex
	var size int
	var got store.RuntimeReport
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		size = len(body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer master.Close()
	a, _ := New(Config{MasterURLs: []string{master.URL}, Token: "tok", Target: "pr2", Version: "v1", Mode: ModeCompare,
		Runtime: c, RuntimeEvery: time.Minute, Log: discardLog()})
	if err := a.SendRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if size == 0 || size > 2<<20 {
		t.Fatalf("тело отчёта %d байт, лимит мастера %d", size, 2<<20)
	}
	if got.Telemt == nil || got.Mtg == nil || len(got.Telemt.Users) == 0 || len(got.Mtg.Users) == 0 {
		t.Fatalf("пользователи не должны пропасть целиком: telemt %d, mtg %d", len(got.Telemt.Users), len(got.Mtg.Users))
	}
	for name, users := range map[string][]store.RuntimeUser{"telemt": got.Telemt.Users, "mtg": got.Mtg.Users} {
		minKept := int64(1 << 62)
		hasTop := false
		for _, u := range users {
			minKept = min(minKept, u.Connections)
			hasTop = hasTop || u.Connections == n
		}
		if !hasTop || minKept != int64(n-len(users)+1) {
			t.Fatalf("%s: должны остаться самые нагруженные: %d строк, минимум %d", name, len(users), minKept)
		}
	}
	if !strings.Contains(got.Telemt.Error, "урезан") || !strings.Contains(got.Mtg.Error, "урезан") {
		t.Fatalf("в error продуктов нужна пометка об урезании: telemt %q, mtg %q", got.Telemt.Error, got.Mtg.Error)
	}
	if got.LogErrors == nil {
		t.Fatal("журнал не должен пропасть")
	}
}
