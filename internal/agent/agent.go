// Package agent - агент telemux на машине флота (ADR 0001): сам забирает
// желаемое состояние у мастера по HTTP long-poll и отчитывается о нём.
//
// Режимы: compare - сравнить users.toml на диске с желаемым и отчитаться, ничего
// не меняя; apply - применить (users.toml + WEB-профили, проверка «telemt принял»,
// откат при отказе, см. apply.go).
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

const (
	// ModeCompare - только сравнивать с желаемым, ничего не менять на машине.
	ModeCompare = "compare"
	// ModeApply - применять желаемое с проверкой и откатом.
	ModeApply = "apply"
)

// Config - настройки агента.
type Config struct {
	MasterURLs   []string      // адреса мастера по приоритету (у релея - только через туннели)
	Token        string        // токен машины
	Target       string        // код машины
	Version      string        // версия агента для отчёта
	Via          string        // через какой мост ходит агент (для отчёта), необязательно
	UsersFile    string        // путь к users.toml на машине
	Mode         string        // ModeCompare или ModeApply
	Applier      *Applier      // для ModeApply; UsersFile берётся из конфига агента
	Wait         time.Duration // сколько мастер держит long-poll; по умолчанию 55 с
	RecheckEvery time.Duration // как часто перепроверять диск без новой ревизии; по умолчанию 5 мин
	Log          *slog.Logger
}

// Agent - агент одной машины.
type Agent struct {
	cfg    Config
	client *http.Client

	mu        sync.Mutex
	known     int64          // последняя полученная ревизия
	users     *store.Desired // последнее желаемое telemt.users
	lastCheck time.Time      // когда последний раз сверяли диск
	force     bool           // перепроверить диск в следующем цикле

	failedRev int64  // ревизия, которую telemt отверг: повторно не применяется
	failedErr string // причина отказа для повторных отчётов
}

