package agentjoin

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type recFW struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]bool
}

func (f *recFW) Allow(_ context.Context, chain string, ip netip.Addr, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, chain+" "+ip.String())
	if f.fail[ip.String()] {
		return errors.New("iptables: нет цепочки")
	}
	return nil
}

func (f *recFW) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type listFn func(context.Context) ([]netip.Addr, error)

func (l listFn) ExitAddresses(ctx context.Context) ([]netip.Addr, error) { return l(ctx) }

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(s))
	for _, a := range s {
		out = append(out, netip.MustParseAddr(a))
	}
	return out
}

// Каждый выход проверяется и при необходимости открывается; сбой одного не
// мешает остальным и попадает в ошибки.
func TestRestoreFirewall(t *testing.T) {
	fw := &recFW{fail: map[string]bool{"203.0.113.6": true}}
	ok, errs := RestoreFirewall(context.Background(), fw, "TELEMUX_AGENTS", 18443, addrs("203.0.113.5", "203.0.113.6", "203.0.113.7"))
	if ok != 2 || len(errs) != 1 || len(fw.calls) != 3 {
		t.Fatalf("ok=%d errs=%v calls=%v", ok, errs, fw.calls)
	}
}

// Без цепочки или с неверным портом ничего не вызывается.
func TestRestoreFirewall_BadSettings(t *testing.T) {
	fw := &recFW{}
	if ok, errs := RestoreFirewall(context.Background(), fw, "bad chain;", 18443, addrs("203.0.113.5")); ok != 0 || len(errs) != 1 || fw.n() != 0 {
		t.Fatalf("неверная цепочка: ok=%d errs=%v calls=%d", ok, errs, fw.n())
	}
	if ok, errs := RestoreFirewall(context.Background(), fw, "TELEMUX_AGENTS", 0, addrs("203.0.113.5")); ok != 0 || len(errs) != 1 || fw.n() != 0 {
		t.Fatalf("неверный порт: ok=%d errs=%v calls=%d", ok, errs, fw.n())
	}
}

// Цикл: первый проход сразу при старте, дальше по таймеру; сбой списка не
// роняет цикл; остановка по контексту.
func TestRestoreLoop(t *testing.T) {
	fw := &recFW{}
	var mu sync.Mutex
	calls := 0
	lister := listFn(func(context.Context) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 2 {
			return nil, errors.New("база недоступна")
		}
		return addrs("203.0.113.5"), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RestoreLoop(ctx, lister, fw, "TELEMUX_AGENTS", 18443, 20*time.Millisecond, nil)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for fw.n() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("цикл не остановился по контексту")
	}
	mu.Lock()
	defer mu.Unlock()
	if fw.n() < 2 || calls < 3 {
		t.Fatalf("ждали проход сразу и после сбоя списка: allow=%d list=%d", fw.n(), calls)
	}
}
