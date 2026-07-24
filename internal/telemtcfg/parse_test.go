package telemtcfg

import "testing"

func TestParseUsersToml_RealShape(t *testing.T) {
	in := `# telemt include
[access.users]
garagefri_3073 = "ca7315aabbccddeeff00112233445566"
alice_42 = "0011223344556677889900aabbccddee"

[access.user_expirations]
garagefri_3073 = "2027-06-13T16:14:08Z"

[access.user_max_tcp_conns]
garagefri_3073 = 25
`
	got := ParseUsersToml(in)
	if len(got) != 2 {
		t.Fatalf("ждали 2 юзера, получили %d", len(got))
	}
	// порядок = порядок в [access.users]
	if got[0].Username != "garagefri_3073" || got[1].Username != "alice_42" {
		t.Fatalf("порядок/имена не те: %+v", got)
	}
	g := got[0]
	if g.Secret != "ca7315aabbccddeeff00112233445566" {
		t.Fatalf("секрет не сохранён: %q", g.Secret)
	}
	if g.ExpirationAt == nil || g.ExpirationAt.Year() != 2027 {
		t.Fatalf("срок не разобран: %v", g.ExpirationAt)
	}
	if g.MaxTCPConns == nil || *g.MaxTCPConns != 25 {
		t.Fatalf("maxconns не разобран: %v", g.MaxTCPConns)
	}
	// у alice нет срока/лимита → nil
	if got[1].ExpirationAt != nil || got[1].MaxTCPConns != nil {
		t.Fatalf("alice не должна иметь срок/лимит: %+v", got[1])
	}
}

func TestParseUsersToml_IgnoresJunkAndEmpty(t *testing.T) {
	if got := ParseUsersToml(""); len(got) != 0 {
		t.Fatalf("пустой вход → 0 юзеров, got %d", len(got))
	}
	in := `[server]
secret = "should-not-be-a-user"
[access.user_expirations]
ghost = "2027-01-01T00:00:00Z"
[access.users]
real_1 = "aa"
`
	got := ParseUsersToml(in)
	// ghost есть только в expirations, но НЕ в [access.users] → не импортируется
	if len(got) != 1 || got[0].Username != "real_1" {
		t.Fatalf("ждали только real_1, got %+v", got)
	}
}
