package agent

// Применение config.toml mtg-multi (ресурс mtg.config). Порядок - как у бота
// (mtg-sync.ts): проверка места -> временный файл со сверкой -> бэкап -> замена ->
// SIGHUP, если изменились только [secrets] (иначе рестарт) -> проверка, что mtg
// работает на новом наборе (secrets_sha256 в /stats) -> при сбое возврат прежнего.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MtgService управляет процессом mtg-multi.
type MtgService interface {
	HUP(ctx context.Context) error
	Restart(ctx context.Context) error
	Active(ctx context.Context) bool
}

// SystemdMtg - mtg-multi под systemd.
type SystemdMtg struct{ Unit string }

func (s SystemdMtg) unit() string {
	if s.Unit == "" {
		return "mtg-multi"
	}
	return s.Unit
}

// HUP - перечитать секреты без рестарта.
func (s SystemdMtg) HUP(ctx context.Context) error {
	return runCmd(ctx, "systemctl", "kill", "-s", "HUP", s.unit())
}

// Restart - перезапуск юнита.
func (s SystemdMtg) Restart(ctx context.Context) error {
	return runCmd(ctx, "systemctl", "restart", s.unit())
}

// Active - юнит активен.
func (s SystemdMtg) Active(ctx context.Context) bool {
	return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", s.unit()).Run() == nil
}

