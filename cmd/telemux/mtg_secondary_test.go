package main

import "testing"

func TestValidateSecondaryMtg(t *testing.T) {
	for _, tc := range []struct {
		file, unit, stats string
		ok                bool
	}{
		{"", "", "", true},
		{"/etc/mtg/s6.toml", "mtg-s6", "http://localhost:8082/stats", true},
		{"/etc/mtg/s6.toml", "", "http://127.0.0.1:8082/stats", false},
		{"/etc/mtg/s6.toml", "mtg-multi.service", "http://127.0.0.1:8082/stats", false},
		{"/etc/mtg/s6.toml", "mtg-s6", "http://localhost:8081/stats", false},
		{"/etc/mtg/s6.toml", "mtg-s6", "http://example.org:8082/stats", false},
	} {
		err := validateSecondaryMtg(tc.file, tc.unit, tc.stats, "mtg-multi", "http://127.0.0.1:8081/stats")
		if (err == nil) != tc.ok {
			t.Errorf("ошибка проверки второго экземпляра для %+v: %v", tc, err)
		}
	}
}
