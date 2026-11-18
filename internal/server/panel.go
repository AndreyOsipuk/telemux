package server

// Синхронизатор входов мастер-панели 3x-ui (mtg «через панель», ADR 0001).
// На машине писать нечего: конфигом владеет панель, она сама доставляет вход до
// своей ноды. Мастер telemux приводит клиентов входа к желаемому набору через API
// мастер-панели. Отставание - в базе (желаемая ревизия > применённой), поэтому
// после смены мастера новый мастер продолжает с того же места.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/xui"
)

// PanelStore - работа и отчёты синхронизатора панели (реализует store.Store).
type PanelStore interface {
	PanelWork(ctx context.Context) ([]store.PanelItem, error)
	RecordObserved(ctx context.Context, rep store.ObservedReport) error
}

// PanelDeps - зависимости синхронизатора панели; nil в Deps - выключен.
type PanelDeps struct {
	Store           PanelStore
	API             *xui.API
	AllowMassRemove bool          // снять защиту от массового удаления
	Every           time.Duration // как часто проверять работу; по умолчанию 5 с
	RecheckEvery    time.Duration // перепроверка применённого (дрейф); по умолчанию 5 мин
}

func panelTarget(it store.PanelItem) (xui.Target, error) {
	num := func(key string) (int, error) {
		n, err := strconv.Atoi(it.Settings[key])
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("в настройках входа %s нет корректного %s", it.Code, key)
		}
		return n, nil
	}
	var t xui.Target
	var err error
	if t.InboundID, err = num("inbound_id"); err != nil {
		return t, err
	}
	if t.NodeID, err = num("node_id"); err != nil {
		return t, err
	}
	if t.Port, err = num("port"); err != nil {
		return t, err
	}
	if t.Code = it.Settings["node_code"]; t.Code == "" {
		return t, fmt.Errorf("в настройках входа %s нет node_code", it.Code)
	}
	return t, nil
}

// panelOnce - один проход: сверяет входы с новой ревизией и давно не проверенные.
func (s *Server) panelOnce(ctx context.Context) {
	p := s.deps.Panel
	st, err := s.currentRole(ctx)
	if err != nil || !st.IsMaster() {
		return // писать в панель может только мастер
	}
	work, err := p.Store.PanelWork(ctx)
	if err != nil {
		s.deps.Log.Error("панель: выборка работы", "err", err)
		return
	}
	recheck := p.RecheckEvery
	if recheck <= 0 {
		recheck = 5 * time.Minute
	}
	for _, it := range work {
		fresh := it.ObservedRevision != nil && *it.ObservedRevision >= it.Desired.Revision &&
			it.SeenAt != nil && time.Since(*it.SeenAt) < recheck
		if fresh {
			continue
		}
		res := store.ObservedResource{Resource: store.ResourceXuiClients, Revision: it.Desired.Revision}
		t, err := panelTarget(it)
		var desired []xui.Client
		if err == nil {
			if jerr := json.Unmarshal(it.Desired.Content, &desired); jerr != nil {
				err = fmt.Errorf("желаемые клиенты входа %s не читаются: %w", it.Code, jerr)
			}
		}
		if err != nil {
			res.Status, res.Error = "failed", err.Error()
		} else {
			r := xui.Sync(ctx, p.API, t, desired, p.AllowMassRemove)
			res.SHA256 = r.Digest
			if r.Error != "" {
				res.Status, res.Error = "failed", r.Error
			} else {
				res.Status = "applied"
			}
		}
		if err := p.Store.RecordObserved(ctx, store.ObservedReport{Target: it.Code, AgentVersion: s.deps.Version, Via: "panel",
			Resources: []store.ObservedResource{res}}); err != nil {
			s.deps.Log.Error("панель: отчёт", "target", it.Code, "err", err)
		}
		s.deps.Log.Info("панель: сверка входа", "target", it.Code, "revision", it.Desired.Revision, "status", res.Status, "err", res.Error)
	}
}

func (s *Server) panelLoop(ctx context.Context) {
	every := s.deps.Panel.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.panelOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
