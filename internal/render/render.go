// Package render собирает конфиги движков сервера (ADR 0006, этап B2): основной
// telemt.toml (ресурс telemt.config) и config.toml mtg-multi (ресурс mtg.config).
// Сборщики повторяют сборщики бота байт в байт для эквивалентных входов: эталоны в
// testdata получены прогоном кода бота (testdata/README.md).
package render

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/AndreyOsipuk/telemux/internal/servercfg"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// Ресурсы, которые собирает telemux.
const (
	ResourceTelemtConfig = store.ResourceTelemtConfig
	ResourceMtgConfig    = store.ResourceMtgConfig
)

// Коды ошибок сборки (поле error в статусе рендера).
const (
	CodeNoAddress       = "no_address"       // у машины нет targets.settings.address
	CodeNoTLSDomain     = "no_tls_domain"    // у сервера не задан tls_domain
	CodeBadSetting      = "bad_setting"      // значение настройки не годится для конфига
	CodeWrongEngine     = "wrong_engine"     // сборщик не того движка
	CodeNotAssigned     = "not_assigned"     // ресурс не назначен машине (targets.resources)
	CodeMachineDisabled = "machine_disabled" // машина выключена: ревизии ей не публикуются
	CodeUnknown         = "unknown_machine"
)

// Error - ошибка сборки с машинным кодом.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func errf(code, format string, a ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// ErrorCode - машинный код ошибки сборки; не Error - "internal".
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "internal"
}

// Machine - машина сервера: код, вид (relay, exit) и публичный адрес
// (targets.settings.address).
type Machine struct {
	Code    string
	Kind    string
	Address string
}

// Job - ресурс одной машины сервера.
type Job = store.RenderJob

// Jobs - общий план для сборки конфигов и проверки переключения источника.
func Jobs(srv store.Server) []Job { return srv.RenderJobs() }

// setting - значение настройки сервера или значение по умолчанию из каталога.
func setting(srv store.Server, key string) string {
	if v, ok := srv.Settings[key]; ok {
		return v
	}
	if f, ok := servercfg.Lookup(srv.Engine, key); ok {
		return f.Default
	}
	return ""
}

// quotable - значение можно вписать в строку TOML в кавычках как есть (бот не
// экранирует, а кавычка или обратная косая сломали бы весь конфиг).
func quotable(key, v string) error {
	if strings.ContainsAny(v, "\"\\\n\r") {
		return errf(CodeBadSetting, "%s: %q - кавычки и обратная косая в конфиг не пишутся", key, v)
	}
	return nil
}

// checkAddress - адрес машины: IP без лишнего.
func checkAddress(m Machine) error {
	if strings.TrimSpace(m.Address) == "" {
		return errf(CodeNoAddress, "у машины %s не задан адрес (targets.settings.address)", m.Code)
	}
	if _, err := netip.ParseAddr(m.Address); err != nil {
		return errf(CodeNoAddress, "адрес машины %s %q - не IP", m.Code, m.Address)
	}
	return nil
}

// TLSDomains - список доменов маскировки как у бота (buildTlsDomainsList): основной
// первым, затем дополнительные, без повторов, порядок сохраняется.
func TLSDomains(srv store.Server) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range append([]string{srv.TLSDomain}, srv.ExtraTLSDomains...) {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// sortedKeys - ключи карты по порядку (детерминированный вывод).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
