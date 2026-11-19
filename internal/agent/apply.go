package agent

// Применение users.toml и WEB-профилей telemt на машине (этап 3 плана).
//
// Порядок повторяет проверенный скрипт синка релеев бота:
// проверка места -> запись во временные файлы и сверка -> бэкап -> замена
// обоих файлов одним шагом -> одна перезагрузка -> ожидание «telemt принял»
// -> при отказе или без подтверждения - возврат обоих файлов.
// Успех = telemt подтвердил конфиг, а не «файл на месте и процесс жив»:
// отвергнутая перезагрузка не роняет telemt, он молча работает на старом.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

// DefaultStateURL - API telemt на самой машине: принят ли конфиг (с 3.5.17).
const DefaultStateURL = "http://127.0.0.1:9091/v1/config/state?sections=access"

const (
	tmpSuffix = ".telemux-tmp"
	bakSuffix = ".telemux-bak"
)

// Reloader просит telemt перечитать конфиг.
type Reloader interface {
	Reload(ctx context.Context) error
}

// SystemdReloader - SIGHUP процессу юнита через systemctl.
type SystemdReloader struct{ Unit string }

// Reload шлёт SIGHUP юниту.
func (r SystemdReloader) Reload(ctx context.Context) error {
	unit := r.Unit
	if unit == "" {
		unit = "telemt"
	}
	out, err := exec.CommandContext(ctx, "systemctl", "kill", "-s", "HUP", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl kill -s HUP %s: %v: %s", unit, err, bytes.TrimSpace(out))
	}
	return nil
}

// Applier применяет желаемый users.toml к telemt на машине.
type Applier struct {
	UsersFile      string
	ConfigFile     string
	StateURL       string        // по умолчанию DefaultStateURL
	Reloader       Reloader      // по умолчанию SystemdReloader{"telemt"}
	ConfirmTimeout time.Duration // сколько ждать подтверждения; по умолчанию 12 с
	PollEvery      time.Duration // как часто спрашивать API; по умолчанию 500 мс
	FreeSpace      func(dir string) (uint64, error)

	writeFile func(path string, data []byte, perm os.FileMode) error // для тестов
}

// ApplyResult - итог применения для отчёта.
type ApplyResult struct {
	Status string // applied | failed
	Error  string
}

func failed(format string, a ...any) ApplyResult {
	return ApplyResult{Status: "failed", Error: fmt.Sprintf(format, a...)}
}

