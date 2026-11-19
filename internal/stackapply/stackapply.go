// Package stackapply - замена бинаря telemt или mtg-multi на машине (ADR 0008,
// команда `telemux stack-apply`). Запускает её агент через systemd-run, вне своей
// песочницы: агенту запись в /usr/local/bin закрыта.
//
// Порядок: версия текущего бинаря (уже нужная - выход без изменений) -> бинарь у
// мастера (GET /v1/stack/blobs/{sha256}, адреса и мосты агента, первый ответивший)
// во временный файл рядом с целевым, с лимитом размера и сверкой SHA256 -> `--version`
// нового файла сверяется с ожидаемой -> копия текущего в <бинарь>.bak -> атомарная
// замена (rename) -> systemctl restart -> проверка: юнит активен и API продукта отвечает
// непрерывно StableFor, ждём не дольше HealthTimeout. Не поднялся - .bak на место,
// снова restart, ошибка.
//
// Внешние команды, HTTP проверки и время - через поля Config: тесты идут без root и systemd.
// Файлы - настоящие (каталог бинаря в тестах - временный).
package stackapply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/agent"
	"github.com/AndreyOsipuk/telemux/internal/stack"
)

// Runner запускает команду и отдаёт её вывод (agent.CombinedRunner - stdout и stderr вместе).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Config - что и как обновить.
type Config struct {
	Product string // stack.ProductTelemt | stack.ProductMtg
	SHA256  string // sha256 нового бинаря
	Version string // версия, которую должен сообщить новый бинарь
	Size    int64  // размер нового бинаря; больше - ошибка

	Masters   []string // адреса мастера, как TELEMUX_AGENT_MASTERS («адрес|socks5://...»)
	MasterPin string
	Token     string
	Target    string

	Bin       string // абсолютный путь заменяемого бинаря
	Unit      string // юнит systemd продукта
	HealthURL string // API продукта: ответ 2xx - живой

	Deadline        time.Duration // на всё обновление, скачивание со всех мастеров тоже; по умолчанию 8 мин
	HealthTimeout   time.Duration // сколько ждать подъёма; по умолчанию 30 с
	StableFor       time.Duration // сколько юнит должен быть живым непрерывно; по умолчанию 5 с
	PollEvery       time.Duration // по умолчанию 1 с
	DownloadTimeout time.Duration // на скачивание с одного мастера; по умолчанию 5 мин

	Runner Runner                                           // по умолчанию agent.CombinedRunner
	HTTP   *http.Client                                     // для HealthURL; по умолчанию таймаут 3 с
	Now    func() time.Time                                 // по умолчанию time.Now
	Sleep  func(ctx context.Context, d time.Duration) error // по умолчанию пауза с учётом ctx
	Log    io.Writer                                        // ход работы; по умолчанию io.Discard
}

// Result - итог обновления.
type Result struct {
	Product  string
	From, To string
	Already  bool // нужная версия уже стоит, ничего не менялось
}

// String - «telemt: 3.5.16 -> 3.5.17».
func (r Result) String() string {
	if r.Already {
		return fmt.Sprintf("%s: уже %s", r.Product, r.To)
	}
	from := r.From
	if from == "" {
		from = "?"
	}
	return fmt.Sprintf("%s: %s -> %s", r.Product, from, r.To)
}

func (c *Config) defaults() {
	if c.Deadline <= 0 {
		c.Deadline = 8 * time.Minute
	}
	if c.HealthTimeout <= 0 {
		c.HealthTimeout = 30 * time.Second
	}
	if c.StableFor <= 0 {
		c.StableFor = 5 * time.Second
	}
	if c.PollEvery <= 0 {
		c.PollEvery = time.Second
	}
	if c.DownloadTimeout <= 0 {
		c.DownloadTimeout = 5 * time.Minute
	}
	if c.Runner == nil {
		c.Runner = agent.CombinedRunner{}
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 3 * time.Second}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if c.Log == nil {
		c.Log = io.Discard
	}
}

func (c *Config) validate() error {
	switch {
	case !stack.ValidProduct(c.Product):
		return fmt.Errorf("продукт %q: нужен %s или %s", c.Product, stack.ProductTelemt, stack.ProductMtg)
	case !stack.ValidSHA256(c.SHA256):
		return errors.New("sha256: нужны 64 строчных hex-символа")
	case strings.TrimSpace(c.Version) == "":
		return errors.New("нужна версия (--version)")
	case c.Size <= 0:
		return errors.New("нужен размер бинаря (--size > 0)")
	case len(c.Masters) == 0:
		return errors.New("нужен хотя бы один адрес мастера")
	case c.Token == "":
		return errors.New("нужен токен машины")
	case c.Target == "":
		return errors.New("нужен код машины")
	case !filepath.IsAbs(c.Bin):
		return fmt.Errorf("путь бинаря %q не абсолютный", c.Bin)
	case c.Unit == "":
		return errors.New("нужен юнит systemd продукта")
	case c.HealthURL == "":
		return errors.New("нужен адрес API продукта для проверки")
	}
	return nil
}

