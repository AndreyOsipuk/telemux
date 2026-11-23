package servercfg

// Каталог полей. Значения по умолчанию - как в конфигурации бота. Способ
// применения выбран консервативно: всё, что движок точно не перечитывает на
// лету, - restart.

func i64(n int64) *int64 { return &n }

type bounds struct{ lo, hi int64 }

var (
	portRange = bounds{1, 65535}
	secs      = bounds{1, 86400}
	millis    = bounds{0, 60000}
)

func intField(engine, group, key, def string, b bounds, apply, ru, en, hintRU, hintEN string) Field {
	return Field{Key: key, Engine: engine, Group: group, Type: TypeInt, Default: def, Min: i64(b.lo), Max: i64(b.hi),
		Apply: apply, TitleRU: ru, TitleEN: en, HintRU: hintRU, HintEN: hintEN}
}

func field(engine, group, key, typ, def, apply, ru, en, hintRU, hintEN string) Field {
	return Field{Key: key, Engine: engine, Group: group, Type: typ, Default: def,
		Apply: apply, TitleRU: ru, TitleEN: en, HintRU: hintRU, HintEN: hintEN}
}

func enumField(engine, group, key, def string, vals []string, apply, ru, en, hintRU, hintEN string) Field {
	f := field(engine, group, key, TypeEnum, def, apply, ru, en, hintRU, hintEN)
	f.Enum = vals
	return f
}

func danger(f Field) Field { f.Danger = true; return f }

func withCheck(f Field, c func(string) error) Field { f.check = c; return f }

const (
	tm = EngineTelemt
	mg = EngineMtg
)

