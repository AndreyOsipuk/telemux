package mtgmulti

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

const key32 = "a14d1a98c73415c674f76ecca9f62da9" // 32 hex = 16 байт

func ptr(s string) *string { return &s }

func TestSecretFor(t *testing.T) {
	got, err := SecretFor(key32, "static-de.proxy-osv.site")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	wantHost := hex.EncodeToString([]byte("static-de.proxy-osv.site"))
	want := "ee" + key32 + wantHost
	if got != want {
		t.Fatalf("secret\n got=%s\nwant=%s", got, want)
	}
	if !strings.HasPrefix(got, "ee") {
		t.Errorf("секрет должен начинаться с ee (FakeTLS-маркер)")
	}
}

func TestSecretFor_BadKey(t *testing.T) {
	if _, err := SecretFor("short", "d"); err == nil {
		t.Error("ожидалась ошибка на короткий ключ")
	}
	if _, err := SecretFor("zzzz1a98c73415c674f76ecca9f62da9", "d"); err == nil {
		t.Error("ожидалась ошибка на не-hex ключ")
	}
}

func TestSecretFor_PerNodeDomain(t *testing.T) {
	// Один ключ → разные секреты на разные ноды (домен в секрете).
	de, _ := SecretFor(key32, "static-de.proxy-osv.site")
	nl, _ := SecretFor(key32, "static-nl.proxy-osv.site")
	if de == nl {
		t.Error("секреты для разных доменов должны отличаться")
	}
	if !strings.Contains(de, key32) || !strings.Contains(nl, key32) {
		t.Error("общий ключ должен присутствовать в обоих секретах")
	}
}

func baseCfg() NodeCfg {
	return NodeCfg{
		BindTo:         "0.0.0.0:8767",
		APIBindTo:      "127.0.0.1:8081",
		FrontingDomain: "static-de.proxy-osv.site",
		PreferIP:       "only-ipv4",
		MaxConns:       2000,
	}
}

func TestGenerateConfig_Basic(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	users := []telemtsync.DesiredUser{
		{Username: "sub_2", Secret: key32},
		{Username: "sub_1", Secret: key32},
	}
	cfg, err := GenerateConfig(baseCfg(), users, now)
	if err != nil {
		t.Fatalf("ошибка: %v", err)
	}
	// Глобальные опции.
	for _, want := range []string{
		`bind-to = "0.0.0.0:8767"`,
		`prefer-ip = "only-ipv4"`,
		`api-bind-to = "127.0.0.1:8081"`,
		"max-connections = 2000",
		"enabled = false", // blocklist выключен
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("в конфиге нет %q", want)
		}
	}
	// Оба юзера присутствуют, отсортированы (sub_1 раньше sub_2).
	i1, i2 := strings.Index(cfg, `"sub_1"`), strings.Index(cfg, `"sub_2"`)
	if i1 < 0 || i2 < 0 {
		t.Fatal("оба юзера должны быть в [secrets]")
	}
	if i1 > i2 {
		t.Error("юзеры должны быть отсортированы (детерминизм)")
	}
}

func TestGenerateConfig_SecretsSectionLast(t *testing.T) {
	cfg, _ := GenerateConfig(baseCfg(), []telemtsync.DesiredUser{{Username: "sub_1", Secret: key32}}, time.Now())
	si := strings.Index(cfg, "[secrets]")
	if si < 0 {
		t.Fatal("нет секции [secrets]")
	}
	// После [secrets] не должно быть других [секций] (иначе ключи утекут в чужую таблицу).
	rest := cfg[si+len("[secrets]"):]
	if strings.Contains(rest, "[throttle]") || strings.Contains(rest, "[defense") {
		t.Error("[secrets] обязана быть последней секцией TOML")
	}
}

func TestGenerateConfig_ExpiredExcluded(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	users := []telemtsync.DesiredUser{
		{Username: "sub_live", Secret: key32, ExpirationRFC3339: ptr("2026-06-27T00:00:00Z")},  // в будущем
		{Username: "sub_dead", Secret: key32, ExpirationRFC3339: ptr("2026-06-25T00:00:00Z")},  // истёк
		{Username: "sub_forever", Secret: key32},                                               // бессрочный
	}
	cfg, _ := GenerateConfig(baseCfg(), users, now)
	if !strings.Contains(cfg, `"sub_live"`) {
		t.Error("живой юзер должен быть включён")
	}
	if !strings.Contains(cfg, `"sub_forever"`) {
		t.Error("бессрочный юзер должен быть включён")
	}
	if strings.Contains(cfg, `"sub_dead"`) {
		t.Error("истёкший юзер должен быть ИСКЛЮЧЁН")
	}
}

func TestGenerateConfig_Deterministic(t *testing.T) {
	now := time.Now()
	users := []telemtsync.DesiredUser{
		{Username: "sub_b", Secret: key32},
		{Username: "sub_a", Secret: key32},
	}
	a, _ := GenerateConfig(baseCfg(), users, now)
	b, _ := GenerateConfig(baseCfg(), users, now)
	if a != b {
		t.Error("одинаковый вход → одинаковый выход (для стабильного хэша diff)")
	}
}

func TestGenerateConfig_Validation(t *testing.T) {
	if _, err := GenerateConfig(NodeCfg{FrontingDomain: "d"}, nil, time.Now()); err == nil {
		t.Error("ожидалась ошибка на пустой BindTo")
	}
	if _, err := GenerateConfig(NodeCfg{BindTo: "0.0.0.0:8767"}, nil, time.Now()); err == nil {
		t.Error("ожидалась ошибка на пустой FrontingDomain")
	}
}

func TestGenerateConfig_DefaultPreferIP(t *testing.T) {
	c := baseCfg()
	c.PreferIP = ""
	cfg, _ := GenerateConfig(c, nil, time.Now())
	if !strings.Contains(cfg, `prefer-ip = "prefer-ipv6"`) {
		t.Error("дефолт prefer-ip = prefer-ipv6")
	}
}

func TestGenerateConfig_NoThrottleWhenZero(t *testing.T) {
	c := baseCfg()
	c.MaxConns = 0
	cfg, _ := GenerateConfig(c, nil, time.Now())
	if strings.Contains(cfg, "[throttle]") {
		t.Error("без MaxConns секции [throttle] быть не должно")
	}
}
