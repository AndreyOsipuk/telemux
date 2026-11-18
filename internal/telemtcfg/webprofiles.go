package telemtcfg

// WEB-профили telemt: блок [[web.vhosts.profiles]] в telemt.toml.
//
// В WEB-режиме telemt пускает пользователя, только если на него есть профиль, а
// профиль на пользователя, которого нет в [access.users], делает весь конфиг
// невалидным: перезагрузка отвергается, рестарт уходит в петлю. Поэтому профили
// всегда пересобираются из того же списка пользователей, что и users.toml, и
// меняются вместе с ним. Профили живут внутри [[web.vhosts]], поэтому правится
// сам telemt.toml, но только этот блок. Логика повторяет бота (web-profiles.ts).

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const profileHeader = "[[web.vhosts.profiles]]"

// Причины, по которым конфиг не тронут.
const (
	SkipNoWebMode = "no-web-mode" // WEB на машине нет - профили не нужны
	SkipNoUsers   = "no-users"    // пустой список при непустых профилях - похоже на недогруженную базу
	SkipOverLimit = "over-limit"  // больше [web.limits] max_profiles - telemt не примет
)

// WebProfilesResult - итог пересборки профилей.
type WebProfilesResult struct {
	Changed bool
	Count   int
	Skipped string
}

type profileTemplate struct {
	secretMode                                    string
	maxSessions, maxStreams, maxStreamsPerSession int
}

var (
	webModeRe     = regexp.MustCompile(`(?m)^\s*\[\[web\.vhosts\]\]`)
	maxProfilesRe = regexp.MustCompile(`(?m)^\s*max_profiles\s*=\s*(\d+)`)
	secretModeRe  = regexp.MustCompile(`(?m)^secret_mode\s*=\s*"([^"]+)"`)
)

func readTemplate(config string) profileTemplate {
	tpl := profileTemplate{secretMode: "dd", maxSessions: 8, maxStreams: 512, maxStreamsPerSession: 64}
	idx := strings.Index(config, profileHeader)
	if idx < 0 {
		return tpl
	}
	block := config[idx:min(len(config), idx+400)]
	num := func(key string, def int) int {
		m := regexp.MustCompile(`(?m)^` + key + `\s*=\s*(\d+)`).FindStringSubmatch(block)
		if m == nil {
			return def
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if m := secretModeRe.FindStringSubmatch(block); m != nil {
		tpl.secretMode = m[1]
	}
	tpl.maxSessions = num("max_sessions", tpl.maxSessions)
	tpl.maxStreams = num("max_streams", tpl.maxStreams)
	tpl.maxStreamsPerSession = num("max_streams_per_session", tpl.maxStreamsPerSession)
	return tpl
}

// ApplyWebProfiles переписывает только блоки [[web.vhosts.profiles]] под список
// пользователей; всё остальное в конфиге сохраняется байт в байт.
func ApplyWebProfiles(config string, users []string) (string, WebProfilesResult) {
	if !webModeRe.MatchString(config) {
		return config, WebProfilesResult{Skipped: SkipNoWebMode}
	}
	lines := strings.Split(config, "\n")
	type span struct{ start, end int }
	var blocks []span
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != profileHeader {
			continue
		}
		end := i + 1
		for end < len(lines) && !strings.HasPrefix(strings.TrimLeft(lines[end], " \t"), "[") {
			end++
		}
		blocks = append(blocks, span{i, end})
		i = end - 1
	}
	if len(users) == 0 && len(blocks) > 0 {
		return config, WebProfilesResult{Count: len(blocks), Skipped: SkipNoUsers}
	}
	if m := maxProfilesRe.FindStringSubmatch(config); m != nil {
		if limit, _ := strconv.Atoi(m[1]); len(users) > limit {
			return config, WebProfilesResult{Count: len(blocks), Skipped: SkipOverLimit}
		}
	}

	tpl := readTemplate(config)
	var rendered []string
	for i, u := range users {
		if i > 0 {
			rendered = append(rendered, "")
		}
		rendered = append(rendered,
			profileHeader,
			fmt.Sprintf("user = %q", u),
			fmt.Sprintf("secret_mode = %q", tpl.secretMode),
			fmt.Sprintf("max_sessions = %d", tpl.maxSessions),
			fmt.Sprintf("max_streams = %d", tpl.maxStreams),
			fmt.Sprintf("max_streams_per_session = %d", tpl.maxStreamsPerSession),
		)
	}
	rendered = append(rendered, "") // пустая строка перед следующей секцией

	var next []string
	if len(blocks) > 0 {
		next = append(next, lines[:blocks[0].start]...)
		next = append(next, rendered...)
		next = append(next, lines[blocks[len(blocks)-1].end:]...)
	} else {
		next = append(next, strings.Split(strings.TrimRight(config, " \t\r\n"), "\n")...)
		next = append(next, "")
		next = append(next, rendered...)
	}
	out := strings.Join(next, "\n")
	return out, WebProfilesResult{Changed: out != config, Count: len(users)}
}

// UserKeys - ключи [access.users] из users.toml в порядке файла, без заглушки.
func UserKeys(usersToml []byte) []string {
	var out []string
	for _, u := range ParseUsersToml(string(usersToml)) {
		if u.Username != placeholderKey {
			out = append(out, u.Username)
		}
	}
	return out
}
