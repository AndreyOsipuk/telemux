package mtgsync

import (
	"context"
	"errors"
	"testing"

	"github.com/AndreyOsipuk/telemux/internal/backend"
	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

type fakeStore struct {
	desired []telemtsync.DesiredUser
	nodes   []backend.Node
	dErr    error
	nErr    error
}

func (f fakeStore) ListDesired(context.Context) ([]telemtsync.DesiredUser, error) {
	return f.desired, f.dErr
}
func (f fakeStore) ListMtgNodes(context.Context) ([]backend.Node, error) {
	return f.nodes, f.nErr
}

// fakeSyncer фиксирует вызовы и отдаёт заданные результаты по коду ноды.
type fakeSyncer struct {
	calls   []string
	gotUsrs int
	results map[string]backend.Result
	errs    map[string]error
}

func (f *fakeSyncer) Sync(_ context.Context, node backend.Node, desired []telemtsync.DesiredUser) (backend.Result, error) {
	f.calls = append(f.calls, node.Code)
	f.gotUsrs = len(desired)
	if err := f.errs[node.Code]; err != nil {
		return backend.Result{}, err
	}
	return f.results[node.Code], nil
}

func nodes(codes ...string) []backend.Node {
	out := make([]backend.Node, len(codes))
	for i, c := range codes {
		out[i] = backend.Node{Code: c, Backend: "mtg-multi", Mtg: &backend.MtgNodeCfg{}}
	}
	return out
}

func TestRun_SyncsAllNodesWithDesired(t *testing.T) {
	store := fakeStore{
		desired: []telemtsync.DesiredUser{{Username: "sub_1"}, {Username: "sub_2"}},
		nodes:   nodes("de", "nl", "lv", "pl"),
	}
	syncer := &fakeSyncer{results: map[string]backend.Result{
		"de": {Changed: true}, "nl": {Changed: false},
		"lv": {Changed: true}, "pl": {Changed: false},
	}}
	sum, err := Run(context.Background(), store, syncer)
	if err != nil {
		t.Fatalf("ошибка: %v", err)
	}
	if len(syncer.calls) != 4 {
		t.Errorf("ожидали Sync на 4 ноды, got %d", len(syncer.calls))
	}
	if syncer.gotUsrs != 2 {
		t.Errorf("desired (2 юзера) должен дойти до каждой ноды, got %d", syncer.gotUsrs)
	}
	if sum.Changed != 2 {
		t.Errorf("Changed=2 ожидалось, got %d", sum.Changed)
	}
	if sum.Failed != 0 {
		t.Errorf("Failed=0 ожидалось, got %d", sum.Failed)
	}
}

func TestRun_OneNodeFailsOthersContinue(t *testing.T) {
	store := fakeStore{nodes: nodes("de", "nl", "lv")}
	syncer := &fakeSyncer{
		results: map[string]backend.Result{"de": {Changed: true}, "lv": {Changed: true}},
		errs:    map[string]error{"nl": errors.New("ssh dead")},
	}
	sum, err := Run(context.Background(), store, syncer)
	if err != nil {
		t.Fatalf("падение одной ноды не должно прерывать проход: %v", err)
	}
	if len(syncer.calls) != 3 {
		t.Errorf("все 3 ноды должны быть обработаны, got %d", len(syncer.calls))
	}
	if sum.Failed != 1 || sum.Changed != 2 {
		t.Errorf("ожидали Failed=1 Changed=2, got Failed=%d Changed=%d", sum.Failed, sum.Changed)
	}
}

func TestRun_NoMtgNodes(t *testing.T) {
	sum, err := Run(context.Background(), fakeStore{nodes: nil}, &fakeSyncer{})
	if err != nil {
		t.Fatalf("пустой реестр — не ошибка: %v", err)
	}
	if len(sum.Results) != 0 {
		t.Errorf("без mtg-нод результатов быть не должно")
	}
}

func TestRun_DesiredErrorAborts(t *testing.T) {
	store := fakeStore{dErr: errors.New("db down"), nodes: nodes("de")}
	if _, err := Run(context.Background(), store, &fakeSyncer{}); err == nil {
		t.Error("ошибка получения desired должна прерывать (синхронить нечем)")
	}
}

func TestRun_NodesErrorAborts(t *testing.T) {
	store := fakeStore{nErr: errors.New("db down")}
	if _, err := Run(context.Background(), store, &fakeSyncer{}); err == nil {
		t.Error("ошибка списка нод должна прерывать")
	}
}
