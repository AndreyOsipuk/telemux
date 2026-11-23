package servercfg

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name     string
		engine   string
		settings map[string]string
		want     error  // nil - без ошибки
		key      string // ключ в тексте ошибки
	}{
		{"пусто", EngineTelemt, nil, nil, ""},
		{"telemt из бота", EngineTelemt, map[string]string{"port": "443", "ipv6": "1", "relay_secret_mode": "ee"}, nil, ""},
		{"mtg из бота", EngineMtg, map[string]string{
			"FRONTING": "203.0.113.5:8466+pp", "RELAY_BIND": "0.0.0.0:2095, 0.0.0.0:2096", "RELAY_MODE": "local-mtg",
			"RELAY_PORT": "2095", "RELAY_PORT_ALT": "0", "RELAY_API_BIND": "127.0.0.1:8082", "RELAY_FRONTING": "127.0.0.1:8080",
			"RELAY_WEB_BIND": "127.0.0.1:18085", "RELAY_HOT_RELOAD": "1", "RELAY_TLS_DOMAIN": "static.example.com",
		}, nil, ""},
		{"второй процесс mtg", EngineMtg, map[string]string{"TELEMUX_RELAY_RESOURCE": "mtg.secondary.config", "RELAY_PROMETHEUS_BIND": "127.0.0.1:3130"}, nil, ""},
		{"чужой ресурс для mtg", EngineMtg, map[string]string{"TELEMUX_RELAY_RESOURCE": "telemt.users"}, ErrBadSetting, "TELEMUX_RELAY_RESOURCE"},
		{"битый порт метрик mtg", EngineMtg, map[string]string{"RELAY_PROMETHEUS_BIND": "127.0.0.1:0"}, ErrBadSetting, "RELAY_PROMETHEUS_BIND"},
		{"пустой RELAY_MODE", EngineMtg, map[string]string{"RELAY_MODE": ""}, nil, ""},
		{"неизвестный движок", "xray", map[string]string{}, ErrUnknownEngine, ""},
		{"неизвестный ключ", EngineTelemt, map[string]string{"nope": "1"}, ErrUnknownSetting, "nope"},
		{"ключ чужого движка", EngineTelemt, map[string]string{"BIND_PORT": "8767"}, ErrUnknownSetting, "BIND_PORT"},
		{"регистр ключа важен", EngineMtg, map[string]string{"bind_port": "8767"}, ErrUnknownSetting, "bind_port"},
		{"не целое", EngineTelemt, map[string]string{"port": "abc"}, ErrBadSetting, "port"},
		{"порт 0", EngineTelemt, map[string]string{"port": "0"}, ErrBadSetting, "port"},
		{"порт 65536", EngineMtg, map[string]string{"BIND_PORT": "65536"}, ErrBadSetting, "BIND_PORT"},
		{"bool мусор", EngineTelemt, map[string]string{"ipv6": "yes"}, ErrBadSetting, "ipv6"},
		{"enum мимо", EngineTelemt, map[string]string{"relay_secret_mode": "tls"}, ErrBadSetting, "relay_secret_mode"},
		{"rst_on_close мимо", EngineTelemt, map[string]string{"rst_on_close": "never"}, ErrBadSetting, "rst_on_close"},
		{"hostport без порта", EngineMtg, map[string]string{"API_BIND": "127.0.0.1"}, ErrBadSetting, "API_BIND"},
		{"hostport порт 0", EngineMtg, map[string]string{"RELAY_WEB_BIND": "127.0.0.1:0"}, ErrBadSetting, "RELAY_WEB_BIND"},
		{"список адресов с мусором", EngineMtg, map[string]string{"RELAY_BIND": "0.0.0.0:2095,nope"}, ErrBadSetting, "RELAY_BIND"},
		{"host со звёздочкой", EngineMtg, map[string]string{"RELAY_TLS_DOMAIN": "*.example.com"}, ErrBadSetting, "RELAY_TLS_DOMAIN"},
		{"перевод строки", EngineMtg, map[string]string{"RELOAD_CMD": "a\nb"}, ErrBadSetting, "RELOAD_CMD"},
		{"слишком длинное", EngineMtg, map[string]string{"RELOAD_CMD": strings.Repeat("x", 2000)}, ErrBadSetting, "RELOAD_CMD"},
		{"floor > ceiling", EngineTelemt, map[string]string{"mask_timing_floor_ms": "400"}, ErrBadSetting, "mask_timing_floor_ms"},
		{"soft > hard", EngineTelemt, map[string]string{"relay_idle_soft_secs": "700", "relay_idle_hard_secs": "600"}, ErrBadSetting, "relay_idle_soft_secs"},
		{"hello min > max", EngineTelemt, map[string]string{"server_hello_delay_min_ms": "200"}, ErrBadSetting, "server_hello_delay_min_ms"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.engine, c.settings)
			if c.want == nil {
				if err != nil {
					t.Fatalf("ждали без ошибки, получили %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("ждали %v, получили %v", c.want, err)
			}
			if c.key != "" && !strings.Contains(err.Error(), c.key) {
				t.Fatalf("в ошибке нет ключа %q: %v", c.key, err)
			}
		})
	}
}