// ErrInvalid - неверные параметры (код выхода 2).
var ErrInvalid = errors.New("неверные параметры")

// Run обновляет бинарь продукта. Ошибка до замены - бинарь не тронут; после замены -
// бинарь возвращён из .bak (если и это не удалось - в тексте ошибки).
func Run(ctx context.Context, c Config) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	c.defaults()
	// Общий дедлайн: скачивание по всем мастерам и проверка делят один бюджет. Откат
	// идёт со своим таймаутом и в бюджет не входит (юнит ограничен RuntimeMaxSec=540).
	ctx, cancel := context.WithTimeout(ctx, c.Deadline)
	defer cancel()
	res := Result{Product: c.Product, To: c.Version}

	if _, err := os.Stat(c.Bin); err != nil {
		return res, fmt.Errorf("бинарь %s: %w", c.Bin, err)
	}
	res.From = c.binVersion(ctx, c.Bin)
	if res.From != "" && stack.SameVersion(res.From, c.Version) {
		return c.already(ctx, res)
	}

	tmp, err := c.download(ctx)
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp) // после замены файла с этим именем уже нет

	if got := c.binVersion(ctx, tmp); !stack.SameVersion(got, c.Version) {
		if got == "" {
			got = "версию не сообщил"
		}
		return res, fmt.Errorf("новый бинарь: %s, ждали %s - бинарь на машине не тронут", got, c.Version)
	}

	bak := c.Bin + ".bak"
	if err := copyFile(c.Bin, bak); err != nil {
		return res, fmt.Errorf("копия %s: %w - бинарь на машине не тронут", bak, err)
	}
	if err := os.Rename(tmp, c.Bin); err != nil {
		return res, fmt.Errorf("замена %s: %w - бинарь на машине не тронут", c.Bin, err)
	}
	fmt.Fprintf(c.Log, "%s: бинарь заменён, перезапуск %s\n", c.Product, c.Unit)

	if err := c.restartAndWait(ctx); err != nil {
		return res, c.rollback(ctx, err)
	}
	return res, nil
}

// already - нужная версия уже на диске. Прошлый запуск могли убить посреди замены
// (SIGKILL после rename, до проверки), поэтому «уже стоит» - только если сервис жив.
// Не жив - restart и проверка; не поднялся и есть .bak - откат из него.
func (c *Config) already(ctx context.Context, res Result) (Result, error) {
	err := c.alive(ctx, -1)
	if err == nil {
		res.Already = true
		return res, nil
	}
	fmt.Fprintf(c.Log, "%s: версия %s уже на диске, но сервис не жив (%v) - перезапуск\n", c.Product, res.From, err)
	if err = c.restartAndWait(ctx); err == nil {
		return res, nil
	}
	if _, serr := os.Stat(c.Bin + ".bak"); serr != nil {
		return res, fmt.Errorf("%v; .bak нет - откатывать не на что", err)
	}
	return res, c.rollback(ctx, err)
}

// binVersion - версия из `<path> --version` (разбор как в runtime-отчёте агента).
func (c *Config) binVersion(ctx context.Context, path string) string {
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := c.Runner.Run(vctx, path, "--version")
	if err != nil {
		fmt.Fprintf(c.Log, "%s --version: %v\n", path, err)
		return ""
	}
	return agent.ParseVersion(out)
}

// download берёт бинарь у первого мастера, который отдал его целиком и с верной суммой.
// Возвращает путь временного файла рядом с целевым бинарём (0755).
func (c *Config) download(ctx context.Context) (string, error) {
	var errs []error
	for _, entry := range c.Masters {
		base, client, err := agent.MasterClient(entry, c.DownloadTimeout, c.MasterPin)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		tmp, err := c.downloadFrom(ctx, base, client)
		if err == nil {
			return tmp, nil
		}
		fmt.Fprintf(c.Log, "%s: %v\n", base, err)
		errs = append(errs, fmt.Errorf("%s: %w", base, err))
		if ctx.Err() != nil {
			break
		}
	}
	return "", fmt.Errorf("бинарь не получен - бинарь на машине не тронут: %w", errors.Join(errs...))
}

