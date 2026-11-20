package agent

// Runtime-отчёт агента (POST /v1/runtime): нагрузка машины, версии и состояние
// telemt и mtg, подключения пользователей, ошибки журнала telemt. Шлётся раз в
// --runtime-every отдельно от long-poll и отчёта observed. Сбой одного источника
// не роняет отчёт: у продукта api_ok=false и error, остальное уходит как есть.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

// Лимиты отчёта - как у мастера: лишнее агент отрезает сам, чтобы не получить 400.
const (
	runtimeMaxUsers     = 5000
	runtimeMaxActiveIPs = 20
	runtimeMaxLogItems  = 5000
	runtimeMaxText      = 1024
	// runtimeMaxBridgeRelays - сколько релеев моста идёт в отчёт (лимит мастера).
	runtimeMaxBridgeRelays = 100
	// journalMaxWindow - на сколько назад читать журнал, если прошлые чтения или
	// отправки не удались. Мастер принимает период не длиннее этого окна с запасом.
	journalMaxWindow = time.Hour
	// runtimeMaxBody - лимит тела у мастера и моста; runtimeBodyBudget - сколько
	// агент себе позволяет (запас на пометки об урезании).
	runtimeMaxBody    = 2 << 20
	runtimeBodyBudget = runtimeMaxBody - 16<<10
	// journalMaxLine - самая длинная строка журнала, которую агент дочитывает.
	journalMaxLine = 1 << 20
	// runtimeMaxPause - потолок паузы, когда мастер или мост не знают /v1/runtime.
	runtimeMaxPause = 30 * time.Minute
)

// ErrRuntimeUnsupported - мастер (или мост) старый и не знает POST /v1/runtime (404/405).
var ErrRuntimeUnsupported = errors.New("мастер не принимает runtime-отчёт (404/405) - старая версия мастера или моста")

// CommandRunner запускает внешнюю команду и возвращает stdout (интерфейс - для тестов).
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// LineRunner - CommandRunner, который отдаёт stdout построчно, не держа весь
// вывод в памяти (журнал telemt за час бывает большим).
type LineRunner interface {
	RunLines(ctx context.Context, fn func(line []byte), name string, args ...string) error
}

// ExecRunner - CommandRunner через os/exec.
type ExecRunner struct{}

// RunLines выполняет команду и вызывает fn на каждую строку stdout (без \n;
// срез действителен только внутри fn). Строка длиннее journalMaxLine - ошибка.
func (ExecRunner) RunLines(ctx context.Context, fn func(line []byte), name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), journalMaxLine)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	scanErr := sc.Err()
	if scanErr != nil {
		_, _ = io.Copy(io.Discard, out) // дочитать, чтобы команда не встала на записи
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		return fmt.Errorf("%s: чтение вывода: %w", name, scanErr)
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("%s: %v %s", name, waitErr, msg)
	}
	return nil
}

// Run выполняет команду; при ошибке в тексте - начало stderr.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return out, fmt.Errorf("%s: %v %s", name, err, msg)
	}
	return out, nil
}

// RuntimeCollector собирает runtime-отчёт машины. Внешние команды, файлы и
// /proc - через поля-функции, чтобы проверять разбор на фикстурах.
type RuntimeCollector struct {
	Telemt       bool   // на машине есть telemt (ресурс telemt.users)
	Mtg          bool   // на машине есть mtg-multi (ресурс mtg.config)
	TelemtAPI    string // по умолчанию http://127.0.0.1:9091
	TelemtBin    string // по умолчанию telemt
	TelemtConfig string // по умолчанию /etc/telemt/telemt.toml
	UsersFile    string // users.toml telemt (сроки и лимиты пользователей)
	MtgStatsURL  string // по умолчанию http://127.0.0.1:8081/stats
	MtgBin       string // по умолчанию mtg
	JournalUnit  string // по умолчанию telemt
	// BridgeURL - адрес моста этого выхода (GET <BridgeURL>/v1/relays); пусто -
	// поля bridge в отчёте нет (R2.3). BridgeTimeout - сколько ждать мост, по умолчанию 3 с.
	BridgeURL     string
	BridgeTimeout time.Duration
	ProcRoot      string // по умолчанию /proc
	DiskPath      string // по умолчанию /
	// CPUSampleGap - пауза между двумя замерами /proc/stat в первом отчёте
	// (дальше CPU считается между отчётами). По умолчанию 1 с.
	CPUSampleGap time.Duration

	Runner    CommandRunner
	ReadFile  func(path string) ([]byte, error)
	Stat      func(path string) (os.FileInfo, error)
	LookPath  func(file string) (string, error)
	DiskUsage func(path string) (float64, error) // занято, %
	HTTP      *http.Client
	Now       func() time.Time
	Log       *slog.Logger

	mu         sync.Mutex
	prevCPU    *cpuSample
	prevNet    *netSample
	lastLogEnd time.Time
	versions   map[string]versionCache
}

