package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestTargetCanDeclarePlannedMaintenance(t *testing.T) {
	s, ops, _, _ := opsServer(false)
	now := time.Now()
	until := now.Add(5 * time.Minute).UTC().Format(time.RFC3339Nano)
	for _, item := range []struct {
		name     string
		target   string
		deadline string
		want     int
	}{
		{"релей открывает окно", "pr2", until, http.StatusOK},
		{"выход открывает окно", "ps2", until, http.StatusOK},
		{"панель не участвует", "pan1", until, http.StatusBadRequest},
		{"неверный срок", "ps2", "tomorrow", http.StatusBadRequest},
		{"прошедший срок", "ps2", now.Add(-time.Minute).Format(time.RFC3339), http.StatusBadRequest},
		{"долгое окно", "ps2", now.Add(16 * time.Minute).Format(time.RFC3339), http.StatusBadRequest},
		{"очистка окна выхода", "ps2", "", http.StatusOK},
	} {
		t.Run(item.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"settings": map[string]string{"maintenance_until": item.deadline}})
			if err != nil {
				t.Fatal(err)
			}
			rec := do(s, "PUT", "/api/v1/targets/"+item.target, botToken, string(body))
			if rec.Code != item.want {
				t.Fatalf("HTTP %d, ждали %d: %s", rec.Code, item.want, rec.Body.String())
			}
			if item.want == http.StatusOK && ops.targets[item.target].Settings["maintenance_until"] != item.deadline {
				t.Fatalf("срок не сохранился: %+v", ops.targets[item.target].Settings)
			}
		})
	}
}