func (c *Config) downloadFrom(ctx context.Context, base string, client *http.Client) (string, error) {
	u := base + "/v1/stack/blobs/" + c.SHA256 + "?" + url.Values{"target": {c.Target}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("мастер ответил %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if resp.ContentLength > c.Size {
		return "", fmt.Errorf("бинарь %d байт больше заявленных %d", resp.ContentLength, c.Size)
	}

	f, err := os.CreateTemp(filepath.Dir(c.Bin), ".telemux-stack-"+c.Product+"-*")
	if err != nil {
		return "", fmt.Errorf("временный файл: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, c.Size+1))
	switch {
	case err != nil:
		return "", fmt.Errorf("скачивание: %w", err)
	case n > c.Size:
		return "", fmt.Errorf("бинарь больше заявленных %d байт", c.Size)
	case n < c.Size:
		return "", fmt.Errorf("бинарь %d байт вместо %d", n, c.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != c.SHA256 {
		return "", fmt.Errorf("sha256 не сошёлся: ждали %s, получили %s", c.SHA256, got)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("запись: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("запись: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		return "", err
	}
	keep = true
	return f.Name(), nil
}

// restartAndWait перезапускает юнит и ждёт, пока он живой непрерывно StableFor.
func (c *Config) restartAndWait(ctx context.Context) error {
	if out, err := c.Runner.Run(ctx, "systemctl", "restart", c.Unit); err != nil {
		return fmt.Errorf("systemctl restart %s: %v %s", c.Unit, err, strings.TrimSpace(string(out)))
	}
	restarts := c.nRestarts(ctx)
	deadline := c.Now().Add(c.HealthTimeout)
	var since time.Time
	last := "проверок не было"
	for {
		now := c.Now()
		err := c.alive(ctx, restarts)
		if err == nil {
			if since.IsZero() {
				since = now
			}
			if now.Sub(since) >= c.StableFor {
				fmt.Fprintf(c.Log, "%s: %s живой %s\n", c.Product, c.Unit, c.StableFor)
				return nil
			}
		} else {
			since, last = time.Time{}, err.Error()
		}
		if !now.Before(deadline) {
			return fmt.Errorf("%s не поднялся за %s: %s", c.Unit, c.HealthTimeout, last)
		}
		if err := c.Sleep(ctx, c.PollEvery); err != nil {
			return fmt.Errorf("%s: проверка прервана: %w", c.Unit, err)
		}
	}
}

// alive - юнит active, systemd не перезапускал его сам (NRestarts тот же) и API отвечает 2xx.
func (c *Config) alive(ctx context.Context, restarts int) error {
	out, err := c.Runner.Run(ctx, "systemctl", "is-active", c.Unit)
	if state := strings.TrimSpace(string(out)); err != nil || state != "active" {
		if state == "" && err != nil {
			state = err.Error()
		}
		return fmt.Errorf("юнит %s", state)
	}
	if restarts >= 0 {
		if n := c.nRestarts(ctx); n > restarts {
			return fmt.Errorf("юнит перезапускался (NRestarts %d)", n)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.HealthURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("API: %v", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("API: HTTP %d", resp.StatusCode)
	}
	return nil
}

// nRestarts - сколько раз systemd перезапускал юнит сам; -1 - не узнать.
func (c *Config) nRestarts(ctx context.Context) int {
	out, err := c.Runner.Run(ctx, "systemctl", "show", c.Unit, "-p", "NRestarts", "--value")
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1
	}
	return n
}

// rollback возвращает .bak на место и перезапускает юнит.
func (c *Config) rollback(ctx context.Context, cause error) error {
	fmt.Fprintf(c.Log, "%s: %v - откат на %s.bak\n", c.Product, cause, c.Bin)
	// Отдельный контекст: откат нужен и если проверку прервали.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := copyFile(c.Bin+".bak", c.Bin); err != nil { // .bak остаётся для разбора
		return fmt.Errorf("%v; откат не удался: %v", cause, err)
	}
	if out, err := c.Runner.Run(rctx, "systemctl", "restart", c.Unit); err != nil {
		return fmt.Errorf("%v; прежний бинарь возвращён, но systemctl restart %s: %v %s",
			cause, c.Unit, err, strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("%v; прежний бинарь возвращён", cause)
}

// copyFile копирует файл с правами источника: во временный рядом и rename - на месте
// dst никогда не бывает половины файла.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), ".telemux-copy-*")
	if err != nil {
		return err
	}
	name := out.Name()
	defer os.Remove(name) // после rename файла с этим именем уже нет
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, fi.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(name, dst)
}
