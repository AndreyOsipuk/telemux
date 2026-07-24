// Package mtgsync — оркестратор синхронизации mtg-multi нод.
//
// Связывает источник истины (desired-юзеры + реестр mtg-нод из store) с backend-
// плагином: для каждой активной mtg-ноды приводит её к desired-набору. Вызывается
// периодически из serve-цикла (тикер) и по требованию (POST /api/sync). Сам по
// себе не ходит в сеть — вся доставка внутри Syncer (плагин mtg-multi + Delivery).
package mtgsync

import (
	"context"
	"fmt"

	"github.com/AndreyOsipuk/telemux/internal/backend"
	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// Store — то, что нужно оркестратору из хранилища.
type Store interface {
	ListDesired(ctx context.Context) ([]telemtsync.DesiredUser, error)
	ListMtgNodes(ctx context.Context) ([]backend.Node, error)
}

// Syncer — backend, приводящий ноду к desired (реализует mtg-multi плагин).
type Syncer interface {
	Sync(ctx context.Context, node backend.Node, desired []telemtsync.DesiredUser) (backend.Result, error)
}

// NodeResult — итог по одной ноде.
type NodeResult struct {
	Code   string
	Result backend.Result
	Err    error
}

// Summary — агрегированный итог прохода.
type Summary struct {
	Results []NodeResult
	Changed int // сколько нод изменилось (рестарт)
	Failed  int // сколько нод упало
}

// Run синхронизирует ВСЕ активные mtg-ноды к desired-набору. Ошибка одной ноды не
// прерывает остальные (собираем в Summary) — частичный успех лучше, чем всё-или-
// ничего при недоступности одной ноды. Возвращает ошибку только если не удалось
// получить desired/список нод (ничего синхронизировать нельзя).
func Run(ctx context.Context, store Store, syncer Syncer) (Summary, error) {
	desired, err := store.ListDesired(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("mtgsync: desired: %w", err)
	}
	nodes, err := store.ListMtgNodes(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("mtgsync: список mtg-нод: %w", err)
	}

	var sum Summary
	for _, node := range nodes {
		res, err := syncer.Sync(ctx, node, desired)
		nr := NodeResult{Code: node.Code, Result: res, Err: err}
		if err != nil {
			sum.Failed++
		} else if res.Changed {
			sum.Changed++
		}
		sum.Results = append(sum.Results, nr)
	}
	return sum, nil
}