// versionCache - результат `--version` для (mtime, size) бинаря; неудача тоже
// кешируется (version пустая), чтобы битый бинарь не запускался каждую минуту.
type versionCache struct {
	mtime   time.Time
	size    int64
	version string
}

func (c *RuntimeCollector) defaults() {
	if c.TelemtAPI == "" {
		c.TelemtAPI = "http://127.0.0.1:9091"
	}
	c.TelemtAPI = strings.TrimRight(c.TelemtAPI, "/")
	if c.TelemtBin == "" {
		c.TelemtBin = "telemt"
	}
	if c.TelemtConfig == "" {
		c.TelemtConfig = "/etc/telemt/telemt.toml"
	}
	if c.MtgStatsURL == "" {
		c.MtgStatsURL = "http://127.0.0.1:8081/stats"
	}
	if c.JournalUnit == "" {
		c.JournalUnit = "telemt"
	}
	if c.ProcRoot == "" {
		c.ProcRoot = "/proc"
	}
	if c.DiskPath == "" {
		c.DiskPath = "/"
	}
	c.BridgeURL = strings.TrimRight(c.BridgeURL, "/")
	if c.BridgeTimeout <= 0 {
		c.BridgeTimeout = 3 * time.Second
	}
	if c.CPUSampleGap <= 0 {
		c.CPUSampleGap = time.Second
	}
	if c.Runner == nil {
		c.Runner = ExecRunner{}
	}
	if c.ReadFile == nil {
		c.ReadFile = os.ReadFile
	}
	if c.Stat == nil {
		c.Stat = os.Stat
	}
	if c.LookPath == nil {
		c.LookPath = exec.LookPath
	}
	if c.DiskUsage == nil {
		c.DiskUsage = diskUsedPct
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.versions == nil {
		c.versions = map[string]versionCache{}
	}
}

// Collect собирает один отчёт (без target и agent_version - их ставит агент).
// every - период отчётов: окно журнала в первом отчёте.
func (c *RuntimeCollector) Collect(ctx context.Context, every time.Duration) store.RuntimeReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaults()
	rep := store.RuntimeReport{Host: c.collectHost(ctx)}
	if c.Telemt {
		rep.Telemt = c.collectTelemt(ctx)
		le, err := c.collectJournal(ctx, every)
		if err != nil {
			c.Log.Warn("агент: журнал telemt не прочитан, счётчики ошибок не отправлены", "err", err)
			rep.Telemt.Error = clip(joinErr(rep.Telemt.Error, "журнал: "+err.Error()))
		}
		rep.LogErrors = le
	}
	if c.Mtg {
		rep.Mtg = c.collectMtg(ctx)
	}
	if c.BridgeURL != "" {
		rep.Bridge = c.collectBridge(ctx)
	}
	return rep
}

