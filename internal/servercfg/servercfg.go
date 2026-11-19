// Package servercfg - каталог настроек движка сервера (ADR 0005): допустимые
// ключи settings, их тип, пределы, значение по умолчанию и способ применения.
// Каталог отдаётся панели (GET /api/v1/servers/fields) и проверяет запись:
// неизвестный ключ - ErrUnknownSetting, значение не того вида - ErrBadSetting.
package servercfg

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Движки.
const (
	EngineTelemt = "telemt"
	EngineMtg    = "mtg"
)

// Типы значений.
const (
	TypeInt      = "int"
	TypeBool     = "bool"
	TypeString   = "string"
	TypeEnum     = "enum"
	TypeHostPort = "hostport" // host:port, хост может быть пустым («:8080»)
	TypeHost     = "host"     // домен или IP
	TypeDuration = "duration" // как в Go: 30s, 5m
)

// Способы применения изменения.
const (
	ApplyInstant  = "instant"  // бот и ссылки, без касания машин
	ApplyReload   = "reload"   // перечитывание конфига без рестарта
	ApplyRestart  = "restart"  // рестарт сервиса, подключения рвутся
	ApplyFirewall = "firewall" // правила iptables на машине
)

var (
	// ErrUnknownSetting - ключа нет в каталоге движка.
	ErrUnknownSetting = errors.New("неизвестная настройка")
	// ErrBadSetting - значение не того типа или вне пределов.
	ErrBadSetting = errors.New("недопустимое значение настройки")
	// ErrUnknownEngine - движок не telemt и не mtg.
	ErrUnknownEngine = errors.New("неизвестный движок")
)

// maxValueLen - предел длины строкового значения.
const maxValueLen = 1024

// Field - описание одной настройки движка.
type Field struct {
	Key     string   `json:"key"`
	Engine  string   `json:"engine"`
	Group   string   `json:"group"`
	Type    string   `json:"type"`
	Default string   `json:"default"` // пусто - значение движка или бота по умолчанию
	Min     *int64   `json:"min,omitempty"`
	Max     *int64   `json:"max,omitempty"`
	Enum    []string `json:"enum,omitempty"`
	Apply   string   `json:"apply"`
	Danger  bool     `json:"danger"`
	TitleRU string   `json:"title_ru"`
	TitleEN string   `json:"title_en"`
	HintRU  string   `json:"hint_ru"`
	HintEN  string   `json:"hint_en"`

	check func(string) error // дополнительная проверка строки (список адресов)
}

// Fields - каталог: поля движка engine, пустой engine - всех движков. Копия.
func Fields(engine string) []Field {
	out := []Field{}
	for _, f := range catalog {
		if engine == "" || f.Engine == engine {
			out = append(out, f)
		}
	}
	return out
}

// Lookup - поле по движку и ключу.
func Lookup(engine, key string) (Field, bool) {
	f, ok := index[engine+"\x00"+key]
	return f, ok
}

// ValidEngine - известный движок.
func ValidEngine(engine string) bool { return engine == EngineTelemt || engine == EngineMtg }

// Validate проверяет настройки движка без изменения карты.
func Validate(engine string, settings map[string]string) error {
	_, err := Normalize(engine, settings)
	return err
}

// Normalize проверяет настройки и возвращает их канонический вид (bool - "0"/"1",
// пробелы по краям убраны). Ошибка называет ключ; ключи перебираются по порядку,
// чтобы ошибка была одной и той же при повторе.
func Normalize(engine string, settings map[string]string) (map[string]string, error) {
	if !ValidEngine(engine) {
		return nil, fmt.Errorf("%w %q: допустимы telemt, mtg", ErrUnknownEngine, engine)
	}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(settings))
	for _, k := range keys {
		f, ok := Lookup(engine, k)
		if !ok {
			return nil, fmt.Errorf("%w %s для движка %s", ErrUnknownSetting, k, engine)
		}
		v, err := f.normalize(settings[k])
		if err != nil {
			return nil, fmt.Errorf("%w %s: %v", ErrBadSetting, k, err)
		}
		out[k] = v
	}
	if err := crossCheck(engine, out); err != nil {
		return nil, err
	}
	return out, nil
}

