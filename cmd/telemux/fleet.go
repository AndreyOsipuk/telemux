package main

// Команды флота: машины, токены агентов, публикация ревизии, агент на машине.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/agent"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// runTarget: telemux target --code pr2 --kind relay --resources telemt.users [--relay pr2] [--disabled]
func runTarget(args []string) int {
	fs := flag.NewFlagSet("target", flag.ContinueOnError)
	dsn := fs.String("db", envOr("DATABASE_URL", ""), "DSN primary PG")
	code := fs.String("code", "", "код машины (ps1..ps8, pr1..pr4)")
	kind := fs.String("kind", "", "exit | relay")
	relay := fs.String("relay", "", "для выхода: релей, к которому он держит туннель")
	resources := fs.String("resources", "", "ресурсы через запятую, например telemt.users")
	disabled := fs.Bool("disabled", false, "машина выключена (ревизий не получает)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *code == "" || (*kind != "exit" && *kind != "relay") {
		fmt.Fprintln(os.Stderr, "нужны --code и --kind exit|relay")
		return 2
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "PG:", err)
		return 1
	}
	defer st.Close()
	t := store.Target{Code: *code, Kind: *kind, RelayCode: *relay, Resources: splitList(*resources), Enabled: !*disabled}
	if err := st.UpsertTarget(ctx, t); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("машина %s сохранена: %s, ресурсы %v, включена %v\n", t.Code, t.Kind, t.Resources, t.Enabled)
	return 0
}

// runAgentToken: telemux agent-token --target pr2 - выпустить токен (прежний перестаёт работать).
func runAgentToken(args []string) int {
	fs := flag.NewFlagSet("agent-token", flag.ContinueOnError)
	dsn := fs.String("db", envOr("DATABASE_URL", ""), "DSN primary PG")
	target := fs.String("target", "", "код машины")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "PG:", err)
		return 1
	}
	defer st.Close()
	tok, err := st.IssueAgentToken(ctx, *target)
	if errors.Is(err, store.ErrUnknownTarget) {
		fmt.Fprintf(os.Stderr, "машины %q нет, сначала: telemux target --code %s ...\n", *target, *target)
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Токен - только в stdout, чтобы его можно было сразу положить в файл на машине.
	fmt.Println(tok)
	return 0
}

// runPublish: telemux publish - собрать users.toml и опубликовать ревизию вручную.
func runPublish(args []string) int {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	dsn := fs.String("db", envOr("DATABASE_URL", ""), "DSN primary PG")
	maxConns := fs.Int("max-conns", 50, "лимит подключений по умолчанию")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "PG:", err)
		return 1
	}
	defer st.Close()
	res, err := st.PublishTelemtUsers(ctx, *maxConns)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("ревизия %d, новая для: %v\n", res.Revision, res.Changed)
	return 0
}

// runAgent: агент на машине. Пока только режим сравнения.
func runAgent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	masters := fs.String("master", envOr("TELEMUX_AGENT_MASTERS", ""), "адреса мастера через запятую по приоритету")
	tokenFile := fs.String("token-file", envOr("TELEMUX_AGENT_TOKEN_FILE", "/etc/telemux/agent.token"), "файл с токеном машины")
	target := fs.String("target", envOr("TELEMUX_AGENT_TARGET", ""), "код этой машины")
	usersFile := fs.String("users-file", envOr("TELEMUX_AGENT_USERS_FILE", "/etc/telemt/users.toml"), "users.toml на машине")
	mode := fs.String("mode", envOr("TELEMUX_AGENT_MODE", agent.ModeCompare), "режим: compare (только сравнить)")
	via := fs.String("via", envOr("TELEMUX_AGENT_VIA", ""), "через какой мост ходит агент (для отчёта)")
	wait := fs.Duration("wait", 55*time.Second, "long-poll")
	recheck := fs.Duration("recheck", 5*time.Minute, "перепроверка диска без новой ревизии")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "токен машины:", err)
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	a, err := agent.New(agent.Config{
		MasterURLs: splitList(*masters), Token: strings.TrimSpace(string(raw)), Target: *target,
		Version: version, Via: *via, UsersFile: *usersFile, Mode: *mode,
		Wait: *wait, RecheckEvery: *recheck, Log: logger,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("агент запущен", "target", *target, "mode", *mode, "masters", *masters, "version", version)
	a.Run(ctx)
	return 0
}
