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
	"github.com/AndreyOsipuk/telemux/internal/bridge"
	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/tlspin"
)

// runTarget: telemux target --code pr2 --kind relay --resources telemt.users [--relay pr2] [--disabled]
func runTarget(args []string) int {
	fs := flag.NewFlagSet("target", flag.ContinueOnError)
	dsn := fs.String("db", envOr("DATABASE_URL", ""), "DSN primary PG")
	code := fs.String("code", "", "код машины (ps1..ps8, pr1..pr4)")
	kind := fs.String("kind", "", "exit | relay | panel")
	settings := fs.String("settings", "", "настройки через запятую key=value (для panel: inbound_id, node_id, port, node_code)")
	relay := fs.String("relay", "", "для выхода: релей, к которому он держит туннель")
	webDomains := fs.String("web-domains", "", "для релея: WEB-домены через запятую (сертификаты tls.web выпускает мастер)")
	resources := fs.String("resources", "", "ресурсы через запятую, например telemt.users")
	addResource := fs.String("add-resource", "", "добавить второму экземпляру на существующем релее ресурс mtg.secondary.config")
	disabled := fs.Bool("disabled", false, "машина выключена (ревизий не получает)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *addResource != "" {
		if *code == "" || *addResource != store.ResourceMtgSecondaryConfig || *kind != "" || *resources != "" || *relay != "" || *webDomains != "" || *settings != "" || *disabled {
			fmt.Fprintln(os.Stderr, "для назначения второму mtg нужны только --code и --add-resource mtg.secondary.config")
			return 2
		}
	} else if *code == "" || (*kind != "exit" && *kind != "relay" && *kind != "panel") {
		fmt.Fprintln(os.Stderr, "нужны --code и --kind exit|relay|panel")
		return 2
	}
	kv := map[string]string{}
	for _, pair := range splitList(*settings) {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			fmt.Fprintf(os.Stderr, "настройка %q - нужен вид key=value\n", pair)
			return 2
		}
		kv[k] = v
	}
	if *webDomains != "" {
		kv[store.SettingWebDomains] = *webDomains
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "PG:", err)
		return 1
	}
	defer st.Close()
	if *addResource != "" {
		if err := st.AssignSecondaryMtg(ctx, *code); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("%s назначен машине %s\n", *addResource, *code)
		return 0
	}
	t := store.Target{Code: *code, Kind: *kind, RelayCode: *relay, Resources: splitList(*resources), Enabled: !*disabled, Settings: kv}
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

