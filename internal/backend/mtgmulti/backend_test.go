package mtgmulti

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/backend"
	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// mockDelivery — управляемая доставка для тестов Sync.
type mockDelivery struct {
	current     string // что «лежит» на ноде
	written     string // что записали
	reloaded    bool
	readErr     error
	writeErr    error
	writeCalled int
}

func (m *mockDelivery) ReadConfig(_ context.Context, _ backend.Node) (string, error) {
	return m.current, m.readErr
}
func (m *mockDelivery) WriteAndReload(_ context.Context, _ backend.Node, cfg string) error {
	m.writeCalled++
	if m.writeErr != nil {
		return m.writeErr
	}
	m.written = cfg
	m.reloaded = true
	m.current = cfg
	return nil
}

func testNode() backend.Node {
	return backend.Node{
		Code:    "de",
		Address: "179.61.145.196",
		Backend: "mtg-multi",
		Mtg: &backend.MtgNodeCfg{
			BindTo:         "0.0.0.0:8767",
			APIBindTo:      "127.0.0.1:8081",
			FrontingDomain: "static-de.proxy-osv.site",
			PreferIP:       "only-ipv4",
			MaxConns:       2000,
		},
	}
}

func pluginAt(d Delivery, now time.Time) *Plugin {
	p := New(d)
	p.now = func() time.Time { return now }
	return p
}

func TestSync_WritesWhenNodeEmpty(t *testing.T) {
	m := &mockDelivery{current: ""} // нода ещё не настроена
	p := pluginAt(m, time.Now())
	res, err := p.Sync(context.Background(), testNode(),
		[]telemtsync.DesiredUser{{Username: "sub_1", Secret: key32}})
	if err != nil {
		t.Fatalf("ошибка: %v", err)
	}
	if !res.Changed || !res.Restarted {
		t.Errorf("на пустой ноде ожидали Changed+Restarted, got %+v", res)
	}
	if res.Applied != 1 {
		t.Errorf("Applied=1 ожидалось, got %d", res.Applied)
	}
	if m.writeCalled != 1 || !m.reloaded {
		t.Errorf("должны были записать+рестартить")
	}
}

func TestSync_NoopWhenUnchanged(t *testing.T) {
	now := time.Now()
	node := testNode()
	users := []telemtsync.DesiredUser{{Username: "sub_1", Secret: key32}}
	// Кладём на ноду РОВНО то, что сгенерит Sync.
	cfg, _ := GenerateConfig(toNodeCfg(node.Mtg), users, now)
	m := &mockDelivery{current: cfg}
	p := pluginAt(m, now)
	res, err := p.Sync(context.Background(), node, users)
	if err != nil {
		t.Fatalf("ошибка: %v", err)
	}
	if res.Changed || res.Restarted {
		t.Errorf("идемпотентность: без изменений не рестартим, got %+v", res)
	}
	if m.writeCalled != 0 {
		t.Errorf("записи быть не должно (unchanged)")
	}
	if res.Applied != 1 {
		t.Errorf("Applied=1, got %d", res.Applied)
	}
}

func TestSync_RewritesWhenUserAdded(t *testing.T) {
	now := time.Now()
	node := testNode()
	old := []telemtsync.DesiredUser{{Username: "sub_1", Secret: key32}}
	cfgOld, _ := GenerateConfig(toNodeCfg(node.Mtg), old, now)
	m := &mockDelivery{current: cfgOld}
	p := pluginAt(m, now)
	// Добавили второго юзера → config меняется → рестарт.
	res, err := p.Sync(context.Background(), node, []telemtsync.DesiredUser{
		{Username: "sub_1", Secret: key32},
		{Username: "sub_2", Secret: key32},
	})
	if err != nil {
		t.Fatalf("ошибка: %v", err)
	}
	if !res.Changed || res.Applied != 2 {
		t.Errorf("добавление юзера → Changed, Applied=2, got %+v", res)
	}
}

func TestSync_ExpiredNotCounted(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	m := &mockDelivery{current: ""}
	p := pluginAt(m, now)
	res, _ := p.Sync(context.Background(), testNode(), []telemtsync.DesiredUser{
		{Username: "sub_live", Secret: key32},
		{Username: "sub_dead", Secret: key32, ExpirationRFC3339: ptr("2026-06-25T00:00:00Z")},
	})
	if res.Applied != 1 {
		t.Errorf("истёкший не считается: Applied=1 ожидалось, got %d", res.Applied)
	}
}

func TestSync_NoMtgCfg(t *testing.T) {
	node := testNode()
	node.Mtg = nil
	p := New(&mockDelivery{})
	if _, err := p.Sync(context.Background(), node, nil); err == nil {
		t.Error("без Mtg-конфига ожидалась ошибка")
	}
}

func TestSync_DeliveryError(t *testing.T) {
	m := &mockDelivery{current: "", writeErr: errors.New("ssh dead")}
	p := New(m)
	if _, err := p.Sync(context.Background(), testNode(),
		[]telemtsync.DesiredUser{{Username: "sub_1", Secret: key32}}); err == nil {
		t.Error("ошибка доставки должна пробрасываться")
	}
}

func TestSync_ReadErrorDoesNotOverwriteNode(t *testing.T) {
	m := &mockDelivery{readErr: errors.New("ssh read timeout")}
	p := New(m)

	if _, err := p.Sync(context.Background(), testNode(),
		[]telemtsync.DesiredUser{{Username: "sub_1", Secret: key32}}); err == nil {
		t.Fatal("ошибка чтения должна пробрасываться")
	}
	if m.writeCalled != 0 {
		t.Fatal("при ошибке чтения нельзя перезаписывать config и рестартить ноду")
	}
}

func TestPlugin_Kind(t *testing.T) {
	if New(&mockDelivery{}).Kind() != "mtg-multi" {
		t.Error("Kind должен быть mtg-multi")
	}
}

func TestRegistry(t *testing.T) {
	backend.Register(New(&mockDelivery{}))
	b, ok := backend.For("mtg-multi")
	if !ok || b.Kind() != "mtg-multi" {
		t.Error("плагин должен регистрироваться и находиться в реестре")
	}
}
