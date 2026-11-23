// Package agentjoin - добавление машины одной командой: проверка ввода,
// команда curl ... | sudo bash, скрипт установки агента и его agent.env.
//
// В скрипт и команду попадают только значения, прошедшие проверку здесь же:
// код машины, hex-токен, https-адрес мастера, IPv4 выхода, socks-вход на
// loopback и base64-отпечаток. Каждое значение в скрипте ещё и экранируется.
package agentjoin

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// TTL - срок жизни одноразового токена установки.
const TTL = time.Hour

// BridgePort - порт моста на машине-выходе (telemux bridge).
const BridgePort = 18480

// Пути на новой машине.
const (
	UsersFile = "/etc/telemt/users.toml"
	MtgFile   = "/etc/mtg-multi/config.toml"
	TokenFile = "/etc/telemux/agent.token"
)

// Ресурсы, которые можно назначить машине при добавлении.
const (
	ResourceTelemtUsers        = "telemt.users"
	ResourceMtgConfig          = "mtg.config"
	ResourceMtgSecondaryConfig = "mtg.secondary.config"
)

//go:embed telemux-agent.service
var agentUnit string

// AgentUnit - юнит telemux-agent.service, который ставит скрипт
// (копия deploy/telemux-agent.service, совпадение проверяет тест).
func AgentUnit() string { return agentUnit }

