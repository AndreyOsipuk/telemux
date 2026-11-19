package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// Без почты и файла токена выпуск выключен; половина настроек - ошибка.
func TestACMESettingsFromEnv(t *testing.T) {
	s, err := acmeSettingsFromEnv(envMap(nil))
	if err != nil || s != nil {
		t.Fatalf("без настроек выпуск выключен: %+v, %v", s, err)
	}
	for name, env := range map[string]map[string]string{
		"только почта": {"TELEMUX_ACME_EMAIL": "ops@example.com"},
		"только токен": {"TELEMUX_ACME_CF_TOKEN_FILE": "/nonexistent"},
	} {
		if _, err := acmeSettingsFromEnv(envMap(env)); err == nil {
			t.Fatalf("%s: ждали ошибку", name)
		}
	}

	tokenFile := filepath.Join(t.TempDir(), "cf.token")
	if err := os.WriteFile(tokenFile, []byte("  cf-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TELEMUX_ACME_EMAIL": "ops@example.com", "TELEMUX_ACME_CF_TOKEN_FILE": tokenFile}
	s, err = acmeSettingsFromEnv(envMap(env))
	if err != nil || s == nil {
		t.Fatalf("полные настройки: %+v, %v", s, err)
	}
	if s.Token != "cf-token" || s.Email != "ops@example.com" || s.RenewBefore != 30*24*time.Hour ||
		s.Directory != "https://acme-v02.api.letsencrypt.org/directory" {
		t.Fatalf("значения по умолчанию: %+v", s)
	}

	env["TELEMUX_ACME_DIR"] = "https://acme-staging-v02.api.letsencrypt.org/directory"
	env["TELEMUX_ACME_RENEW_BEFORE"] = "720h"
	s, err = acmeSettingsFromEnv(envMap(env))
	if err != nil || s.RenewBefore != 720*time.Hour || s.Directory != env["TELEMUX_ACME_DIR"] {
		t.Fatalf("свои значения: %+v, %v", s, err)
	}
	env["TELEMUX_ACME_RENEW_BEFORE"] = "30d"
	if _, err := acmeSettingsFromEnv(envMap(env)); err == nil {
		t.Fatal("RENEW_BEFORE=30d (не длительность Go) принят")
	}
	env["TELEMUX_ACME_RENEW_BEFORE"] = ""

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env["TELEMUX_ACME_CF_TOKEN_FILE"] = empty
	if _, err := acmeSettingsFromEnv(envMap(env)); err == nil {
		t.Fatal("пустой файл токена принят")
	}
	env["TELEMUX_ACME_CF_TOKEN_FILE"] = filepath.Join(t.TempDir(), "missing")
	if _, err := acmeSettingsFromEnv(envMap(env)); err == nil {
		t.Fatal("отсутствующий файл токена принят")
	}
}
