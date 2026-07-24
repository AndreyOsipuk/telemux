// Package mtgmulti — backend-плагин telemux для mtg-multi (форк 9seconds/mtg).
//
// В отличие от telemt (machine-API /v1/users, чтение состояния + diff), mtg-multi
// управляется ДЕКЛАРАТИВНО: генерим весь config.toml из desired-набора юзеров и
// рестартим демон. Чтения состояния с ноды нет (нет API), поэтому diff не нужен —
// сравниваем сгенерированный config с текущим по хэшу и рестартим при изменении.
//
// Ключевое отличие секрета: telemt-секрет = 16-байтный ключ (hex). mtg-секрет =
// "ee" + тот же ключ + hex(hostname). Значит общий ключ юзера оборачивается в
// per-node mtg-секрет с доменом конкретной ноды (static-<code>.proxy-osv.site).
// Один ключ → разные ссылки на разные страны.
//
// Эта часть — ЧИСТАЯ (без сети/SSH/времени-now передаётся аргументом), полностью
// покрывается unit-тестами. Доставка config на ноду и reload — отдельный слой.
package mtgmulti

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// NodeCfg — параметры mtg-multi инстанса на конкретной ноде.
type NodeCfg struct {
	BindTo         string // "0.0.0.0:8767" — слушатель наружу
	APIBindTo      string // "127.0.0.1:8081" — /stats (loopback)
	FrontingDomain string // "static-de.proxy-osv.site" — host в секрете + domain-fronting
	PreferIP       string // "only-ipv4" (на нодах с мёртвым IPv6) | "prefer-ipv6"
	MaxConns       int    // throttle max-connections; 0 = без throttle
}

// validateNodeCfg проверяет обязательные поля.
func (c NodeCfg) validate() error {
	if c.BindTo == "" {
		return fmt.Errorf("mtgmulti: BindTo обязателен")
	}
	if c.FrontingDomain == "" {
		return fmt.Errorf("mtgmulti: FrontingDomain обязателен (host для секрета)")
	}
	return nil
}

// SecretFor строит mtg-секрет юзера для данной ноды из общего 16-байтного ключа.
// key — hex 16 байт (32 символа), как в telemt. Результат: "ee" + key + hex(domain).
func SecretFor(key, frontingDomain string) (string, error) {
	if len(key) != 32 {
		return "", fmt.Errorf("mtgmulti: ключ должен быть 32 hex-символа (16 байт), got %d", len(key))
	}
	if _, err := hex.DecodeString(key); err != nil {
		return "", fmt.Errorf("mtgmulti: ключ не hex: %w", err)
	}
	return "ee" + strings.ToLower(key) + hex.EncodeToString([]byte(frontingDomain)), nil
}

// isExpired — истёк ли срок (nil = бессрочный). now передаётся явно (чистота).
func isExpired(expirationRFC3339 *string, now time.Time) bool {
	if expirationRFC3339 == nil || *expirationRFC3339 == "" {
		return false
	}
	exp, err := time.Parse(time.RFC3339, *expirationRFC3339)
	if err != nil {
		// Невалидную дату трактуем как НЕ истёкшую (не выкидываем юзера из-за бага парсинга),
		// но это сигнал для лога на уровне выше.
		return false
	}
	return now.After(exp)
}

// GenerateConfig рендерит полный config.toml для mtg-multi ноды из desired-юзеров.
// Включаются только enabled и не истёкшие. [secrets] — ПОСЛЕДНЯЯ секция (требование
// TOML: ключи после [section] принадлежат таблице). Детерминирован (сортировка по
// username), поэтому одинаковый вход → одинаковый выход → стабильный хэш для diff.
func GenerateConfig(cfg NodeCfg, users []telemtsync.DesiredUser, now time.Time) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}

	preferIP := cfg.PreferIP
	if preferIP == "" {
		preferIP = "prefer-ipv6"
	}

	var b strings.Builder
	b.WriteString("# Сгенерировано telemux (backend mtg-multi). НЕ редактировать вручную.\n")
	b.WriteString("debug = false\n")
	fmt.Fprintf(&b, "bind-to = %q\n", cfg.BindTo)
	b.WriteString("concurrency = 8192\n")
	fmt.Fprintf(&b, "prefer-ip = %q\n", preferIP)
	b.WriteString("tolerate-time-skewness = \"5s\"\n")
	if cfg.APIBindTo != "" {
		fmt.Fprintf(&b, "api-bind-to = %q\n", cfg.APIBindTo)
	}
	b.WriteString("\n[defense.anti-replay]\nenabled = true\n")
	// firehol-blocklist НЕ включаем: level1 содержит bogon/LAN-диапазоны → бан своих.
	b.WriteString("\n[defense.blocklist]\nenabled = false\n")
	if cfg.MaxConns > 0 {
		fmt.Fprintf(&b, "\n[throttle]\nmax-connections = %d\ncheck-interval = \"5s\"\n", cfg.MaxConns)
	}

	// Собираем секреты в детерминированном порядке.
	type kv struct{ name, secret string }
	rows := make([]kv, 0, len(users))
	for _, u := range users {
		if isExpired(u.ExpirationRFC3339, now) {
			continue
		}
		secret, err := SecretFor(u.Secret, cfg.FrontingDomain)
		if err != nil {
			return "", fmt.Errorf("юзер %q: %w", u.Username, err)
		}
		rows = append(rows, kv{name: u.Username, secret: secret})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	// [secrets] — последней секцией.
	b.WriteString("\n[secrets]\n")
	for _, r := range rows {
		// quoted-ключ: username может содержать символы, ломающие bare-key.
		fmt.Fprintf(&b, "%q = %q\n", r.name, r.secret)
	}

	return b.String(), nil
}
