// Package telemtcfg — разбор telemt users.toml для импорта существующих юзеров
// (с СЕКРЕТАМИ) в telemux. Это путь обратной совместимости: импортированные
// секреты совпадают с теми, что на ноде → старые клиентские ссылки работают.
package telemtcfg

import (
	"bufio"
	"strconv"
	"strings"
	"time"
)

// ImportedUser — юзер, разобранный из users.toml.
type ImportedUser struct {
	Username     string
	Secret       string
	ExpirationAt *time.Time
	MaxTCPConns  *int
}

// ParseUsersToml разбирает include-файл telemt с секциями:
//
//	[access.users]            username = "<secret-hex>"
//	[access.user_expirations] username = "<rfc3339>"
//	[access.user_max_tcp_conns] username = <int>
//
// Возвращает юзеров (ключ = username из [access.users]); срок/лимит подтягиваются
// из своих секций по совпадению username. Незнакомые секции/строки игнорируются.
func ParseUsersToml(data string) []ImportedUser {
	secrets := map[string]string{}
	order := []string{}
	exps := map[string]*time.Time{}
	conns := map[string]*int{}

	section := ""
	sc := bufio.NewScanner(strings.NewReader(data))
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, val, ok := splitKV(line)
		if !ok {
			continue
		}
		switch section {
		case "access.users":
			if _, seen := secrets[key]; !seen {
				order = append(order, key)
			}
			secrets[key] = unquote(val)
		case "access.user_expirations":
			if t, err := time.Parse(time.RFC3339, unquote(val)); err == nil {
				tt := t
				exps[key] = &tt
			}
		case "access.user_max_tcp_conns":
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				nn := n
				conns[key] = &nn
			}
		}
	}

	out := make([]ImportedUser, 0, len(order))
	for _, name := range order {
		out = append(out, ImportedUser{
			Username: name, Secret: secrets[name],
			ExpirationAt: exps[name], MaxTCPConns: conns[name],
		})
	}
	return out
}

// splitKV разбивает `key = value` (значение может содержать '=').
func splitKV(line string) (string, string, bool) {
	i := strings.Index(line, "=")
	if i < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:i])
	val := strings.TrimSpace(line[i+1:])
	if key == "" {
		return "", "", false
	}
	return key, val, true
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