// runAgent: агент на машине.
func runAgent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	masters := fs.String("master", envOr("TELEMUX_AGENT_MASTERS", ""), "адреса мастера через запятую по приоритету")
	masterPin := fs.String("master-pin", envOr("TELEMUX_AGENT_MASTER_PIN", ""), "sha256 сертификата мастера для https-адресов")
	tokenFile := fs.String("token-file", envOr("TELEMUX_AGENT_TOKEN_FILE", "/etc/telemux/agent.token"), "файл с токеном машины")
	target := fs.String("target", envOr("TELEMUX_AGENT_TARGET", ""), "код этой машины")
	pathsCache := fs.String("relay-paths-cache", envOr("TELEMUX_AGENT_RELAY_PATHS_CACHE", agent.DefaultRelayPathsCache), "кэш подтверждённых путей релея; none - только память")
	usersFile := fs.String("users-file", envOr("TELEMUX_AGENT_USERS_FILE", "/etc/telemt/users.toml"), "users.toml на машине; none - telemt на машине нет")
	mtgFile := fs.String("mtg-file", envOr("TELEMUX_AGENT_MTG_FILE", "none"), "config.toml mtg-multi; none - mtg не ведётся")
	mtgStats := fs.String("mtg-stats-url", envOr("TELEMUX_AGENT_MTG_STATS_URL", "http://127.0.0.1:8081/stats"), "/stats mtg-multi (secrets_sha256)")
	mtgUnit := fs.String("mtg-unit", envOr("TELEMUX_AGENT_MTG_UNIT", "mtg-multi"), "юнит systemd mtg-multi")
	secondaryFile := fs.String("mtg-secondary-file", envOr("TELEMUX_AGENT_MTG_SECONDARY_FILE", ""), "отдельный config.toml второго процесса mtg")
	secondaryUnit := fs.String("mtg-secondary-unit", envOr("TELEMUX_AGENT_MTG_SECONDARY_UNIT", ""), "отдельный юнит второго процесса mtg")
	secondaryStats := fs.String("mtg-secondary-stats-url", envOr("TELEMUX_AGENT_MTG_SECONDARY_STATS_URL", ""), "отдельный локальный /stats второго процесса mtg")
	mtgHot := fs.Bool("mtg-hot-reload", envOr("TELEMUX_AGENT_MTG_HOT_RELOAD", "1") == "1", "SIGHUP, если изменились только секреты")
	mode := fs.String("mode", envOr("TELEMUX_AGENT_MODE", agent.ModeCompare), "режим: compare (только сравнить) или apply (применять)")
	configFile := fs.String("config-file", envOr("TELEMUX_AGENT_CONFIG_FILE", "/etc/telemt/telemt.toml"), "telemt.toml (WEB-профили и ресурс telemt.config)")
	mainConfig := fs.Bool("main-config", envOr("TELEMUX_AGENT_MAIN_CONFIG", "0") == "1", "вести основной telemt.toml (ресурс telemt.config)")
	localConfig := fs.String("local-config", envOr("TELEMUX_AGENT_LOCAL_CONFIG", "/etc/telemt/telemt.local.toml"), "машинная часть telemt.toml: сильнее telemt.config, файла может не быть")
	configStateURL := fs.String("config-state-url", envOr("TELEMUX_AGENT_CONFIG_STATE_URL", agent.DefaultConfigStateURL), "API telemt: состояние всего конфига (для telemt.config)")
	stateURL := fs.String("state-url", envOr("TELEMUX_AGENT_STATE_URL", agent.DefaultStateURL), "API telemt: принят ли конфиг")
	unit := fs.String("telemt-unit", envOr("TELEMUX_AGENT_TELEMT_UNIT", "telemt"), "юнит systemd telemt (SIGHUP)")
	via := fs.String("via", envOr("TELEMUX_AGENT_VIA", ""), "через какой мост ходит агент (для отчёта)")
	bridgeURL := fs.String("bridge-url", envOr("TELEMUX_AGENT_BRIDGE_URL", ""), "адрес моста этого выхода (обычно http://127.0.0.1:18480): сводка релеев в runtime-отчёте; пусто - не читать")
	checks := fs.String("checks", envOr("TELEMUX_AGENT_CHECKS", ""), "локальные проверки для отчёта: имя=http://адрес[,...], например bridge=http://127.0.0.1:18480/healthz")
	wait := fs.Duration("wait", 55*time.Second, "long-poll")
	recheck := fs.Duration("recheck", 5*time.Minute, "перепроверка диска без новой ревизии")
	runtimeEvery := fs.Duration("runtime-every", envDuration("TELEMUX_AGENT_RUNTIME_EVERY", 60*time.Second), "период runtime-отчёта (нагрузка, подключения, журнал); 0 - выключить")
	telemtAPI := fs.String("telemt-api", envOr("TELEMUX_AGENT_TELEMT_API", "http://127.0.0.1:9091"), "API telemt для runtime-отчёта")
	telemtBin := fs.String("telemt-bin", envOr("TELEMUX_AGENT_TELEMT_BIN", "telemt"), "бинарь telemt (версия через --version)")
	telemtConfig := fs.String("telemt-config", envOr("TELEMUX_AGENT_TELEMT_CONFIG", "/etc/telemt/telemt.toml"), "telemt.toml для сводки в runtime-отчёте")
	mtgBin := fs.String("mtg-bin", envOr("TELEMUX_AGENT_MTG_BIN", ""), "бинарь mtg для версии (--version); пусто - mtg-multi, если есть в PATH, иначе mtg")
	journalUnit := fs.String("journal-unit", envOr("TELEMUX_AGENT_JOURNAL_UNIT", "telemt"), "юнит telemt для счётчиков ошибок журнала")
	tlsWeb := fs.Bool("tls-web", envOr("TELEMUX_AGENT_TLS_WEB", "0") == "1", "раскладывать сертификаты WEB-доменов (ресурс tls.web)")
	tlsDir := fs.String("tls-dir", envOr("TELEMUX_AGENT_TLS_DIR", agent.DefaultTLSDir), "каталог сертификатов nginx: <домен>.pem и <домен>.key")
	nginxUnit := fs.String("nginx-unit", envOr("TELEMUX_AGENT_NGINX_UNIT", "nginx"), "юнит systemd nginx (reload)")
	nginxBin := fs.String("nginx-bin", envOr("TELEMUX_AGENT_NGINX_BIN", "nginx"), "бинарь nginx (nginx -t)")
	envFile := fs.String("env-file", envOr("TELEMUX_AGENT_ENV_FILE", agent.DefaultAgentEnvFile), "env-файл агента: его же получает stack-apply (обновление telemt и mtg-multi)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "токен машины:", err)
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	checkList, err := agent.ParseChecks(*checks)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if *usersFile == "none" {
		*usersFile = ""
	}
	if *mtgFile == "none" {
		*mtgFile = ""
	}
	if err := validateSecondaryMtg(*secondaryFile, *secondaryUnit, *secondaryStats, *mtgUnit, *mtgStats); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	mainFile := ""
	if *mainConfig {
		if *usersFile == "" {
			fmt.Fprintln(os.Stderr, "telemt.config ведётся только вместе с users.toml (--users-file)")
			return 2
		}
		mainFile = *configFile
	}
	certDir := ""
	if *tlsWeb {
		if *tlsDir == "" {
			fmt.Fprintln(os.Stderr, "для tls.web нужен каталог сертификатов (--tls-dir)")
			return 2
		}
		certDir = *tlsDir
	}
	var applier *agent.Applier
	var mtgApplier, secondaryApplier *agent.MtgApplier
	var configApplier *agent.ConfigApplier
	var tlsApplier *agent.TLSApplier
	var stackApplier *agent.StackApplier
	if *mode == agent.ModeApply {
		stackApplier = &agent.StackApplier{EnvFile: *envFile}
		if certDir != "" {
			tlsApplier = &agent.TLSApplier{Nginx: agent.SystemdNginx{Unit: *nginxUnit, Bin: *nginxBin}}
		}
		if mainFile != "" {
			configApplier = &agent.ConfigApplier{StateURL: *configStateURL, Service: agent.SystemdTelemt{Unit: *unit}}
		}
		if *usersFile != "" {
			applier = &agent.Applier{ConfigFile: *configFile, StateURL: *stateURL, Reloader: agent.SystemdReloader{Unit: *unit}}
		}
		if *secondaryFile != "" {
			secondaryApplier = &agent.MtgApplier{StatsURL: *secondaryStats, Service: agent.SystemdMtg{Unit: *secondaryUnit}, HotReload: *mtgHot}
		}
		if *mtgFile != "" {
			mtgApplier = &agent.MtgApplier{StatsURL: *mtgStats, Service: agent.SystemdMtg{Unit: *mtgUnit}, HotReload: *mtgHot}
		}
	}
	if *pathsCache == "none" {
		*pathsCache = ""
	}
	a, err := agent.New(agent.Config{
		RelayPathsCache:  *pathsCache,
		MtgSecondaryFile: *secondaryFile, MtgSecondaryApplier: secondaryApplier,
		Applier: applier, MtgApplier: mtgApplier, MtgFile: *mtgFile, MasterPin: *masterPin,
		TelemtConfigFile: mainFile, TelemtLocalFile: *localConfig, ConfigApplier: configApplier,
		TLSDir: certDir, TLSApplier: tlsApplier, StackApplier: stackApplier,
		MasterURLs: splitList(*masters), Token: strings.TrimSpace(string(raw)), Target: *target,
		Version: version, Via: *via, UsersFile: *usersFile, Mode: *mode,
		Wait: *wait, RecheckEvery: *recheck, Checks: checkList, Log: logger,
		RuntimeEvery: *runtimeEvery,
		Runtime: &agent.RuntimeCollector{
			Telemt: *usersFile != "", Mtg: *mtgFile != "", UsersFile: *usersFile,
			TelemtAPI: *telemtAPI, TelemtBin: *telemtBin, TelemtConfig: *telemtConfig,
			MtgStatsURL: *mtgStats, MtgBin: *mtgBin, JournalUnit: *journalUnit, Log: logger,
			BridgeURL: *bridgeURL,
		},
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

// runBridge: мост на машине-выходе - принимает агента релея из туннеля и
// пересылает его запросы мастеру; при недоступном мастере отдаёт кэш.
func runBridge(args []string) int {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	listen := fs.String("listen", envOr("TELEMUX_BRIDGE_LISTEN", "127.0.0.1:18480"), "адрес моста (loopback, куда приходит туннель)")
	masters := fs.String("master", envOr("TELEMUX_BRIDGE_MASTERS", ""), "адреса мастера через запятую по приоритету")
	masterPin := fs.String("master-pin", envOr("TELEMUX_BRIDGE_MASTER_PIN", ""), "sha256 сертификата мастера для https-адресов")
	code := fs.String("code", envOr("TELEMUX_BRIDGE_CODE", ""), "код этого выхода: заголовок X-Telemux-Bridge в запросах к мастеру; пусто - без заголовка")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	b, err := bridge.New(bridge.Config{Code: *code, MasterURLs: splitList(*masters), MasterPin: *masterPin, Log: logger})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := b.Run(ctx, *listen); err != nil {
		logger.Error("мост", "err", err)
		return 1
	}
	return 0
}

// runTLSCert: выпустить самоподписанный сертификат мастера и вывести отпечаток
// для закрепления у агентов и мостов.
func runTLSCert(args []string) int {
	fs := flag.NewFlagSet("tls-cert", flag.ContinueOnError)
	cert := fs.String("cert", "/etc/telemux/tls/cert.pem", "куда положить сертификат")
	key := fs.String("key", "/etc/telemux/tls/key.pem", "куда положить ключ (0600)")
	cn := fs.String("cn", "telemux-master", "CommonName")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if _, err := os.Stat(*cert); err == nil {
		fmt.Fprintf(os.Stderr, "%s уже есть - не перезаписываю (отпечаток сменился бы у всех агентов)\n", *cert)
		if fp, err := tlspin.FingerprintFile(*cert); err == nil {
			fmt.Println(fp)
		}
		return 1
	}
	fp, err := tlspin.Generate(*cert, *key, *cn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(fp)
	return 0
}

// runMachines: telemux machines - машины флота и последний отчёт их агентов.
func runMachines(args []string) int {
	fs := flag.NewFlagSet("machines", flag.ContinueOnError)
	dsn := fs.String("db", envOr("DATABASE_URL", ""), "DSN PG")
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
	const page = 100
	for offset := 0; ; offset += page {
		list, total, err := st.Machines(ctx, store.MachineFilter{}, page, offset)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, m := range list {
			fmt.Println(formatMachine(m, time.Now()))
		}
		if offset+len(list) >= total || len(list) == 0 {
			fmt.Printf("всего машин: %d\n", total)
			return 0
		}
	}
}

// formatMachine - строка сводки: код, вид, когда отчитывался агент, проверки, ресурсы.
func formatMachine(m store.Machine, now time.Time) string {
	seen := "агента нет"
	if m.SeenAt != nil {
		seen = fmt.Sprintf("агент %s назад", now.Sub(*m.SeenAt).Truncate(time.Second))
	}
	parts := []string{fmt.Sprintf("%-6s %-5s", m.Code, m.Kind), seen}
	if !m.Enabled {
		parts = append(parts, "выключена")
	}
	for _, c := range m.Checks {
		if c.OK {
			parts = append(parts, c.Name+" ok")
		} else {
			parts = append(parts, fmt.Sprintf("%s НЕ ok (%s)", c.Name, c.Error))
		}
	}
	for _, o := range m.Observed {
		s := fmt.Sprintf("%s r%d %s", o.Resource, o.Revision, o.Status)
		if o.Error != "" {
			s += " (" + o.Error + ")"
		}
		parts = append(parts, s)
	}
	if len(m.Resources) == 0 {
		parts = append(parts, "без ресурсов")
	}
	return strings.Join(parts, " | ")
}
