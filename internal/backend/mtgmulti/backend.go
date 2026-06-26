package mtgmulti

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/backend"
	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// Delivery — абстракция доставки config на ноду. Реальная реализация ходит по SSH
// (ssh_delivery.go); в тестах подменяется моком. Sync-логика (сравнение, решение
// рестартить) от транспорта не зависит и полностью тестируема.
type Delivery interface {
	// ReadConfig читает текущий config.toml с ноды. Если файла нет — вернуть "" без
	// ошибки (нода ещё не настраивалась), чтобы Sync записал свежий.
	ReadConfig(ctx context.Context, node backend.Node) (string, error)
	// WriteAndReload атомарно пишет config + рестартит демон (mtg-multi не умеет
	// hot-reload, нужен рестарт — ~1.3с обрыв, Telegram переподключается сам).
	WriteAndReload(ctx context.Context, node backend.Node, config string) error
}

// Plugin реализует backend.NodeBackend для mtg-multi.
type Plugin struct {
	deliver Delivery
	now     func() time.Time
}

// New создаёт плагин с заданной доставкой. Сервер вызывает
// backend.Register(mtgmulti.New(sshDelivery)) при старте (delivery нужны SSH-креды,
// поэтому не self-register в init() — регистрация явная и сконфигурированная).
func New(deliver Delivery) *Plugin {
	return &Plugin{deliver: deliver, now: time.Now}
}

func (p *Plugin) Kind() string { return "mtg-multi" }

// Sync приводит mtg-ноду к desired-набору: генерит config, сравнивает с текущим,
// при отличии — пишет и рестартит. Идемпотентен (нет изменений → нет рестарта).
func (p *Plugin) Sync(ctx context.Context, node backend.Node, desired []telemtsync.DesiredUser) (backend.Result, error) {
	if node.Mtg == nil {
		return backend.Result{}, fmt.Errorf("mtgmulti: у ноды %q не задан Mtg-конфиг", node.Code)
	}
	now := p.now()
	cfg, err := GenerateConfig(toNodeCfg(node.Mtg), desired, now)
	if err != nil {
		return backend.Result{}, fmt.Errorf("mtgmulti: генерация config для %q: %w", node.Code, err)
	}
	applied := countActive(desired, now)

	// Текущий config с ноды (ошибка чтения = считаем пустым → перезапишем).
	cur, _ := p.deliver.ReadConfig(ctx, node)
	if strings.TrimSpace(cur) == strings.TrimSpace(cfg) {
		return backend.Result{Changed: false, Restarted: false, Applied: applied}, nil
	}

	if err := p.deliver.WriteAndReload(ctx, node, cfg); err != nil {
		return backend.Result{}, fmt.Errorf("mtgmulti: доставка config на %q: %w", node.Code, err)
	}
	return backend.Result{Changed: true, Restarted: true, Applied: applied}, nil
}

// toNodeCfg конвертирует backend.MtgNodeCfg в локальный NodeCfg генератора.
func toNodeCfg(m *backend.MtgNodeCfg) NodeCfg {
	return NodeCfg{
		BindTo:         m.BindTo,
		APIBindTo:      m.APIBindTo,
		FrontingDomain: m.FrontingDomain,
		PreferIP:       m.PreferIP,
		MaxConns:       m.MaxConns,
	}
}

// countActive — сколько юзеров реально попадёт в config (не истёкших).
func countActive(users []telemtsync.DesiredUser, now time.Time) int {
	n := 0
	for _, u := range users {
		if !isExpired(u.ExpirationRFC3339, now) {
			n++
		}
	}
	return n
}
