// Command telemux — панель управления кластером telemt (MTProxy).
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/agentjoin"
	"github.com/AndreyOsipuk/telemux/internal/backend/mtgmulti"
	"github.com/AndreyOsipuk/telemux/internal/patroni"
	"github.com/AndreyOsipuk/telemux/internal/peers"
	"github.com/AndreyOsipuk/telemux/internal/role"
	"github.com/AndreyOsipuk/telemux/internal/selfupdate"
	"github.com/AndreyOsipuk/telemux/internal/server"
	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/telemt"
	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
	"github.com/AndreyOsipuk/telemux/internal/xui"
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
	case "import-toml":
		os.Exit(runImportToml(os.Args[2:]))
	case "update":
		os.Exit(runUpdate(os.Args[2:]))
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "fence":
		os.Exit(runFence(os.Args[2:]))
	case "target":
		os.Exit(runTarget(os.Args[2:]))
	case "agent-token":
		os.Exit(runAgentToken(os.Args[2:]))
	case "publish":
		os.Exit(runPublish(os.Args[2:]))
	case "agent":
		os.Exit(runAgent(os.Args[2:]))
	case "bridge":
		os.Exit(runBridge(os.Args[2:]))
	case "tls-cert":
		os.Exit(runTLSCert(os.Args[2:]))
	case "machines":
		os.Exit(runMachines(os.Args[2:]))
	case "-version", "--version", "version":
		fmt.Printf("telemux %s\n", version)
	default:
		fmt.Fprintln(os.Stderr, "telemux "+version)
		fmt.Fprintln(os.Stderr, "команды:")
		fmt.Fprintln(os.Stderr, "  probe --api <url> [--auth h]            — проверить связь с telemt-API")
		fmt.Fprintln(os.Stderr, "  role  --db <dsn>                        — роль ноды (master/replica) из локального PG")
		fmt.Fprintln(os.Stderr, "  target --code c --kind exit|relay [--resources telemt.users] — машина флота")
		fmt.Fprintln(os.Stderr, "  agent-token --target c                  — выпустить токен агента машины")
		fmt.Fprintln(os.Stderr, "  publish                                 — опубликовать ревизию users.toml вручную")
		fmt.Fprintln(os.Stderr, "  agent --master u[,u] --target c [--mode compare|apply] — агент на машине")
		fmt.Fprintln(os.Stderr, "  bridge --master u[,u] [--listen 127.0.0.1:18480] — мост для агента релея")
		fmt.Fprintln(os.Stderr, "  tls-cert [--cert f --key f]             — сертификат мастера для API агентов, печатает отпечаток")
		fmt.Fprintln(os.Stderr, "  machines                                — машины флота и последний отчёт агентов")
		fmt.Fprintln(os.Stderr, "  sync  --db <dsn> --api <url> [--apply]  — синхронизировать локальный telemt (shadow по умолч.)")
		fmt.Fprintln(os.Stderr, "  import-toml --db <dsn> --file users.toml — импорт существующих юзеров С СЕКРЕТАМИ (обратная совместимость)")
		fmt.Fprintln(os.Stderr, "  update [--owner o --repo r]             — обновить бинарь до последнего релиза (checksum+swap)")
		fmt.Fprintln(os.Stderr, "  serve --db <dsn> --api <url> [--apply --listen :8080]  — демон: HTTP API + дашборд + автосинхра")
		fmt.Fprintln(os.Stderr, "  fence --db <dsn> [--roles bot] on|off|status - ручное управление PG-fence (аварийно)")
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
	// Миграция 0007 (runtime-отчёты агентов) обязана быть применена до мастера:
	// иначе /api/v1/machines и ручки runtime отвечали бы 500 на каждый запрос.
	if err := st.CheckRuntimeSchema(ctx); err != nil {
		logger.Error("схема базы не готова, мастер не запущен", "err", err)
		return 1
	}
	runtimeStale, err := runtimeStaleAfter()
	if err != nil {
		logger.Error("настройка runtime-отчётов", "err", err)
		return 2
	}

	mode := syncpkg.Shadow
	if *apply {
		mode = syncpkg.Apply
	}

	// mtg-multi backend (опционально): включается заданием TELEMUX_MTG_SSH_KEY —
	// путь к приватному ключу для доставки config на mtg-ноды по SSH. Без него
	// синхронизируются только telemt-ноды (обратная совместимость).
	var mtg *server.MtgDeps
	if key := os.Getenv("TELEMUX_MTG_SSH_KEY"); key != "" {
		mtg = &server.MtgDeps{Store: st, Syncer: mtgmulti.New(mtgmulti.NewSSHDelivery(key))}
		logger.Info("mtg-multi backend включён", "ssh_key", key)
	}

	selfCode := envOr("TELEMUX_NODE_CODE", "")
	clusterSecret := envOr("TELEMUX_CLUSTER_SECRET", "")
	peerMap, err := peers.Parse(envOr("TELEMUX_PEERS", ""))
	if err != nil {
		logger.Error("TELEMUX_PEERS", "err", err)
		return 2
	}
	roleSrc, err := startRoleMonitor(ctx, logger, st, selfCode, clusterSecret, peerMap)
	if err != nil {
		logger.Error("фейловер", "err", err)
		return 2
	}

	defaultMaxConns, err := strconv.Atoi(envOr("TELEMUX_DEFAULT_MAX_CONNS", "50"))
	if err != nil || defaultMaxConns < 1 {
		logger.Error("TELEMUX_DEFAULT_MAX_CONNS - целое больше 0", "value", os.Getenv("TELEMUX_DEFAULT_MAX_CONNS"))
		return 2
	}

	// Синхронизатор входов мастер-панели 3x-ui (mtg через панель): включается адресом панели.
	var panel *server.PanelDeps
	if xuiURL := envOr("TELEMUX_XUI_URL", ""); xuiURL != "" {
		var tlsConf *tls.Config
		if pin := envOr("TELEMUX_XUI_CERT_SHA256", ""); pin != "" {
			tlsConf = xui.PinnedTLS(pin)
		} else {
			logger.Warn("TELEMUX_XUI_CERT_SHA256 не задан - сертификат панели проверяется обычной цепочкой")
		}
		panel = &server.PanelDeps{Store: st, API: xui.NewAPI(xuiURL, envOr("TELEMUX_XUI_TOKEN", ""), tlsConf),
			AllowMassRemove: envOr("TELEMUX_XUI_ALLOW_MASS_REMOVE", "") == "1"}
		logger.Info("синхронизатор панели 3x-ui включён")
	}

	// Добавление машины одной командой: внешний адрес API агентов и цепочка файрвола.
	agentPublicURL := envOr("TELEMUX_AGENT_PUBLIC_URL", "")
	if agentPublicURL != "" {
		if err := agentjoin.ValidPublicURL(agentPublicURL); err != nil {
			logger.Warn("команды установки агентов выдаваться не будут", "err", err)
		}
	}
	fwChain := envOr("TELEMUX_AGENT_FW_CHAIN", "")
	if fwChain != "" && !agentjoin.ValidChain(fwChain) {
		logger.Error("TELEMUX_AGENT_FW_CHAIN - имя цепочки iptables: буквы, цифры, _ и -, до 28 символов", "value", fwChain)
		return 2
	}

	srv := server.New(server.Deps{
		Joins:          st, // /api/v1/targets и /v1/join/* - добавление машины одной командой
		AgentPublicURL: agentPublicURL,
		AgentFWChain:   fwChain,
		Firewall:       agentjoin.IPTables{},
		Panel:          panel,
		TLSListen:      envOr("TELEMUX_TLS_LISTEN", ""),
		TLSCertFile:    envOr("TELEMUX_TLS_CERT", "/etc/telemux/tls/cert.pem"),
		TLSKeyFile:     envOr("TELEMUX_TLS_KEY", "/etc/telemux/tls/key.pem"),
		Store:          st, Node: telemt.New(*api, *auth), Version: version,
		Interval: *interval, SyncOpts: syncpkg.Options{Mode: mode}, Log: logger,
		AdminUser:     envOr("TELEMUX_ADMIN_USER", "admin"),
		AdminPassword: envOr("TELEMUX_ADMIN_PASSWORD", ""),
		Users:         st, // store реализует user-CRUD
		Cluster:       st, // store реализует реестр нод + join-token
		ClusterSecret: clusterSecret,
		// API бота (/api/v1/users, /api/v1/revisions): ревизии желаемого состояния.
		Revisions:       st,
		Agents:          st, // /v1/desired и /v1/observed для агентов
		BotToken:        envOr("TELEMUX_BOT_TOKEN", ""),
		DefaultMaxConns: defaultMaxConns,
		PublicURL:       envOr("TELEMUX_PUBLIC_URL", "http://127.0.0.1"+*listen),
		SelfCode:        selfCode,
		SelfAddress:     envOr("TELEMUX_NODE_ADDRESS", ""),
		SelfTelemtURL:   *api,
		MasterURL:       envOr("TELEMUX_MASTER_URL", ""),
		Mtg:             mtg, // nil → mtg-ноды не синхронизируются
		Role:            roleSrc,
		Peers:           peerMap,

		RuntimeStaleAfter: runtimeStale,
	})
	if err := srv.Run(ctx, *listen); err != nil {
		logger.Error("serve", "err", err)
		return 1
	}
	return 0
}

