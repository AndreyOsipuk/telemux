package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTelemtLocal(t *testing.T) {
	base := "[server]\nport = 443\n\n[access]\ninclude = \"/etc/telemt/users.toml\"\n"
	cur := "[server]\nport = 8453\n\n[access]\ninclude = \"/etc/telemt/users.toml\"\n\n[web]\nlisten = \"127.0.0.1:18080\"\n"
	var out, errw bytes.Buffer
	if code := telemtLocal(base, cur, &out, &errw); code != 0 || errw.Len() != 0 {
		t.Fatalf("код %d, ошибки %q", code, errw.String())
	}
	if got := out.String(); !strings.Contains(got, "port = 8453") || !strings.Contains(got, "[web]") || strings.Contains(got, "include") {
		t.Fatalf("машинная часть: %q", got)
	}
	out.Reset()
	if code := telemtLocal(base, "[server]\nport = 443\n", &out, &errw); code != 1 || !strings.Contains(errw.String(), "[access]") {
		t.Fatalf("удалённая на машине секция не замечена: код %d, %q", code, errw.String())
	}
}
