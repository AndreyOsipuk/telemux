package role

import (
	"context"
	"errors"
	"testing"
)

type fakeChecker struct {
	inRecovery bool
	err        error
}

func (f fakeChecker) IsInRecovery(context.Context) (bool, error) {
	return f.inRecovery, f.err
}

func TestDetect_Master(t *testing.T) {
	r, err := Detect(context.Background(), fakeChecker{inRecovery: false})
	if err != nil {
		t.Fatal(err)
	}
	if r != Master || !r.IsMaster() {
		t.Fatalf("PG primary → master, получили %q", r)
	}
}

func TestDetect_Replica(t *testing.T) {
	r, err := Detect(context.Background(), fakeChecker{inRecovery: true})
	if err != nil {
		t.Fatal(err)
	}
	if r != Replica || r.IsMaster() {
		t.Fatalf("PG standby → replica, получили %q", r)
	}
}

func TestDetect_Error(t *testing.T) {
	_, err := Detect(context.Background(), fakeChecker{err: errors.New("conn refused")})
	if err == nil {
		t.Fatal("ошибка PG должна пробрасываться (fail-closed), а не маскироваться ролью")
	}
}