func (a *Applier) defaults() {
	if a.StateURL == "" {
		a.StateURL = DefaultStateURL
	}
	if a.Reloader == nil {
		a.Reloader = SystemdReloader{Unit: "telemt"}
	}
	if a.ConfirmTimeout <= 0 {
		a.ConfirmTimeout = 12 * time.Second
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

// Apply приводит users.toml и WEB-профили к желаемому и ждёт подтверждения telemt.
func (a *Applier) Apply(ctx context.Context, desired []byte) ApplyResult {
	a.defaults()
	curUsers, err := os.ReadFile(a.UsersFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return failed("users.toml не читается: %v", err)
	}
	curCfg, err := os.ReadFile(a.ConfigFile)
	if err != nil {
		return failed("telemt.toml не читается: %v", err)
	}

	keys := telemtcfg.UserKeys(desired)
	if len(keys) == 0 && len(telemtcfg.UserKeys(curUsers)) > 0 {
		return failed("желаемый список пуст, а на машине есть пользователи - похоже на недогруженную базу мастера, не применяю")
	}
	newCfg, wp := telemtcfg.ApplyWebProfiles(string(curCfg), keys)
	if wp.Skipped == telemtcfg.SkipOverLimit {
		return failed("WEB-профилей нужно %d, больше max_profiles в telemt.toml - telemt такой конфиг не примет", len(keys))
	}
	usersChanged := !bytes.Equal(curUsers, desired)
	cfgChanged := newCfg != string(curCfg)
	if !usersChanged && !cfgChanged {
		return ApplyResult{Status: "applied"}
	}

	// Место: временные файлы + бэкапы + запас.
	need := uint64(2*(len(desired)+len(newCfg))) + 1<<20
	if free, err := a.FreeSpace(filepath.Dir(a.UsersFile)); err != nil {
		return failed("свободное место не проверить: %v", err)
	} else if free < need {
		return failed("мало места на диске: свободно %d байт, нужно %d", free, need)
	}

	type change struct {
		path       string
		old, next  []byte
		existedOld bool
	}
	var changes []change
	if usersChanged {
		changes = append(changes, change{a.UsersFile, curUsers, desired, curUsers != nil})
	}
	if cfgChanged {
		changes = append(changes, change{a.ConfigFile, curCfg, []byte(newCfg), true})
	}

	// Временные файлы рядом с рабочими, сверка записанного.
	cleanup := func() {
		for _, c := range changes {
			_ = os.Remove(c.path + tmpSuffix)
		}
	}
	for _, c := range changes {
		if err := a.writeFile(c.path+tmpSuffix, c.next, 0o600); err != nil {
			cleanup()
			return failed("запись %s: %v", c.path+tmpSuffix, err)
		}
		got, err := os.ReadFile(c.path + tmpSuffix)
		if err != nil || !bytes.Equal(got, c.next) {
			cleanup()
			return failed("записанный %s обрезан или не совпал с желаемым (%d из %d байт)", c.path+tmpSuffix, len(got), len(c.next))
		}
	}
	// Бэкапы прежних файлов.
	for _, c := range changes {
		if !c.existedOld {
			continue
		}
		if err := a.writeFile(c.path+bakSuffix, c.old, 0o600); err != nil {
			cleanup()
			return failed("бэкап %s: %v", c.path, err)
		}
	}
	// Откатываются только файлы этой попытки: бэкап несменённого файла может
	// остаться от прошлого применения и быть устаревшим. Файл, которого раньше не
	// было, при откате удаляется, а не поднимается из давнего бэкапа.
	var paths []rollbackPath
	for _, c := range changes {
		paths = append(paths, rollbackPath{path: c.path, existed: c.existedOld})
	}
	// Замена одним шагом: оба rename подряд, затем одна перезагрузка.
	for _, c := range changes {
		if err := os.Rename(c.path+tmpSuffix, c.path); err != nil {
			cleanup()
			return a.rollback(ctx, fmt.Sprintf("замена %s: %v", c.path, err), paths)
		}
	}
	if err := a.Reloader.Reload(ctx); err != nil {
		return a.rollback(ctx, fmt.Sprintf("перезагрузка telemt: %v", err), paths)
	}
	state, reason := a.waitConfirm(ctx)
	switch state {
	case stateAccepted:
		return ApplyResult{Status: "applied"}
	case stateRejected:
		return a.rollback(ctx, "telemt отверг конфиг: "+reason, paths)
	default:
		return a.rollback(ctx, fmt.Sprintf("telemt не подтвердил применение за %s (%s)", a.ConfirmTimeout, reason), paths)
	}
}

// rollbackPath - файл попытки: был ли он до неё.
type rollbackPath struct {
	path    string
	existed bool
}

// rollbackCtx - контекст отката: он должен дойти до telemt, даже если агента
// останавливают (SIGTERM отменил ctx посреди применения), иначе на диске прежний
// файл, а процесс работает на новом.
func rollbackCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

// rollback возвращает изменённые файлы из бэкапов и перезагружает telemt на них.
func (a *Applier) rollback(ctx context.Context, cause string, paths []rollbackPath) ApplyResult {
	ctx, cancel := rollbackCtx(ctx)
	defer cancel()
	var errs []string
	for _, p := range paths {
		if !p.existed {
			if err := os.Remove(p.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Sprintf("удаление нового %s: %v", p.path, err))
			}
			continue
		}
		if err := os.Rename(p.path+bakSuffix, p.path); err != nil {
			errs = append(errs, fmt.Sprintf("возврат %s: %v", p.path, err))
		}
	}
	if err := a.Reloader.Reload(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("перезагрузка после возврата: %v", err))
	}
	if len(errs) > 0 {
		return failed("%s; откат не полностью: %s", cause, strings.Join(errs, "; "))
	}
	return failed("%s; прежние файлы возвращены", cause)
}

