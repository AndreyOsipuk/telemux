package main

// Настройки выпуска сертификатов WEB-доменов на мастере (ADR 0004).

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/acme"
)

// acmeSettings - выпуск сертификатов на мастере; nil - выключен.
type acmeSettings struct {
	Email       string
	Token       string // токен Cloudflare из файла (в окружение не кладётся)
	TokenFile   string
	Directory   string
	RenewBefore time.Duration
}

// acmeSettingsFromEnv читает TELEMUX_ACME_*. Без почты и файла токена выпуск
// выключен (nil, nil); задана только половина - ошибка настройки.
func acmeSettingsFromEnv(getenv func(string) string) (*acmeSettings, error) {
	email := strings.TrimSpace(getenv("TELEMUX_ACME_EMAIL"))
	tokenFile := strings.TrimSpace(getenv("TELEMUX_ACME_CF_TOKEN_FILE"))
	if email == "" && tokenFile == "" {
		return nil, nil
	}
	if email == "" || tokenFile == "" {
		return nil, errors.New("для выпуска сертификатов нужны оба: TELEMUX_ACME_EMAIL и TELEMUX_ACME_CF_TOKEN_FILE")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("TELEMUX_ACME_CF_TOKEN_FILE: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, fmt.Errorf("TELEMUX_ACME_CF_TOKEN_FILE: файл %s пуст", tokenFile)
	}
	s := &acmeSettings{Email: email, Token: token, TokenFile: tokenFile,
		Directory: strings.TrimSpace(getenv("TELEMUX_ACME_DIR")), RenewBefore: 30 * 24 * time.Hour}
	if s.Directory == "" {
		s.Directory = acme.DirectoryProduction
	}
	if v := strings.TrimSpace(getenv("TELEMUX_ACME_RENEW_BEFORE")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("TELEMUX_ACME_RENEW_BEFORE=%q: нужна положительная длительность, например 720h", v)
		}
		s.RenewBefore = d
	}
	return s, nil
}
