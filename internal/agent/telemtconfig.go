package agent

// Применение основного telemt.toml (ресурс telemt.config).
//
// Кто чем владеет в telemt.toml (подробно - telemtcfg/compose.go):
//   - база - текст ресурса telemt.config от бота;
//   - машинная часть - файл telemt.local.toml на машине (WEB-режим, локальный
//     listener, mask_port, свои каталоги), сильнее базы;
//   - WEB-профили - из users.toml машины, их же пересобирает ресурс telemt.users.
// Агент собирает файл из трёх частей; telemt.users при этом трогает только блок
// профилей, поэтому два ресурса сходятся к одному и тому же тексту.
//
// Порядок: сборка и проверки (WEB-режим не теряется, include ведёт на users.toml,
// API не выключен) -> место -> временный файл со сверкой -> бэкап -> замена ->
// SIGHUP -> telemt проверил и принял конфиг (/v1/config/state) -> если часть полей
// telemt применит только рестартом - рестарт и проверка, что процесс поднялся и
// systemd его не перезапускает -> при любом сбое возврат прежнего файла.
// Рестарт делается только после того, как telemt сам признал конфиг валидным:
// невалидный файл при рестарте уводит сервис в цикл перезапусков.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

// DefaultConfigStateURL - состояние всего конфига telemt (все секции).
const DefaultConfigStateURL = "http://127.0.0.1:9091/v1/config/state"

// cfgBakSuffix - бэкап основного конфига. Свой суффикс: бэкап telemt.toml от
// применения профилей (bakSuffix) и от telemt.config не путаются.
const cfgBakSuffix = ".telemux-cfg-bak"

// TelemtService управляет процессом telemt.
type TelemtService interface {
	Reload(ctx context.Context) error
	Restart(ctx context.Context) error
	Active(ctx context.Context) bool
	Restarts(ctx context.Context) (int, error) // счётчик авто-перезапусков systemd (NRestarts)
}

// SystemdTelemt - telemt под systemd.
type SystemdTelemt struct{ Unit string }

func (s SystemdTelemt) unit() string {
	if s.Unit == "" {
		return "telemt"
	}
	return s.Unit
}

// Reload - SIGHUP процессу.
func (s SystemdTelemt) Reload(ctx context.Context) error {
	return SystemdReloader{Unit: s.unit()}.Reload(ctx)
}

// Restart - перезапуск юнита.
func (s SystemdTelemt) Restart(ctx context.Context) error {
	return runCmd(ctx, "systemctl", "restart", s.unit())
}

// Active - юнит активен.
func (s SystemdTelemt) Active(ctx context.Context) bool {
	return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", s.unit()).Run() == nil
}

// Restarts - NRestarts юнита.
func (s SystemdTelemt) Restarts(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", s.unit(), "-p", "NRestarts", "--value").Output()
	if err != nil {
		return 0, fmt.Errorf("systemctl show %s NRestarts: %w", s.unit(), err)
	}
	return parseNRestarts(string(out))
}

func parseNRestarts(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("NRestarts %q: %w", strings.TrimSpace(s), err)
	}
	return n, nil
}

// ConfigApplier применяет основной telemt.toml.
type ConfigApplier struct {
	ConfigFile     string        // telemt.toml
	LocalFile      string        // машинная часть; файла нет - пусто
	UsersFile      string        // users.toml: профили и проверка include; "" - не ведётся
	StateURL       string        // по умолчанию DefaultConfigStateURL
	Service        TelemtService // по умолчанию SystemdTelemt{"telemt"}
	ConfirmTimeout time.Duration // ожидание подтверждения; по умолчанию 15 с
	PollEvery      time.Duration // по умолчанию 500 мс
	SettleTime     time.Duration // наблюдение после рестарта (больше RestartSec=3); по умолчанию 8 с
	FreeSpace      func(dir string) (uint64, error)

	writeFile func(path string, data []byte, perm os.FileMode) error
}

func (a *ConfigApplier) defaults() {
	if a.StateURL == "" {
		a.StateURL = DefaultConfigStateURL
	}
	if a.Service == nil {
		a.Service = SystemdTelemt{}
	}
	if a.ConfirmTimeout <= 0 {
		a.ConfirmTimeout = 15 * time.Second
	}
	if a.PollEvery <= 0 {
		a.PollEvery = 500 * time.Millisecond
	}
	if a.SettleTime <= 0 {
		a.SettleTime = 8 * time.Second
	}
	if a.FreeSpace == nil {
		a.FreeSpace = freeSpace
	}
	if a.writeFile == nil {
		a.writeFile = writeFileSync
	}
}