var catalog = []Field{
	// telemt: сеть.
	danger(intField(tm, "network", "port", "443", portRange, ApplyRestart, "Порт", "Port",
		"Порт telemt на машине; смена меняет прямые ссылки", "telemt port on the machine; changes direct links")),
	field(tm, "network", "ipv6", TypeBool, "0", ApplyRestart, "IPv6", "IPv6",
		"Слушать и ходить по IPv6", "Listen and connect over IPv6"),
	intField(tm, "network", "listen_backlog", "8192", bounds{1, 65535}, ApplyRestart, "Очередь accept", "Listen backlog",
		"Длина очереди входящих подключений", "Incoming connection queue length"),
	intField(tm, "network", "accept_permit_timeout_ms", "5000", millis, ApplyRestart, "Ожидание слота, мс", "Accept permit timeout, ms",
		"Сколько ждать свободного слота подключения", "How long to wait for a free connection slot"),
	intField(tm, "network", "max_connections", "10000", bounds{1, 1000000}, ApplyRestart, "Предел подключений", "Max connections",
		"Общий предел подключений telemt", "Total telemt connection limit"),
	field(tm, "network", "client_mss", TypeString, "tspu", ApplyRestart, "MSS клиента", "Client MSS",
		"MSS на сокете клиента (число или профиль)", "Client socket MSS (number or profile)"),
	field(tm, "network", "client_mss_bulk", TypeString, "1400", ApplyRestart, "MSS после рукопожатия", "Bulk client MSS",
		"MSS после рукопожатия", "MSS after the handshake"),
	enumField(tm, "network", "rst_on_close", "errors", []string{"off", "errors", "always"}, ApplyRestart, "RST при закрытии", "RST on close",
		"Когда закрывать подключение RST", "When to close a connection with RST"),

	// telemt: ссылки.
	danger(enumField(tm, "links", "relay_secret_mode", "dd", []string{"dd", "ee"}, ApplyInstant,
		"Режим секрета релея", "Relay secret mode",
		"Вид секрета в ссылках через релей; смена меняет ссылки всех пользователей",
		"Secret kind in relay links; changing it changes every user's links")),

	// telemt: таймауты (секунды).
	intField(tm, "timeouts", "client_handshake", "90", secs, ApplyRestart, "Рукопожатие клиента, с", "Client handshake, s", "", ""),
	intField(tm, "timeouts", "tg_connect", "30", secs, ApplyRestart, "Подключение к Telegram, с", "Telegram connect, s", "", ""),
	intField(tm, "timeouts", "client_keepalive", "60", secs, ApplyRestart, "Keepalive клиента, с", "Client keepalive, s", "", ""),
	intField(tm, "timeouts", "client_ack", "30", secs, ApplyRestart, "ACK клиента, с", "Client ACK, s", "", ""),
	intField(tm, "timeouts", "client_first_byte_idle_secs", "60", secs, ApplyRestart, "Ожидание первого байта, с", "First byte idle, s", "", ""),
	intField(tm, "timeouts", "relay_idle_soft_secs", "240", secs, ApplyRestart, "Простой релея (мягкий), с", "Relay idle soft, s", "", ""),
	intField(tm, "timeouts", "relay_idle_hard_secs", "600", secs, ApplyRestart, "Простой релея (жёсткий), с", "Relay idle hard, s",
		"Не меньше мягкого", "Not less than the soft one"),
	intField(tm, "timeouts", "relay_idle_grace_secs", "60", bounds{0, 86400}, ApplyRestart, "Отсрочка простоя, с", "Relay idle grace, s", "", ""),

	// telemt: маскировка.
	intField(tm, "masking", "mask_timing_floor_ms", "180", millis, ApplyRestart, "Задержка маскировки от, мс", "Mask timing floor, ms", "", ""),
	intField(tm, "masking", "mask_timing_ceiling_ms", "320", millis, ApplyRestart, "Задержка маскировки до, мс", "Mask timing ceiling, ms",
		"Не меньше нижней границы", "Not less than the floor"),
	intField(tm, "masking", "server_hello_delay_min_ms", "30", millis, ApplyRestart, "Задержка ServerHello от, мс", "ServerHello delay min, ms", "", ""),
	intField(tm, "masking", "server_hello_delay_max_ms", "100", millis, ApplyRestart, "Задержка ServerHello до, мс", "ServerHello delay max, ms",
		"Не меньше нижней границы", "Not less than the minimum"),

	// telemt: защита от SYN-флуда (iptables на машине).
	intField(tm, "flood", "flood_connlimit", "45", bounds{1, 100000}, ApplyFirewall, "Подключений с одного IP", "Connections per IP", "", ""),
	intField(tm, "flood", "flood_syn_rate", "600", bounds{1, 1000000}, ApplyFirewall, "SYN в минуту", "SYN per minute", "", ""),
	intField(tm, "flood", "flood_syn_burst", "200", bounds{1, 1000000}, ApplyFirewall, "Всплеск SYN", "SYN burst", "", ""),
	enumField(tm, "flood", "flood_mode", "count", []string{"count", "drop"}, ApplyFirewall, "Режим защиты", "Flood mode",
		"count - только считать, drop - отбрасывать", "count - only count, drop - drop packets"),

	// mtg: узел.
	danger(intField(mg, "node", "BIND_PORT", "8767", portRange, ApplyRestart, "Порт", "Port",
		"Порт mtg на машине", "mtg port on the machine")),
	intField(mg, "node", "MAX_CONNS", "2000", bounds{1, 1000000}, ApplyRestart, "Предел подключений", "Max connections", "", ""),
	enumField(mg, "node", "PREFER_IP", "", []string{"only-ipv4", "prefer-ipv4", "prefer-ipv6", "only-ipv6"}, ApplyRestart,
		"Выбор IP", "IP preference", "", ""),
	field(mg, "node", "API_BIND", TypeHostPort, "", ApplyRestart, "Адрес API", "API bind", "", ""),
	field(mg, "node", "HOT_RELOAD", TypeBool, "", ApplyRestart, "Горячая перезагрузка", "Hot reload",
		"Секреты перечитываются без рестарта", "Secrets are reloaded without a restart"),
	field(mg, "node", "FRONTING", TypeString, "", ApplyRestart, "Fronting", "Fronting",
		"Куда отдавать чужой трафик: host:port, можно с суффиксом +pp", "Where foreign traffic goes: host:port, optionally +pp"),

	// mtg: веб-заглушка.
	field(mg, "web", "WEB_BIND", TypeHostPort, "", ApplyRestart, "Адрес веб-заглушки", "Web bind", "", ""),
	field(mg, "web", "WEB_DECOY_DIR", TypeString, "", ApplyRestart, "Каталог веб-заглушки", "Web decoy dir", "", ""),

	// mtg: через панель 3x-ui (настройки бота).
	enumField(mg, "xui", "BACKEND", "", []string{"ssh", "xui"}, ApplyInstant, "Бэкенд", "Backend",
		"Как бот доставляет клиентов: по SSH или через панель", "How the bot delivers clients: SSH or panel"),
	intField(mg, "xui", "XUI_INBOUND_ID", "", bounds{1, 1 << 31}, ApplyInstant, "Вход панели", "Panel inbound", "", ""),
	intField(mg, "xui", "XUI_NODE_ID", "", bounds{1, 1 << 31}, ApplyInstant, "Узел панели", "Panel node", "", ""),

	// mtg: релей.
	danger(intField(mg, "relay", "RELAY_PORT", "8767", portRange, ApplyRestart, "Порт релея", "Relay port",
		"Порт в ссылках через релей", "Port in relay links")),
	danger(intField(mg, "relay", "RELAY_PORT_ALT", "8768", bounds{0, 65535}, ApplyRestart, "Второй порт релея", "Relay alt port",
		"0 - выключен", "0 - off")),
	enumField(mg, "relay", "RELAY_MODE", "", []string{"", "local-mtg"}, ApplyRestart, "Режим релея", "Relay mode", "", ""),
	danger(enumField(mg, "relay", "TELEMUX_RELAY_RESOURCE", "mtg.config", []string{"mtg.config", "mtg.secondary.config"}, ApplyRestart,
		"Конфиг процесса mtg на релее", "Relay mtg process config",
		"Выберите отдельный ресурс для второго процесса на той же машине", "Select a separate resource for the second process on the same machine")),
	field(mg, "relay", "RELAY_PROMETHEUS_BIND", TypeHostPort, "127.0.0.1:3129", ApplyRestart,
		"Адрес метрик mtg на релее", "Relay mtg metrics address", "У каждого процесса свой адрес", "Each process needs its own address"),
	danger(field(mg, "relay", "RELAY_TLS_DOMAIN", TypeHost, "", ApplyRestart, "Домен маскировки релея", "Relay TLS domain",
		"Смена меняет ссылки всех пользователей", "Changing it changes every user's links")),
	field(mg, "relay", "RELAY_DROP_NODE_DOMAIN", TypeBool, "", ApplyRestart, "Убрать домен узла", "Drop node domain", "", ""),
	field(mg, "relay", "RELAY_HOT_RELOAD", TypeBool, "", ApplyRestart, "Горячая перезагрузка релея", "Relay hot reload", "", ""),
	withCheck(field(mg, "relay", "RELAY_BIND", TypeString, "", ApplyRestart, "Адреса релея", "Relay bind",
		"host:port через запятую", "Comma-separated host:port"), checkHostPortList),
	field(mg, "relay", "RELAY_API_BIND", TypeHostPort, "", ApplyRestart, "Адрес API релея", "Relay API bind", "", ""),
	intField(mg, "relay", "RELAY_CLIENT_MSS", "92", bounds{0, 9000}, ApplyRestart, "MSS клиента релея", "Relay client MSS", "", ""),
	intField(mg, "relay", "RELAY_CLIENT_MSS_BULK", "1400", bounds{0, 9000}, ApplyRestart, "MSS релея после рукопожатия", "Relay bulk MSS", "", ""),
	field(mg, "relay", "RELAY_FRONTING", TypeString, "", ApplyRestart, "Fronting релея", "Relay fronting", "", ""),
	field(mg, "web", "RELAY_WEB_BIND", TypeHostPort, "", ApplyRestart, "Адрес веб-заглушки релея", "Relay web bind", "", ""),
	field(mg, "web", "RELAY_WEB_DECOY_DIR", TypeString, "", ApplyRestart, "Каталог веб-заглушки релея", "Relay web decoy dir", "", ""),

	// mtg: пути и команды (настройки бота).
	field(mg, "paths", "RELAY_UNIT", TypeString, "", ApplyInstant, "Unit релея", "Relay unit", "systemd-unit mtg на релее", "systemd unit of mtg on the relay"),
	field(mg, "paths", "CONFIG_PATH", TypeString, "", ApplyInstant, "Путь конфига", "Config path", "", ""),
	field(mg, "paths", "RELOAD_CMD", TypeString, "", ApplyInstant, "Команда перезагрузки", "Reload command", "", ""),
	field(mg, "paths", "RELAY_CONFIG_PATH", TypeString, "", ApplyInstant, "Путь конфига релея", "Relay config path", "", ""),
	field(mg, "paths", "RELAY_RELOAD_CMD", TypeString, "", ApplyInstant, "Команда перезагрузки релея", "Relay reload command", "", ""),
}

var index = func() map[string]Field {
	idx := make(map[string]Field, len(catalog))
	for _, f := range catalog {
		idx[f.Engine+"\x00"+f.Key] = f
	}
	return idx
}()
