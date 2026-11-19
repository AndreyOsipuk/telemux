package agent

// Обновление бинарей telemt и mtg-multi (ресурс stack, ADR 0008). Мастер публикует,
// какая версия какого продукта должна стоять, с sha256 и размером бинаря. Агент
// сравнивает с версией на машине (тот же `--version`, что и в runtime-отчёте) и в
// режиме apply запускает `telemux stack-apply` отдельным юнитом systemd.
//
// Почему systemd-run, а не замена бинаря самим агентом: юнит агента собран с
// ProtectSystem=strict (вся система, кроме /etc/telemt и /etc/mtg-multi и т.п., только
// для чтения), /usr/local/bin ему недоступен. systemd-run просит PID 1 по D-Bus создать
// временный юнит; тот запускается с настройками по умолчанию, без песочницы агента.
// Из песочницы это допустимо так же, как уже используемые агентом `systemctl reload`
// и `systemctl restart`: сокет D-Bus в /run - не обычный файл, подключение к нему
// read-only монтирование не запрещает. NoNewPrivileges=true действует только на
// процессы-потомки агента (запрет setuid и файловых capabilities); systemd-run ни
// того, ни другого не требует (агент и так root), а временный юнит - потомок PID 1,
// а не агента, флаг он не наследует. Окружение агента юнит тоже не наследует - отсюда
// EnvironmentFile с тем же agent.env.
//
// Ревизия stack - разовая команда: агент запускает обновление по ревизии один раз и
// помнит её итог. Перепроверка той же ревизии (раз в RecheckEvery) только сравнивает
// версии и повторяет прежний итог - иначе агент откатывал бы версию, поставленную
// вручную после обновления. Новая ревизия - новый запуск. Мастер после итога сам
// публикует ревизию без продукта, в ревизии не больше одного продукта.
//
// Итог живёт в памяти: после рестарта агента та же ревизия запустится ещё раз. Это
// возможно только в окне (до ~30 с), пока мастер не записал итог и не убрал продукт из
// ресурса, - допустимо: stack-apply идемпотентен (нужная версия и живой сервис - выход 0).
//
// Обновление синхронное: пока оно идёт, цикл агента (long-poll, отчёты) стоит. Это
// ограничено: один продукт на ревизию, у stack-apply общий дедлайн 8 мин, у юнита
// RuntimeMaxSec=540, у агента на вызов - 10 мин.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/stack"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// DefaultAgentEnvFile - env-файл агента; его же читает временный юнит stack-apply.
const DefaultAgentEnvFile = "/etc/telemux/agent.env"

// stackOutputTail - сколько последних байт вывода stack-apply попадает в причину провала.
const stackOutputTail = 500

// CombinedRunner - CommandRunner, который отдаёт stdout и stderr вместе: для причины
// провала stack-apply важен его stderr.
type CombinedRunner struct{}