func runCmd(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// MtgApplier применяет config.toml mtg-multi.
type MtgApplier struct {
	ConfigFile      string // по умолчанию /etc/mtg-multi/config.toml
	StatsURL        string // /stats mtg; по умолчанию http://127.0.0.1:8081/stats
	Service         MtgService
	verifyUnchanged bool          // второй процесс подтверждает уже лежащий на диске конфиг своим /stats
	HotReload       bool          // SIGHUP, если изменились только секреты
	ConfirmTimeout  time.Duration // ожидание подтверждения; по умолчанию 10 с
	PollEvery       time.Duration // по умолчанию 500 мс
	FreeSpace       func(dir string) (uint64, error)

	writeFile func(path string, data []byte, perm os.FileMode) error
}

func (a *MtgApplier) defaults() {
	if a.ConfigFile == "" {
		a.ConfigFile = "/etc/mtg-multi/config.toml"
	}
	if a.StatsURL == "" {
		a.StatsURL = "http://127.0.0.1:8081/stats"
	}
	if a.Service == nil {
		a.Service = SystemdMtg{}
	}
	if a.ConfirmTimeout <= 0 {
		a.ConfirmTimeout = 10 * time.Second
	}
	if a.PollEvery <= 0 {
		a.PollEvery = 500 * time.Millisecond
	}
	if a.FreeSpace == nil {
		a.FreeSpace = freeSpace
	}
	if a.writeFile == nil {
		a.writeFile = writeFileSync
	}
}

var (
	secretsHeaderRe = regexp.MustCompile(`(?m)^\[secrets\]\s*$`)
	secretRowRe     = regexp.MustCompile(`(?m)^"([^"]+)" = "([0-9a-fA-F]+)"`)
)

func secretsStart(cfg string) int {
	loc := secretsHeaderRe.FindStringIndex(cfg)
	if loc == nil {
		return -1
	}
	return loc[0]
}

// onlySecretsChanged - отличается только раздел [secrets] (он всегда последний).
func onlySecretsChanged(cur, next string) bool {
	a, b := secretsStart(cur), secretsStart(next)
	if a < 0 || b < 0 {
		return false
	}
	return strings.TrimSpace(cur[:a]) == strings.TrimSpace(next[:b])
}

func secretRows(cfg string) [][2]string {
	start := secretsStart(cfg)
	if start < 0 {
		return nil
	}
	var rows [][2]string
	for _, m := range secretRowRe.FindAllStringSubmatch(cfg[start:], -1) {
		rows = append(rows, [2]string{m[1], strings.ToLower(m[2])})
	}
	return rows
}

// MtgSecretsDigest - отпечаток набора секретов, как его считает mtg-multi
// (secretSet.digest): sha256 строк "имя=секрет" в байтовом порядке имён через \n.
func MtgSecretsDigest(cfg []byte) string {
	rows := secretRows(string(cfg))
	sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r[0] + "=" + r[1]
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// Apply приводит config.toml к желаемому и ждёт, что mtg работает на новом наборе.
func (a *MtgApplier) Apply(ctx context.Context, desired []byte) ApplyResult {
	a.defaults()
	cur, err := os.ReadFile(a.ConfigFile)
	if err != nil && !os.IsNotExist(err) {
		return failed("config.toml не читается: %v", err)
	}
	if bytes.Equal(cur, desired) {
		if a.verifyUnchanged {
			want := MtgSecretsDigest(desired)
			client := &http.Client{Timeout: 3 * time.Second}
			digest := a.statsDigest(ctx, client)
			if digest == "" || !a.Service.Active(ctx) {
				return ApplyResult{Status: "failed", Error: "второй процесс mtg не подтвердил набор секретов в /stats", retry: true}
			}
			if digest != want {
				if !a.HotReload || a.Service.HUP(ctx) != nil || !a.waitDigest(ctx, want) {
					return ApplyResult{Status: "failed", Error: "второй процесс mtg не подхватил набор секретов с диска", retry: true}
				}
			}
		}
		return ApplyResult{Status: "applied"}
	}
	if len(secretRows(string(desired))) == 0 && len(secretRows(string(cur))) > 0 {
		return failed("в желаемом конфиге нет секретов, а на машине есть - похоже на недогруженную базу, не применяю")
	}
	need := uint64(2*len(desired)) + 1<<20
	if free, err := a.FreeSpace(filepath.Dir(a.ConfigFile)); err != nil {
		return failed("свободное место не проверить: %v", err)
	} else if free < need {
		return failed("мало места на диске: свободно %d байт, нужно %d", free, need)
	}
	tmp, bak := a.ConfigFile+tmpSuffix, a.ConfigFile+bakSuffix
	if err := a.writeFile(tmp, desired, 0o600); err != nil {
		_ = os.Remove(tmp)
		return failed("запись %s: %v", tmp, err)
	}
	if got, err := os.ReadFile(tmp); err != nil || !bytes.Equal(got, desired) {
		_ = os.Remove(tmp)
		return failed("записанный %s обрезан или не совпал с желаемым (%d из %d байт)", tmp, len(got), len(desired))
	}
	hadOld := cur != nil
	if hadOld {
		if err := a.writeFile(bak, cur, 0o600); err != nil {
			_ = os.Remove(tmp)
			return failed("бэкап %s: %v", a.ConfigFile, err)
		}
	}
	if err := os.Rename(tmp, a.ConfigFile); err != nil {
		_ = os.Remove(tmp)
		return failed("замена %s: %v", a.ConfigFile, err)
	}

	want := MtgSecretsDigest(desired)
	if a.HotReload && hadOld && onlySecretsChanged(string(cur), string(desired)) {
		if err := a.Service.HUP(ctx); err == nil && a.waitDigest(ctx, want) {
			return ApplyResult{Status: "applied"}
		}
		// SIGHUP не подхватил набор - пробуем рестартом.
	}
	if err := a.Service.Restart(ctx); err == nil && a.waitDigest(ctx, want) {
		return ApplyResult{Status: "applied"}
	}
	cause := "mtg-multi не поднялся на новом конфиге или работает не на том наборе секретов"
	if !hadOld {
		return failed("%s; прежнего конфига не было", cause)
	}
	if err := os.Rename(bak, a.ConfigFile); err != nil {
		return failed("%s; возврат прежнего не удался: %v", cause, err)
	}
	if err := a.Service.Restart(ctx); err != nil {
		return failed("%s; прежний конфиг возвращён, но рестарт не удался: %v", cause, err)
	}
	return failed("%s; прежний конфиг возвращён", cause)
}

// waitDigest ждёт, пока mtg активен и отдаёт в /stats ожидаемый отпечаток.
func (a *MtgApplier) waitDigest(ctx context.Context, want string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(a.ConfirmTimeout)
	for {
		if a.Service.Active(ctx) && a.statsDigest(ctx, client) == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(a.PollEvery):
		}
	}
}

func (a *MtgApplier) statsDigest(ctx context.Context, client *http.Client) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.StatsURL, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var s struct {
		Digest string `json:"secrets_sha256"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s) != nil {
		return ""
	}
	return s.Digest
}

// До первой публикации config.toml может ещё отсутствовать. Для нового файла проверяем
// реальное расположение каталога, для существующих также учитываем жёсткие ссылки.
func mtgFileLocation(file string) (string, os.FileInfo, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(parent, filepath.Base(file))
	_, err = os.Lstat(path)
	if os.IsNotExist(err) {
		return path, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s должен быть обычным файлом", file)
	}
	return path, info, nil
}

func validateMtgFiles(primary, secondary string) error {
	if primary == "" || !filepath.IsAbs(secondary) {
		return fmt.Errorf("нужны основной конфиг и абсолютный путь второго конфига mtg")
	}
	first, firstInfo, err := mtgFileLocation(primary)
	if err != nil {
		return fmt.Errorf("основной конфиг mtg: %w", err)
	}
	second, secondInfo, err := mtgFileLocation(secondary)
	if err != nil {
		return fmt.Errorf("второй конфиг mtg: %w", err)
	}
	if first == second || (firstInfo != nil && secondInfo != nil && os.SameFile(firstInfo, secondInfo)) ||
		strings.HasSuffix(first, tmpSuffix) || strings.HasSuffix(first, bakSuffix) || strings.HasSuffix(second, tmpSuffix) || strings.HasSuffix(second, bakSuffix) {
		return fmt.Errorf("пути конфигов mtg и их служебных файлов должны различаться")
	}
	return nil
}
