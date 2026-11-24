package telemtcfg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RenderUser - пользователь для включаемого файла users.toml.
type RenderUser struct {
	Key            string     // имя в [access.users]
	Secret         string     // 32 hex-символа
	ExpiresAt      *time.Time // nil - без срока
	MaxTCPConns    *int       // nil - лимит по умолчанию
	DataQuotaBytes int64      // 0 - без квоты

	// Старый секрет после смены ключа: пишется отдельной записью <Key>_legacy
	// со своим сроком, срок отсекает сам telemt.
	PreviousSecret          string
	PreviousSecretExpiresAt *time.Time
}

const (
	legacySuffix      = "_legacy"
	placeholderKey    = "placeholder"
	placeholderSecret = "00000000000000000000000000000000"
	// Формат срока - как у бота (Date.toISOString): UTC, миллисекунды, Z.
	expiresLayout = "2006-01-02T15:04:05.000Z"
)

var (
	keyRe    = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	secretRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ValidateRenderUser проверяет ключ и секреты: мусор в ключе ломает TOML,
// а с ним и весь конфиг telemt.
func ValidateRenderUser(u RenderUser) error {
	if u.DataQuotaBytes < 0 {
		return fmt.Errorf("пользователь %q: data_quota_bytes должен быть неотрицательным целым", u.Key)
	}
	if !keyRe.MatchString(u.Key) {
		return fmt.Errorf("ключ %q: допустимы a-z, 0-9 и _, от 1 до 64 символов", u.Key)
	}
	if u.Key == placeholderKey || strings.HasSuffix(u.Key, legacySuffix) {
		return fmt.Errorf("ключ %q зарезервирован", u.Key)
	}
	if !secretRe.MatchString(u.Secret) {
		return fmt.Errorf("секрет пользователя %q: нужно 32 символа 0-9a-f", u.Key)
	}
	if u.PreviousSecret != "" && !secretRe.MatchString(u.PreviousSecret) {
		return fmt.Errorf("старый секрет пользователя %q: нужно 32 символа 0-9a-f", u.Key)
	}
	return nil
}

// RenderUsersToml собирает включаемый файл telemt с секциями [access.users],
// [access.user_max_tcp_conns], [access.user_expirations] и [access.user_data_quota]. Результат зависит
// только от входных данных: записи сортируются по ключу, время не учитывается.
// Пользователи должны пройти ValidateRenderUser.
func RenderUsersToml(users []RenderUser, defaultMaxConns int) []byte {
	type entry struct {
		key, secret string
		conns       int
		expires     *time.Time
		quotaBytes  int64
	}
	var entries []entry
	for _, u := range users {
		conns := defaultMaxConns
		if u.MaxTCPConns != nil {
			conns = *u.MaxTCPConns
		}
		entries = append(entries, entry{u.Key, u.Secret, conns, u.ExpiresAt, u.DataQuotaBytes})
		if u.PreviousSecret != "" && u.PreviousSecretExpiresAt != nil {
			// Старое имя тоже должно иметь локальный лимит, пока его секрет действителен.
			entries = append(entries, entry{u.Key + legacySuffix, u.PreviousSecret, conns, u.PreviousSecretExpiresAt, u.DataQuotaBytes})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	var b strings.Builder
	b.WriteString("# Сгенерировано telemux. Не редактировать вручную.\n")
	b.WriteString("# Пользовательские секции telemt, подключаются из telemt.toml через include.\n\n")

	b.WriteString("[access.users]\n")
	if len(entries) == 0 {
		fmt.Fprintf(&b, "%s = %q\n\n[access.user_max_tcp_conns]\n%s = 1\n", placeholderKey, placeholderSecret, placeholderKey)
		return []byte(b.String())
	}
	for _, e := range entries {
		fmt.Fprintf(&b, "%s = %q\n", e.key, e.secret)
	}
	b.WriteString("\n[access.user_max_tcp_conns]\n")
	for _, e := range entries {
		b.WriteString(e.key + " = " + strconv.Itoa(e.conns) + "\n")
	}
	var exp strings.Builder
	for _, e := range entries {
		if e.expires != nil {
			fmt.Fprintf(&exp, "%s = %q\n", e.key, e.expires.UTC().Format(expiresLayout))
		}
	}
	if exp.Len() > 0 {
		b.WriteString("\n[access.user_expirations]\n")
		b.WriteString(exp.String())
	}
	var quotas strings.Builder
	for _, e := range entries {
		if e.quotaBytes > 0 {
			fmt.Fprintf(&quotas, "%s = %d\n", e.key, e.quotaBytes)
		}
	}
	if quotas.Len() > 0 {
		b.WriteString("\n[access.user_data_quota]\n")
		b.WriteString(quotas.String())
	}
	return []byte(b.String())
}

// SHA256 - hex-хеш содержимого (ревизии сравниваются по нему).
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