// Run выполняет команду и возвращает общий вывод.
func (CombinedRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// StackApplier запускает обновление одного продукта вне песочницы агента.
type StackApplier struct {
	Runner     CommandRunner          // по умолчанию CombinedRunner
	Executable func() (string, error) // путь к своему бинарю telemux; по умолчанию os.Executable
	EnvFile    string                 // по умолчанию DefaultAgentEnvFile
	Timeout    time.Duration          // на всё обновление (скачивание + проверка); по умолчанию 10 мин
}

// stackRuntimeMaxSec - предел жизни временного юнита: больше дедлайна stack-apply (8 мин)
// с запасом на откат, меньше таймаута агента (10 мин).
const stackRuntimeMaxSec = 540

// StackJob - одно обновление: продукт, бинарь из ресурса, ревизия и путь заменяемого
// бинаря (тот же, по которому агент берёт версию).
type StackJob struct {
	Product  string
	Binary   stack.Binary
	Revision int64
	Bin      string // абсолютный путь
}

func (s *StackApplier) defaults() {
	if s.Runner == nil {
		s.Runner = CombinedRunner{}
	}
	if s.Executable == nil {
		s.Executable = os.Executable
	}
	if s.EnvFile == "" {
		s.EnvFile = DefaultAgentEnvFile
	}
	if s.Timeout <= 0 {
		s.Timeout = 10 * time.Minute
	}
}

// Command - команда systemd-run для обновления продукта.
//
// --pipe отдаёт вывод stack-apply агенту (без него вывод уходит только в журнал и
// причина провала была бы пустой); вместе с --wait код выхода systemd-run - код
// выхода stack-apply. Имя юнита - с ревизией: если юнит прошлой ревизии ещё жив,
// новый запуск не упадёт на «unit already exists».
func (s *StackApplier) Command(j StackJob) (string, []string, error) {
	s.defaults()
	exe, err := s.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("путь к бинарю telemux: %w", err)
	}
	return "systemd-run", []string{
		"--wait", "--pipe", "--collect", "--quiet",
		"--unit=telemux-stack-" + j.Product + "-" + strconv.FormatInt(j.Revision, 10),
		"-p", "EnvironmentFile=" + s.EnvFile,
		"-p", "RuntimeMaxSec=" + strconv.Itoa(stackRuntimeMaxSec),
		exe, "stack-apply",
		"--product", j.Product,
		"--sha256", j.Binary.SHA256,
		"--version", j.Binary.Version,
		"--size", strconv.FormatInt(j.Binary.Size, 10),
		"--bin", j.Bin,
	}, nil
}

// Update обновляет продукт и ждёт результата. Ошибка - с хвостом вывода stack-apply.
func (s *StackApplier) Update(ctx context.Context, j StackJob) error {
	name, args, err := s.Command(j)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	out, err := s.Runner.Run(ctx, name, args...)
	if err != nil {
		if tail := outputTail(out, stackOutputTail); tail != "" {
			return fmt.Errorf("%v: %s", err, tail)
		}
		return err
	}
	return nil
}

// outputTail - последние n байт вывода без пробелов по краям; обрезка - по границе строки UTF-8.
func outputTail(out []byte, n int) string {
	s := strings.TrimSpace(string(out))
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for i := 0; i < len(s) && i < 4; i++ { // не начинать с середины многобайтового символа
		if s[i]&0xC0 != 0x80 {
			return "..." + s[i:]
		}
	}
	return "..." + s
}

// ProductBinary - абсолютный путь бинаря продукта и его версия (как в runtime-отчёте):
// telemt - TelemtBin, mtg - mtg-multi или MtgBin, через LookPath. Пустой путь - бинаря
// нет; пустая версия - `--version` не удался.
func (c *RuntimeCollector) ProductBinary(ctx context.Context, product string) (path, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaults()
	bin := c.TelemtBin
	if product != stack.ProductTelemt {
		bin = c.mtgBin()
	}
	p, err := c.LookPath(bin)
	if err != nil {
		return "", ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return p, c.binVersion(ctx, bin)
}

// ForgetVersions сбрасывает кэш версий бинарей. Кэш и так обновится по mtime и размеру
// нового файла; сброс - чтобы версия после обновления не зависела от этой эвристики.
func (c *RuntimeCollector) ForgetVersions() {
	c.mu.Lock()
	c.versions = nil
	c.mu.Unlock()
}

// hasProduct - ведёт ли агент продукт на машине (так же решает runtime-отчёт).
func (a *Agent) hasProduct(product string) bool {
	switch product {
	case stack.ProductTelemt:
		return a.cfg.UsersFile != ""
	case stack.ProductMtg:
		return a.cfg.MtgFile != ""
	}
	return false
}

// stackStep - что делать с продуктом.
type stackStep struct {
	product string
	want    stack.Binary
	current string // версия на машине; пусто - не определена
	bin     string // абсолютный путь бинаря на машине
	err     string // продукта на машине нет или бинарь не найден
}

// stackPlan разбирает ресурс и сравнивает версии. Продукты, у которых версия уже нужная,
// в план не попадают.
func (a *Agent) stackPlan(ctx context.Context, content []byte) ([]stackStep, error) {
	c, err := stack.Parse(content)
	if err != nil {
		return nil, err
	}
	var plan []stackStep
	for _, p := range stack.Products {
		want, ok := c[p]
		if !ok {
			continue
		}
		if !a.hasProduct(p) {
			plan = append(plan, stackStep{product: p, want: want, err: p + ": продукта на машине нет"})
			continue
		}
		bin, cur := a.versions.ProductBinary(ctx, p)
		if bin == "" {
			plan = append(plan, stackStep{product: p, want: want, err: p + ": бинарь на машине не найден"})
			continue
		}
		if cur != "" && stack.SameVersion(cur, want.Version) {
			continue
		}
		plan = append(plan, stackStep{product: p, want: want, current: cur, bin: bin})
	}
	return plan, nil
}

func (s stackStep) diff() string {
	cur := s.current
	if cur == "" {
		cur = "версия не определена"
	}
	return fmt.Sprintf("%s: на машине %s, нужно %s", s.product, cur, s.want.Version)
}

// compareStack - режим compare: только разница версий.
func (a *Agent) compareStack(ctx context.Context, d *store.Desired) store.ObservedResource {
	res := store.ObservedResource{Resource: d.Resource, Revision: d.Revision, SHA256: d.SHA256, Status: "compared"}
	plan, err := a.stackPlan(ctx, d.Content)
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		return res
	}
	var parts []string
	for _, s := range plan {
		if s.err != "" {
			res.Status = "failed"
			parts = append(parts, s.err)
			continue
		}
		parts = append(parts, s.diff())
	}
	res.Error = strings.Join(parts, "; ")
	return res
}

