package telemtcfg

import (
	"strings"
	"testing"
	"time"
)

func tp(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func ip(n int) *int { return &n }

// Порядок входных данных не влияет на результат: одинаковый набор - одинаковые байты.
func TestRenderUsersToml_Deterministic(t *testing.T) {
	a := []RenderUser{
		{Key: "bob_2", Secret: "22222222222222222222222222222222", ExpiresAt: tp("2027-01-02T03:04:05Z")},
		{Key: "alice_1", Secret: "11111111111111111111111111111111", ExpiresAt: tp("2027-06-13T16:14:08Z"), MaxTCPConns: ip(25)},
	}
	b := []RenderUser{a[1], a[0]}
	ra, rb := RenderUsersToml(a, 50), RenderUsersToml(b, 50)
	if string(ra) != string(rb) {
		t.Fatalf("разный результат при разном порядке:\n%s\n---\n%s", ra, rb)
	}
	if SHA256(ra) != SHA256(rb) {
		t.Fatal("разный хеш при одинаковых данных")
	}
}

// Формат совместим с тем, что сейчас пишет бот: три секции, срок в UTC с миллисекундами.
func TestRenderUsersToml_Format(t *testing.T) {
	got := string(RenderUsersToml([]RenderUser{
		{Key: "alice_1", Secret: "11111111111111111111111111111111", ExpiresAt: tp("2027-06-13T19:14:08+03:00"), MaxTCPConns: ip(25)},
		{Key: "bob_2", Secret: "22222222222222222222222222222222"},
	}, 50))
	for _, want := range []string{
		"[access.users]\nalice_1 = \"11111111111111111111111111111111\"\nbob_2 = \"22222222222222222222222222222222\"\n",
		"[access.user_max_tcp_conns]\nalice_1 = 25\nbob_2 = 50\n",
		"[access.user_expirations]\nalice_1 = \"2027-06-13T16:14:08.000Z\"\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("нет фрагмента:\n%s\nв результате:\n%s", want, got)
		}
	}
	if strings.Contains(got, "bob_2 = \"20") {
		t.Fatal("у пользователя без срока не должно быть строки в user_expirations")
	}
}

// Старый секрет после смены ключа пишется всегда, пока он задан (а не «пока не
// наступил срок»): результат не зависит от текущего времени, срок отсекает telemt.
func TestRenderUsersToml_PreviousSecretAlwaysRendered(t *testing.T) {
	u := []RenderUser{{
		Key: "alice_1", Secret: "11111111111111111111111111111111", ExpiresAt: tp("2027-06-13T16:14:08Z"),
		PreviousSecret: "99999999999999999999999999999999", PreviousSecretExpiresAt: tp("2020-01-01T00:00:00Z"),
	}}
	got := string(RenderUsersToml(u, 50))
	for _, want := range []string{
		"alice_1_legacy = \"99999999999999999999999999999999\"",
		"alice_1_legacy = 50",
		"alice_1_legacy = \"2020-01-01T00:00:00.000Z\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("нет строки %q в:\n%s", want, got)
		}
	}
}

// Старый секрет без срока не пишется: без срока он жил бы вечно.
func TestRenderUsersToml_PreviousSecretWithoutExpirySkipped(t *testing.T) {
	got := string(RenderUsersToml([]RenderUser{{
		Key: "alice_1", Secret: "11111111111111111111111111111111", PreviousSecret: "99999999999999999999999999999999",
	}}, 50))
	if strings.Contains(got, "_legacy") {
		t.Fatalf("старый секрет без срока попал в конфиг:\n%s", got)
	}
}

// Пустой набор - заглушка: telemt не принимает пустую секцию пользователей.
func TestRenderUsersToml_EmptyPlaceholder(t *testing.T) {
	got := string(RenderUsersToml(nil, 50))
	if !strings.Contains(got, "[access.users]\nplaceholder = \"00000000000000000000000000000000\"\n") ||
		!strings.Contains(got, "[access.user_max_tcp_conns]\nplaceholder = 1\n") {
		t.Fatalf("нет заглушки:\n%s", got)
	}
}

// Отрендеренное читается обратно тем же парсером, что импортирует users.toml с машин.
func TestRenderUsersToml_RoundTrip(t *testing.T) {
	in := []RenderUser{
		{Key: "alice_1", Secret: "11111111111111111111111111111111", ExpiresAt: tp("2027-06-13T16:14:08Z"), MaxTCPConns: ip(25)},
		{Key: "bob_2", Secret: "22222222222222222222222222222222", ExpiresAt: tp("2027-01-02T03:04:05Z")},
	}
	parsed := ParseUsersToml(string(RenderUsersToml(in, 50)))
	if len(parsed) != 2 {
		t.Fatalf("ждали 2 пользователя, получили %d", len(parsed))
	}
	byName := map[string]ImportedUser{}
	for _, p := range parsed {
		byName[p.Username] = p
	}
	for _, u := range in {
		p := byName[u.Key]
		if p.Secret != u.Secret || p.ExpirationAt == nil || !p.ExpirationAt.Equal(*u.ExpiresAt) {
			t.Fatalf("%s прочитан не так: %+v", u.Key, p)
		}
	}
	if *byName["alice_1"].MaxTCPConns != 25 || *byName["bob_2"].MaxTCPConns != 50 {
		t.Fatal("лимиты подключений прочитаны не так")
	}
}

// Ключ и секрет проверяются: мусор в ключе ломает TOML и весь конфиг telemt.
func TestValidateRenderUser(t *testing.T) {
	ok := RenderUser{Key: "alice_1", Secret: "11111111111111111111111111111111"}
	if err := ValidateRenderUser(ok); err != nil {
		t.Fatalf("валидный пользователь отвергнут: %v", err)
	}
	for _, bad := range []RenderUser{
		{Key: "", Secret: ok.Secret},
		{Key: "al ice", Secret: ok.Secret},
		{Key: "alice\"x", Secret: ok.Secret},
		{Key: "placeholder", Secret: ok.Secret},
		{Key: "x_legacy", Secret: ok.Secret},
		{Key: "alice_1", Secret: "short"},
		{Key: "alice_1", Secret: "zz111111111111111111111111111111"},
		{Key: "alice_1", Secret: ok.Secret, PreviousSecret: "nothex"},
	} {
		if err := ValidateRenderUser(bad); err == nil {
			t.Fatalf("мусор принят: %+v", bad)
		}
	}
}
