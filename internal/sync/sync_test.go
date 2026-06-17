package sync

import (
	"context"
	"testing"

	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

func sptr(s string) *string { return &s }

type fakeDesired struct{ users []telemtsync.DesiredUser }

func (f fakeDesired) ListDesired(context.Context) ([]telemtsync.DesiredUser, error) {
	return f.users, nil
}

// fakeError реализует classifiedError.
type fakeError struct{ rev, ro, done bool }

func (e fakeError) Error() string           { return "fake" }
func (e fakeError) RevisionConflict() bool   { return e.rev }
func (e fakeError) ReadOnly() bool           { return e.ro }
func (e fakeError) AlreadyDone() bool        { return e.done }

// fakeNode — управляемая телемт-нода.
type fakeNode struct {
	remote   []telemtsync.RemoteUser
	rev      string
	applied  []telemtsync.SyncOp
	applyErr map[string]error // username → ошибка на ApplyOp
	// conflictOnce: первый ApplyOp кидает revision_conflict, дальше ок
	conflictArmed bool
	listCalls     int
}

func (n *fakeNode) ListUsers(context.Context) ([]telemtsync.RemoteUser, string, error) {
	n.listCalls++
	return n.remote, n.rev, nil
}
func (n *fakeNode) ApplyOp(_ context.Context, op telemtsync.SyncOp, _ string) (string, error) {
	if n.conflictArmed {
		n.conflictArmed = false
		// после конфликта «применяем» желаемое к remote, чтобы следующий проход сошёлся
		n.remote = append(n.remote, telemtsync.RemoteUser{Username: op.Username, Enabled: true})
		return "", fakeError{rev: true}
	}
	if err := n.applyErr[op.Username]; err != nil {
		return "", err
	}
	n.applied = append(n.applied, op)
	return n.rev, nil
}

func desUser(name string) telemtsync.DesiredUser {
	return telemtsync.DesiredUser{Username: name, Secret: "ee" + name, ExpirationRFC3339: sptr("2026-07-01T00:00:00Z"), MaxTCPConns: nil}
}

func TestSync_Shadow_NoApply(t *testing.T) {
	d := fakeDesired{users: []telemtsync.DesiredUser{desUser("sub_1")}}
	n := &fakeNode{rev: "r1"}
	res, err := SyncNode(context.Background(), d, n, Options{Mode: Shadow})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ops) != 1 || res.Applied != 0 || len(n.applied) != 0 {
		t.Fatalf("shadow не должен применять: %+v applied=%d", res, len(n.applied))
	}
}

func TestSync_Apply_Create(t *testing.T) {
	d := fakeDesired{users: []telemtsync.DesiredUser{desUser("sub_1"), desUser("sub_2")}}
	n := &fakeNode{rev: "r1"}
	res, err := SyncNode(context.Background(), d, n, Options{Mode: Apply})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 2 || len(n.applied) != 2 {
		t.Fatalf("ждали 2 create применёнными, получили %+v", res)
	}
}

func TestSync_Apply_AlreadyDoneIsSkip(t *testing.T) {
	d := fakeDesired{users: []telemtsync.DesiredUser{desUser("sub_1")}}
	n := &fakeNode{rev: "r1", applyErr: map[string]error{"sub_1": fakeError{done: true}}}
	res, err := SyncNode(context.Background(), d, n, Options{Mode: Apply})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("user_exists/404 → skip, получили %+v", res)
	}
}

func TestSync_Apply_ReadOnlyAborts(t *testing.T) {
	d := fakeDesired{users: []telemtsync.DesiredUser{desUser("sub_1")}}
	n := &fakeNode{rev: "r1", applyErr: map[string]error{"sub_1": fakeError{ro: true}}}
	_, err := SyncNode(context.Background(), d, n, Options{Mode: Apply})
	if err == nil {
		t.Fatal("read_only должен прерывать с ошибкой")
	}
}

func TestSync_RevisionConflictRetries(t *testing.T) {
	d := fakeDesired{users: []telemtsync.DesiredUser{desUser("sub_1")}}
	n := &fakeNode{rev: "r1", conflictArmed: true}
	res, err := SyncNode(context.Background(), d, n, Options{Mode: Apply})
	if err != nil {
		t.Fatal(err)
	}
	if n.listCalls < 2 {
		t.Fatalf("после конфликта должен перечитать список (listCalls=%d)", n.listCalls)
	}
	// после повтора sub_1 уже в remote → diff пуст → ничего не применяем, без ошибок
	if res.Failed != 0 {
		t.Fatalf("после успешного повтора ошибок быть не должно: %+v", res)
	}
}

func TestSync_MassDeleteGuard(t *testing.T) {
	// desired пуст, на ноде 10 управляемых → diff хочет снести все → guard
	var remote []telemtsync.RemoteUser
	for i := 0; i < 10; i++ {
		remote = append(remote, telemtsync.RemoteUser{Username: "sub_" + string(rune('0'+i)), Enabled: true})
	}
	n := &fakeNode{rev: "r1", remote: remote}
	res, err := SyncNode(context.Background(), fakeDesired{}, n, Options{Mode: Apply})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Aborted || len(n.applied) != 0 {
		t.Fatalf("массовый снос должен быть заблокирован guard'ом: %+v", res)
	}
}
