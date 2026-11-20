// Package agent - агент telemux на машине флота (ADR 0001): сам забирает
// желаемое состояние у мастера по HTTP long-poll и отчитывается о нём.
//
// Режимы: compare - сравнить файлы на диске с желаемым и отчитаться, ничего
// не меняя; apply - применить (users.toml + WEB-профили, основной telemt.toml,
// config.toml mtg, сертификаты nginx; проверка «принято», откат при отказе -
// apply.go, telemtconfig.go, mtg.go, tlsweb.go).
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

	"github.com/AndreyOsipuk/telemux/internal/stack"
	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
	"github.com/AndreyOsipuk/telemux/internal/tlspin"
)

const (
	// ModeCompare - только сравнивать с желаемым, ничего не менять на машине.
	ModeCompare = "compare"
	// ModeApply - применять желаемое с проверкой и откатом.
	ModeApply = "apply"
)

// Config - настройки агента.
type Config struct {
	MasterURLs []string // адреса мастера по приоритету; "адрес|прокси" - через свой туннель (socks5://...)
	MasterPin  string   // sha256 сертификата мастера для https-адресов (закрепление)
	Token      string   // токен машины
	Target     string   // код машины
	Version    string   // версия агента для отчёта
	Via        string   // через какой мост ходит агент (для отчёта), необязательно
	UsersFile  string   // путь к users.toml на машине; пусто - telemt.users не ведётся
	MtgFile    string   // путь к config.toml mtg-multi; пусто - mtg.config не ведётся
	// TelemtConfigFile - telemt.toml под ресурсом telemt.config; пусто - не ведётся.
	TelemtConfigFile string
	// TelemtLocalFile - машинная часть telemt.toml (telemtcfg/compose.go); файла может не быть.
	TelemtLocalFile string
	ConfigApplier   *ConfigApplier // для ModeApply и telemt.config; пути берутся из конфига агента
	Mode            string         // ModeCompare или ModeApply
	Applier         *Applier       // для ModeApply и telemt.users; UsersFile берётся из конфига агента
	MtgApplier      *MtgApplier    // для ModeApply и mtg.config; MtgFile берётся из конфига агента
	// TLSDir - каталог сертификатов nginx под ресурсом tls.web; пусто - не ведётся.
	TLSDir     string
	TLSApplier *TLSApplier // для ModeApply и tls.web; Dir берётся из конфига агента
	// StackApplier - обновление telemt и mtg-multi (ресурс stack) в ModeApply; nil - по умолчанию.
	StackApplier *StackApplier
	Wait         time.Duration     // сколько мастер держит long-poll; по умолчанию 55 с
	RecheckEvery time.Duration     // как часто перепроверять диск и отчитываться без новой ревизии; по умолчанию 5 мин
	Checks       []Check           // локальные проверки машины для отчёта (мост и т.п.)
	Runtime      *RuntimeCollector // сборщик runtime-отчёта; nil - не шлётся
	RuntimeEvery time.Duration     // период runtime-отчёта; 0 - не шлётся
	// ProbeEvery - период пробы путей к мастеру (ProbeMasters) при двух и более
	// адресах; по умолчанию и не реже - раз в probeMaxEvery.
	ProbeEvery time.Duration
	Log        *slog.Logger
}

// Agent - агент одной машины.
type Agent struct {
	cfg     Config
	masters []master

	mu        sync.Mutex
	known     int64                     // последняя полученная ревизия
	desired   map[string]*store.Desired // последнее желаемое по ресурсам, которые ведёт агент
	lastCheck time.Time                 // когда последний раз сверяли диск
	force     bool                      // перепроверить диск в следующем цикле
	failed    map[string]failedRev      // отвергнутые ревизии по ресурсам: повторно не применяются
	versions  *RuntimeCollector         // версии бинарей для ресурса stack (тот же кэш, что у runtime-отчёта)
	stackDone *stackOutcome             // итог последней ревизии stack, по которой агент запускал обновление

	probeTimeout time.Duration // сколько ждать пробу одного адреса мастера (U17)
}

// master - адрес мастера и HTTP-клиент со своим прокси (туннелем).
type master struct {
	base   string
	client *http.Client
}