// startRoleMonitor включает автомат роли (ADR 0002), если задан кластер голосующих
// машин или Patroni. Иначе nil - роль узла = роль локального PG, как раньше.
func startRoleMonitor(ctx context.Context, logger *slog.Logger, st *store.Store,
	selfCode, clusterSecret string, peerMap map[string]string) (server.RoleSource, error) {
	patroniURL := envOr("TELEMUX_PATRONI_URL", "")
	if len(peerMap) == 0 && patroniURL == "" {
		return nil, nil
	}
	if selfCode == "" {
		return nil, fmt.Errorf("TELEMUX_NODE_CODE обязателен при TELEMUX_PEERS/TELEMUX_PATRONI_URL")
	}
	if _, self := peerMap[selfCode]; self {
		return nil, fmt.Errorf("TELEMUX_PEERS не должен содержать себя (%s)", selfCode)
	}
	fenceAfter, err := durEnv("TELEMUX_FENCE_AFTER", 10*time.Second)
	if err != nil {
		return nil, err
	}
	hold, err := durEnv("TELEMUX_MASTER_HOLD", 5*time.Second)
	if err != nil {
		return nil, err
	}
	interval, err := durEnv("TELEMUX_ROLE_INTERVAL", 2*time.Second)
	if err != nil {
		return nil, err
	}

	cfg := role.Config{
		Self: selfCode, Peers: peers.Codes(peerMap), RequirePatroni: patroniURL != "",
		FenceAfter: fenceAfter, MasterHold: hold,
	}
	probes := role.Probes{PG: st, Standbys: st}
	if len(peerMap) > 0 {
		if clusterSecret == "" {
			return nil, fmt.Errorf("TELEMUX_CLUSTER_SECRET обязателен при TELEMUX_PEERS (пинг соседей)")
		}
		probes.Peers = &peers.Prober{Peers: peerMap, Secret: clusterSecret}
	}
	if patroniURL != "" {
		pc := patroni.New(patroniURL, nil)
		probes.Patroni = pc
		// Проверка инварианта fencing: telemux должен прекратить запись раньше,
		// чем реплика вправе промоутиться. Не фатально - Patroni мог ещё не подняться.
		if dc, err := pc.Config(ctx); err != nil {
			logger.Warn("Patroni: не удалось прочитать /config для проверки таймаутов", "err", err)
		} else {
			for _, v := range dc.CheckTimings(fenceAfter + 2*interval) {
				logger.Warn("НАРУШЕН ИНВАРИАНТ FENCING", "detail", v)
			}
		}
	}
	if roles := splitList(envOr("TELEMUX_PG_FENCE_ROLES", "")); len(roles) > 0 {
		probes.Fence = store.Fencer{Store: st, Roles: roles}
		logger.Info("PG-fence включён", "roles", roles)
	}

	mon := role.NewMonitor(cfg, probes, interval, logger)
	mon.Tick(ctx) // первое решение до старта HTTP
	go mon.Run(ctx)
	logger.Info("автомат роли запущен", "self", selfCode, "peers", cfg.Peers, "quorum", cfg.Quorum(),
		"patroni", patroniURL != "", "fence_after", fenceAfter, "master_hold", hold)
	return mon, nil
}