// collectBridge читает сводку моста выхода (R2.1). Мост не ответил за
// BridgeTimeout или ответил не по контракту - error, relays пустой (R2.2).
func (c *RuntimeCollector) collectBridge(ctx context.Context) *store.BridgeSummary {
	ctx, cancel := context.WithTimeout(ctx, c.BridgeTimeout)
	defer cancel()
	var body struct {
		Bridge string              `json:"bridge"`
		Relays []store.BridgeRelay `json:"relays"`
	}
	out := &store.BridgeSummary{Relays: []store.BridgeRelay{}}
	if err := c.getJSON(ctx, c.BridgeURL+"/v1/relays", &body); err != nil {
		c.Log.Warn("агент: мост не ответил, сводка релеев не отправлена", "err", err)
		out.Error = clip("мост: " + err.Error())
		return out
	}
	out.Code = body.Bridge
	if body.Relays != nil {
		out.Relays = body.Relays
	}
	if len(out.Relays) > runtimeMaxBridgeRelays {
		out.Relays = out.Relays[:runtimeMaxBridgeRelays]
	}
	return out
}

// --- telemt ---

type telemtEnvelope struct {
	OK   bool            `json:"ok"`
	Data json.RawMessage `json:"data"`
}

func (c *RuntimeCollector) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("%s: чтение: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: разбор: %w", url, err)
	}
	return nil
}

// telemtData - data из конверта telemt {ok, data}; без конверта - тело целиком.
func (c *RuntimeCollector) telemtData(ctx context.Context, path string, v any) error {
	var raw json.RawMessage
	if err := c.getJSON(ctx, c.TelemtAPI+path, &raw); err != nil {
		return err
	}
	var env telemtEnvelope
	if json.Unmarshal(raw, &env) == nil && env.Data != nil {
		if !env.OK {
			return fmt.Errorf("%s: ok=false", path)
		}
		raw = env.Data
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: разбор data: %w", path, err)
	}
	return nil
}

type telemtAPIUser struct {
	Username           string  `json:"username"`
	CurrentConnections int64   `json:"current_connections"`
	ExpirationRFC3339  *string `json:"expiration_rfc3339"`
}

type telemtActiveIPs struct {
	Username  string   `json:"username"`
	ActiveIPs []string `json:"active_ips"`
}

func isServiceUser(key string) bool { return key == "placeholder" }

func (c *RuntimeCollector) collectTelemt(ctx context.Context) *store.RuntimeTelemt {
	t := &store.RuntimeTelemt{Version: c.binVersion(ctx, c.TelemtBin), APIOK: true}
	var errs []string

	var users []telemtAPIUser
	var ips []telemtActiveIPs
	if err := c.telemtData(ctx, "/v1/users", &users); err != nil {
		t.APIOK = false
		errs = append(errs, err.Error())
	} else if err := c.telemtData(ctx, "/v1/stats/users/active-ips", &ips); err != nil {
		t.APIOK = false
		errs = append(errs, err.Error())
	}

	// Сроки и лимиты - из users.toml (то, что telemt действительно читает).
	var expires map[string]*time.Time
	var conns []int
	if c.UsersFile != "" {
		if data, err := c.ReadFile(c.UsersFile); err != nil {
			errs = append(errs, fmt.Sprintf("users.toml: %v", err))
		} else {
			expires = map[string]*time.Time{}
			for _, u := range telemtcfg.ParseUsersToml(string(data)) {
				if isServiceUser(u.Username) {
					continue
				}
				expires[u.Username] = u.ExpirationAt
				if u.MaxTCPConns != nil {
					conns = append(conns, *u.MaxTCPConns)
				}
			}
		}
	}
	t.MaxTCPConnsMedian = median(conns)

	now := c.Now()
	byKey := map[string]*store.RuntimeUser{}
	for _, u := range users {
		if isServiceUser(u.Username) {
			continue
		}
		t.UsersConfigured++
		t.ConnectionsTotal += u.CurrentConnections
		exp := parseTimePtr(u.ExpirationRFC3339)
		if expires != nil {
			exp = expires[u.Username]
		}
		if exp == nil || exp.After(now) {
			t.UsersEffective++
		}
		if u.CurrentConnections > 0 {
			byKey[u.Username] = &store.RuntimeUser{Key: u.Username, Connections: u.CurrentConnections}
		}
	}
	for _, a := range ips {
		if isServiceUser(a.Username) || len(a.ActiveIPs) == 0 {
			continue
		}
		u := byKey[a.Username]
		if u == nil {
			u = &store.RuntimeUser{Key: a.Username}
			byKey[a.Username] = u
		}
		u.ActiveIPs = capIPs(a.ActiveIPs)
	}
	t.Users = sortedUsers(byKey)

	if data, err := c.ReadFile(c.TelemtConfig); err != nil {
		errs = append(errs, fmt.Sprintf("telemt.toml: %v", err))
	} else {
		sum := sha256.Sum256(data)
		t.ConfigSHA256 = hex.EncodeToString(sum[:])
		t.Keepalive, t.Ack, t.TLSDomain = ParseTelemtSummary(data)
	}
	t.Error = clip(strings.Join(errs, "; "))
	return t
}

