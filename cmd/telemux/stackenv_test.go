package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func stackEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestStackConfigFromEnv(t *testing.T) {
	c, err := stackConfigFromEnv(stackEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.TelemtRepo != "telemt/telemt" || c.MtgRepo != "dolonet/mtg-multi" || c.Dir != "/var/lib/telemux/stack" || c.Token != "" {
		t.Fatalf("по умолчанию: %+v", c)
	}
	c, err = stackConfigFromEnv(stackEnv(map[string]string{"TELEMUX_STACK_MTG_REPO": "someone/mtg-multi",
		"TELEMUX_GITHUB_TOKEN": "tok", "TELEMUX_STACK_DIR": "/tmp/x"}))
	if err != nil || c.MtgRepo != "someone/mtg-multi" || c.Token != "tok" || c.Dir != "/tmp/x" {
		t.Fatalf("из окружения: %+v %v", c, err)
	}
	if _, err := stackConfigFromEnv(stackEnv(map[string]string{"TELEMUX_STACK_TELEMT_REPO": "https://github.com/x/y"})); err == nil {
		t.Fatal("репозиторий не owner/repo - ошибка")
	}
}

type noStackSchema struct{}

func (noStackSchema) CheckStackSchema(context.Context) error {
	return errors.New("нет таблиц")
}

// Миграция 0013 не применена - ручки выключены, а не ошибка запуска.
func TestStackDepsFromEnv_NoSchema(t *testing.T) {
	d, err := stackDepsFromEnv(context.Background(), noStackSchema{}, stackEnv(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || d != nil {
		t.Fatalf("без миграции: %+v %v", d, err)
	}
	if _, err := stackDepsFromEnv(context.Background(), noStackSchema{}, stackEnv(map[string]string{"TELEMUX_STACK_MTG_REPO": "bad"}),
		slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("неверный репозиторий - ошибка запуска")
	}
}