// runFence - ручное управление PG-fence (аварийный выход, если telemux лежит в fenced).
func runFence(args []string) int {
	fs := flag.NewFlagSet("fence", flag.ContinueOnError)
	dsn := fs.String("db", envOr("DATABASE_URL", ""), "DSN локального PG")
	roles := fs.String("roles", envOr("TELEMUX_PG_FENCE_ROLES", ""), "роли приложений, чьи сессии рвать при on")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	action := fs.Arg(0)
	if action != "on" && action != "off" && action != "status" {
		fmt.Fprintln(os.Stderr, "использование: telemux fence --db <dsn> [--roles bot] on|off|status")
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
	if action != "status" {
		if err := (store.Fencer{Store: st, Roles: splitList(*roles)}).SetWriteFence(ctx, action == "on"); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
	}
	on, err := st.WriteFenceStatus(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	fmt.Printf("PG-fence: %v\n", map[bool]string{true: "ВКЛЮЧЁН (default_transaction_read_only=on)", false: "выключен"}[on])
	if action == "off" {
		fmt.Println("внимание: если telemux запущен и узел не мастер, он включит fence снова")
	}
	return 0
}

// runtimeStaleAfter - через сколько runtime-снимок машины считается устаревшим:
// TELEMUX_RUNTIME_STALE_AFTER, иначе три периода отчёта агентов
// (TELEMUX_RUNTIME_EVERY, по умолчанию 60s - как --runtime-every агента).
func runtimeStaleAfter() (time.Duration, error) {
	every, err := durEnv("TELEMUX_RUNTIME_EVERY", server.RuntimeEveryDefault)
	if err != nil {
		return 0, err
	}
	return durEnv("TELEMUX_RUNTIME_STALE_AFTER", 3*every)
}

func durEnv(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s=%q: нужна положительная длительность (10s, 1m)", key, v)
	}
	return d, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDuration - длительность из окружения (например 60s); пусто или мусор - def.
func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

// runImportToml импортирует юзеров из telemt users.toml (с секретами) в telemux-PG.
// Путь обратной совместимости: секреты совпадают с тем, что на ноде → старые ссылки живут.
func runImportToml(args []string) int {
	fs := flag.NewFlagSet("import-toml", flag.ContinueOnError)
	dsn := fs.String("db", envOr("DATABASE_URL", ""), "DSN telemux-PG")
	file := fs.String("file", "", "путь к users.toml (пусто = stdin)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var data []byte
	var err error
	if *file == "" || *file == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "чтение users.toml: %v\n", err)
		return 1
	}
	parsed := telemtcfg.ParseUsersToml(string(data))
	if len(parsed) == 0 {
		fmt.Fprintln(os.Stderr, "в файле не найдено юзеров (секция [access.users])")
		return 1
	}
	rows := make([]store.ImportRow, 0, len(parsed))
	for _, u := range parsed {
		rows = append(rows, store.ImportRow{
			Username: u.Username, Secret: u.Secret,
			ExpirationAt: u.ExpirationAt, MaxTCPConns: u.MaxTCPConns,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PG: %v\n", err)
		return 1
	}
	defer st.Close()

	r, err := role.Detect(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "роль: %v\n", err)
		return 1
	}
	if !r.IsMaster() {
		fmt.Fprintln(os.Stderr, "это replica (PG read-only); импорт делается на master")
		return 1
	}

	ins, upd, err := st.ImportUsers(ctx, rows)
	if err != nil {
		fmt.Fprintf(os.Stderr, "импорт: %v\n", err)
		return 1
	}
	fmt.Printf("импортировано: новых=%d обновлено=%d (всего в файле=%d)\n", ins, upd, len(parsed))
	return 0
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