var (
	codeRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	tokenRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	socksRe  = regexp.MustCompile(`^socks5://127\.0\.0\.1:([1-9][0-9]{0,4})$`)
	pinRe    = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)
	hexPinRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	chainRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,28}$`)
)

// ValidCode - код машины по шаблону ^[a-z][a-z0-9-]{1,31}$.
func ValidCode(s string) bool { return codeRe.MatchString(s) }

// ValidToken - токен установки: 64 hex в нижнем регистре.
func ValidToken(s string) bool { return tokenRe.MatchString(s) }

// ValidChain - имя цепочки iptables (буквы, цифры, _ и -, до 28 символов).
func ValidChain(s string) bool { return chainRe.MatchString(s) }

// ParseExitAddress - публичный IPv4 выхода. Отвергает всё, кроме записи
// вида a.b.c.d, а также 0.0.0.0, loopback, multicast и link-local.
func ParseExitAddress(s string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(s)
	if err != nil || !ip.Is4() || ip.String() != s {
		return netip.Addr{}, fmt.Errorf("address %q: нужен IPv4 вида 203.0.113.10", s)
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return netip.Addr{}, fmt.Errorf("address %q: нужен адрес машины, а не служебный", s)
	}
	return ip, nil
}

// ValidBridgeSocks проверяет socks-вход на релее: socks5://127.0.0.1:<1-65535>.
func ValidBridgeSocks(s string) error {
	m := socksRe.FindStringSubmatch(s)
	if m == nil {
		return fmt.Errorf("bridge_socks %q: нужен вид socks5://127.0.0.1:<порт>", s)
	}
	if p, err := strconv.Atoi(m[1]); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("bridge_socks %q: порт от 1 до 65535", s)
	}
	return nil
}

// ValidPublicURL проверяет внешний адрес мастера для агентов: https://<IPv4 или
// имя>[:port] без пути, параметров и учётных данных.
func ValidPublicURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("TELEMUX_AGENT_PUBLIC_URL %q: нужен вид https://<адрес>:<порт> без пути", s)
	}
	host := u.Hostname()
	// IPv6 в квадратных скобках не принимается: в команде без кавычек bash
	// понял бы [..] как шаблон имён файлов.
	if ip, err := netip.ParseAddr(host); err == nil {
		if !ip.Is4() {
			return fmt.Errorf("TELEMUX_AGENT_PUBLIC_URL %q: нужен IPv4 или имя хоста", s)
		}
	} else if !hostRe.MatchString(host) {
		return fmt.Errorf("TELEMUX_AGENT_PUBLIC_URL %q: недопустимое имя хоста", s)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("TELEMUX_AGENT_PUBLIC_URL %q: порт от 1 до 65535", s)
		}
	}
	return nil
}

// ValidateResources проверяет список ресурсов агента и связь двух экземпляров mtg.
func ValidateResources(list []string) error {
	seen := map[string]bool{}
	for _, r := range list {
		if r != ResourceTelemtUsers && r != ResourceMtgConfig && r != ResourceMtgSecondaryConfig {
			return fmt.Errorf("ресурс %q: допустимы %s, %s, %s", r, ResourceTelemtUsers, ResourceMtgConfig, ResourceMtgSecondaryConfig)
		}
		if seen[r] {
			return fmt.Errorf("ресурс %q указан дважды", r)
		}
		seen[r] = true
	}
	if seen[ResourceMtgSecondaryConfig] && !seen[ResourceMtgConfig] {
		return errors.New("второму процессу нужен основной ресурс mtg.config")
	}
	return nil
}

// Route - как новая машина ходит к мастеру при установке: выход - напрямую
// по TLS с проверкой ключа сертификата, релей - через мост своего выхода по
// socks-входу.
type Route struct {
	Base  string // https://<мастер>:<порт> или http://<IPv4 выхода>:18480
	Pin   string // base64 sha256 SPKI сертификата мастера (только для https)
	Proxy string // socks5://127.0.0.1:<порт> (только для релея)
}

// DirectRoute - маршрут выхода: на мастер напрямую с закреплённым ключом.
func DirectRoute(publicURL, spkiPin string) (Route, error) {
	r := Route{Base: strings.TrimRight(publicURL, "/"), Pin: spkiPin}
	return r, r.Validate()
}

// BridgeRoute - маршрут релея: через мост выхода с адресом address по socks-входу.
func BridgeRoute(address, socks string) (Route, error) {
	ip, err := ParseExitAddress(address)
	if err != nil {
		return Route{}, err
	}
	r := Route{Base: fmt.Sprintf("http://%s:%d", ip, BridgePort), Proxy: socks}
	return r, r.Validate()
}

// Validate - повторная проверка перед подстановкой в команду и скрипт.
func (r Route) Validate() error {
	switch {
	case r.Pin != "" && r.Proxy != "":
		return errors.New("маршрут: либо отпечаток, либо прокси")
	case r.Pin != "":
		if !pinRe.MatchString(r.Pin) {
			return errors.New("маршрут: отпечаток ключа - base64 от sha256")
		}
		return ValidPublicURL(r.Base)
	case r.Proxy != "":
		if err := ValidBridgeSocks(r.Proxy); err != nil {
			return err
		}
		host, ok := strings.CutPrefix(r.Base, "http://")
		addr, port, found := strings.Cut(host, ":")
		if !ok || !found || port != strconv.Itoa(BridgePort) {
			return fmt.Errorf("маршрут %q: нужен вид http://<IPv4>:%d", r.Base, BridgePort)
		}
		_, err := ParseExitAddress(addr)
		return err
	default:
		return errors.New("маршрут: нужен отпечаток ключа мастера или socks-вход")
	}
}

// curlOpts - опции curl маршрута (без -fsSL).
func (r Route) curlOpts() []string {
	if r.Pin != "" {
		return []string{"-k", "--pinnedpubkey", "sha256//" + r.Pin}
	}
	return []string{"-x", r.Proxy}
}

// Command - одна строка для root на новой машине.
func Command(r Route, token string) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	if !ValidToken(token) {
		return "", errors.New("токен установки - 64 hex")
	}
	u := r.Base + "/v1/join/" + token
	if r.Pin != "" {
		return fmt.Sprintf("curl -fsSLk --pinnedpubkey 'sha256//%s' %s | sudo bash", r.Pin, u), nil
	}
	return fmt.Sprintf("curl -fsSL -x %s %s | sudo bash", r.Proxy, u), nil
}

// Exit - выход релея, через мост которого агент релея ходит к мастеру.
type Exit struct {
	Address string // IPv4 выхода
	Socks   string // socks-вход на релее к этому выходу
}

// EnvParams - всё для agent.env новой машины.
type EnvParams struct {
	Target    string
	Kind      string // exit | relay
	Resources []string
	PublicURL string // для выхода
	MasterPin string // для выхода: sha256 сертификата мастера (hex, как tlspin)
	Exits     []Exit // для релея
}

// Env собирает agent.env (формат deploy/agent.env.example). Режим - compare:
// в apply машину переводит человек.
func Env(p EnvParams) (string, error) {
	if !ValidCode(p.Target) {
		return "", fmt.Errorf("код машины %q не по шаблону", p.Target)
	}
	if err := ValidateResources(p.Resources); err != nil {
		return "", err
	}
	var masters []string
	switch p.Kind {
	case "exit":
		if err := ValidPublicURL(p.PublicURL); err != nil {
			return "", err
		}
		if !hexPinRe.MatchString(p.MasterPin) {
			return "", errors.New("отпечаток сертификата мастера - 64 hex")
		}
		masters = []string{strings.TrimRight(p.PublicURL, "/")}
	case "relay":
		for _, e := range p.Exits {
			r, err := BridgeRoute(e.Address, e.Socks)
			if err != nil {
				return "", err
			}
			masters = append(masters, r.Base+"|"+r.Proxy)
		}
		if len(masters) == 0 {
			return "", errors.New("у релея нет выходов с адресом и socks-входом")
		}
	default:
		return "", fmt.Errorf("агент ставится только на exit и relay, а не на %q", p.Kind)
	}
	users, mtg := "none", "none"
	for _, r := range p.Resources {
		switch r {
		case ResourceTelemtUsers:
			users = UsersFile
		case ResourceMtgSecondaryConfig:
			if p.Kind != "relay" {
				return "", errors.New("второй mtg назначается релею")
			}
		case ResourceMtgConfig:
			mtg = MtgFile
		}
	}
	var b strings.Builder
	b.WriteString("# Агент telemux - выдано мастером при добавлении машины.\n")
	b.WriteString("# Режим compare: только сравнение. Применять - TELEMUX_AGENT_MODE=apply и systemctl restart telemux-agent.\n")
	fmt.Fprintf(&b, "TELEMUX_AGENT_TARGET=%s\n", p.Target)
	fmt.Fprintf(&b, "TELEMUX_AGENT_MASTERS=%s\n", strings.Join(masters, ","))
	if p.Kind == "exit" {
		fmt.Fprintf(&b, "TELEMUX_AGENT_MASTER_PIN=%s\n", p.MasterPin)
	}
	fmt.Fprintf(&b, "TELEMUX_AGENT_TOKEN_FILE=%s\n", TokenFile)
	fmt.Fprintf(&b, "TELEMUX_AGENT_USERS_FILE=%s\n", users)
	fmt.Fprintf(&b, "TELEMUX_AGENT_MTG_FILE=%s\n", mtg)
	b.WriteString("TELEMUX_AGENT_MODE=compare\n")
	return b.String(), nil
}

// shellQuote - строка в одинарных кавычках для bash.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var scriptTpl = template.Must(template.New("join").Funcs(template.FuncMap{"q": shellQuote}).Parse(scriptText))

// Script - bash-скрипт установки агента. Значения проверяются ещё раз и
// подставляются только в одинарных кавычках.
func Script(r Route, token, target string) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if !ValidToken(token) {
		return nil, errors.New("токен установки - 64 hex")
	}
	if !ValidCode(target) {
		return nil, fmt.Errorf("код машины %q не по шаблону", target)
	}
	if strings.Contains("\n"+agentUnit+"\n", "\nTELEMUX_UNIT\n") {
		return nil, errors.New("юнит агента содержит маркер heredoc")
	}
	var buf bytes.Buffer
	err := scriptTpl.Execute(&buf, map[string]any{
		"Base": r.Base, "Token": token, "Target": target, "CurlOpts": r.curlOpts(), "Unit": strings.TrimRight(agentUnit, "\n"),
	})
	if err != nil {
		return nil, fmt.Errorf("шаблон скрипта: %w", err)
	}
	return buf.Bytes(), nil
}

const scriptText = `#!/usr/bin/env bash
# telemux: установка агента на новую машину (выдано мастером, токен одноразовый).
set -euo pipefail
umask 077

