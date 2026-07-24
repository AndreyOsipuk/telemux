// Package backend — плагинный слой telemux для разных типов MTProxy-нод.
//
// Ядро telemux протокол-агностично: оно знает только интерфейс NodeBackend и
// выбирает реализацию по nodes.backend ("telemt" | "mtg-multi"). Реализации лежат
// отдельными пакетами (internal/backend/telemt, internal/backend/mtgmulti) и
// саморегистрируются в реестре через init()→Register. Это compile-time «плагин»
// (НЕ Go .so — хрупкий, ломает static-сборку). При нужде истинной динамики —
// переход на hashicorp/go-plugin под тем же интерфейсом.
package backend

import (
	"context"
	"sort"

	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// MtgNodeCfg — параметры mtg-multi инстанса на ноде (если Backend == "mtg-multi").
type MtgNodeCfg struct {
	BindTo         string // "0.0.0.0:8767"
	APIBindTo      string // "127.0.0.1:8081"
	FrontingDomain string // "static-de.proxy-osv.site"
	PreferIP       string // "only-ipv4"
	MaxConns       int    // throttle; 0 = off
	ConfigPath     string // "/etc/mtg-multi/config.toml"
	ServiceName    string // "mtg-multi"
}

// Node — нода с точки зрения backend-слоя (подмножество store.Node + backend-поля).
type Node struct {
	Code    string
	Address string // host/IP для доставки конфига
	Backend string // "telemt" | "mtg-multi"

	// SSH-доступ (для backend'ов, доставляющих конфиг по SSH).
	SSHUser string
	SSHPort int

	// mtg-специфика (заполнено, если Backend == "mtg-multi").
	Mtg *MtgNodeCfg
}

// Result — итог синхронизации ноды.
type Result struct {
	Changed   bool // изменилось ли состояние ноды
	Restarted bool // потребовался ли рестарт демона
	Applied   int  // сколько юзеров в итоговом наборе ноды
}

// NodeBackend — контракт backend'а конкретного типа ноды.
type NodeBackend interface {
	Kind() string // "telemt" | "mtg-multi"
	// Sync приводит ноду к desired-набору юзеров. Идемпотентен: если нода уже в
	// нужном состоянии — Changed=false, без рестарта.
	Sync(ctx context.Context, node Node, desired []telemtsync.DesiredUser) (Result, error)
}

// ─── Реестр ───

var registry = map[string]NodeBackend{}

// Register регистрирует backend по его Kind(). Вызывается из init() реализаций.
// Паника при дубликате — это программная ошибка сборки, не рантайм-условие.
func Register(b NodeBackend) {
	k := b.Kind()
	if _, dup := registry[k]; dup {
		panic("backend: дубликат регистрации " + k)
	}
	registry[k] = b
}

// For возвращает backend по типу ноды.
func For(kind string) (NodeBackend, bool) {
	b, ok := registry[kind]
	return b, ok
}

// Kinds — список зарегистрированных типов (для диагностики/healthz).
func Kinds() []string {
	ks := make([]string, 0, len(registry))
	for k := range registry {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