func parseMaster(entry string, timeout time.Duration, pin string) (master, error) {
	base, proxy, hasProxy := strings.Cut(entry, "|")
	m := master{base: strings.TrimRight(strings.TrimSpace(base), "/")}
	if u, err := url.Parse(m.base); err != nil || u.Scheme == "" || u.Host == "" {
		return m, fmt.Errorf("адрес мастера %q: нужен вид http://host:port", base)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if strings.HasPrefix(m.base, "https://") && pin != "" {
		tr.TLSClientConfig = tlspin.Config(pin)
	}
	if hasProxy {
		pu, err := url.Parse(strings.TrimSpace(proxy))
		if err != nil || pu.Scheme == "" || pu.Host == "" {
			return m, fmt.Errorf("прокси для %q: нужен вид socks5://127.0.0.1:port", base)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	m.client = &http.Client{Timeout: timeout, Transport: tr}
	return m, nil
}

// MasterClient - адрес мастера и HTTP-клиент для одной записи списка мастеров агента
// («адрес» или «адрес|socks5://...»), с закреплением сертификата для https. Тот же
// разбор, что у агента: им пользуется stack-apply.
func MasterClient(entry string, timeout time.Duration, pin string) (string, *http.Client, error) {
	m, err := parseMaster(entry, timeout, pin)
	if err != nil {
		return "", nil, err
	}
	return m.base, m.client, nil
}

type failedRev struct {
	rev    int64
	inputs string // отпечаток машинных входов (telemt.config): изменились - пробуем снова
	err    string
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
	case cfg.Mode != ModeCompare && cfg.Mode != ModeApply:
		return nil, fmt.Errorf("режим %q не поддержан: %q или %q", cfg.Mode, ModeCompare, ModeApply)
	case cfg.Mode == ModeApply && cfg.UsersFile != "" && cfg.Applier == nil:
		return nil, errors.New("для режима apply с users.toml нужен Applier")
	case cfg.Mode == ModeApply && cfg.MtgFile != "" && cfg.MtgApplier == nil:
		return nil, errors.New("для режима apply с mtg нужен MtgApplier")
	case cfg.Mode == ModeApply && cfg.TelemtConfigFile != "" && cfg.ConfigApplier == nil:
		return nil, errors.New("для режима apply с telemt.config нужен ConfigApplier")
	case cfg.Mode == ModeApply && cfg.TLSDir != "" && cfg.TLSApplier == nil:
		return nil, errNoTLSApplier
	}
	if cfg.TLSApplier != nil {
		cfg.TLSApplier.Dir = cfg.TLSDir
	}
	if cfg.Applier != nil {
		cfg.Applier.UsersFile = cfg.UsersFile
	}
	if cfg.MtgApplier != nil {
		cfg.MtgApplier.ConfigFile = cfg.MtgFile
	}
	if cfg.ConfigApplier != nil {
		cfg.ConfigApplier.ConfigFile = cfg.TelemtConfigFile
		cfg.ConfigApplier.LocalFile = cfg.TelemtLocalFile
		cfg.ConfigApplier.UsersFile = cfg.UsersFile
	}
	if cfg.Wait <= 0 {
		cfg.Wait = 55 * time.Second
	}
	if cfg.RecheckEvery <= 0 {
		cfg.RecheckEvery = 5 * time.Minute
	}
	if cfg.ProbeEvery <= 0 || cfg.ProbeEvery > probeMaxEvery {
		cfg.ProbeEvery = probeMaxEvery
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Mode == ModeApply && cfg.StackApplier == nil {
		cfg.StackApplier = &StackApplier{}
	}
	a := &Agent{cfg: cfg, desired: map[string]*store.Desired{}, failed: map[string]failedRev{}, versions: cfg.Runtime,
		probeTimeout: probeTimeout}
	if a.versions == nil {
		a.versions = &RuntimeCollector{Log: cfg.Log}
	}
	for _, e := range cfg.MasterURLs {
		m, err := parseMaster(e, cfg.Wait+15*time.Second, cfg.MasterPin)
		if err != nil {
			return nil, err
		}
		a.masters = append(a.masters, m)
	}
	return a, nil
}

// handles - ведёт ли агент этот ресурс на машине.
func (a *Agent) handles(resource string) bool {
	switch resource {
	case store.ResourceTelemtUsers:
		return a.cfg.UsersFile != ""
	case store.ResourceMtgConfig:
		return a.cfg.MtgFile != ""
	case store.ResourceTelemtConfig:
		return a.cfg.TelemtConfigFile != ""
	case store.ResourceTLSWeb:
		return a.cfg.TLSDir != ""
	case stack.Resource:
		// Всегда: продукта на машине нет - агент так и ответит (failed), мастер увидит.
		return true
	}
	return false
}

// ForceRecheck просит сверить диск в следующем цикле, даже без новой ревизии.
func (a *Agent) ForceRecheck() {
	a.mu.Lock()
	a.force = true
	a.mu.Unlock()
}

// Run крутит циклы до отмены ctx; при недоступности мастеров - с нарастающей паузой.
func (a *Agent) Run(ctx context.Context) {
	if a.cfg.Runtime != nil && a.cfg.RuntimeEvery > 0 {
		go a.runtimeLoop(ctx) // своим тикером: не ждёт long-poll и отчёт observed
	}
	if len(a.masters) > 1 {
		go a.probeLoop(ctx) // своим тикером: не зависит от runtime-цикла и его пауз (U17)
	}
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
	for _, m := range a.masters {
		bundle, changed, err := a.poll(ctx, m, known)
		if err != nil {
			var fatal *fatalError
			if errors.As(err, &fatal) {
				return known, err
			}
			errs = append(errs, fmt.Errorf("%s: %w", m.base, err))
			continue
		}
		if changed {
			a.mu.Lock()
			a.known = bundle.Revision
			for i := range bundle.Resources {
				if d := bundle.Resources[i]; a.handles(d.Resource) {
					a.desired[d.Resource] = &d
				}
			}
			a.force = true
			a.mu.Unlock()
		}
		if err := a.checkAndReport(ctx, m); err != nil {
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
func (a *Agent) poll(ctx context.Context, m master, known int64) (store.Bundle, bool, error) {
	var b store.Bundle
	q := url.Values{"target": {a.cfg.Target}, "rev": {strconv.FormatInt(known, 10)}, "wait": {a.cfg.Wait.String()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+"/v1/desired?"+q.Encode(), nil)
	if err != nil {
		return b, false, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	resp, err := m.client.Do(req)
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
func (a *Agent) checkAndReport(ctx context.Context, m master) error {
	a.mu.Lock()
	var items []*store.Desired
	// Порядок важен: сначала пользователи, затем основной конфиг - он собирает
	// профили из users.toml, который только что лёг на диск.
	for _, r := range []string{store.ResourceTelemtUsers, store.ResourceTelemtConfig, store.ResourceMtgConfig, store.ResourceTLSWeb, stack.Resource} {
		if d := a.desired[r]; d != nil {
			items = append(items, d)
		}
	}
	// Отчёт о машине уходит и без ресурсов (выход без telemt и mtg): мастер
	// видит, что агент жив, и результат локальных проверок.
	due := (a.force && len(items) > 0) || a.lastCheck.IsZero() || time.Since(a.lastCheck) >= a.cfg.RecheckEvery
	a.mu.Unlock()
	if !due {
		return nil
	}
	rep := store.ObservedReport{Target: a.cfg.Target, AgentVersion: a.cfg.Version, Via: a.cfg.Via,
		Checks: runChecks(ctx, a.cfg.Checks)}
	for _, d := range items {
		var res store.ObservedResource
		if a.cfg.Mode == ModeApply {
			res = a.apply(ctx, d)
		} else {
			res = a.compare(ctx, d)
		}
		rep.Resources = append(rep.Resources, res)
		a.cfg.Log.Info("агент: сверка", "resource", d.Resource, "revision", d.Revision, "status", res.Status, "diff", res.Error)
	}
	if err := a.report(ctx, m, rep); err != nil {
		return err
	}
	a.mu.Lock()
	a.lastCheck = time.Now()
	a.force = false
	a.mu.Unlock()
	return nil
}

func (a *Agent) compare(ctx context.Context, d *store.Desired) store.ObservedResource {
	switch d.Resource {
	case stack.Resource:
		return a.compareStack(ctx, d)
	case store.ResourceMtgConfig:
		return a.compareMtg(d)
	case store.ResourceTelemtConfig:
		return a.compareMainConfig(d)
	case store.ResourceTLSWeb:
		return a.compareTLSWeb(d)
	}
	return a.compareUsers(d)
}

// compareMainConfig - чем telemt.toml на машине отличается от собранного желаемого.
func (a *Agent) compareMainConfig(d *store.Desired) store.ObservedResource {
	res := store.ObservedResource{Resource: d.Resource, Revision: d.Revision, SHA256: d.SHA256, Status: "compared"}
	cur, next, err := buildMainConfig(a.cfg.TelemtConfigFile, a.cfg.TelemtLocalFile, a.cfg.UsersFile, d.Content)
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		return res
	}
	res.Error = telemtcfg.MainConfigDiff(next, cur)
	return res
}

func (a *Agent) compareMtg(d *store.Desired) store.ObservedResource {
	res := store.ObservedResource{Resource: d.Resource, Revision: d.Revision, SHA256: d.SHA256, Status: "compared"}
	onDisk, err := os.ReadFile(a.cfg.MtgFile)
	if err != nil {
		res.Status, res.Error = "failed", fmt.Sprintf("config.toml mtg не читается: %v", err)
		return res
	}
	if bytes.Equal(onDisk, d.Content) {
		return res
	}
	var parts []string
	if MtgSecretsDigest(onDisk) != MtgSecretsDigest(d.Content) {
		parts = append(parts, fmt.Sprintf("секреты отличаются (на машине %d, желаемых %d)",
			len(secretRows(string(onDisk))), len(secretRows(string(d.Content)))))
	}
	if !onlySecretsChanged(string(onDisk), string(d.Content)) {
		parts = append(parts, "отличаются настройки вне [secrets]")
	}
	if len(parts) == 0 {
		parts = append(parts, "отличается форматирование")
	}
	res.Error = strings.Join(parts, "; ")
	return res
}

// apply применяет ревизию ресурса; отвергнутую ревизию не повторяет - ждёт новую.
func (a *Agent) apply(ctx context.Context, d *store.Desired) store.ObservedResource {
	if d.Resource == stack.Resource {
		return a.applyStackOnce(ctx, d) // разовая команда: свой учёт ревизий, не a.failed
	}
	res := store.ObservedResource{Resource: d.Resource, Revision: d.Revision, SHA256: d.SHA256}
	a.mu.Lock()
	f, wasFailed := a.failed[d.Resource]
	a.mu.Unlock()
	// Основной конфиг зависит и от машинной части с users.toml: человек исправил
	// telemt.local.toml - та же ревизия пробуется снова.
	inputs := ""
	if d.Resource == store.ResourceTelemtConfig {
		inputs = mainConfigInputs(a.cfg.TelemtLocalFile, a.cfg.UsersFile)
	}
	if wasFailed && f.rev == d.Revision && f.inputs == inputs {
		res.Status, res.Error = "failed", f.err
		return res
	}
	var r ApplyResult
	switch d.Resource {
	case store.ResourceMtgConfig:
		r = a.cfg.MtgApplier.Apply(ctx, d.Content)
	case store.ResourceTelemtConfig:
		r = a.cfg.ConfigApplier.Apply(ctx, d.Content)
	case store.ResourceTLSWeb:
		r = a.cfg.TLSApplier.Apply(ctx, d.Content)
	default:
		r = a.cfg.Applier.Apply(ctx, d.Content)
	}
	res.Status, res.Error = r.Status, r.Error
	a.mu.Lock()
	if r.Status == "failed" {
		a.failed[d.Resource] = failedRev{rev: d.Revision, inputs: inputs, err: r.Error}
	} else {
		delete(a.failed, d.Resource)
	}
	a.mu.Unlock()
	return res
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

func (a *Agent) report(ctx context.Context, m master, rep store.ObservedReport) error {
	body, _ := json.Marshal(rep)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+"/v1/observed", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("отчёт: %w", err)
	}
	defer resp.Body.Close()
	// 202 - мост принял отчёт в очередь и дошлёт мастеру сам.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
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