// mainConfigInputs - отпечаток машинных входов сборки (машинная часть и users.toml):
// неудачная ревизия telemt.config повторяется, если они изменились.
func mainConfigInputs(localFile, usersFile string) string {
	h := sha256.New()
	for _, p := range []string{localFile, usersFile} {
		b, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(h, "нет:%s\n", p)
			continue
		}
		fmt.Fprintf(h, "%s:%d\n", p, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildMainConfig собирает желаемый telemt.toml и проверяет его против текущего.
func buildMainConfig(configFile, localFile, usersFile string, base []byte) (cur, next string, err error) {
	raw, err := os.ReadFile(configFile)
	if err != nil {
		return "", "", fmt.Errorf("telemt.toml не читается: %v", err)
	}
	var local []byte
	if localFile != "" {
		if local, err = os.ReadFile(localFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("машинная часть %s не читается: %v", localFile, err)
		}
	}
	var users []string
	if usersFile != "" {
		u, err := os.ReadFile(usersFile)
		if err != nil {
			return "", "", fmt.Errorf("users.toml не читается: %v", err)
		}
		users = telemtcfg.UserKeys(u)
	}
	next, err = telemtcfg.ComposeMainConfigFrom(string(base), string(local), string(raw), users)
	if err != nil {
		return "", "", err
	}
	if err := telemtcfg.GuardMainConfig(string(raw), next, usersFile); err != nil {
		return "", "", err
	}
	return string(raw), next, nil
}

// Apply приводит telemt.toml к базе + машинной части + профилям.
func (a *ConfigApplier) Apply(ctx context.Context, base []byte) ApplyResult {
	a.defaults()
	cur, next, err := buildMainConfig(a.ConfigFile, a.LocalFile, a.UsersFile, base)
	if err != nil {
		return failed("%v", err)
	}
	if cur == next {
		return ApplyResult{Status: "applied"}
	}
	need := uint64(2*(len(cur)+len(next))) + 1<<20
	if free, err := a.FreeSpace(filepath.Dir(a.ConfigFile)); err != nil {
		return failed("свободное место не проверить: %v", err)
	} else if free < need {
		return failed("мало места на диске: свободно %d байт, нужно %d", free, need)
	}
	tmp, bak := a.ConfigFile+tmpSuffix, a.ConfigFile+cfgBakSuffix
	if err := a.writeFile(tmp, []byte(next), 0o600); err != nil {
		_ = os.Remove(tmp)
		return failed("запись %s: %v", tmp, err)
	}
	if got, err := os.ReadFile(tmp); err != nil || !bytes.Equal(got, []byte(next)) {
		_ = os.Remove(tmp)
		return failed("записанный %s обрезан или не совпал с желаемым (%d из %d байт)", tmp, len(got), len(next))
	}
	if err := a.writeFile(bak, []byte(cur), 0o600); err != nil {
		_ = os.Remove(tmp)
		return failed("бэкап %s: %v", a.ConfigFile, err)
	}
	if err := os.Rename(tmp, a.ConfigFile); err != nil {
		_ = os.Remove(tmp)
		return failed("замена %s: %v", a.ConfigFile, err)
	}

	// 1. Перезагрузка: telemt сам проверяет конфиг, отвергнутый он не применяет.
	if err := a.Service.Reload(ctx); err != nil {
		return a.rollback(ctx, fmt.Sprintf("перезагрузка telemt: %v", err), false)
	}
	st, reason, deferred := waitTelemtState(ctx, a.StateURL, a.ConfirmTimeout, a.PollEvery)
	switch st {
	case stateAccepted:
	case stateRejected:
		return a.rollback(ctx, "telemt отверг конфиг: "+reason, false)
	default:
		return a.rollback(ctx, fmt.Sprintf("telemt не подтвердил применение за %s (%s)", a.ConfirmTimeout, reason), false)
	}
	if len(deferred) == 0 {
		return ApplyResult{Status: "applied"}
	}

	// 2. Часть полей применяется только рестартом (порт, listener и т.п.).
	cause := fmt.Sprintf("рестарт ради %s", strings.Join(deferred, ", "))
	if err := a.Service.Restart(ctx); err != nil {
		return a.rollback(ctx, fmt.Sprintf("%s: %v", cause, err), true)
	}
	if ok, why := a.healthyAfterRestart(ctx); !ok {
		return a.rollback(ctx, fmt.Sprintf("%s: %s", cause, why), true)
	}
	return ApplyResult{Status: "applied"}
}

// healthyAfterRestart: API подтвердил конфиг без отложенных полей, процесс активен
// и systemd не перезапускал его за время наблюдения (цикл падений между
// попытками тоже выглядит как active).
func (a *ConfigApplier) healthyAfterRestart(ctx context.Context) (bool, string) {
	before, err := a.Service.Restarts(ctx)
	if err != nil {
		return false, err.Error()
	}
	st, reason, deferred := waitTelemtState(ctx, a.StateURL, a.ConfirmTimeout, a.PollEvery)
	switch {
	case st != stateAccepted:
		return false, "telemt после рестарта не подтвердил конфиг (" + reason + ")"
	case len(deferred) > 0:
		return false, "после рестарта telemt всё ещё ждёт применения " + strings.Join(deferred, ", ")
	}
	select {
	case <-ctx.Done():
		return false, "прервано: " + ctx.Err().Error()
	case <-time.After(a.SettleTime):
	}
	after, err := a.Service.Restarts(ctx)
	if err != nil {
		return false, err.Error()
	}
	if !a.Service.Active(ctx) || after > before {
		return false, fmt.Sprintf("telemt не держится на новом конфиге (активен %v, авто-перезапусков %d -> %d)", a.Service.Active(ctx), before, after)
	}
	return true, ""
}

// rollback возвращает прежний telemt.toml; restart - процесс уже перезапускался
// на новом конфиге, поэтому и возврат - рестартом.
func (a *ConfigApplier) rollback(ctx context.Context, cause string, restart bool) ApplyResult {
	ctx, cancel := rollbackCtx(ctx)
	defer cancel()
	if err := os.Rename(a.ConfigFile+cfgBakSuffix, a.ConfigFile); err != nil {
		return failed("%s; возврат прежнего не удался: %v", cause, err)
	}
	var err error
	if restart {
		err = a.Service.Restart(ctx)
	} else {
		err = a.Service.Reload(ctx)
	}
	if err != nil {
		return failed("%s; прежний telemt.toml возвращён, но telemt не перечитал его: %v", cause, err)
	}
	return failed("%s; прежний telemt.toml возвращён", cause)
}
