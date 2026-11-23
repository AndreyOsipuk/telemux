package render

import (
	"encoding/hex"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/servercfg"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// User - пользователь telemux для [secrets] mtg: имя (ключ бота), 16-байтный ключ в
// hex, срок и прежний ключ после смены до своего срока.
type User struct {
	Key                     string
	Secret                  string
	ExpiresAt               *time.Time // nil - без срока
	PreviousSecret          string
	PreviousSecretExpiresAt *time.Time
}

// MtgOpts - параметры сборки mtg.
type MtgOpts struct {
	// Now - момент сборки: истёкшие пользователи и отозванные прежние ключи в
	// [secrets] не попадают (mtg сам по сроку не отбивает). Нулевое - time.Now().
	Now time.Time
}

// Значения по умолчанию - как в разборе mtg-нод бота (parseMtgNodes, parseRelayLocal).
const (
	mtgDefaultPreferIP   = "only-ipv4"
	mtgDefaultAPIBind    = "127.0.0.1:8081"
	mtgDefaultDecoyDir   = "/var/www/decoy"
	mtgPrometheusBind    = "127.0.0.1:3129"
	mtgSecuredFrameTO    = "5s"
	mtgExtraDomainSuffix = "~sni"
	mtgLegacySuffix      = "_legacy"
)

// mtgDCPoolDCs - DC тёплого пула (MTG_DC_POOL_DCS бота).
var mtgDCPoolDCs = []int{2, -2, 203, -4}

var (
	frontingRe  = regexp.MustCompile(`^(\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-fA-F:]+\]):(\d{1,5})(\+pp)?$`)
	loopbackRe  = regexp.MustCompile(`^(?:(127\.\d{1,3}\.\d{1,3}\.\d{1,3})|\[(::1)\]):(\d{1,5})$`)
	relayTLSRe  = regexp.MustCompile(`^([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	mtgKeyRe    = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	mtgSecretRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type mtgFronting struct {
	host          string
	port          int
	proxyProtocol bool
}

type mtgWeb struct{ bindTo, host, decoyDir string }

// mtgNodeOpts - параметры config.toml (MtgNodeOpts бота).
type mtgNodeOpts struct {
	bindTo         string
	bindAddrs      []string
	prometheusBind string
	apiBindTo      string
	frontingDomain string
	extraDomains   []string
	preferIP       string
	maxConns       int
	clientMss      int
	clientMssBulk  int
	web            *mtgWeb
	fronting       *mtgFronting
}

// parseFronting - как parseFrontingAddr бота: "127.0.0.1:8080" или "...:8446+pp";
// пусто или мусор - nil, раздел не пишется.
func parseFronting(raw string) *mtgFronting {
	m := frontingRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return nil
	}
	port, _ := strconv.Atoi(m[2])
	if port < 1 || port > 65535 {
		return nil
	}
	return &mtgFronting{host: strings.TrimSuffix(strings.TrimPrefix(m[1], "["), "]"), port: port, proxyProtocol: m[3] != ""}
}

// isLoopbackBind - "127.x.x.x:порт" или "[::1]:порт" (isLoopbackBind бота).
func isLoopbackBind(bind string) bool {
	m := loopbackRe.FindStringSubmatch(bind)
	if m == nil {
		return false
	}
	p, _ := strconv.Atoi(m[3])
	return p > 0 && p < 65536
}

// parseWeb - раздел [web]: нужны и домен, и адрес на loopback (parseMtgWeb бота).
func parseWeb(bind, decoy, host string) *mtgWeb {
	bind = strings.TrimSpace(bind)
	if host == "" || bind == "" || !isLoopbackBind(bind) {
		return nil
	}
	d := strings.TrimSpace(decoy)
	if d == "" {
		d = mtgDefaultDecoyDir
	}
	return &mtgWeb{bindTo: bind, host: host, decoyDir: d}
}

// uintOr - неотрицательное целое; пусто или мусор - def (envUint бота).
func uintOr(v string, def int) int {
	t := strings.TrimSpace(v)
	if t == "" || strings.Trim(t, "0123456789") != "" {
		return def
	}
	n, err := strconv.Atoi(t)
	if err != nil {
		return def
	}
	return n
}

// webDomain - домен WEB-ссылки сервера: первая включённая точка входа web
// (MTG_<CODE>_WEB_DOMAIN бота).
func webDomain(srv store.Server) string {
	for _, ep := range srv.Endpoints {
		if ep.Protocol == "web" && ep.Enabled {
			return ep.Host
		}
	}
	return ""
}

// nodeOpts - параметры ноды (parseMtgNodes бота).
func nodeOpts(srv store.Server) (mtgNodeOpts, error) {
	if strings.TrimSpace(srv.TLSDomain) == "" {
		return mtgNodeOpts{}, errf(CodeNoTLSDomain, "у сервера %d не задан tls_domain (домен секретов mtg)", srv.No)
	}
	port := uintOr(setting(srv, "BIND_PORT"), 8767)
	o := mtgNodeOpts{
		prometheusBind: mtgPrometheusBind,
		bindTo:         "0.0.0.0:" + strconv.Itoa(port),
		apiBindTo:      setting(srv, "API_BIND"),
		frontingDomain: srv.TLSDomain,
		preferIP:       setting(srv, "PREFER_IP"),
		maxConns:       uintOr(setting(srv, "MAX_CONNS"), 2000),
		web:            parseWeb(setting(srv, "WEB_BIND"), setting(srv, "WEB_DECOY_DIR"), webDomain(srv)),
		fronting:       parseFronting(setting(srv, "FRONTING")),
	}
	if o.apiBindTo == "" {
		o.apiBindTo = mtgDefaultAPIBind
	}
	if o.preferIP == "" {
		o.preferIP = mtgDefaultPreferIP
	}
	return o, nil
}

// relayOpts - параметры локального mtg релея (relayMtgOpts бота): всё как у ноды,
// но свои адреса, /stats, client-mss, заглушка и WEB; домен секретов - свой домен
// релея (RELAY_TLS_DOMAIN), прежний домен ноды остаётся дополнительным, пока не
// задан RELAY_DROP_NODE_DOMAIN.
func relayOpts(srv store.Server, node mtgNodeOpts) mtgNodeOpts {
	o := node
	o.web, o.fronting = nil, nil
	var binds []string
	for _, a := range strings.Split(setting(srv, "RELAY_BIND"), ",") {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(binds, a) {
			binds = append(binds, a)
		}
	}
	if len(binds) == 0 {
		binds = []string{node.bindTo}
	}
	tlsDomain := strings.ToLower(strings.TrimSpace(setting(srv, "RELAY_TLS_DOMAIN")))
	if l := len(tlsDomain); l < 4 || l > 253 || !relayTLSRe.MatchString(tlsDomain) || tlsDomain == strings.ToLower(node.frontingDomain) {
		tlsDomain = ""
	}
	if tlsDomain != "" {
		o.frontingDomain = tlsDomain
		if setting(srv, "RELAY_DROP_NODE_DOMAIN") != "1" {
			o.extraDomains = []string{node.frontingDomain}
		}
	}
	o.bindTo, o.bindAddrs = binds[0], binds
	o.prometheusBind = setting(srv, "RELAY_PROMETHEUS_BIND")
	o.apiBindTo = strings.TrimSpace(setting(srv, "RELAY_API_BIND"))
	if o.apiBindTo == "" {
		o.apiBindTo = mtgDefaultAPIBind
	}
	o.clientMss = uintOr(setting(srv, "RELAY_CLIENT_MSS"), 92)
	o.clientMssBulk = uintOr(setting(srv, "RELAY_CLIENT_MSS_BULK"), 1400)
	o.fronting = parseFronting(setting(srv, "RELAY_FRONTING"))
	o.web = parseWeb(setting(srv, "RELAY_WEB_BIND"), setting(srv, "RELAY_WEB_DECOY_DIR"), webDomain(srv))
	return o
}

// MtgConfig собирает config.toml mtg-multi машины сервера (ресурс mtg.config) - как
// buildMtgConfigText бота: машина за рубежом (exit) - конфиг ноды, релей (relay) -
// конфиг локального mtg релея (RELAY_MODE=local-mtg). [secrets] - включённые
// пользователи со сроком позже opts.Now (и без срока), прежний ключ - пока не отозван.
func MtgConfig(srv store.Server, m Machine, users []User, opts MtgOpts) ([]byte, error) {
	if srv.Engine != servercfg.EngineMtg {
		return nil, errf(CodeWrongEngine, "сервер %d - %s, а не mtg", srv.No, srv.Engine)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	o, err := nodeOpts(srv)
	if err != nil {
		return nil, err
	}
	switch m.Kind {
	case "exit":
	case "relay":
		if setting(srv, "RELAY_MODE") != "local-mtg" {
			return nil, errf(CodeBadSetting, "у релея %s нет своего mtg: RELAY_MODE не local-mtg", m.Code)
		}
		o = relayOpts(srv, o)
	default:
		return nil, errf(CodeUnknown, "машина %s вида %q: mtg собирается для exit и relay", m.Code, m.Kind)
	}
	for _, v := range append([]string{o.apiBindTo, o.prometheusBind, o.frontingDomain, o.preferIP}, o.bindAddrs...) {
		if err := quotable("mtg", v); err != nil {
			return nil, err
		}
	}
	if o.web != nil {
		if err := quotable("WEB_DECOY_DIR", o.web.decoyDir); err != nil {
			return nil, err
		}
	}
	return []byte(buildMtg(users, o, now)), nil
}

// bindTo - строка для одного адреса, массив - для нескольких.
func (o mtgNodeOpts) bindToTOML() string {
	addrs := o.bindAddrs
	if len(addrs) == 0 {
		addrs = []string{o.bindTo}
	}
	if len(addrs) == 1 {
		return `"` + addrs[0] + `"`
	}
	q := make([]string, len(addrs))
	for i, a := range addrs {
		q[i] = `"` + a + `"`
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func buildMtg(users []User, o mtgNodeOpts, now time.Time) string {
	preferIP := o.preferIP
	if preferIP == "" {
		preferIP = "prefer-ipv6"
	}
	lines := []string{
		"# Auto-generated by mtproxy-bot (mtg-multi). DO NOT EDIT MANUALLY.",
		"debug = false",
		"bind-to = " + o.bindToTOML(),
		"concurrency = 8192",
		`prefer-ip = "` + preferIP + `"`,
		`tolerate-time-skewness = "5s"`,
	}
	if o.apiBindTo != "" {
		lines = append(lines, `api-bind-to = "`+o.apiBindTo+`"`)
	}
	lines = append(lines, "", "[defense.anti-replay]", "enabled = true")
	lines = append(lines, "", "[defense.blocklist]", "enabled = false")
	var network []string
	if o.clientMss > 0 {
		network = append(network, "client-mss = "+strconv.Itoa(o.clientMss))
	}
	if o.clientMssBulk > 0 {
		network = append(network, "client-mss-bulk = "+strconv.Itoa(o.clientMssBulk))
	}
	if len(network) > 0 {
		lines = append(append(lines, "", "[network]"), network...)
	}
	lines = append(lines, "", "[network.timeout]", `tcp = "5s"`)
	dcs := make([]string, len(mtgDCPoolDCs))
	for i, dc := range mtgDCPoolDCs {
		dcs[i] = strconv.Itoa(dc)
	}
	lines = append(lines, "", "[dc-pool]", "enabled = true", "size = 2", "dcs = ["+strings.Join(dcs, ", ")+"]")
	lines = append(lines, "", "[secured]", "enabled = true", `frame-timeout = "`+mtgSecuredFrameTO+`"`)
	if f := o.fronting; f != nil {
		lines = append(lines, "", "[domain-fronting]", `host = "`+f.host+`"`, "port = "+strconv.Itoa(f.port),
			"proxy-protocol = "+strconv.FormatBool(f.proxyProtocol))
	}
	if w := o.web; w != nil {
		lines = append(lines, "", "[web]", `bind-to = "`+w.bindTo+`"`, `host = "`+w.host+`"`, `decoy-dir = "`+w.decoyDir+`"`)
	}
	lines = append(lines, "", "[stats.prometheus]", "enabled = true", `bind-to = "`+o.prometheusBind+`"`,
		`http-path = "/"`, `metric-prefix = "mtg"`)
	if o.maxConns > 0 {
		lines = append(lines, "", "[throttle]", "max-connections = "+strconv.Itoa(o.maxConns), `check-interval = "5s"`)
	}
	lines = append(lines, "", "[secrets]")
	for _, r := range mtgConfigSecretRows(users, o, now) {
		lines = append(lines, `"`+r.key+`" = "`+r.secret+`"`)
	}
	return strings.Join(lines, "\n") + "\n"
}

type secretRow struct{ key, secret string }

// fullSecret - ee + ключ + hex(домен), как в ссылке (buildFullSecret бота).
func fullSecret(secret, domain string) string {
	return "ee" + secret + hex.EncodeToString([]byte(domain))
}

// validUser - ключ и секреты, которые можно вписать в [secrets] без поломки TOML.
// Такие же проверки проходит пользователь при записи через API.
func validUser(u User) bool {
	return mtgKeyRe.MatchString(u.Key) && mtgSecretRe.MatchString(u.Secret) &&
		(u.PreviousSecret == "" || mtgSecretRe.MatchString(u.PreviousSecret))
}

// mtgSecretRows - строки [secrets] одного домена (mtgSecretRows бота).
func mtgSecretRows(users []User, domain string, now time.Time) []secretRow {
	var rows []secretRow
	for _, u := range users {
		if !validUser(u) {
			continue
		}
		if u.ExpiresAt != nil && !u.ExpiresAt.After(now) {
			continue
		}
		rows = append(rows, secretRow{u.Key, fullSecret(u.Secret, domain)})
		if u.PreviousSecret != "" && u.PreviousSecretExpiresAt != nil && u.PreviousSecretExpiresAt.After(now) {
			rows = append(rows, secretRow{u.Key + mtgLegacySuffix, fullSecret(u.PreviousSecret, domain)})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	return rows
}

// mtgConfigSecretRows - основной домен плюс копия на каждый дополнительный с
// суффиксом ~sniN (mtgConfigSecretRows бота).
func mtgConfigSecretRows(users []User, o mtgNodeOpts, now time.Time) []secretRow {
	rows := mtgSecretRows(users, o.frontingDomain, now)
	main := strings.ToLower(o.frontingDomain)
	var extra []string
	for _, d := range o.extraDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" && d != main && !slices.Contains(extra, d) {
			extra = append(extra, d)
		}
	}
	if len(extra) == 0 {
		return rows
	}
	for i, d := range extra {
		for _, r := range mtgSecretRows(users, d, now) {
			rows = append(rows, secretRow{r.key + mtgExtraDomainSuffix + strconv.Itoa(i+1), r.secret})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	return rows
}
