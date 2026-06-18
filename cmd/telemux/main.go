// Command telemux — панель управления кластером telemt (MTProxy).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/role"
	"github.com/AndreyOsipuk/telemux/internal/selfupdate"
	"github.com/AndreyOsipuk/telemux/internal/server"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/telemt"
)

var version = "dev"

func main() {
	cmd := ""
	if len(os.Args) >= 2 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "probe":
		os.Exit(runProbe(os.Args[2:]))
	case "role":
		os.Exit(runRole(os.Args[2:]))
	case "sync":
		os.Exit(runSync(os.Args[2:]))
	case "update":
		os.Exit(runUpdate(os.Args[2:]))
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "-version", "--version", "version":
		fmt.Printf("telemux %s\n", version)
	default:
		fmt.Fprintln(os.Stderr, "telemux "+version)
		fmt.Fprintln(os.Stderr, "команды:")
		fmt.Fprintln(os.Stderr, "  probe --api <url> [--auth h]            — проверить связь с telemt-API")
		fmt.Fprintln(os.Stderr, "  role  --db <dsn>                        — роль ноды (master/replica) из локального PG")
		fmt.Fprintln(os.Stderr, "  sync  --db <dsn> --api <url> [--apply]  — синхронизировать локальный telemt (shadow по умолч.)")
		fmt.Fprintln(os.Stderr, "  update [--owner o --repo r]             — обновить бинарь до последнего релиза (checksum+swap)")
		fmt.Fprintln(os.Stderr, "  serve --db <dsn> --api <url> [--apply --listen :8080]  — демон: HTTP API + дашборд + автосинхра")
	}
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dsn := fs.String("db", envOr("DATABASE_URL", ""), "DSN локального PG")
	api := fs.String("api", envOr("TELEMT_API_URL", "http://127.0.0.1:9091"), "URL telemt machine-API")
	auth := fs.String("auth", envOr("TELEMT_API_AUTH", ""), "Authorization для API")
	listen := fs.String("listen", envOr("TELEMUX_LISTEN", ":8080"), "адрес HTTP-демона")
	apply := fs.Bool("apply", false, "автосинхра применяет изменения (по умолчанию shadow)")
	interval := fs.Duration("interval", 60*time.Second, "период автосинхры")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, *dsn)
	if err != nil {
		logger.Error("PG", "err", err)
		return 1
	}
	defer st.Close()

	mode := syncpkg.Shadow
	if *apply {
		mode = syncpkg.Apply
	}
	srv := server.New(server.Deps{
		Store: st, Node: telemt.New(*api, *auth), Version: version,
		Interval: *interval, SyncOpts: syncpkg.Options{Mode: mode}, Log: logger,
		Users:         st, // store реализует user-CRUD
		Cluster:       st, // store реализует реестр нод + join-token
		ClusterSecret: envOr("TELEMUX_CLUSTER_SECRET", ""),
		PublicURL:     envOr("TELEMUX_PUBLIC_URL", "http://127.0.0.1"+*listen),
		SelfCode:      envOr("TELEMUX_NODE_CODE", ""),
		SelfAddress:   envOr("TELEMUX_NODE_ADDRESS", ""),
		SelfTelemtURL: *api,
		MasterURL:     envOr("TELEMUX_MASTER_URL", ""),
	})
	if err := srv.Run(ctx, *listen); err != nil {
		logger.Error("serve", "err", err)
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	owner := fs.String("owner", "AndreyOsipuk", "GitHub owner")
	repo := fs.String("repo", "telemux", "GitHub repo")
	apiBase := fs.String("api-base", "", "база GitHub API (для зеркала); пусто = api.github.com")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "не определить путь бинаря: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	from, to, err := selfupdate.Update(ctx, selfupdate.Options{
		Owner: *owner, Repo: *repo, CurrentVersion: version,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, BinaryPath: exe, APIBase: *apiBase,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "обновление: %v\n", err)
		return 1
	}
	if from == to {
		fmt.Printf("уже последняя версия (%s)\n", to)
		return 0
	}
	fmt.Printf("обновлено %s → %s. Перезапустите сервис: systemctl restart telemux\n", from, to)
	fmt.Printf("(старый бинарь сохранён в %s.bak — авто-откат при сбое делает контрол-плейн)\n", exe)
	return 0
}

func runProbe(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	api := fs.String("api", "http://127.0.0.1:9091", "базовый URL telemt machine-API")
	auth := fs.String("auth", "", "значение заголовка Authorization")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := telemt.New(*api, *auth)
	h, err := c.Health(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: ОШИБКА: %v\n", err)
		return 1
	}
	fmt.Printf("health: status=%s read_only=%v\n", h.Status, h.ReadOnly)
	users, rev, err := c.ListUsers(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "users: ОШИБКА: %v\n", err)
		return 1
	}
	fmt.Printf("users: всего=%d revision=%s\ntelemux видит ноду ✓\n", len(users), rev)
	return 0
}

func runRole(args []string) int {
	fs := flag.NewFlagSet("role", flag.ContinueOnError)
	dsn := fs.String("db", "", "DSN локального PG (postgres://...@127.0.0.1:5432/telemux)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PG: %v\n", err)
		return 1
	}
	defer st.Close()
	r, err := role.Detect(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	fmt.Printf("роль ноды: %s\n", r)
	return 0
}

func runSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	dsn := fs.String("db", "", "DSN локального PG")
	api := fs.String("api", "http://127.0.0.1:9091", "базовый URL telemt machine-API")
	auth := fs.String("auth", "", "значение заголовка Authorization")
	apply := fs.Bool("apply", false, "применять изменения (по умолчанию shadow — только показать)")
	force := fs.Bool("force", false, "обойти guard массового сноса")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PG: %v\n", err)
		return 1
	}
	defer st.Close()

	mode := syncpkg.Shadow
	if *apply {
		mode = syncpkg.Apply
	}
	res, err := syncpkg.SyncNode(ctx, st, telemt.New(*api, *auth), syncpkg.Options{Mode: mode, Force: *force})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync: %v\n", err)
		return 1
	}

	creates, patches, deletes := 0, 0, 0
	for _, op := range res.Ops {
		switch op.Kind {
		case "create":
			creates++
		case "patch":
			patches++
		case "delete":
			deletes++
		}
	}
	fmt.Printf("режим=%s diff: create=%d patch=%d delete=%d\n", mode, creates, patches, deletes)
	if res.Aborted {
		fmt.Println("⚠️  ЗАБЛОКИРОВАНО guard'ом массового сноса (нужен --force)")
		return 1
	}
	if mode == syncpkg.Apply {
		fmt.Printf("применено=%d пропущено(идемпот.)=%d ошибок=%d\n", res.Applied, res.Skipped, res.Failed)
		for _, e := range res.Errors {
			fmt.Fprintf(os.Stderr, "  ошибка: %v\n", e)
		}
		if res.Failed > 0 {
			return 1
		}
	}
	return 0
}
