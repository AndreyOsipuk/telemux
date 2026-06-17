package telemtsync

import "testing"

const exp = "2026-07-01T00:00:00Z"

func sptr(s string) *string { return &s }
func iptr(i int) *int       { return &i }

func desired(name string, mut func(*DesiredUser)) DesiredUser {
	d := DesiredUser{Username: name, Secret: "ee" + name, ExpirationRFC3339: sptr(exp), MaxTCPConns: iptr(8)}
	if mut != nil {
		mut(&d)
	}
	return d
}
func remote(name string, mut func(*RemoteUser)) RemoteUser {
	r := RemoteUser{Username: name, Enabled: true, ExpirationRFC3339: sptr(exp), MaxTCPConns: iptr(8)}
	if mut != nil {
		mut(&r)
	}
	return r
}

func kinds(ops []SyncOp) []OpKind {
	out := make([]OpKind, len(ops))
	for i, o := range ops {
		out[i] = o.Kind
	}
	return out
}

func TestComputeDiff_Create(t *testing.T) {
	ops := ComputeDiff([]DesiredUser{desired("sub_1", nil)}, nil, Options{})
	if len(ops) != 1 || ops[0].Kind != OpCreate || ops[0].Username != "sub_1" || ops[0].Secret != "eesub_1" {
		t.Fatalf("ждали один create sub_1, получили %+v", ops)
	}
}

func TestComputeDiff_Idempotent(t *testing.T) {
	ops := ComputeDiff([]DesiredUser{desired("sub_1", nil)}, []RemoteUser{remote("sub_1", nil)}, Options{})
	if len(ops) != 0 {
		t.Fatalf("desired==remote должно дать 0 операций, получили %+v", ops)
	}
}

func TestComputeDiff_PatchExpiration(t *testing.T) {
	d := desired("sub_1", func(d *DesiredUser) { d.ExpirationRFC3339 = sptr("2026-08-01T00:00:00Z") })
	ops := ComputeDiff([]DesiredUser{d}, []RemoteUser{remote("sub_1", nil)}, Options{})
	if len(ops) != 1 || ops[0].Kind != OpPatch || !ops[0].Fields.SetExpiration || ops[0].Fields.SetMaxTCPConns || ops[0].Fields.Enabled != nil {
		t.Fatalf("ждали patch только expiration, получили %+v", ops)
	}
}

func TestComputeDiff_PatchMaxConns(t *testing.T) {
	d := desired("sub_1", func(d *DesiredUser) { d.MaxTCPConns = iptr(16) })
	ops := ComputeDiff([]DesiredUser{d}, []RemoteUser{remote("sub_1", nil)}, Options{})
	if len(ops) != 1 || !ops[0].Fields.SetMaxTCPConns || ops[0].Fields.SetExpiration {
		t.Fatalf("ждали patch только maxConns, получили %+v", ops)
	}
}

func TestComputeDiff_PatchEnable(t *testing.T) {
	r := remote("sub_1", func(r *RemoteUser) { r.Enabled = false })
	ops := ComputeDiff([]DesiredUser{desired("sub_1", nil)}, []RemoteUser{r}, Options{})
	if len(ops) != 1 || ops[0].Fields.Enabled == nil || !*ops[0].Fields.Enabled {
		t.Fatalf("ждали patch enabled:true, получили %+v", ops)
	}
}

func TestComputeDiff_Delete(t *testing.T) {
	ops := ComputeDiff(nil, []RemoteUser{remote("sub_9", nil)}, Options{})
	if len(ops) != 1 || ops[0].Kind != OpDelete || ops[0].Username != "sub_9" {
		t.Fatalf("ждали delete sub_9, получили %+v", ops)
	}
}

func TestComputeDiff_IgnoresUnmanaged(t *testing.T) {
	r := []RemoteUser{remote("sub_1", nil), remote("placeholder", nil), remote("admin_manual", nil)}
	ops := ComputeDiff([]DesiredUser{desired("sub_1", nil)}, r, Options{})
	if len(ops) != 0 {
		t.Fatalf("чужих юзеров трогать нельзя, получили %+v", ops)
	}
}

