package agentjoin

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"
)

// ExitLister - адреса включённых машин-выходов (реализует store.Store).
type ExitLister interface {
	ExitAddresses(ctx context.Context) ([]netip.Addr, error)
}

// RestoreFirewall проверяет, что адрес каждого выхода открыт в цепочке chain на
// порту port, и открывает недостающие. Правила, добавленные при добавлении машины,
// живут только до перезагрузки - этот проход их возвращает. Сбой одного адреса не
// мешает остальным. Возвращает число адресов, открытых или уже открытых.
func RestoreFirewall(ctx context.Context, fw Firewall, chain string, port int, addrs []netip.Addr) (int, []error) {
	if !ValidChain(chain) {
		return 0, []error{fmt.Errorf("цепочка %q: допустимы буквы, цифры, _ и -, до 28 символов", chain)}
	}
	if port < 1 || port > 65535 {
		return 0, []error{fmt.Errorf("порт %d: от 1 до 65535", port)}
	}
	ok := 0
	var errs []error
	for _, a := range addrs {
		if err := fw.Allow(ctx, chain, a, port); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
			continue
		}
		ok++
	}
	return ok, errs
}

// RestoreLoop - RestoreFirewall сразу и затем раз в every, пока жив ctx. Ошибки
// пишутся в лог (log может быть nil) и цикл не прерывают.
func RestoreLoop(ctx context.Context, lister ExitLister, fw Firewall, chain string, port int, every time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	run := func() {
		addrs, err := lister.ExitAddresses(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("файрвол: список выходов не получен", "err", err)
			}
			return
		}
		ok, errs := RestoreFirewall(ctx, fw, chain, port, addrs)
		for _, e := range errs {
			log.Warn("файрвол: адрес выхода не открыт", "chain", chain, "err", e)
		}
		log.Debug("файрвол: адреса выходов проверены", "chain", chain, "ok", ok, "total", len(addrs))
	}
	run()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