var (
	keepaliveRe = regexp.MustCompile(`(?m)^\s*client_keepalive\s*=\s*(\d+)`)
	ackRe       = regexp.MustCompile(`(?m)^\s*client_ack\s*=\s*(\d+)`)
	tlsDomainRe = regexp.MustCompile(`(?m)^\s*tls_domain\s*=\s*"([^"]+)"`)
)

// ParseTelemtSummary - client_keepalive, client_ack и tls_domain из telemt.toml.
func ParseTelemtSummary(cfg []byte) (keepalive, ack int, tlsDomain string) {
	if m := keepaliveRe.FindSubmatch(cfg); m != nil {
		keepalive, _ = strconv.Atoi(string(m[1]))
	}
	if m := ackRe.FindSubmatch(cfg); m != nil {
		ack, _ = strconv.Atoi(string(m[1]))
	}
	if m := tlsDomainRe.FindSubmatch(cfg); m != nil {
		tlsDomain = clip(string(m[1]))
	}
	return keepalive, ack, tlsDomain
}

// --- mtg ---

type mtgStats struct {
	TotalConnections int64 `json:"total_connections"`
	Users            map[string]struct {
		Connections int64    `json:"connections"`
		BytesIn     int64    `json:"bytes_in"`
		BytesOut    int64    `json:"bytes_out"`
		LastSeen    *string  `json:"last_seen"`
		ActiveIPs   []string `json:"active_ips"` // нет у старых сборок
	} `json:"users"`
}

// ParseMtgStats разбирает /stats mtg-multi. Старые сборки без active_ips - пустые списки.
func ParseMtgStats(body []byte) (total int64, users []store.RuntimeUser, err error) {
	var s mtgStats
	if err := json.Unmarshal(body, &s); err != nil {
		return 0, nil, fmt.Errorf("/stats mtg: разбор: %w", err)
	}
	byKey := map[string]*store.RuntimeUser{}
	for key, u := range s.Users {
		if u.Connections <= 0 && len(u.ActiveIPs) == 0 {
			continue // не в сети: строка только раздует отчёт
		}
		byKey[key] = &store.RuntimeUser{Key: key, Connections: u.Connections, BytesIn: max(u.BytesIn, 0),
			BytesOut: max(u.BytesOut, 0), LastSeen: parseTimePtr(u.LastSeen), ActiveIPs: capIPs(u.ActiveIPs)}
	}
	return max(s.TotalConnections, 0), sortedUsers(byKey), nil
}

func (c *RuntimeCollector) collectMtg(ctx context.Context) *store.RuntimeMtg {
	m := &store.RuntimeMtg{Version: c.binVersion(ctx, c.mtgBin()), APIOK: true}
	var raw json.RawMessage
	err := c.getJSON(ctx, c.MtgStatsURL, &raw)
	if err == nil {
		m.ConnectionsTotal, m.Users, err = ParseMtgStats(raw)
	}
	if err != nil {
		m.APIOK, m.Error = false, clip(err.Error())
	}
	return m
}

// --- версии бинарей ---

var versionRe = regexp.MustCompile(`^v?\d+\.\d+[0-9A-Za-z.+\-]*$`)