// applyStack - режим apply: обновить продукты, у которых версия не та. Продукты
// независимы: провал одного не останавливает другой, но ревизия тогда failed.
func (a *Agent) applyStack(ctx context.Context, d *store.Desired) ApplyResult {
	plan, err := a.stackPlan(ctx, d.Content)
	if err != nil {
		return failed("%v", err)
	}
	var errs []string
	updated := false
	for _, s := range plan {
		if s.err != "" {
			errs = append(errs, s.err)
			continue
		}
		a.cfg.Log.Info("агент: обновление продукта", "product", s.product, "from", s.current, "to", s.want.Version)
		j := StackJob{Product: s.product, Binary: s.want, Revision: d.Revision, Bin: s.bin}
		if err := a.cfg.StackApplier.Update(ctx, j); err != nil {
			errs = append(errs, fmt.Sprintf("%s: обновление до %s не удалось: %v", s.product, s.want.Version, err))
			continue
		}
		updated = true
	}
	if updated {
		a.versions.ForgetVersions()
	}
	if len(errs) > 0 {
		return ApplyResult{Status: "failed", Error: clip(strings.Join(errs, "; "))}
	}
	return ApplyResult{Status: "applied"}
}

// stackOutcome - итог ревизии stack, по которой агент уже запускал обновление.
type stackOutcome struct {
	rev int64
	res ApplyResult
}

// applyStackOnce применяет ревизию stack один раз. Перепроверка той же ревизии -
// только сравнение (в журнал) и прежний итог в отчёте, без запуска stack-apply.
func (a *Agent) applyStackOnce(ctx context.Context, d *store.Desired) store.ObservedResource {
	res := store.ObservedResource{Resource: d.Resource, Revision: d.Revision, SHA256: d.SHA256}
	a.mu.Lock()
	done := a.stackDone
	a.mu.Unlock()
	if done != nil && done.rev == d.Revision {
		if cmp := a.compareStack(ctx, d); cmp.Error != "" {
			a.cfg.Log.Info("агент: stack уже применялся по этой ревизии, повтора нет",
				"revision", d.Revision, "diff", cmp.Error)
		}
		res.Status, res.Error = done.res.Status, done.res.Error
		return res
	}
	r := a.applyStack(ctx, d)
	a.mu.Lock()
	a.stackDone = &stackOutcome{rev: d.Revision, res: r}
	a.mu.Unlock()
	res.Status, res.Error = r.Status, r.Error
	return res
}
