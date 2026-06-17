// Package sync — автономный sync-loop одной ноды: desired (локальный PG) →
// diff против локального telemt → shadow (только лог) или apply (мутации).
//
// Идемпотентно и отказоустойчиво: revision-конфликт (кто-то изменил конфиг
// параллельно) → перечитать список и пересчитать diff; create-существующего /
// delete-отсутствующего трактуются как успех; guard от катастрофического сноса.
package sync

import (
	"context"
	"fmt"

	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// DesiredSource отдаёт желаемое состояние (из локального PG). Реализует store.
type DesiredSource interface {
	ListDesired(ctx context.Context) ([]telemtsync.DesiredUser, error)
}

// NodeAPI — telemt-API ноды (реализует telemt.Client).
type NodeAPI interface {
	ListUsers(ctx context.Context) ([]telemtsync.RemoteUser, string, error)
	ApplyOp(ctx context.Context, op telemtsync.SyncOp, ifMatch string) (string, error)
}

// classifiedError — ошибка мутации, умеющая себя классифицировать (реализует telemt.APIError).
type classifiedError interface {
	RevisionConflict() bool
	ReadOnly() bool
	AlreadyDone() bool
}

// Mode — режим синхронизации.
type Mode string

const (
	Shadow Mode = "shadow" // считаем diff, НЕ применяем
	Apply  Mode = "apply"  // применяем мутации
)

// Options — параметры синхронизации.
type Options struct {
	Mode               Mode
	MassDelete         telemtsync.MassDeleteOptions
	Force              bool // обойти guard массового сноса
	MaxConflictRetries int  // 0 → 3
}

// Result — итог синхронизации.
type Result struct {
	Ops     []telemtsync.SyncOp // что собирались сделать
	Applied int
	Skipped int // AlreadyDone (идемпотентность)
	Failed  int
	Errors  []error
	Aborted bool // сработал guard массового сноса
}

// SyncNode приводит локальный telemt к desired.
func SyncNode(ctx context.Context, desired DesiredSource, node NodeAPI, opts Options) (Result, error) {
	des, err := desired.ListDesired(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("чтение desired: %w", err)
	}
	maxRetry := opts.MaxConflictRetries
	if maxRetry == 0 {
		maxRetry = 3
	}

	var res Result
	for attempt := 0; attempt <= maxRetry; attempt++ {
		remote, rev, err := node.ListUsers(ctx)
		if err != nil {
			return res, fmt.Errorf("список юзеров ноды: %w", err)
		}
		ops := telemtsync.ComputeDiff(des, remote, opts.MassDelete.Options)
		res = Result{Ops: ops}

		if telemtsync.IsMassDelete(ops, remote, opts.MassDelete) && !opts.Force {
			res.Aborted = true
			return res, nil // защита: не сносим массово без force
		}
		if opts.Mode == Shadow {
			return res, nil
		}

		conflict := false
		for _, op := range ops {
			newRev, aerr := node.ApplyOp(ctx, op, rev)
			if aerr != nil {
				if ce, ok := aerr.(classifiedError); ok {
					switch {
					case ce.AlreadyDone():
						res.Skipped++
						continue
					case ce.ReadOnly():
						return res, fmt.Errorf("нода в read_only=true, мутации запрещены: %w", aerr)
					case ce.RevisionConflict():
						conflict = true
					}
					if conflict {
						break // перечитаем и пересчитаем
					}
				}
				// прочие ошибки: НЕ глотаем — копим, продолжаем с остальными
				res.Failed++
				res.Errors = append(res.Errors, aerr)
				continue
			}
			rev = newRev
			res.Applied++
		}
		if !conflict {
			return res, nil
		}
		// revision-конфликт → следующая попытка с перечитанным состоянием
	}
	return res, fmt.Errorf("revision-конфликт не сошёлся за %d попыток", maxRetry)
}