// ParseVersion - версия из вывода `<bin> --version`: первое слово вида x.y[.z],
// иначе первая строка целиком (сборки без номера печатают коммит).
func ParseVersion(out []byte) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	for _, w := range strings.Fields(line) {
		if w = strings.Trim(w, ",;"); versionRe.MatchString(w) {
			return w
		}
	}
	if len(line) > 64 {
		line = line[:64]
	}
	return line
}

// binVersion - версия бинаря; `--version` вызывается заново только при смене mtime или размера.
// mtgBin - бинарь mtg для версии: заданный явно, иначе mtg-multi (так он называется на
// машинах флота), а если его нет в PATH - mtg.
func (c *RuntimeCollector) mtgBin() string {
	if c.MtgBin != "" {
		return c.MtgBin
	}
	if _, err := c.LookPath("mtg-multi"); err == nil {
		return "mtg-multi"
	}
	return "mtg"
}

func (c *RuntimeCollector) binVersion(ctx context.Context, bin string) string {
	path, err := c.LookPath(bin)
	if err != nil {
		return ""
	}
	fi, err := c.Stat(path)
	if err != nil {
		return ""
	}
	if v, ok := c.versions[path]; ok && v.mtime.Equal(fi.ModTime()) && v.size == fi.Size() {
		return v.version
	}
	vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v := ""
	if out, err := c.Runner.Run(vctx, path, "--version"); err != nil {
		c.Log.Warn("агент: версия бинаря не определена, повтор - после замены файла", "bin", path, "err", err)
	} else {
		v = clip(ParseVersion(out))
	}
	c.versions[path] = versionCache{mtime: fi.ModTime(), size: fi.Size(), version: v}
	return v
}

// --- журнал ---

var (
	// Строки, которые считает /logs бота, и их вид - те же правила.
	journalMatchRe = regexp.MustCompile(`(?i)limit exceeded|handshake.*(fail|closed|error)|auth.*(fail|denied)|timed? ?out`)
	limitRe        = regexp.MustCompile(`(?i)limit exceeded`)
	handshakeRe    = regexp.MustCompile(`(?i)handshake`)
	authRe         = regexp.MustCompile(`(?i)auth`)
	timeoutRe      = regexp.MustCompile(`(?i)timed? ?out`)
	userKeyRe      = regexp.MustCompile(`user="?([A-Za-z0-9_.\-]+)`)
)

// ClassifyLogLine - вид ошибки и ключ пользователя (user=<key>, пусто - нет);
// ok=false - строка не ошибка подключения.
func ClassifyLogLine(line string) (kind, key string, ok bool) {
	if !journalMatchRe.MatchString(line) {
		return "", "", false
	}
	switch {
	case limitRe.MatchString(line):
		kind = "limit"
	case handshakeRe.MatchString(line):
		kind = "handshake"
	case authRe.MatchString(line):
		kind = "auth"
	case timeoutRe.MatchString(line):
		kind = "timeout"
	default:
		kind = "other"
	}
	if m := userKeyRe.FindStringSubmatch(line); m != nil {
		key = m[1]
	}
	return kind, key, true
}

// logCounter - счётчики ошибок журнала по (ключ, вид), заполняется построчно.
type logCounter map[[2]string]int64

func (lc logCounter) add(line string) {
	if kind, key, ok := ClassifyLogLine(line); ok {
		if len(key) > 128 {
			key = key[:128]
		}
		lc[[2]string{key, kind}]++
	}
}

// CountLogErrors - счётчики по (ключ, вид), самые частые первыми, не больше runtimeMaxLogItems.
func CountLogErrors(out []byte) []store.LogErrorItem {
	lc := logCounter{}
	for _, line := range strings.Split(string(out), "\n") {
		lc.add(line)
	}
	return lc.items()
}

func (lc logCounter) items() []store.LogErrorItem {
	items := make([]store.LogErrorItem, 0, len(lc))
	for k, n := range lc {
		items = append(items, store.LogErrorItem{Key: k[0], Kind: k[1], Count: n})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		if items[i].Key != items[j].Key {
			return items[i].Key < items[j].Key
		}
		return items[i].Kind < items[j].Kind
	})
	if len(items) > runtimeMaxLogItems {
		items = items[:runtimeMaxLogItems]
	}
	return items
}

