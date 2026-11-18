package patroni

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newServer поднимает фейковый Patroni, отвечающий на path кодом code и телом body.
func newServer(t *testing.T, path string, code int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStatusIsLeader_Roles(t *testing.T) {
	tests := []struct {
		name  string
		role  string
		state string
		want  bool
	}{
		{"primary running - лидер", "primary", "running", true},
		{"master running (старый Patroni) - лидер", "master", "running", true},
		{"replica - не лидер", "replica", "running", false},
		{"standby_leader - не лидер", "standby_leader", "running", false},
		{"primary starting - не лидер", "primary", "starting", false},
		{"primary stopped - не лидер", "primary", "stopped", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			if got := (Status{Role: tt.role, State: tt.state}).IsLeader(); got != tt.want {
				t.Fatalf("Status.IsLeader() = %v, ожидали %v", got, tt.want)
			}

			body := `{"state":"` + tt.state + `","role":"` + tt.role + `","timeline":3,"patroni":{"name":"ps1","scope":"telemux"}}`
			srv := newServer(t, "/patroni", http.StatusOK, body)
			c := New(srv.URL+"/", nil) // завершающий слеш должен обрезаться

			// Act
			got, err := c.IsLeader(context.Background())

			// Assert
			if err != nil {
				t.Fatalf("IsLeader: неожиданная ошибка: %v", err)
			}
			if got != tt.want {
				t.Fatalf("IsLeader = %v, ожидали %v", got, tt.want)
			}
		})
	}
}

func TestStatus_ParsesFields(t *testing.T) {
	srv := newServer(t, "/patroni", http.StatusOK,
		`{"state":"running","role":"primary","timeline":7,"patroni":{"name":"ps1","scope":"tm"}}`)

	st, err := New(srv.URL, nil).Status(context.Background())

	if err != nil {
		t.Fatalf("Status: неожиданная ошибка: %v", err)
	}
	if st.Timeline != 7 || st.Patroni.Name != "ps1" || st.Patroni.Scope != "tm" {
		t.Fatalf("Status разобран неверно: %+v", st)
	}
}

func TestStatus_ErrorsAndUnhealthy(t *testing.T) {
	tests := []struct {
		name       string
		code       int
		body       string
		wantErr    bool
		wantLeader bool
	}{
		{"503 с валидным JSON replica - без ошибки, не лидер", http.StatusServiceUnavailable,
			`{"state":"running","role":"replica"}`, false, false},
		{"503 с валидным JSON primary starting - без ошибки, не лидер", http.StatusServiceUnavailable,
			`{"state":"starting","role":"primary"}`, false, false},
		{"ответ не JSON - ошибка", http.StatusOK, `<html>bad gateway</html>`, true, false},
		{"пустое тело - ошибка", http.StatusOK, ``, true, false},
		{"JSON без role - ошибка", http.StatusOK, `{"state":"running"}`, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, "/patroni", tt.code, tt.body)
			c := New(srv.URL, nil)

			leader, err := c.IsLeader(context.Background())

			if tt.wantErr && err == nil {
				t.Fatalf("ожидали ошибку, получили nil (leader=%v)", leader)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if leader != tt.wantLeader {
				t.Fatalf("IsLeader = %v, ожидали %v", leader, tt.wantLeader)
			}
		})
	}
}

func TestIsLeader_Unreachable(t *testing.T) {
	// Arrange: поднимаем и сразу закрываем сервер - порт недоступен.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := New(url, &http.Client{Timeout: time.Second})

	// Act
	leader, err := c.IsLeader(context.Background())

	// Assert: fail-closed - ошибка и не лидер.
	if err == nil {
		t.Fatal("ожидали ошибку при недоступном Patroni")
	}
	if leader {
		t.Fatal("при недоступном Patroni узел не должен считаться лидером")
	}
}

func TestIsLeader_ContextCanceled(t *testing.T) {
	srv := newServer(t, "/patroni", http.StatusOK, `{"state":"running","role":"primary"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	leader, err := New(srv.URL, nil).IsLeader(ctx)

	if err == nil || leader {
		t.Fatalf("при отменённом контексте ожидали ошибку и не лидера, получили leader=%v err=%v", leader, err)
	}
}

func TestConfig_Parse(t *testing.T) {
	srv := newServer(t, "/config", http.StatusOK,
		`{"ttl":30,"loop_wait":10,"retry_timeout":10,"maximum_lag_on_failover":1048576}`)

	dc, err := New(srv.URL, nil).Config(context.Background())

	if err != nil {
		t.Fatalf("Config: неожиданная ошибка: %v", err)
	}
	want := DynamicConfig{TTL: 30, LoopWait: 10, RetryTimeout: 10}
	if dc != want {
		t.Fatalf("Config = %+v, ожидали %+v", dc, want)
	}
}

func TestConfig_Errors(t *testing.T) {
	t.Run("не JSON - ошибка", func(t *testing.T) {
		srv := newServer(t, "/config", http.StatusOK, "not json")
		if _, err := New(srv.URL, nil).Config(context.Background()); err == nil {
			t.Fatal("ожидали ошибку на не-JSON ответ")
		}
	})
	t.Run("недоступен - ошибка", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		if _, err := New(url, nil).Config(context.Background()); err == nil {
			t.Fatal("ожидали ошибку при недоступном Patroni")
		}
	})
}

func TestDynamicConfig_CheckTimings(t *testing.T) {
	tests := []struct {
		name     string
		dc       DynamicConfig
		deadline time.Duration
		wantN    int
		wantSub  []string
	}{
		{"дефолты Patroni и 14s - нарушений нет", DynamicConfig{TTL: 30, LoopWait: 10, RetryTimeout: 10}, 14 * time.Second, 0, nil},
		{"граница loop_wait+2*retry == ttl - нарушений нет", DynamicConfig{TTL: 30, LoopWait: 10, RetryTimeout: 10}, 29 * time.Second, 0, nil},
		{"retry_timeout=15 - нарушение демоута", DynamicConfig{TTL: 30, LoopWait: 10, RetryTimeout: 15}, 14 * time.Second, 1, []string{"loop_wait(10)", "retry_timeout(15)", "ttl(30)"}},
		{"fenceDeadline == ttl - нарушение", DynamicConfig{TTL: 30, LoopWait: 10, RetryTimeout: 10}, 30 * time.Second, 1, []string{"fenced"}},
		{"fenceDeadline > ttl - нарушение", DynamicConfig{TTL: 30, LoopWait: 10, RetryTimeout: 10}, 45 * time.Second, 1, []string{"fenced"}},
		{"оба нарушения сразу", DynamicConfig{TTL: 30, LoopWait: 10, RetryTimeout: 15}, 31 * time.Second, 2, []string{"loop_wait", "fenced"}},
		{"ttl=0 - нарушение", DynamicConfig{TTL: 0, LoopWait: 10, RetryTimeout: 10}, 14 * time.Second, 1, []string{"ttl не задан"}},
		{"ttl<0 - нарушение", DynamicConfig{TTL: -1}, time.Second, 1, []string{"ttl не задан"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.dc.CheckTimings(tt.deadline)

			if len(got) != tt.wantN {
				t.Fatalf("нарушений %d, ожидали %d: %q", len(got), tt.wantN, got)
			}
			joined := strings.Join(got, "\n")
			for _, sub := range tt.wantSub {
				if !strings.Contains(joined, sub) {
					t.Fatalf("в нарушениях нет %q: %q", sub, got)
				}
			}
		})
	}
}
