package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Файл импорта - массив ServerWrite; объект вместо массива и мусор - ошибка.
func TestReadServersFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	items, err := readServersFile(write("ok.json",
		`[{"no":1,"name":"Финляндия","engine":"telemt","relay_code":"pr1","settings":{"port":"443"},
		   "endpoints":[{"protocol":"dd","host":"pr1.example.net","port":2091}]}]`))
	if err != nil || len(items) != 1 || items[0].No != 1 || *items[0].Endpoints[0].Port != 2091 || items[0].Settings["port"] != "443" {
		t.Fatalf("разбор: %+v %v", items, err)
	}
	for _, body := range []string{`{"no":1}`, `[{`, ``} {
		if _, err := readServersFile(write("bad.json", body)); err == nil {
			t.Fatalf("%q: ждали ошибку", body)
		}
	}
	if _, err := readServersFile(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("нет файла: ждали ошибку")
	}
	if code := runServers([]string{"export"}); code != 2 {
		t.Fatalf("неизвестная подкоманда: код %d", code)
	}
}