// New проверяет конфиг и создаёт агента.
func New(cfg Config) (*Agent, error) {
	switch {
	case len(cfg.MasterURLs) == 0:
		return nil, errors.New("нужен хотя бы один адрес мастера")
	case cfg.Token == "":
		return nil, errors.New("нужен токен машины")
	case cfg.Target == "":
		return nil, errors.New("нужен код машины")
	case cfg.UsersFile == "":
		return nil, errors.New("нужен путь к users.toml")
	case cfg.Mode != ModeCompare && cfg.Mode != ModeApply:
		return nil, fmt.Errorf("режим %q не поддержан: %q или %q", cfg.Mode, ModeCompare, ModeApply)
	case cfg.Mode == ModeApply && cfg.Applier == nil:
		return nil, errors.New("для режима apply нужен Applier")
	}
	if cfg.Applier != nil {
		cfg.Applier.UsersFile = cfg.UsersFile
	}
	if cfg.Wait <= 0 {
		cfg.Wait = 55 * time.Second
	}
	if cfg.RecheckEvery <= 0 {
		cfg.RecheckEvery = 5 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Agent{cfg: cfg, client: &http.Client{Timeout: cfg.Wait + 15*time.Second}}, nil
}

// ForceRecheck просит сверить диск в следующем цикле, даже без новой ревизии.
func (a *Agent) ForceRecheck() {
	a.mu.Lock()
	a.force = true
	a.mu.Unlock()
}

// Run крутит циклы до отмены ctx; при недоступности мастеров - с нарастающей паузой.
func (a *Agent) Run(ctx context.Context) {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		if _, err := a.RunOnce(ctx); err != nil {
			a.cfg.Log.Warn("агент: цикл не удался", "err", err, "пауза", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = 2 * time.Second
	}
}

// RunOnce - один цикл: long-poll к первому доступному мастеру, сравнение,
// отчёт. Возвращает последнюю известную ревизию.
func (a *Agent) RunOnce(ctx context.Context) (int64, error) {
	a.mu.Lock()
	known := a.known
	a.mu.Unlock()

	var errs []error
	for _, base := range a.cfg.MasterURLs {
		bundle, changed, err := a.poll(ctx, base, known)
		if err != nil {
			var fatal *fatalError
			if errors.As(err, &fatal) {
				return known, err
			}
			errs = append(errs, fmt.Errorf("%s: %w", base, err))
			continue
		}
		if changed {
			a.mu.Lock()
			a.known = bundle.Revision
			for i := range bundle.Resources {
				if bundle.Resources[i].Resource == store.ResourceTelemtUsers {
					d := bundle.Resources[i]
					a.users = &d
				}
			}
			a.force = true
			a.mu.Unlock()
		}
		if err := a.checkAndReport(ctx, base); err != nil {
			return a.knownRev(), err
		}
		return a.knownRev(), nil
	}
	return known, fmt.Errorf("ни один мастер не ответил: %w", errors.Join(errs...))
}

func (a *Agent) knownRev() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.known
}

// fatalError - ошибка, при которой перебирать другие адреса бессмысленно (токен).
type fatalError struct{ msg string }

func (e *fatalError) Error() string { return e.msg }

// poll - один long-poll запрос. changed=false - новее нет (304).
func (a *Agent) poll(ctx context.Context, base string, known int64) (store.Bundle, bool, error) {
	var b store.Bundle
	q := url.Values{"target": {a.cfg.Target}, "rev": {strconv.FormatInt(known, 10)}, "wait": {a.cfg.Wait.String()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/desired?"+q.Encode(), nil)
	if err != nil {
		return b, false, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	resp, err := a.client.Do(req)
	if err != nil {
		return b, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&b); err != nil {
			return b, false, fmt.Errorf("бандл не читается: %w", err)
		}
		return b, true, nil
	case http.StatusNotModified:
		return b, false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return b, false, &fatalError{fmt.Sprintf("мастер отверг токен машины %q: %d", a.cfg.Target, resp.StatusCode)}
	default:
		return b, false, fmt.Errorf("мастер ответил %d", resp.StatusCode)
	}
}

// checkAndReport сверяет диск с желаемым, если пришла новая ревизия или пора
// перепроверить, и отправляет отчёт на тот же мастер.
func (a *Agent) checkAndReport(ctx context.Context, base string) error {
	a.mu.Lock()
	d := a.users
	due := d != nil && (a.force || time.Since(a.lastCheck) >= a.cfg.RecheckEvery)
	a.mu.Unlock()
	if !due {
		return nil
	}
	var res store.ObservedResource
	if a.cfg.Mode == ModeApply {
		res = a.applyUsers(ctx, d)
	} else {
		res = a.compareUsers(d)
	}
	rep := store.ObservedReport{Target: a.cfg.Target, AgentVersion: a.cfg.Version, Via: a.cfg.Via, Resources: []store.ObservedResource{res}}
	if err := a.report(ctx, base, rep); err != nil {
		return err
	}
	a.mu.Lock()
	a.lastCheck = time.Now()
	a.force = false
	a.mu.Unlock()
	a.cfg.Log.Info("агент: сверка", "revision", d.Revision, "status", res.Status, "diff", res.Error)
	return nil
}

func (a *Agent) compareUsers(d *store.Desired) store.ObservedResource {
	res := store.ObservedResource{Resource: store.ResourceTelemtUsers, Revision: d.Revision, SHA256: d.SHA256, Status: "compared"}
	onDisk, err := os.ReadFile(a.cfg.UsersFile)
	if err != nil {
		res.Status = "failed"
		res.Error = fmt.Sprintf("users.toml не читается: %v", err)
	} else if eq, diff := CompareUsers(d.Content, onDisk); !eq {
		res.Error = diff
	}
	return res
}

// applyUsers применяет ревизию; отвергнутую ревизию не повторяет - ждёт новую.
func (a *Agent) applyUsers(ctx context.Context, d *store.Desired) store.ObservedResource {
	res := store.ObservedResource{Resource: store.ResourceTelemtUsers, Revision: d.Revision, SHA256: d.SHA256}
	a.mu.Lock()
	failedRev, failedErr := a.failedRev, a.failedErr
	a.mu.Unlock()
	if failedRev == d.Revision {
		res.Status, res.Error = "failed", failedErr
		return res
	}
	r := a.cfg.Applier.Apply(ctx, d.Content)
	res.Status, res.Error = r.Status, r.Error
	a.mu.Lock()
	if r.Status == "failed" {
		a.failedRev, a.failedErr = d.Revision, r.Error
	} else {
		a.failedRev, a.failedErr = 0, ""
	}
	a.mu.Unlock()
	return res
}

func (a *Agent) report(ctx context.Context, base string, rep store.ObservedReport) error {
	body, _ := json.Marshal(rep)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/observed", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("отчёт: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("отчёт: мастер ответил %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// CompareUsers сравнивает два users.toml по смыслу: набор пользователей,
// секреты, сроки (до секунды) и лимиты подключений. Порядок строк, комментарии
// и формат времени не важны. diff - краткое описание расхождения.
func CompareUsers(desired, onDisk []byte) (bool, string) {
	type rec struct {
		secret  string
		expires int64 // unix-секунды, 0 - без срока
		conns   int   // -1 - не задан
	}
	index := func(data []byte) map[string]rec {
		m := map[string]rec{}
		for _, u := range telemtcfg.ParseUsersToml(string(data)) {
			r := rec{secret: u.Secret, conns: -1}
			if u.ExpirationAt != nil {
				r.expires = u.ExpirationAt.Unix()
			}
			if u.MaxTCPConns != nil {
				r.conns = *u.MaxTCPConns
			}
			m[u.Username] = r
		}
		return m
	}
	want, have := index(desired), index(onDisk)
	var missing, extra, differ []string
	for k, w := range want {
		h, ok := have[k]
		switch {
		case !ok:
			missing = append(missing, k)
		case h != w:
			differ = append(differ, k)
		}
	}
	for k := range have {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	if len(missing)+len(extra)+len(differ) == 0 {
		return true, ""
	}
	var parts []string
	add := func(title string, keys []string) {
		if len(keys) == 0 {
			return
		}
		sort.Strings(keys)
		sample := keys
		if len(sample) > 5 {
			sample = sample[:5]
		}
		parts = append(parts, fmt.Sprintf("%s: %d (%s)", title, len(keys), strings.Join(sample, ", ")))
	}
	add("нет на машине", missing)
	add("лишних на машине", extra)
	add("отличаются", differ)
	return false, strings.Join(parts, "; ")
}
