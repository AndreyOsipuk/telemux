package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/AndreyOsipuk/telemux/internal/server"
	"github.com/AndreyOsipuk/telemux/internal/stack"
	"github.com/AndreyOsipuk/telemux/internal/stackrel"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// stackSchemaChecker - проверка миграции 0013 (реализует *store.Store).
type stackSchemaChecker interface {
	CheckStackSchema(ctx context.Context) error
}

// stackConfig - настройки обновления продуктов из окружения мастера.
type stackConfig struct {
	TelemtRepo, MtgRepo, Token, Dir string
}

// stackConfigFromEnv читает TELEMUX_STACK_TELEMT_REPO, TELEMUX_STACK_MTG_REPO,
// TELEMUX_GITHUB_TOKEN и TELEMUX_STACK_DIR; репозиторий не вида owner/repo - ошибка.
func stackConfigFromEnv(getenv func(string) string) (stackConfig, error) {
	get := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	c := stackConfig{
		TelemtRepo: get("TELEMUX_STACK_TELEMT_REPO", stackrel.DefaultTelemtRepo),
		MtgRepo:    get("TELEMUX_STACK_MTG_REPO", stackrel.DefaultMtgRepo),
		Token:      getenv("TELEMUX_GITHUB_TOKEN"),
		Dir:        get("TELEMUX_STACK_DIR", stackrel.DefaultCacheDir),
	}
	for k, v := range map[string]string{"TELEMUX_STACK_TELEMT_REPO": c.TelemtRepo, "TELEMUX_STACK_MTG_REPO": c.MtgRepo} {
		if !stackrel.ValidRepo(v) {
			return c, fmt.Errorf("%s=%q: нужен репозиторий GitHub вида owner/repo", k, v)
		}
	}
	return c, nil
}

// stackDepsFromEnv - зависимости ручек обновления; миграция 0013 не применена - nil
// (ручки и цикл выключены, предупреждение в журнал). Ошибка - неверная настройка.
func stackDepsFromEnv(ctx context.Context, st stackSchemaChecker, getenv func(string) string, logger *slog.Logger) (*server.StackDeps, error) {
	cfg, err := stackConfigFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	if err := st.CheckStackSchema(ctx); err != nil {
		logger.Warn("обновление telemt и mtg-multi: /api/v1/stack и обновление машин выключены", "err", err)
		return nil, nil
	}
	ss, ok := st.(server.StackStore)
	if !ok {
		return nil, fmt.Errorf("хранилище не реализует StackStore")
	}
	logger.Info("обновление telemt и mtg-multi включено", "telemt_repo", cfg.TelemtRepo, "mtg_repo", cfg.MtgRepo,
		"cache", cfg.Dir, "github_token", cfg.Token != "")
	return &server.StackDeps{
		Store: ss,
		Releases: &stackrel.Client{Token: cfg.Token, Repos: map[string]string{
			stack.ProductTelemt: cfg.TelemtRepo, stack.ProductMtg: cfg.MtgRepo}},
		Cache: stackrel.Cache{Dir: cfg.Dir},
	}, nil
}

// Проверка на этапе сборки: *store.Store годится для ручек обновления.
var _ server.StackStore = (*store.Store)(nil)