func TestComputeDiff_Order(t *testing.T) {
	desiredList := []DesiredUser{desired("sub_new", nil), desired("sub_upd", func(d *DesiredUser) { d.MaxTCPConns = iptr(99) })}
	remoteList := []RemoteUser{remote("sub_upd", nil), remote("sub_gone", nil)}
	got := kinds(ComputeDiff(desiredList, remoteList, Options{}))
	want := []OpKind{OpCreate, OpPatch, OpDelete}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("порядок create→patch→delete нарушен: %v", got)
		}
	}
}

func TestComputeDiff_CustomPrefix(t *testing.T) {
	ops := ComputeDiff(nil, []RemoteUser{remote("u_1", nil), remote("sub_1", nil)}, Options{ManagedPrefix: "u_"})
	if len(ops) != 1 || ops[0].Username != "u_1" {
		t.Fatalf("с префиксом u_ удаляем только u_1, получили %+v", ops)
	}
}

func TestExpirationEquals(t *testing.T) {
	cases := []struct {
		a, b *string
		want bool
	}{
		{sptr("2026-07-01T00:00:00Z"), sptr("2026-07-01T00:00:00+00:00"), true},
		{nil, nil, true},
		{nil, sptr(exp), false},
		{sptr("2026-07-01T00:00:00Z"), sptr("2026-07-02T00:00:00Z"), false},
		{sptr("2026-07-01T03:00:00+03:00"), sptr("2026-07-01T00:00:00Z"), true},
	}
	for i, c := range cases {
		if got := ExpirationEquals(c.a, c.b); got != c.want {
			t.Fatalf("кейс %d: ждали %v, получили %v", i, c.want, got)
		}
	}
}

func TestSafety(t *testing.T) {
	r := []RemoteUser{remote("sub_1", nil), remote("sub_2", nil), remote("sub_3", nil), remote("sub_4", nil), remote("other", nil)}
	ops := ComputeDiff([]DesiredUser{desired("sub_1", nil)}, r, Options{})
	s := ComputeSafety(ops, r, Options{})
	if s.TotalRemoteManaged != 4 || s.DeleteCount != 3 {
		t.Fatalf("ждали 4 управляемых / 3 удаления, получили %+v", s)
	}
	if s.DeleteFraction < 0.74 || s.DeleteFraction > 0.76 {
		t.Fatalf("доля удаления ~0.75, получили %v", s.DeleteFraction)
	}
}

func manyRemote(n int) []RemoteUser {
	out := make([]RemoteUser, n)
	for i := 0; i < n; i++ {
		out[i] = remote("sub_"+itoa(i), nil)
	}
	return out
}
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestMassDelete(t *testing.T) {
	r := manyRemote(10)
	if !IsMassDelete(ComputeDiff(nil, r, Options{}), r, MassDeleteOptions{}) {
		t.Fatal("снос всех 10 — должен быть mass delete")
	}
	// мелкая чистка: 2 из 3 (< minDeletes=5)
	r3 := []RemoteUser{remote("sub_1", nil), remote("sub_2", nil), remote("sub_3", nil)}
	if IsMassDelete(ComputeDiff([]DesiredUser{desired("sub_1", nil)}, r3, Options{}), r3, MassDeleteOptions{}) {
		t.Fatal("2 удаления (< minDeletes) — не mass delete")
	}
	if IsMassDelete(nil, nil, MassDeleteOptions{}) {
		t.Fatal("пустая нода — не mass delete")
	}
	// настраиваемый порог: 6 из 10 = 0.6
	var d4 []DesiredUser
	for i := 0; i < 4; i++ {
		d4 = append(d4, desired("sub_"+itoa(i), nil))
	}
	ops := ComputeDiff(d4, r, Options{})
	if IsMassDelete(ops, r, MassDeleteOptions{FractionThreshold: 0.8}) {
		t.Fatal("0.6 < порог 0.8 — не mass delete")
	}
	if !IsMassDelete(ops, r, MassDeleteOptions{FractionThreshold: 0.5}) {
		t.Fatal("0.6 > порог 0.5 — mass delete")
	}
}