// collectJournal читает журнал telemt за период с конца последнего принятого
// мастером отчёта. Окно сдвигает только CommitLogWindow (после 2xx мастера):
// сбой чтения или отправки - следующий отчёт прочитает период целиком (до часа).
// Сбой чтения - nil и ошибка (вызывающий пишет её в лог и в telemt.error).
func (c *RuntimeCollector) collectJournal(ctx context.Context, every time.Duration) (*store.RuntimeLogErrors, error) {
	end := c.Now().Truncate(time.Second)
	if c.lastLogEnd.IsZero() {
		c.lastLogEnd = end.Add(-every) // первый отчёт - один период назад
	}
	start := c.lastLogEnd
	if end.Sub(start) > journalMaxWindow {
		start = end.Add(-journalMaxWindow)
	}
	if !end.After(start) {
		return nil, nil
	}
	jctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{"-u", c.JournalUnit,
		"--since", fmt.Sprintf("@%d", start.Unix()), "--until", fmt.Sprintf("@%d", end.Unix()),
		"-o", "cat", "--no-pager"}
	lc := logCounter{}
	if lr, ok := c.Runner.(LineRunner); ok {
		if err := lr.RunLines(jctx, func(line []byte) { lc.add(string(line)) }, "journalctl", args...); err != nil {
			return nil, err
		}
	} else {
		out, err := c.Runner.Run(jctx, "journalctl", args...)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(out), "\n") {
			lc.add(line)
		}
	}
	return &store.RuntimeLogErrors{PeriodStart: start.UTC(), PeriodEnd: end.UTC(), Items: lc.items()}, nil
}

// CommitLogWindow сдвигает начало следующего окна журнала на конец периода,
// который мастер принял (вызывается после 2xx).
func (c *RuntimeCollector) CommitLogWindow(le *store.RuntimeLogErrors) {
	if le == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if le.PeriodEnd.After(c.lastLogEnd) {
		c.lastLogEnd = le.PeriodEnd
	}
}

// --- вспомогательное ---

func capIPs(ips []string) []string {
	out := make([]string, 0, min(len(ips), runtimeMaxActiveIPs))
	for _, ip := range ips {
		if ip == "" || len(ip) > 64 {
			continue
		}
		out = append(out, ip)
		if len(out) == runtimeMaxActiveIPs {
			break
		}
	}
	return out
}

