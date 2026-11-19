package main

// telemux stack-apply - замена бинаря telemt или mtg-multi (ADR 0008). Запускает агент
// через systemd-run с EnvironmentFile агента; вручную - так же, от root.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/stackapply"
)

// runStackApply: telemux stack-apply --product telemt --sha256 <s> --version 3.5.17 --size <n>
func runStackApply(args []string) int {
	fs := flag.NewFlagSet("stack-apply", flag.ContinueOnError)
	product := fs.String("product", "", "telemt или mtg")
	sum := fs.String("sha256", "", "sha256 нового бинаря")
	ver := fs.String("version", "", "версия, которую должен сообщить новый бинарь")
	size := fs.Int64("size", 0, "размер нового бинаря в байтах")
	masters := fs.String("master", envOr("TELEMUX_AGENT_MASTERS", ""), "адреса мастера через запятую по приоритету")
	masterPin := fs.String("master-pin", envOr("TELEMUX_AGENT_MASTER_PIN", ""), "sha256 сертификата мастера для https-адресов")
	tokenFile := fs.String("token-file", envOr("TELEMUX_AGENT_TOKEN_FILE", "/etc/telemux/agent.token"), "файл с токеном машины")
	target := fs.String("target", envOr("TELEMUX_AGENT_TARGET", ""), "код этой машины")
	bin := fs.String("bin", "", "заменяемый бинарь; пусто - из окружения агента по продукту")
	unit := fs.String("unit", "", "юнит systemd продукта; пусто - из окружения агента")
	health := fs.String("health-url", "", "API продукта для проверки; пусто - из окружения агента")
	healthTimeout := fs.Duration("health-timeout", 30*time.Second, "сколько ждать подъёма сервиса")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	def, err := stackapply.Defaults(*product, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stack-apply:", err)
		return 2
	}
	if *bin != "" {
		def.Bin = *bin
	}
	if *unit != "" {
		def.Unit = *unit
	}
	if *health != "" {
		def.HealthURL = *health
	}
	path, err := stackapply.ResolveBin(def.Bin, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stack-apply:", err)
		return 1
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stack-apply: токен машины:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := stackapply.Run(ctx, stackapply.Config{
		Product: *product, SHA256: strings.ToLower(strings.TrimSpace(*sum)), Version: *ver, Size: *size,
		Masters: splitList(*masters), MasterPin: *masterPin, Token: strings.TrimSpace(string(raw)), Target: *target,
		Bin: path, Unit: def.Unit, HealthURL: def.HealthURL, HealthTimeout: *healthTimeout,
		Log: os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "stack-apply:", err)
		if errors.Is(err, stackapply.ErrInvalid) {
			return 2
		}
		return 1
	}
	fmt.Println(res.String())
	return 0
}
