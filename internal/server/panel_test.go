package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/xui"
)

type fakePanelStore struct {
	mu       sync.Mutex
	work     []store.PanelItem
	observed []store.ObservedReport
}

func (f *fakePanelStore) PanelWork(context.Context) ([]store.PanelItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.work, nil
}

func (f *fakePanelStore) RecordObserved(_ context.Context, rep store.ObservedReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = append(f.observed, rep)
	return nil
}

// panelAPI - мастер-панель с одним пустым MTProto-входом; считает запросы.
func panelAPI(t *testing.T) (*xui.API, *int) {
	t.Helper()
	calls := 0
	var clients []xui.Client
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		ok := func(obj any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": obj}) }
		switch r.URL.Path {
		case "/panel/api/inbounds/get/7":
			ok(map[string]any{"id": 7, "protocol": "mtproto", "port": 8767, "remark": "ps7 [mtproxy-bot]", "nodeId": 3,
				"settings": map[string]any{"clients": clients}})
		case "/panel/api/clients/bulkCreate":
			var items []struct {
				Client xui.Client `json:"client"`
			}
			_ = json.NewDecoder(r.Body).Decode(&items)
			for _, it := range items {
				clients = append(clients, it.Client)
			}
			ok(nil)
		default:
			ok(nil)
		}
	}))
	t.Cleanup(srv.Close)
	return xui.NewAPI(srv.URL, "tok", nil), &calls
}

func panelItem(rev int64, observed *int64, seen time.Time) store.PanelItem {
	return store.PanelItem{
		Code:     "xui-ps7",
		Settings: map[string]string{"inbound_id": "7", "node_id": "3", "port": "8767", "node_code": "ps7"},
		Desired: store.Desired{Resource: store.ResourceXuiClients, Revision: rev,
			Content: []byte(`[{"email":"a_1_ps7","secret":"aa","subId":"mtpx-x","enable":true}]`)},
		ObservedRevision: observed, ObservedStatus: "applied", SeenAt: &seen,
	}
}

func panelServer(t *testing.T, inRecovery bool, ps *fakePanelStore, api *xui.API) *Server {
	return New(Deps{Store: fakeStore{inRecovery: inRecovery}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		Panel: &PanelDeps{Store: ps, API: api, RecheckEvery: time.Hour}})
}

// Новая ревизия - вход сверяется, отчёт applied.
func TestPanel_SyncsNewRevision(t *testing.T) {
	api, calls := panelAPI(t)
	ps := &fakePanelStore{work: []store.PanelItem{panelItem(5, nil, time.Time{})}}
	s := panelServer(t, false, ps, api)
	s.panelOnce(context.Background())
	if *calls == 0 || len(ps.observed) != 1 {
		t.Fatalf("ждали сверку и отчёт: запросов %d, отчётов %d", *calls, len(ps.observed))
	}
	r := ps.observed[0].Resources[0]
	if r.Status != "applied" || r.Revision != 5 || r.SHA256 == "" || ps.observed[0].Via != "panel" {
		t.Fatalf("неверный отчёт: %+v", ps.observed[0])
	}
}

// Ревизия уже применена и проверялась недавно - панель не трогаем.
func TestPanel_SkipsWhenApplied(t *testing.T) {
	api, calls := panelAPI(t)
	five := int64(5)
	ps := &fakePanelStore{work: []store.PanelItem{panelItem(5, &five, time.Now())}}
	panelServer(t, false, ps, api).panelOnce(context.Background())
	if *calls != 0 || len(ps.observed) != 0 {
		t.Fatalf("применённое недавно трогать не нужно: запросов %d", *calls)
	}
}

// Давно не проверялось - перепроверка дрейфа.
func TestPanel_RechecksStale(t *testing.T) {
	api, calls := panelAPI(t)
	five := int64(5)
	ps := &fakePanelStore{work: []store.PanelItem{panelItem(5, &five, time.Now().Add(-2*time.Hour))}}
	panelServer(t, false, ps, api).panelOnce(context.Background())
	if *calls == 0 {
		t.Fatal("давно не проверенный вход должен перепроверяться")
	}
}

// На не-мастере синхронизатор панели не работает.
func TestPanel_OnlyOnMaster(t *testing.T) {
	api, calls := panelAPI(t)
	ps := &fakePanelStore{work: []store.PanelItem{panelItem(5, nil, time.Time{})}}
	panelServer(t, true, ps, api).panelOnce(context.Background())
	if *calls != 0 {
		t.Fatal("реплика не должна писать в панель")
	}
}

// Битые настройки входа или клиенты - отчёт failed без обращения к панели.
func TestPanel_BadSettings(t *testing.T) {
	api, calls := panelAPI(t)
	it := panelItem(5, nil, time.Time{})
	it.Settings["inbound_id"] = "x"
	ps := &fakePanelStore{work: []store.PanelItem{it}}
	panelServer(t, false, ps, api).panelOnce(context.Background())
	if *calls != 0 || len(ps.observed) != 1 || ps.observed[0].Resources[0].Status != "failed" {
		t.Fatalf("ждали failed без запросов: запросов %d, отчёт %+v", *calls, ps.observed)
	}
}