// normalize проверяет значение по типу поля.
func (f Field) normalize(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if len(v) > maxValueLen {
		return "", fmt.Errorf("длиннее %d символов", maxValueLen)
	}
	if strings.ContainsFunc(v, unicode.IsControl) {
		return "", errors.New("управляющие символы недопустимы")
	}
	switch f.Type {
	case TypeInt:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return "", fmt.Errorf("%q - не целое", raw)
		}
		if (f.Min != nil && n < *f.Min) || (f.Max != nil && n > *f.Max) {
			return "", fmt.Errorf("%d вне пределов %s", n, f.bounds())
		}
		return strconv.FormatInt(n, 10), nil
	case TypeBool:
		switch strings.ToLower(v) {
		case "1", "true":
			return "1", nil
		case "0", "false":
			return "0", nil
		}
		return "", fmt.Errorf("%q - нужно 0 или 1", raw)
	case TypeEnum:
		for _, e := range f.Enum {
			if v == e {
				return v, nil
			}
		}
		return "", fmt.Errorf("%q - допустимы %s", raw, enumList(f.Enum))
	case TypeHostPort:
		if err := CheckHostPort(v); err != nil {
			return "", err
		}
		return v, nil
	case TypeHost:
		if !ValidHost(v) {
			return "", fmt.Errorf("%q - нужен домен или IP", raw)
		}
		return strings.ToLower(v), nil
	case TypeDuration:
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return "", fmt.Errorf("%q - нужна длительность вида 30s, 5m", raw)
		}
		return v, nil
	case TypeString:
		if f.check != nil {
			if err := f.check(v); err != nil {
				return "", err
			}
		}
		return v, nil
	}
	return "", fmt.Errorf("тип %q не поддерживается", f.Type)
}

func (f Field) bounds() string {
	lo, hi := "-∞", "+∞"
	if f.Min != nil {
		lo = strconv.FormatInt(*f.Min, 10)
	}
	if f.Max != nil {
		hi = strconv.FormatInt(*f.Max, 10)
	}
	return lo + ".." + hi
}

func enumList(vals []string) string {
	q := make([]string, len(vals))
	for i, v := range vals {
		q[i] = strconv.Quote(v)
	}
	return strings.Join(q, ", ")
}

// ValidHost - домен (буквы, цифры, дефис в метках, без звёздочек) или IP-адрес.
func ValidHost(h string) bool {
	if h == "" {
		return false
	}
	if net.ParseIP(strings.Trim(h, "[]")) != nil {
		return true
	}
	return ValidDomain(strings.ToLower(h))
}

// ValidDomain - имя хоста: строчные буквы, цифры и дефис в метках, хотя бы две
// метки, зона верхнего уровня не число (так отсекаются IP-адреса).
func ValidDomain(d string) bool {
	if len(d) == 0 || len(d) > 253 {
		return false
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	tld := labels[len(labels)-1]
	return strings.ContainsFunc(tld, func(c rune) bool { return c >= 'a' && c <= 'z' })
}

// CheckHostPort - адрес вида host:port: хост - домен, IP или пусто, порт 1-65535.
func CheckHostPort(v string) error {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%q - нужен адрес вида host:port", v)
	}
	if host != "" && !ValidHost(host) {
		return fmt.Errorf("%q - хост не домен и не IP", v)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q - порт от 1 до 65535", v)
	}
	return nil
}

// checkHostPortList - список адресов host:port через запятую.
func checkHostPortList(v string) error {
	if v == "" {
		return nil
	}
	for _, part := range strings.Split(v, ",") {
		if err := CheckHostPort(strings.TrimSpace(part)); err != nil {
			return err
		}
	}
	return nil
}

// crossCheck - связи между полями (с учётом значений по умолчанию).
func crossCheck(engine string, s map[string]string) error {
	if engine != EngineTelemt {
		return nil
	}
	pairs := [][2]string{
		{"mask_timing_floor_ms", "mask_timing_ceiling_ms"},
		{"server_hello_delay_min_ms", "server_hello_delay_max_ms"},
		{"relay_idle_soft_secs", "relay_idle_hard_secs"},
	}
	for _, p := range pairs {
		lo, hi := intOrDefault(engine, s, p[0]), intOrDefault(engine, s, p[1])
		if lo > hi {
			return fmt.Errorf("%w %s: %d больше %s (%d)", ErrBadSetting, p[0], lo, p[1], hi)
		}
	}
	return nil
}

func intOrDefault(engine string, s map[string]string, key string) int64 {
	v, ok := s[key]
	if !ok {
		f, _ := Lookup(engine, key)
		v = f.Default
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}
