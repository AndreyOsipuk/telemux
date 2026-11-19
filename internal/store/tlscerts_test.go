package store

import (
	"errors"
	"slices"
	"testing"
)

// Список доменов релея: нормализуется (регистр, пробелы, повторы), звёздочки и
// мусор отвергаются - имя домена потом становится именем файла на машине.
func TestParseWebDomains(t *testing.T) {
	got, err := ParseWebDomains(" Web1.Example.com, web5.example.net ,web1.example.com,")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"web1.example.com", "web5.example.net"}; !slices.Equal(got, want) {
		t.Fatalf("ждали %v, получили %v", want, got)
	}
	if got, err := ParseWebDomains(""); err != nil || len(got) != 0 {
		t.Fatalf("пустой список - без ошибки и без доменов, получили %v, %v", got, err)
	}
	for _, bad := range []string{
		"*.example.com", "web1.example.com,*.example.net", "example", "../etc/passwd", "a/b.example.com",
		"web 1.example.com", "-web.example.com", "web-.example.com", "web..example.com", ".example.com",
		"example.com.", "xn--.example.com/x", "web_1.example.com", "203.0.113.5",
	} {
		if _, err := ParseWebDomains(bad); err == nil {
			t.Fatalf("%q принят", bad)
		}
	}
}

func TestValidDomain(t *testing.T) {
	for _, ok := range []string{"example.com", "web1.example.com", "a-b.example.net", "xn--80ak6aa92e.example.com"} {
		if !ValidDomain(ok) {
			t.Fatalf("%q отвергнут", ok)
		}
	}
	long := ""
	for len(long) < 260 {
		long += "abcdefghij."
	}
	for _, bad := range []string{"", "Example.com", "*.example.com", long + "com", "com"} {
		if ValidDomain(bad) {
			t.Fatalf("%q принят", bad)
		}
	}
}

// web_domains в настройках: приводятся к каноническому виду, у не-релея - ошибка.
func TestNormalizeSettings_WebDomains(t *testing.T) {
	s := map[string]string{SettingWebDomains: "Web2.example.com , web6.example.com"}
	if err := normalizeSettings("relay", s); err != nil {
		t.Fatal(err)
	}
	if s[SettingWebDomains] != "web2.example.com,web6.example.com" {
		t.Fatalf("не канонический вид: %q", s[SettingWebDomains])
	}
	if err := normalizeSettings("relay", map[string]string{SettingWebDomains: "*.example.com"}); !errors.Is(err, ErrBadSettings) {
		t.Fatalf("звёздочка: ждали ErrBadSettings, получили %v", err)
	}
	if err := normalizeSettings("exit", map[string]string{SettingWebDomains: "web2.example.com"}); !errors.Is(err, ErrBadSettings) {
		t.Fatalf("web_domains у выхода: ждали ErrBadSettings, получили %v", err)
	}
	empty := map[string]string{SettingWebDomains: " , "}
	if err := normalizeSettings("relay", empty); err != nil {
		t.Fatal(err)
	}
	if _, ok := empty[SettingWebDomains]; ok {
		t.Fatal("пустой список доменов должен убираться из настроек")
	}
	if err := normalizeSettings("panel", map[string]string{"node_id": "1"}); err != nil {
		t.Fatalf("прочие настройки не трогаются: %v", err)
	}
}
