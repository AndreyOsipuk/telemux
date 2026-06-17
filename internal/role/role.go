// Package role — определение роли ноды (master/replica) из состояния локального PG.
//
// Принцип: роль telemux ЖЁСТКО привязана к роли локального PostgreSQL, а не к
// отдельной election. PG-primary (через Patroni) = master telemux; PG-replica =
// slave. Один источник истины → split-brain невозможен (primary в кластере один,
// гарантируется лидер-локом Patroni в DCS). При failover Patroni промоутит
// самую свежую реплику, и её telemux автоматически становится master.
package role

import (
	"context"
	"fmt"
)

// Role — роль ноды в кластере telemux.
type Role string

const (
	// Master — локальный PG это primary: принимаем записи, веб-UI/бот, оркестрация.
	Master Role = "master"
	// Replica — локальный PG это standby: read-only, синхроним только свой telemt.
	Replica Role = "replica"
)

// IsMaster — удобный предикат.
func (r Role) IsMaster() bool { return r == Master }

// RecoveryChecker отвечает на `SELECT pg_is_in_recovery()` локального PG.
// Реализуется тонкой обёрткой над pgx (см. internal/store) — а в тестах фейком.
type RecoveryChecker interface {
	// IsInRecovery: true → standby (replica), false → primary (master).
	IsInRecovery(ctx context.Context) (bool, error)
}

// Detect определяет роль по состоянию локального PG.
func Detect(ctx context.Context, c RecoveryChecker) (Role, error) {
	inRecovery, err := c.IsInRecovery(ctx)
	if err != nil {
		return "", fmt.Errorf("определение роли: pg_is_in_recovery: %w", err)
	}
	if inRecovery {
		return Replica, nil
	}
	return Master, nil
}