// bool приводится к "0"/"1", пробелы по краям убираются, исходная карта не меняется.
func TestNormalize(t *testing.T) {
	in := map[string]string{"ipv6": "true", "port": " 443 "}
	out, err := Normalize(EngineTelemt, in)
	if err != nil {
		t.Fatal(err)
	}
	if out["ipv6"] != "1" || out["port"] != "443" {
		t.Fatalf("нормализация: %v", out)
	}
	if in["ipv6"] != "true" {
		t.Fatal("исходная карта изменилась")
	}
	out, err = Normalize(EngineMtg, map[string]string{"HOT_RELOAD": "FALSE"})
	if err != nil || out["HOT_RELOAD"] != "0" {
		t.Fatalf("FALSE → 0: %v %v", out, err)
	}
}

// Каталог согласован: значение по умолчанию проходит свою же проверку, у enum
// есть варианты, способ применения и группа заданы, ключи не повторяются.
func TestCatalogConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range Fields("") {
		id := f.Engine + "/" + f.Key
		if seen[id] {
			t.Fatalf("%s повторяется", id)
		}
		seen[id] = true
		if !ValidEngine(f.Engine) || f.Group == "" || f.TitleRU == "" || f.TitleEN == "" {
			t.Fatalf("%s: не заполнены движок, группа или заголовки", id)
		}
		switch f.Apply {
		case ApplyInstant, ApplyReload, ApplyRestart, ApplyFirewall:
		default:
			t.Fatalf("%s: способ применения %q", id, f.Apply)
		}
		if f.Type == TypeEnum && len(f.Enum) == 0 {
			t.Fatalf("%s: enum без вариантов", id)
		}
		if f.Default != "" {
			if _, err := f.normalize(f.Default); err != nil {
				t.Fatalf("%s: значение по умолчанию %q не проходит проверку: %v", id, f.Default, err)
			}
		}
	}
	if len(Fields(EngineTelemt)) == 0 || len(Fields(EngineMtg)) == 0 || len(Fields("xray")) != 0 {
		t.Fatal("фильтр по движку")
	}
	for _, k := range []string{"relay_secret_mode", "port"} {
		if f, _ := Lookup(EngineTelemt, k); !f.Danger {
			t.Fatalf("%s должен быть помечен danger", k)
		}
	}
	if f, _ := Lookup(EngineMtg, "RELAY_TLS_DOMAIN"); !f.Danger {
		t.Fatal("RELAY_TLS_DOMAIN должен быть помечен danger")
	}
	if f, _ := Lookup(EngineTelemt, "flood_mode"); f.Apply != ApplyFirewall {
		t.Fatal("flood_* применяются файрволом")
	}
}

func TestValidHost(t *testing.T) {
	for h, want := range map[string]bool{
		"example.com": true, "Example.COM": true, "203.0.113.1": true, "2001:db8::1": true, "[2001:db8::1]": true,
		"": false, "localhost": false, "*.example.com": false, "exa mple.com": false, "-a.example.com": false, "1.2.3": false,
	} {
		if got := ValidHost(h); got != want {
			t.Errorf("ValidHost(%q) = %v, ждали %v", h, got, want)
		}
	}
}
