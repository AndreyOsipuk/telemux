package agentjoin

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
)

// Firewall открывает адрес машины-выхода на TLS-порту агентов мастера.
type Firewall interface {
	// Allow добавляет в цепочку chain правило «tcp с ip на port - ACCEPT»,
	// если его ещё нет. Вызывается только с проверенными chain, ip и port.
	Allow(ctx context.Context, chain string, ip netip.Addr, port int) error
}

// Runner запускает команду и возвращает код выхода и вывод. err - только если
// команду не удалось запустить или она прервана (не ненулевой код).
type Runner func(ctx context.Context, name string, args ...string) (code int, out []byte, err error)

// IPTables - Firewall через iptables: -C (есть ли правило), при отсутствии -I.
// Цепочку на мастере создают один раз руками (см. README).
type IPTables struct {
	Bin string // по умолчанию iptables
	Run Runner // по умолчанию exec
}

// ExecRunner - настоящий запуск команды.
func ExecRunner(ctx context.Context, name string, args ...string) (int, []byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		return exitErr.ExitCode(), out, nil
	}
	if err != nil {
		return -1, out, err
	}
	return 0, out, nil
}

// Allow - см. Firewall.
func (f IPTables) Allow(ctx context.Context, chain string, ip netip.Addr, port int) error {
	if !ValidChain(chain) {
		return fmt.Errorf("цепочка %q: допустимы буквы, цифры, _ и -, до 28 символов", chain)
	}
	if !ip.Is4() || ip.IsUnspecified() {
		return fmt.Errorf("адрес %s: нужен IPv4 машины", ip)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("порт %d: от 1 до 65535", port)
	}
	bin, run := f.Bin, f.Run
	if bin == "" {
		bin = "iptables"
	}
	if run == nil {
		run = ExecRunner
	}
	rule := []string{chain, "-s", ip.String(), "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "ACCEPT"}
	code, out, err := run(ctx, bin, append([]string{"-w", "-C"}, rule...)...)
	if err != nil {
		return fmt.Errorf("%s -C: %w", bin, err)
	}
	switch code {
	case 0:
		return nil // правило уже есть
	case 1: // правила нет - добавляем
	default:
		return fmt.Errorf("%s -C: код %d: %s", bin, code, strings.TrimSpace(string(out)))
	}
	code, out, err = run(ctx, bin, append([]string{"-w", "-I"}, rule...)...)
	if err != nil {
		return fmt.Errorf("%s -I: %w", bin, err)
	}
	if code != 0 {
		return fmt.Errorf("%s -I: код %d: %s", bin, code, strings.TrimSpace(string(out)))
	}
	return nil
}
