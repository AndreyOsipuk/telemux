package telemtcfg

import (
	"strings"
	"testing"
)

const webConfig = `[general]
use_middle_proxy = false

[[web.vhosts]]
domain = "web.example.com"

[[web.vhosts.profiles]]
user = "old_1"
secret_mode = "ee"
max_sessions = 4
max_streams = 256
max_streams_per_session = 32

[[web.vhosts.profiles]]
user = "gone_2"
secret_mode = "ee"
max_sessions = 4
max_streams = 256
max_streams_per_session = 32

[web.limits]
max_profiles = 100

[server.api]
listen = "127.0.0.1:9091"
`

// Профили пересобираются из списка пользователей, остальное - байт в байт;
// настройки профиля берутся из первого существующего блока.
func TestApplyWebProfiles_RebuildsKeepingRest(t *testing.T) {
	got, res := ApplyWebProfiles(webConfig, []string{"alice_1", "bob_2"})
	if res.Skipped != "" || !res.Changed || res.Count != 2 {
		t.Fatalf("неожиданный результат: %+v", res)
	}
	if strings.Contains(got, `user = "gone_2"`) || strings.Contains(got, `user = "old_1"`) {
		t.Fatalf("профили выбывших пользователей остались:\n%s", got)
	}
	for _, want := range []string{
		"[[web.vhosts.profiles]]\nuser = \"alice_1\"\nsecret_mode = \"ee\"\nmax_sessions = 4\nmax_streams = 256\nmax_streams_per_session = 32\n",
		"[[web.vhosts.profiles]]\nuser = \"bob_2\"\n",
		"[general]\nuse_middle_proxy = false\n\n[[web.vhosts]]\ndomain = \"web.example.com\"\n\n",
		"[web.limits]\nmax_profiles = 100\n\n[server.api]\nlisten = \"127.0.0.1:9091\"\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("нет фрагмента:\n%s\nв:\n%s", want, got)
		}
	}
	// Повторное применение ничего не меняет.
	again, res2 := ApplyWebProfiles(got, []string{"alice_1", "bob_2"})
	if again != got || res2.Changed {
		t.Fatal("повторное применение изменило конфиг")
	}
}

func TestApplyWebProfiles_NoWebMode(t *testing.T) {
	cfg := "[general]\nuse_middle_proxy = false\n"
	got, res := ApplyWebProfiles(cfg, []string{"alice_1"})
	if got != cfg || res.Skipped != SkipNoWebMode {
		t.Fatalf("без WEB конфиг трогать нельзя: %+v", res)
	}
}

// Пустой список при непустых профилях - похоже на недогруженную базу: не трогаем.
func TestApplyWebProfiles_EmptyUsersKeepsConfig(t *testing.T) {
	got, res := ApplyWebProfiles(webConfig, nil)
	if got != webConfig || res.Skipped != SkipNoUsers {
		t.Fatalf("пустой список не должен снести профили: %+v", res)
	}
}

// Больше max_profiles - telemt такой конфиг не примет и уйдёт в перезапуски.
func TestApplyWebProfiles_OverLimit(t *testing.T) {
	cfg := strings.Replace(webConfig, "max_profiles = 100", "max_profiles = 1", 1)
	got, res := ApplyWebProfiles(cfg, []string{"alice_1", "bob_2"})
	if got != cfg || res.Skipped != SkipOverLimit {
		t.Fatalf("превышение лимита должно отказать: %+v", res)
	}
}

// WEB есть, профилей ещё нет - дописываются в конец со значениями по умолчанию.
func TestApplyWebProfiles_AppendsWhenNone(t *testing.T) {
	cfg := "[[web.vhosts]]\ndomain = \"web.example.com\"\n"
	got, res := ApplyWebProfiles(cfg, []string{"alice_1"})
	if !res.Changed || !strings.Contains(got, "domain = \"web.example.com\"\n\n[[web.vhosts.profiles]]\nuser = \"alice_1\"\nsecret_mode = \"dd\"\nmax_sessions = 8\n") {
		t.Fatalf("профиль не дописан:\n%s", got)
	}
}

// Список пользователей для профилей - ключи [access.users] без заглушки.
func TestUserKeys(t *testing.T) {
	data := RenderUsersToml([]RenderUser{
		{Key: "bob_2", Secret: "22222222222222222222222222222222"},
		{Key: "alice_1", Secret: "11111111111111111111111111111111", PreviousSecret: "99999999999999999999999999999999", PreviousSecretExpiresAt: tp("2027-01-01T00:00:00Z")},
	}, 50)
	got := strings.Join(UserKeys(data), ",")
	if got != "alice_1,alice_1_legacy,bob_2" {
		t.Fatalf("ключи: %s", got)
	}
	if keys := UserKeys(RenderUsersToml(nil, 50)); len(keys) != 0 {
		t.Fatalf("заглушка попала в ключи: %v", keys)
	}
}