BASE={{q .Base}}
TOKEN={{q .Token}}
TARGET={{q .Target}}
CURL=(curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 20{{range .CurlOpts}} {{q .}}{{end}})

die() { echo "telemux join: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "нужен root: curl ... | sudo bash"
[ "$(uname -s)" = Linux ] || die "нужен Linux, а здесь $(uname -s)"
[ "$(uname -m)" = x86_64 ] || die "нужен amd64 (x86_64), а здесь $(uname -m)"
command -v systemctl >/dev/null || die "нужен systemd"

echo "telemux join: машина $TARGET"

bin_tmp=""
resp=""
cleanup() { rm -f -- ${bin_tmp:+"$bin_tmp"} ${resp:+"$resp"}; }
trap cleanup EXIT

# 1. Бинарь telemux - тем же путём, каким пришёл этот скрипт.
bin_tmp=$(mktemp /usr/local/bin/.telemux.XXXXXX)
"${CURL[@]}" -o "$bin_tmp" "$BASE/v1/join/$TOKEN/telemux"
chmod 0755 "$bin_tmp"
"$bin_tmp" version >/dev/null || die "скачанный бинарь не запускается"
mv -f -- "$bin_tmp" /usr/local/bin/telemux
bin_tmp=""
echo "telemux join: $(/usr/local/bin/telemux version)"

# 2. Обмен одноразового токена на токен агента. Ответ format=env:
# первая строка - токен агента, остальное - содержимое agent.env.
install -d -m 0755 /etc/telemux
resp=$(mktemp /etc/telemux/.join.XXXXXX)
"${CURL[@]}" -X POST -o "$resp" "$BASE/v1/join/$TOKEN/agent?format=env"
agent_token=$(head -n 1 "$resp")
[[ "$agent_token" =~ ^[0-9a-f]{64}$ ]] || die "мастер вернул неожиданный ответ на обмен токена"
grep -q '^TELEMUX_AGENT_TARGET=' "$resp" || die "в ответе мастера нет agent.env"

# 3. agent.token и agent.env (600). Прежний agent.env не теряется: бэкап рядом.
stamp=$(date +%Y%m%d-%H%M%S)
if [ -e /etc/telemux/agent.env ]; then
	cp -p /etc/telemux/agent.env "/etc/telemux/agent.env.bak-$stamp"
	echo "telemux join: прежний agent.env сохранён в /etc/telemux/agent.env.bak-$stamp"
fi
printf '%s\n' "$agent_token" > /etc/telemux/agent.token.new
tail -n +2 "$resp" > /etc/telemux/agent.env.new
chmod 600 /etc/telemux/agent.token.new /etc/telemux/agent.env.new
mv -f /etc/telemux/agent.token.new /etc/telemux/agent.token
mv -f /etc/telemux/agent.env.new /etc/telemux/agent.env

# Каталог сертификатов nginx (ресурс tls.web): должен быть до старта юнита, иначе
# ReadWritePaths его пропустит и агент не сможет туда писать.
if [ -d /etc/nginx ]; then
	install -d -m 0755 /etc/nginx/certs
fi

# 4. Юнит telemux-agent.
cat > /etc/systemd/system/telemux-agent.service.new <<'TELEMUX_UNIT'
{{.Unit}}
TELEMUX_UNIT
chmod 644 /etc/systemd/system/telemux-agent.service.new
mv -f /etc/systemd/system/telemux-agent.service.new /etc/systemd/system/telemux-agent.service

was_active=$(systemctl is-active telemux-agent 2>/dev/null || true)
systemctl daemon-reload
systemctl enable --now telemux-agent
if [ "$was_active" = active ]; then
	systemctl restart telemux-agent
fi

sleep 3
systemctl --no-pager --lines=0 status telemux-agent || true
journalctl -u telemux-agent -n 20 --no-pager || true

echo
echo "telemux join: агент $TARGET установлен в режиме compare (только сравнение, файлы не меняет)."
echo "Перевести в apply: TELEMUX_AGENT_MODE=apply в /etc/telemux/agent.env и systemctl restart telemux-agent"
`