// sortedUsers - пользователи по ключу; больше лимита - отрезаются самые малые по подключениям.
func sortedUsers(m map[string]*store.RuntimeUser) []store.RuntimeUser {
	out := make([]store.RuntimeUser, 0, len(m))
	for _, u := range m {
		if u.ActiveIPs == nil {
			u.ActiveIPs = []string{}
		}
		out = append(out, *u)
	}
	if len(out) > runtimeMaxUsers {
		sort.Slice(out, func(i, j int) bool { return out[i].Connections > out[j].Connections })
		out = out[:runtimeMaxUsers]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func median(v []int) int {
	if len(v) == 0 {
		return 0
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func parseTimePtr(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil
	}
	return &t
}

// joinErr дописывает причину к тексту ошибки продукта через "; ".
func joinErr(base, add string) string {
	if base == "" {
		return add
	}
	return base + "; " + add
}

func clip(s string) string {
	if len(s) > runtimeMaxText {
		return s[:runtimeMaxText]
	}
	return s
}

// --- отправка ---

// nextRuntimePause - пауза до следующего отчёта: мастер или мост не знают
// /v1/runtime - удваивается до runtimeMaxPause; любой другой исход - период отчёта.
func nextRuntimePause(cur, every time.Duration, err error) time.Duration {
	if !errors.Is(err, ErrRuntimeUnsupported) || every >= runtimeMaxPause {
		return every
	}
	return min(max(cur, every)*2, runtimeMaxPause)
}

// runtimeLoop шлёт runtime-отчёт сразу и затем раз в RuntimeEvery. Не копит:
// не дошёл - следующий раз соберёт свежий снимок. Старый мастер (404/405) -
// пауза растёт, чтобы не собирать отчёт впустую каждую минуту.
func (a *Agent) runtimeLoop(ctx context.Context) {
	every := a.cfg.RuntimeEvery
	pause := every
	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C
	for {
		err := a.SendRuntime(ctx)
		if ctx.Err() != nil {
			return
		}
		pause = nextRuntimePause(pause, every, err)
		switch {
		case errors.Is(err, ErrRuntimeUnsupported):
			a.cfg.Log.Warn("агент: мастер не принимает runtime-отчёт, следующая попытка позже", "pause", pause, "err", err)
		case err != nil:
			a.cfg.Log.Warn("агент: runtime-отчёт не отправлен", "err", err)
		}
		timer.Reset(pause)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// trimUsers оставляет keep пользователей с наибольшим числом подключений
// (порядок - по ключу, как у sortedUsers).
func trimUsers(users []store.RuntimeUser, keep int) []store.RuntimeUser {
	if keep >= len(users) {
		return users
	}
	out := append([]store.RuntimeUser(nil), users...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Connections != out[j].Connections {
			return out[i].Connections > out[j].Connections
		}
		return out[i].Key < out[j].Key
	})
	out = out[:keep]
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// encodeRuntime сериализует отчёт и, если он больше limit, урезает по шагам:
// сначала active_ips, затем пользователей (остаются с наибольшим connections),
// затем строки журнала (остаются самые частые). В error продукта - пометка.
func encodeRuntime(rep store.RuntimeReport, limit int) ([]byte, error) {
	body, err := json.Marshal(rep)
	if err != nil || len(body) <= limit {
		return body, err
	}
	type prod struct {
		users  *[]store.RuntimeUser
		errp   *string
		base   string
		total  int
		telemt bool // пометка об урезании журнала - у telemt (журнал - его)
	}
	var prods []prod
	if rep.Telemt != nil {
		t := *rep.Telemt
		rep.Telemt = &t
		prods = append(prods, prod{&t.Users, &t.Error, t.Error, len(t.Users), true})
	}
	if rep.Mtg != nil {
		m := *rep.Mtg
		rep.Mtg = &m
		prods = append(prods, prod{&m.Users, &m.Error, m.Error, len(m.Users), false})
	}
	logTotal := 0
	if rep.LogErrors != nil {
		le := *rep.LogErrors
		rep.LogErrors = &le
		logTotal = len(le.Items)
	}
	logCut := false
	mark := func() {
		for _, p := range prods {
			note := "отчёт урезан до лимита мастера: без active_ips"
			if len(*p.users) < p.total {
				note += fmt.Sprintf(", пользователей %d из %d (с наибольшим числом подключений)", len(*p.users), p.total)
			}
			if logCut && p.telemt {
				note += fmt.Sprintf(", строк журнала %d из %d", len(rep.LogErrors.Items), logTotal)
			}
			*p.errp = clip(joinErr(p.base, note))
		}
	}
	// Шаг 1: без адресов.
	for _, p := range prods {
		users := make([]store.RuntimeUser, len(*p.users))
		for i, u := range *p.users {
			u.ActiveIPs = []string{}
			users[i] = u
		}
		*p.users = users
	}
	for {
		mark()
		if body, err = json.Marshal(rep); err != nil || len(body) <= limit {
			return body, err
		}
		// Шаг 2: пользователей - по четверти, пока есть кого резать.
		cut := false
		for _, p := range prods {
			if n := len(*p.users); n > 0 {
				*p.users = trimUsers(*p.users, n*3/4)
				cut = true
			}
		}
		if cut {
			continue
		}
		// Шаг 3: журнал (строки уже по убыванию счётчика).
		if rep.LogErrors != nil && len(rep.LogErrors.Items) > 0 {
			rep.LogErrors.Items = rep.LogErrors.Items[:len(rep.LogErrors.Items)*3/4]
			logCut = true
			continue
		}
		return nil, fmt.Errorf("runtime-отчёт %d байт не укладывается в %d и после урезания", len(body), limit)
	}
}

// SendRuntime собирает отчёт и отправляет первому мастеру, который его примет.
// После 2xx сдвигает окно журнала. Все адреса ответили 404/405 - ErrRuntimeUnsupported.
func (a *Agent) SendRuntime(ctx context.Context) error {
	tctx, cancel := context.WithTimeout(ctx, max(a.cfg.RuntimeEvery, 30*time.Second))
	defer cancel()
	rep := a.cfg.Runtime.Collect(tctx, a.cfg.RuntimeEvery)
	rep.Target, rep.AgentVersion = a.cfg.Target, a.cfg.Version
	body, err := encodeRuntime(rep, runtimeBodyBudget)
	if err != nil {
		return err
	}
	var errs []error
	unsupported := 0
	for _, m := range a.masters {
		req, err := http.NewRequestWithContext(tctx, http.MethodPost, m.base+"/v1/runtime", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := m.client.Do(req)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.base, err))
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch {
		case resp.StatusCode < 300:
			a.cfg.Runtime.CommitLogWindow(rep.LogErrors)
			return nil
		case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusMethodNotAllowed:
			// Старый мастер или мост - может, следующий адрес новее.
			unsupported++
			errs = append(errs, fmt.Errorf("%s: %d", m.base, resp.StatusCode))
		case resp.StatusCode >= 500, resp.StatusCode == http.StatusConflict:
			// Сбой или не мастер - пробуем следующий адрес.
			errs = append(errs, fmt.Errorf("%s: %d %s", m.base, resp.StatusCode, bytes.TrimSpace(msg)))
		default:
			return fmt.Errorf("%s отверг отчёт: %d %s", m.base, resp.StatusCode, bytes.TrimSpace(msg))
		}
	}
	if unsupported > 0 && unsupported == len(a.masters) {
		return fmt.Errorf("%w: %w", ErrRuntimeUnsupported, errors.Join(errs...))
	}
	return fmt.Errorf("ни один мастер не принял runtime-отчёт: %w", errors.Join(errs...))
}

// probeTimeout - сколько ждать ответа на пробу одного адреса мастера (U17).
const probeTimeout = 5 * time.Second

// probeMaxEvery - период пробы путей к мастеру по умолчанию и самый длинный (U17).
const probeMaxEvery = time.Minute

// probeLoop пробует пути к мастеру сразу и затем раз в ProbeEvery. Отдельно от
// runtime-цикла: его пауза при ErrRuntimeUnsupported пробы не останавливает (U17).
func (a *Agent) probeLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.ProbeEvery)
	defer t.Stop()
	for {
		a.ProbeMasters(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ProbeMasters - проба путей к мастеру: GET /v1/desired без ожидания через каждый
// адрес из списка, параллельно, с таймаутом probeTimeout на адрес (U17). Нужна
// релею с несколькими своими нодами: агент ходит к мастеру через первую доступную,
// и без пробы мост второй ноды релея не видел бы (пара навсегда relay_silent,
// спецификация relay-via-exit, U1). Ответ не используется: новые ревизии агент
// берёт своим long-poll. С одним адресом - ничего.
func (a *Agent) ProbeMasters(ctx context.Context) {
	if len(a.masters) < 2 {
		return
	}
	q := url.Values{"target": {a.cfg.Target}, "rev": {strconv.FormatInt(a.knownRev(), 10)}, "wait": {"0s"}}
	var wg sync.WaitGroup
	for _, m := range a.masters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.probeMaster(ctx, m, q)
		}()
	}
	wg.Wait()
}

// probeMaster - проба одного адреса мастера.
func (a *Agent) probeMaster(ctx context.Context, m master, q url.Values) {
	ctx, cancel := context.WithTimeout(ctx, a.probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+"/v1/desired?"+q.Encode(), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	resp, err := m.client.Do(req)
	if err != nil {
		a.cfg.Log.Warn("агент: проба пути к мастеру не прошла", "master", m.base, "err", err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<20))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
		a.cfg.Log.Warn("агент: проба пути к мастеру", "master", m.base, "status", resp.StatusCode)
	}
}