type confirmState int

const (
	stateUnknown confirmState = iota
	statePending
	stateAccepted
	stateRejected
)

// waitConfirm спрашивает API telemt, пока тот не подтвердит или не отвергнет конфиг.
func (a *Applier) waitConfirm(ctx context.Context) (confirmState, string) {
	st, reason, _ := waitTelemtState(ctx, a.StateURL, a.ConfirmTimeout, a.PollEvery)
	return st, reason
}

// waitTelemtState спрашивает /v1/config/state, пока telemt не подтвердит или не
// отвергнет конфиг на диске. deferred - поля, которые telemt применит только
// после рестарта процесса (при подтверждении).
func waitTelemtState(ctx context.Context, url string, timeout, every time.Duration) (confirmState, string, []string) {
	deadline := time.Now().Add(timeout)
	last, reason := stateUnknown, "API telemt не ответил"
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		st, msg, deferred := probeTelemtState(ctx, client, url)
		switch st {
		case stateAccepted, stateRejected:
			return st, msg, deferred
		case statePending:
			last, reason = st, "конфиг ещё не применён: "+msg
		case stateUnknown:
			if last == stateUnknown {
				reason = msg
			}
		}
		if time.Now().After(deadline) {
			return last, reason, nil
		}
		select {
		case <-ctx.Done():
			return last, "прервано: " + ctx.Err().Error(), nil
		case <-time.After(every):
		}
	}
}

// probeTelemtState разбирает ответ /v1/config/state (та же логика, что у бота).
func probeTelemtState(ctx context.Context, client *http.Client, url string) (confirmState, string, []string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return stateUnknown, err.Error(), nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return stateUnknown, err.Error(), nil
	}
	defer resp.Body.Close()
	var d struct {
		OK   bool `json:"ok"`
		Data *struct {
			DesiredStatus string `json:"desired_status"`
			DesiredError  struct {
				Path, Code, Message string
			} `json:"desired_error"`
			Watcher struct {
				Phase     string `json:"phase"`
				ErrorCode string `json:"error_code"`
			} `json:"watcher"`
			WatcherMatchesDesired *bool `json:"watcher_matches_desired"`
			Comparison            struct {
				Status                string   `json:"status"`
				ProcessDeferredPaths  []string `json:"process_deferred_paths"`
				DeferredProcessFields []string `json:"deferred_process_fields"`
			} `json:"comparison"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&d); err != nil || !d.OK || d.Data == nil {
		return stateUnknown, fmt.Sprintf("API ответил %d без данных", resp.StatusCode), nil
	}
	x := d.Data
	switch x.DesiredStatus {
	case "invalid", "unreadable":
		msg := strings.TrimSpace(strings.Join([]string{x.DesiredError.Path, x.DesiredError.Code, x.DesiredError.Message}, " "))
		if msg == "" {
			msg = x.DesiredStatus
		}
		return stateRejected, "конфиг на диске не проходит проверку: " + msg, nil
	case "valid":
	default:
		return stateUnknown, "неизвестный desired_status " + x.DesiredStatus, nil
	}
	matches := x.WatcherMatchesDesired
	if (x.Watcher.Phase == "rejected" || x.Watcher.Phase == "publication_failed") && matches != nil && *matches {
		return stateRejected, strings.TrimSpace("перезагрузка отвергнута (" + x.Watcher.Phase + " " + x.Watcher.ErrorCode + ")"), nil
	}
	if x.Comparison.Status == "pending_publication" || (matches != nil && !*matches) {
		return statePending, x.Comparison.Status, nil
	}
	deferred := append(append([]string(nil), x.Comparison.ProcessDeferredPaths...), x.Comparison.DeferredProcessFields...)
	return stateAccepted, x.Comparison.Status, deferred
}

// writeFileSync пишет файл и сбрасывает его на диск.
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
