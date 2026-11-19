package render

import (
	"strings"

	"github.com/AndreyOsipuk/telemux/internal/servercfg"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// TelemtUsersInclude - путь include на users.toml: как у бота при config_path по
// умолчанию (/etc/telemt/telemt.toml → /etc/telemt/users.toml) и как у агента.
const TelemtUsersInclude = "/etc/telemt/users.toml"

// Значения, которых нет в каталоге настроек: в боте они постоянные (config/telemt.json).
const (
	telemtRelayIdleV2    = "true"
	telemtMaskTimingNorm = "true"
)

// telemtNetworkBlock - блок [network] машины с IPv6 (как telemtNetworkBlock бота).
const telemtNetworkBlock = `
# IPv6 к Telegram, IPv4 - запасной путь (servers.telemt_ipv6 = true)
[network]
ipv4 = true
ipv6 = true
prefer = 6
`

// telemtIPv6Listener - второй слушатель на IPv6 (как telemtIpv6Listener бота).
const telemtIPv6Listener = `
[[server.listeners]]
ip = "::"
`

// TelemtMain собирает основной telemt.toml машины сервера (ресурс telemt.config) -
// как generateMainConfig бота: релей и машина за рубежом получают один и тот же
// текст, отличается только announce_ip (адрес машины). Настройки - из
// server.settings, отсутствующие - по умолчанию каталога (они же значения бота).
func TelemtMain(srv store.Server, m Machine) ([]byte, error) {
	if srv.Engine != servercfg.EngineTelemt {
		return nil, errf(CodeWrongEngine, "сервер %d - %s, а не telemt", srv.No, srv.Engine)
	}
	if err := checkAddress(m); err != nil {
		return nil, err
	}
	if strings.TrimSpace(srv.TLSDomain) == "" {
		return nil, errf(CodeNoTLSDomain, "у сервера %d не задан tls_domain", srv.No)
	}
	ipv6 := setting(srv, "ipv6") == "1"
	vals := map[string]string{
		"announce_ip": m.Address,
		"include":     TelemtUsersInclude,
		"tls_domain":  srv.TLSDomain,

		"relay_idle_v2_enabled":             telemtRelayIdleV2,
		"mask_timing_normalization_enabled": telemtMaskTimingNorm,
	}
	if ipv6 {
		vals["prefer_ipv6"], vals["network"], vals["ipv6_listener"] = "", telemtNetworkBlock, telemtIPv6Listener
	} else {
		vals["prefer_ipv6"], vals["network"], vals["ipv6_listener"] = "prefer_ipv6 = false\n", "", ""
	}
	if list := TLSDomains(srv); len(list) > 1 {
		q := make([]string, len(list))
		for i, d := range list {
			q[i] = `"` + d + `"`
		}
		vals["tls_domains"] = "\ntls_domains = [" + strings.Join(q, ", ") + "]"
	} else {
		vals["tls_domains"] = ""
	}
	// Подстановка → ключ каталога.
	for name, key := range map[string]string{
		"port":                                 "port",
		"rst_on_close":                         "rst_on_close",
		"tg_connect":                           "tg_connect",
		"client_mss":                           "client_mss",
		"client_mss_bulk":                      "client_mss_bulk",
		"listen_backlog":                       "listen_backlog",
		"accept_permit_timeout_ms":             "accept_permit_timeout_ms",
		"max_connections":                      "max_connections",
		"client_handshake":                     "client_handshake",
		"client_keepalive":                     "client_keepalive",
		"client_ack":                           "client_ack",
		"client_first_byte_idle_secs":          "client_first_byte_idle_secs",
		"relay_idle_soft_secs":                 "relay_idle_soft_secs",
		"relay_idle_hard_secs":                 "relay_idle_hard_secs",
		"relay_idle_grace_secs":                "relay_idle_grace_secs",
		"mask_timing_normalization_floor_ms":   "mask_timing_floor_ms",
		"mask_timing_normalization_ceiling_ms": "mask_timing_ceiling_ms",
		"server_hello_delay_min_ms":            "server_hello_delay_min_ms",
		"server_hello_delay_max_ms":            "server_hello_delay_max_ms",
	} {
		v := setting(srv, key)
		if err := quotable(key, v); err != nil {
			return nil, err
		}
		if v == "" {
			return nil, errf(CodeBadSetting, "%s: пустое значение", key)
		}
		vals[name] = v
	}
	for _, d := range TLSDomains(srv) {
		if err := quotable("tls_domain", d); err != nil {
			return nil, err
		}
	}
	return []byte(fill(telemtMainTemplate, vals)), nil
}

// fill подставляет {{имя}} в шаблон за один проход (подставленный текст не
// разбирается повторно).
func fill(tpl string, vals map[string]string) string {
	pairs := make([]string, 0, 2*len(vals))
	for _, k := range sortedKeys(vals) {
		pairs = append(pairs, "{{"+k+"}}", vals[k])
	}
	return strings.NewReplacer(pairs...).Replace(tpl)
}
